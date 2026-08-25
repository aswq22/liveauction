$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$deployDir = Join-Path $root 'live-auction-bid-backend\deploy'
$composeFile = Join-Path $deployDir 'docker-compose.loadtest.yml'

Push-Location $deployDir
try {
  docker compose -p live-auction-loadtest -f $composeFile down --remove-orphans
} finally {
  Pop-Location
}

Write-Host 'Docker loadtest stack stopped. Add -v manually if you want to delete loadtest volumes.'
