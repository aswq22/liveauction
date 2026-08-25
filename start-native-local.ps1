param(
  [switch]$SkipInstall,
  [switch]$NoBrowser
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$backendDir = Join-Path $root 'live-auction-bid-backend'
$merchantDir = Join-Path $root 'live-auction-bid-frontend'
$h5Dir = Join-Path $root 'live-auction-user-h5'
$logsDir = Join-Path $root 'logs'
$runDir = Join-Path $root '.local-run'
$assetDir = Join-Path $root 'native-local-assets'

$mysqlBin = 'D:\Program Files\MySQL\MySQL Server 8.0\bin'
$redisServer = 'D:\Program Files\Redis\redis-server.exe'
$consulExe = 'D:\consul\consul.exe'

$mysqlPort = 3306
$redisPort = 6379
$rabbitMQPort = 5672
$consulPort = 8500
$backendPort = 18080
$rabbitMQUrl = $env:AUCTION_REALTIME_BUS_RABBITMQ_URL
if ([string]::IsNullOrWhiteSpace($rabbitMQUrl)) {
  $rabbitMQUrl = "amqp://guest:guest@127.0.0.1:$rabbitMQPort/"
}

New-Item -ItemType Directory -Force -Path $logsDir, $runDir, $assetDir | Out-Null

function Require-File($path, $label) {
  if (-not (Test-Path -LiteralPath $path)) {
    throw "$label not found: $path"
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

function Test-TcpPort($port) {
  $client = [Net.Sockets.TcpClient]::new()
  try {
    $iar = $client.BeginConnect('127.0.0.1', $port, $null, $null)
    if (-not $iar.AsyncWaitHandle.WaitOne(500, $false)) { return $false }
    $client.EndConnect($iar)
    return $true
  } catch {
    return $false
  } finally {
    $client.Close()
  }
}

function Wait-MySQL($timeoutSeconds) {
  $deadline = (Get-Date).AddSeconds($timeoutSeconds)
  do {
    $oldPreference = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    & (Join-Path $mysqlBin 'mysql.exe') --protocol=tcp --host=127.0.0.1 --port=$mysqlPort --user=root --skip-password -e "SELECT 1" *> $null
    $code = $LASTEXITCODE
    $ErrorActionPreference = $oldPreference
    if ($code -eq 0) { return }
    Start-Sleep -Seconds 2
  } while ((Get-Date) -lt $deadline)
  throw "Native MySQL did not become ready on 127.0.0.1:$mysqlPort"
}

function Start-RabbitMQIfAvailable($timeoutSeconds) {
  if (Test-TcpPort $rabbitMQPort) {
    return
  }

  $service = Get-Service -ErrorAction SilentlyContinue |
    Where-Object { $_.Name -like '*Rabbit*' -or $_.DisplayName -like '*Rabbit*' } |
    Select-Object -First 1
  if ($null -eq $service) {
    throw "RabbitMQ is not listening on 127.0.0.1:$rabbitMQPort and no RabbitMQ Windows service was found. Install/start RabbitMQ locally, or set AUCTION_REALTIME_BUS_RABBITMQ_URL to an available broker."
  }

  if ($service.Status -ne 'Running') {
    Write-Host "Starting RabbitMQ service: $($service.Name)"
    Start-Service -Name $service.Name
  }

  $deadline = (Get-Date).AddSeconds($timeoutSeconds)
  do {
    if (Test-TcpPort $rabbitMQPort) {
      Write-Host "RabbitMQ is reachable: 127.0.0.1:$rabbitMQPort"
      return
    }
    Start-Sleep -Seconds 2
  } while ((Get-Date) -lt $deadline)

  throw "RabbitMQ did not become ready on 127.0.0.1:$rabbitMQPort within $timeoutSeconds seconds."
}

Require-File (Join-Path $mysqlBin 'mysqld.exe') 'mysqld.exe'
Require-File (Join-Path $mysqlBin 'mysql.exe') 'mysql.exe'
Require-File $redisServer 'redis-server.exe'
Require-File $consulExe 'consul.exe'

if (Test-TcpPort $backendPort) {
  throw "Port $backendPort is already in use. Stop the Docker/backend process first, then rerun this script."
}

if (-not (Test-TcpPort $mysqlPort)) {
  throw "Local MySQL80 is not listening on 127.0.0.1:$mysqlPort. Start the MySQL80 service first."
}
$env:MYSQL_PWD = 'auction_dev'
try {
  & (Join-Path $mysqlBin 'mysql.exe') --protocol=tcp --host=127.0.0.1 --port=$mysqlPort --user=auction live_auction -e "SELECT 1" *> $null
  if ($LASTEXITCODE -ne 0) {
    throw "Cannot connect to local MySQL as auction/auction_dev or database live_auction is missing."
  }
} finally {
  Remove-Item Env:\MYSQL_PWD -ErrorAction SilentlyContinue
}

if (-not (Test-TcpPort $redisPort)) {
  $redisOutLog = Join-Path $logsDir 'native-redis.out.log'
  $redisErrLog = Join-Path $logsDir 'native-redis.err.log'
  $redis = Start-Process -FilePath $redisServer -ArgumentList @('--port', "$redisPort") -RedirectStandardOutput $redisOutLog -RedirectStandardError $redisErrLog -WindowStyle Hidden -PassThru
  $redis.Id | Set-Content -LiteralPath (Join-Path $runDir 'native-redis.pid') -Encoding ascii
}

if (-not (Test-TcpPort $consulPort)) {
  $consulOutLog = Join-Path $logsDir 'native-consul.out.log'
  $consulErrLog = Join-Path $logsDir 'native-consul.err.log'
  $consul = Start-Process -FilePath $consulExe -ArgumentList @('agent', '-dev', '-client=127.0.0.1') -RedirectStandardOutput $consulOutLog -RedirectStandardError $consulErrLog -WindowStyle Hidden -PassThru
  $consul.Id | Set-Content -LiteralPath (Join-Path $runDir 'native-consul.pid') -Encoding ascii
}

Start-RabbitMQIfAvailable 90

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

$backendLog = Join-Path $logsDir 'native-backend.log'
$backendCommand = @"
`$env:GOTOOLCHAIN='go1.22.12'
`$env:AUCTION_HTTP_ADDR=':$backendPort'
`$env:AUCTION_MYSQL_DSN='auction:auction_dev@tcp(127.0.0.1:$mysqlPort)/live_auction?parseTime=true&charset=utf8mb4&loc=Local'
`$env:AUCTION_REDIS_ADDR='127.0.0.1:$redisPort'
`$env:AUCTION_REDIS_PASSWORD='__NO_PASSWORD__'
`$env:AUCTION_REDIS_STREAM_REQUIRED='false'
`$env:AUCTION_REALTIME_BUS='rabbitmq'
`$env:AUCTION_REALTIME_BUS_RABBITMQ_URL='$rabbitMQUrl'
`$env:AUCTION_REALTIME_BUS_RABBITMQ_EXCHANGE='auction.events'
`$env:AUCTION_REALTIME_BUS_RABBITMQ_ROUTING_KEY='auction.realtime.event'
`$env:AUCTION_REALTIME_BUS_RABBITMQ_QUEUE='auction.realtime.websocket'
`$env:AUCTION_REALTIME_BUS_RABBITMQ_PREFETCH='100'
`$env:AUCTION_CONSUL_ADDR='127.0.0.1:$consulPort'
`$env:AUCTION_SERVICE_NAME='auction-backend-native'
`$env:AUCTION_SERVICE_ADDR='127.0.0.1'
`$env:AUCTION_JWT_SECRET='local-dev-jwt-secret-change-me'
`$env:AUCTION_JWT_ISSUER='auction-backend'
Remove-Item Env:\AUCTION_BOOTSTRAP_MAIN_ACCOUNT_USERNAME -ErrorAction SilentlyContinue
Remove-Item Env:\AUCTION_BOOTSTRAP_MAIN_ACCOUNT_PASSWORD -ErrorAction SilentlyContinue
Remove-Item Env:\AUCTION_BOOTSTRAP_MAIN_ACCOUNT_NICKNAME -ErrorAction SilentlyContinue
`$env:AUCTION_SEED_RBAC_DEFAULTS='false'
`$env:AUCTION_SEED_SHOP_DEFAULTS='true'
`$env:AUCTION_RUNTIME_PROJECTION_WORKER_ENABLED='false'
`$env:AUCTION_STORAGE_PROVIDER='local'
`$env:AUCTION_LOCAL_STORAGE_DIR='$assetDir'
`$env:AUCTION_LOCAL_STORAGE_BUCKET='local'
`$env:AUCTION_LOCAL_STORAGE_PUBLIC_BASE_URL='http://localhost:$backendPort/assets'
`$env:AUCTION_AI_PROVIDER='mock'
`$env:AUCTION_AI_BASE_URL=''
`$env:AUCTION_AI_MODEL=''
`$env:AUCTION_AI_TIMEOUT='12s'
`$env:AUCTION_AI_MOCK_ON_FAILURE='true'
`$env:AUCTION_AI_API_KEY=''
`$env:AUCTION_PAYMENT_PROVIDER='mock'
`$env:AUCTION_SEARCH_PROVIDER=''
Set-Location '$backendDir'
go run ./app/auction/service/cmd/server *> '$backendLog'
"@
$backend = Start-Process -FilePath 'powershell.exe' -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-Command', $backendCommand) -WindowStyle Hidden -PassThru
$backend.Id | Set-Content -LiteralPath (Join-Path $runDir 'native-backend.pid') -Encoding ascii

Wait-HttpOk "http://127.0.0.1:$backendPort/healthz" 'Native backend health' 180
Wait-HttpOk "http://127.0.0.1:$backendPort/readyz" 'Native backend readiness' 180

$merchantLog = Join-Path $logsDir 'merchant-vite.log'
$h5Log = Join-Path $logsDir 'h5-vite.log'

if (-not (Test-TcpPort 5173)) {
  $merchant = Start-Process -FilePath 'powershell.exe' -ArgumentList @(
    '-NoProfile',
    '-ExecutionPolicy', 'Bypass',
    '-Command',
    "Set-Location '$merchantDir'; npm.cmd run dev -- --host 0.0.0.0 --port 5173 *> '$merchantLog'"
  ) -WindowStyle Hidden -PassThru
  $merchant.Id | Set-Content -LiteralPath (Join-Path $runDir 'merchant.pid') -Encoding ascii
}

if (-not (Test-TcpPort 5174)) {
  $h5 = Start-Process -FilePath 'powershell.exe' -ArgumentList @(
    '-NoProfile',
    '-ExecutionPolicy', 'Bypass',
    '-Command',
    "Set-Location '$h5Dir'; npm.cmd run dev -- --host 0.0.0.0 --port 5174 *> '$h5Log'"
  ) -WindowStyle Hidden -PassThru
  $h5.Id | Set-Content -LiteralPath (Join-Path $runDir 'h5.pid') -Encoding ascii
}

Wait-HttpOk 'http://127.0.0.1:5173' 'Merchant console' 90
Wait-HttpOk 'http://127.0.0.1:5174' 'User H5' 90

if (-not $NoBrowser) {
  Start-Process 'http://127.0.0.1:5173'
  Start-Sleep -Seconds 1
  Start-Process 'http://127.0.0.1:5174'
}

Write-Host ''
Write-Host 'Native local LiveAuction is running without Docker deployment.'
Write-Host "Local MySQL80:    127.0.0.1:$mysqlPort / live_auction / auction / auction_dev"
Write-Host "RabbitMQ:         $rabbitMQUrl"
Write-Host "Backend API:      http://127.0.0.1:$backendPort"
Write-Host 'Merchant console: http://127.0.0.1:5173'
Write-Host 'User H5:          http://127.0.0.1:5174'
Write-Host 'Stop command:     powershell -ExecutionPolicy Bypass -File .\stop-native-local.ps1'
