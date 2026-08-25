param(
  [switch]$SkipInstall,
  [switch]$NoBrowser
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$backendDeploy = Join-Path $root 'live-auction-bid-backend\deploy'
$merchantDir = Join-Path $root 'live-auction-bid-frontend'
$h5Dir = Join-Path $root 'live-auction-user-h5'
$logsDir = Join-Path $root 'logs'
$runDir = Join-Path $root '.local-run'
$envFile = Join-Path $backendDeploy '.env.local'

New-Item -ItemType Directory -Force -Path $logsDir, $runDir, (Join-Path $backendDeploy 'local-assets') | Out-Null

function Require-Command($name) {
  if (-not (Get-Command $name -ErrorAction SilentlyContinue)) {
    throw "Required command '$name' was not found in PATH."
  }
}

function Wait-HttpOk($url, $name, $timeoutSeconds) {
  $deadline = (Get-Date).AddSeconds($timeoutSeconds)
  do {
    try {
      $response = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 3
      if ($response.StatusCode -ge 200 -and $response.StatusCode -lt 500) {
        Write-Host "$name is reachable: $url"
        return
      }
    } catch {
      Start-Sleep -Seconds 2
    }
  } while ((Get-Date) -lt $deadline)
  throw "$name did not become reachable at $url within $timeoutSeconds seconds."
}

Require-Command docker
Require-Command npm.cmd

if (-not (Test-Path -LiteralPath $envFile)) {
@'
AUCTION_STORAGE_PROVIDER=local
AUCTION_LOCAL_STORAGE_DIR=/data/live-auction-assets
AUCTION_LOCAL_STORAGE_BUCKET=local
AUCTION_LOCAL_STORAGE_PUBLIC_BASE_URL=http://localhost:18080/assets
AUCTION_AI_PROVIDER=mock
AUCTION_AI_BASE_URL=
AUCTION_AI_MODEL=
AUCTION_AI_TIMEOUT=12s
AUCTION_AI_MOCK_ON_FAILURE=true
AUCTION_AI_API_KEY=
AUCTION_PAYMENT_PROVIDER=mock
AUCTION_SEARCH_PROVIDER=
AUCTION_EMBEDDING_API_KEY=
'@ | Set-Content -LiteralPath $envFile -Encoding utf8
}

if (-not $SkipInstall) {
  if (-not (Test-Path -LiteralPath (Join-Path $merchantDir 'node_modules'))) {
    Push-Location $merchantDir
    try { npm.cmd ci } finally { Pop-Location }
  }
  if (-not (Test-Path -LiteralPath (Join-Path $h5Dir 'node_modules'))) {
    Push-Location $h5Dir
    try { npm.cmd ci } finally { Pop-Location }
  }
}

Push-Location $backendDeploy
try {
  docker compose --env-file $envFile up -d --build
  if ($LASTEXITCODE -ne 0) {
    throw "docker compose up failed with exit code $LASTEXITCODE"
  }
} finally {
  Pop-Location
}

Wait-HttpOk 'http://127.0.0.1:18080/healthz' 'Backend health' 180
Wait-HttpOk 'http://127.0.0.1:18080/readyz' 'Backend readiness' 180

$merchantLog = Join-Path $logsDir 'merchant-vite.log'
$h5Log = Join-Path $logsDir 'h5-vite.log'

$merchant = Start-Process -FilePath 'powershell.exe' -ArgumentList @(
  '-NoProfile',
  '-ExecutionPolicy', 'Bypass',
  '-Command',
  "Set-Location '$merchantDir'; npm.cmd run dev -- --host 0.0.0.0 --port 5173 *> '$merchantLog'"
) -WindowStyle Hidden -PassThru
$merchant.Id | Set-Content -LiteralPath (Join-Path $runDir 'merchant.pid') -Encoding ascii

$h5 = Start-Process -FilePath 'powershell.exe' -ArgumentList @(
  '-NoProfile',
  '-ExecutionPolicy', 'Bypass',
  '-Command',
  "Set-Location '$h5Dir'; npm.cmd run dev -- --host 0.0.0.0 --port 5174 *> '$h5Log'"
) -WindowStyle Hidden -PassThru
$h5.Id | Set-Content -LiteralPath (Join-Path $runDir 'h5.pid') -Encoding ascii

Wait-HttpOk 'http://127.0.0.1:5173' 'Merchant console' 90
Wait-HttpOk 'http://127.0.0.1:5174' 'User H5' 90

if (-not $NoBrowser) {
  Start-Process 'http://127.0.0.1:5173'
  Start-Sleep -Seconds 1
  Start-Process 'http://127.0.0.1:5174'
}

Write-Host ''
Write-Host 'Local LiveAuction is running.'
Write-Host 'Merchant console: http://127.0.0.1:5173'
Write-Host 'User H5:          http://127.0.0.1:5174'
Write-Host 'Backend API:      http://127.0.0.1:18080'
Write-Host 'Stop command:     powershell -ExecutionPolicy Bypass -File .\stop-local.ps1'
