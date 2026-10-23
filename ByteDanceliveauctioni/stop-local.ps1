$ErrorActionPreference = 'Continue'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$backendDeploy = Join-Path $root 'live-auction-bid-backend\deploy'
$runDir = Join-Path $root '.local-run'
$envFile = Join-Path $backendDeploy '.env.local'

foreach ($name in @('merchant', 'h5')) {
  $pidFile = Join-Path $runDir "$name.pid"
  if (Test-Path -LiteralPath $pidFile) {
    $rawPid = (Get-Content -LiteralPath $pidFile -ErrorAction SilentlyContinue | Select-Object -First 1)
    if ($rawPid -match '^\d+$') {
      Stop-Process -Id ([int]$rawPid) -Force -ErrorAction SilentlyContinue
      Write-Host "Stopped $name dev server process $rawPid"
    }
    Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
  }
}

Push-Location $backendDeploy
try {
  if (Test-Path -LiteralPath $envFile) {
    docker compose --env-file $envFile down
  } else {
    docker compose down
  }
} finally {
  Pop-Location
}

Write-Host 'Local LiveAuction services stopped.'
