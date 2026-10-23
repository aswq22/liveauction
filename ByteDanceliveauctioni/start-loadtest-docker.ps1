param(
  [switch]$Reset,
  [switch]$NoBuild
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$backendDir = Join-Path $root 'live-auction-bid-backend'
$deployDir = Join-Path $root 'live-auction-bid-backend\deploy'
$composeFile = Join-Path $deployDir 'docker-compose.loadtest.yml'
$binDir = Join-Path $backendDir '.loadtest-bin'
$linuxBinary = Join-Path $binDir 'auction-service-linux'

if (-not (Test-Path -LiteralPath $composeFile)) {
  throw "Loadtest compose file not found: $composeFile"
}

if (-not $NoBuild) {
  New-Item -ItemType Directory -Force -Path $binDir | Out-Null
  Push-Location $backendDir
  try {
    $oldGOOS = $env:GOOS
    $oldGOARCH = $env:GOARCH
    $oldCGO = $env:CGO_ENABLED
    $env:GOOS = 'linux'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    go build -o $linuxBinary ./app/auction/service/cmd/server
    if ($LASTEXITCODE -ne 0) {
      throw 'Failed to build Linux loadtest backend binary.'
    }
  } finally {
    if ($null -eq $oldGOOS) { Remove-Item Env:\GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $oldGOOS }
    if ($null -eq $oldGOARCH) { Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $oldGOARCH }
    if ($null -eq $oldCGO) { Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $oldCGO }
    Pop-Location
  }
}

Push-Location $deployDir
try {
  if ($Reset) {
    docker compose -p live-auction-loadtest -f $composeFile down -v --remove-orphans
  }
  if ($NoBuild) {
    docker compose -p live-auction-loadtest -f $composeFile up -d
  } else {
    docker compose -p live-auction-loadtest -f $composeFile up -d --build
  }
} finally {
  Pop-Location
}

$deadline = (Get-Date).AddMinutes(5)
do {
  try {
    $response = Invoke-WebRequest -Uri 'http://127.0.0.1:18081/readyz' -UseBasicParsing -TimeoutSec 3
    if ($response.StatusCode -ge 200 -and $response.StatusCode -lt 500) {
      Write-Host 'Docker loadtest backend is ready: http://127.0.0.1:18081'
      Write-Host 'RabbitMQ management: http://127.0.0.1:25673 (auction_loadtest / auction_mq_loadtest)'
      Write-Host 'MySQL: 127.0.0.1:23306 / live_auction_loadtest / auction / auction_dev'
      exit 0
    }
  } catch {
    Start-Sleep -Seconds 3
  }
} while ((Get-Date) -lt $deadline)

throw 'Docker loadtest backend did not become ready within 5 minutes.'
