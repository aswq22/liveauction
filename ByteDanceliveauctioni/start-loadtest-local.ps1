param(
  [switch]$ResetDatabase
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$backendDir = Join-Path $root 'live-auction-bid-backend'
$logsDir = Join-Path $root 'logs'
$runDir = Join-Path $root '.loadtest-run'
$assetDir = Join-Path $root 'native-loadtest-assets'
$redisDataDir = Join-Path $runDir 'redis-data'

$mysqlBin = 'D:\Program Files\MySQL\MySQL Server 8.0\bin'
$redisServer = 'D:\Program Files\Redis\redis-server.exe'
$consulExe = 'D:\consul\consul.exe'

$mysqlPort = 3306
$redisPort = 6380
$rabbitMQPort = 5672
$consulPort = 8500
$backendPort = 18081
$databaseName = 'live_auction_loadtest'
$rabbitMQUrl = $env:AUCTION_LOADTEST_RABBITMQ_URL
if ([string]::IsNullOrWhiteSpace($rabbitMQUrl)) {
  $rabbitMQUrl = "amqp://guest:guest@127.0.0.1:$rabbitMQPort/"
}

New-Item -ItemType Directory -Force -Path $logsDir, $runDir, $assetDir, $redisDataDir | Out-Null

function Require-File($path, $label) {
  if (-not (Test-Path -LiteralPath $path)) {
    throw "$label not found: $path"
  }
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

function Invoke-NativeCommand($scriptBlock) {
  $oldPreference = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try {
    & $scriptBlock
    return $LASTEXITCODE
  } finally {
    $ErrorActionPreference = $oldPreference
  }
}

function Start-RabbitMQIfAvailable($timeoutSeconds) {
  if (Test-TcpPort $rabbitMQPort) {
    return
  }

  $service = Get-Service -ErrorAction SilentlyContinue |
    Where-Object { $_.Name -like '*Rabbit*' -or $_.DisplayName -like '*Rabbit*' } |
    Select-Object -First 1
  if ($null -eq $service) {
    throw "RabbitMQ is not listening on 127.0.0.1:$rabbitMQPort and no RabbitMQ Windows service was found."
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

Require-File (Join-Path $mysqlBin 'mysql.exe') 'mysql.exe'
Require-File $redisServer 'redis-server.exe'
Require-File $consulExe 'consul.exe'

if (Test-TcpPort $backendPort) {
  throw "Port $backendPort is already in use. Stop the loadtest backend first, then rerun this script."
}
if (-not (Test-TcpPort $mysqlPort)) {
  throw "Local MySQL80 is not listening on 127.0.0.1:$mysqlPort. Start the MySQL80 service first."
}

$mysql = Join-Path $mysqlBin 'mysql.exe'
$databaseSql = "CREATE DATABASE IF NOT EXISTS $databaseName CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci; GRANT ALL PRIVILEGES ON $databaseName.* TO 'auction'@'%'; FLUSH PRIVILEGES;"
if ($ResetDatabase) {
  $databaseSql = "DROP DATABASE IF EXISTS $databaseName; $databaseSql"
}
$mysqlExitCode = Invoke-NativeCommand { & $mysql --protocol=tcp --host=127.0.0.1 --port=$mysqlPort --user=root --skip-password -e $databaseSql *> $null }
if ($mysqlExitCode -ne 0 -and -not [string]::IsNullOrWhiteSpace($env:MYSQL_ROOT_PWD)) {
  $env:MYSQL_PWD = $env:MYSQL_ROOT_PWD
  try {
    $mysqlExitCode = Invoke-NativeCommand { & $mysql --protocol=tcp --host=127.0.0.1 --port=$mysqlPort --user=root -e $databaseSql *> $null }
  } finally {
    Remove-Item Env:\MYSQL_PWD -ErrorAction SilentlyContinue
  }
}
if ($mysqlExitCode -ne 0) {
  throw "Failed to create or grant MySQL loadtest database $databaseName. Set MYSQL_ROOT_PWD to the local MySQL root password, or create/grant $databaseName manually."
}

$env:MYSQL_PWD = 'auction_dev'
try {
  & $mysql --protocol=tcp --host=127.0.0.1 --port=$mysqlPort --user=auction $databaseName -e "SELECT 1" *> $null
  if ($LASTEXITCODE -ne 0) {
    throw "Cannot connect to MySQL loadtest database as auction/auction_dev."
  }
} finally {
  Remove-Item Env:\MYSQL_PWD -ErrorAction SilentlyContinue
}

if (-not (Test-TcpPort $redisPort)) {
  $redisOutLog = Join-Path $logsDir 'loadtest-redis.out.log'
  $redisErrLog = Join-Path $logsDir 'loadtest-redis.err.log'
  $redis = Start-Process -FilePath $redisServer -ArgumentList @('--port', "$redisPort", '--save', '""', '--appendonly', 'no', '--dir', "$redisDataDir") -RedirectStandardOutput $redisOutLog -RedirectStandardError $redisErrLog -WindowStyle Hidden -PassThru
  $redis.Id | Set-Content -LiteralPath (Join-Path $runDir 'loadtest-redis.pid') -Encoding ascii
}

if (-not (Test-TcpPort $consulPort)) {
  $consulOutLog = Join-Path $logsDir 'loadtest-consul.out.log'
  $consulErrLog = Join-Path $logsDir 'loadtest-consul.err.log'
  $consul = Start-Process -FilePath $consulExe -ArgumentList @('agent', '-dev', '-client=127.0.0.1') -RedirectStandardOutput $consulOutLog -RedirectStandardError $consulErrLog -WindowStyle Hidden -PassThru
  $consul.Id | Set-Content -LiteralPath (Join-Path $runDir 'loadtest-consul.pid') -Encoding ascii
}

Start-RabbitMQIfAvailable 90

$backendLog = Join-Path $logsDir 'loadtest-backend.log'
$backendCommand = @"
`$env:GOTOOLCHAIN='go1.22.12'
`$env:AUCTION_ENV='loadtest'
`$env:AUCTION_INSTANCE_ID='loadtest-local'
`$env:AUCTION_HTTP_ADDR=':$backendPort'
`$env:AUCTION_MYSQL_DSN='auction:auction_dev@tcp(127.0.0.1:$mysqlPort)/$databaseName?parseTime=true&charset=utf8mb4&loc=Local'
`$env:AUCTION_DB_MAX_OPEN_CONNS='80'
`$env:AUCTION_DB_MAX_IDLE_CONNS='40'
`$env:AUCTION_REDIS_ADDR='127.0.0.1:$redisPort'
`$env:AUCTION_REDIS_PASSWORD='__NO_PASSWORD__'
`$env:AUCTION_REDIS_POOL_SIZE='256'
`$env:AUCTION_REDIS_MIN_IDLE_CONNS='32'
`$env:AUCTION_REDIS_STREAM_REQUIRED='false'
`$env:AUCTION_REALTIME_BUS='rabbitmq'
`$env:AUCTION_REALTIME_BUS_RABBITMQ_URL='$rabbitMQUrl'
`$env:AUCTION_REALTIME_BUS_RABBITMQ_EXCHANGE='auction.loadtest.events'
`$env:AUCTION_REALTIME_BUS_RABBITMQ_ROUTING_KEY='auction.loadtest.realtime.event'
`$env:AUCTION_REALTIME_BUS_RABBITMQ_QUEUE='auction.loadtest.realtime.websocket'
`$env:AUCTION_REALTIME_BUS_RABBITMQ_CONSUMER='loadtest-local'
`$env:AUCTION_REALTIME_BUS_RABBITMQ_PREFETCH='500'
`$env:AUCTION_CONSUL_ADDR='127.0.0.1:$consulPort'
`$env:AUCTION_SERVICE_NAME='auction-backend-loadtest'
`$env:AUCTION_SERVICE_ADDR='127.0.0.1'
`$env:AUCTION_JWT_SECRET='loadtest-local-jwt-secret-change-me'
`$env:AUCTION_JWT_ISSUER='auction-backend-loadtest'
Remove-Item Env:\AUCTION_BOOTSTRAP_MAIN_ACCOUNT_USERNAME -ErrorAction SilentlyContinue
Remove-Item Env:\AUCTION_BOOTSTRAP_MAIN_ACCOUNT_PASSWORD -ErrorAction SilentlyContinue
Remove-Item Env:\AUCTION_BOOTSTRAP_MAIN_ACCOUNT_NICKNAME -ErrorAction SilentlyContinue
`$env:AUCTION_SEED_RBAC_DEFAULTS='true'
`$env:AUCTION_SEED_SHOP_DEFAULTS='true'
`$env:AUCTION_RUNTIME_PROJECTION_WORKER_ENABLED='false'
`$env:AUCTION_STORAGE_PROVIDER='local'
`$env:AUCTION_LOCAL_STORAGE_DIR='$assetDir'
`$env:AUCTION_LOCAL_STORAGE_BUCKET='loadtest-local'
`$env:AUCTION_LOCAL_STORAGE_PUBLIC_BASE_URL='http://localhost:$backendPort/assets'
`$env:AUCTION_AI_PROVIDER='mock'
`$env:AUCTION_AI_MOCK_ON_FAILURE='true'
`$env:AUCTION_PAYMENT_PROVIDER='mock'
`$env:AUCTION_SEARCH_PROVIDER=''
Set-Location '$backendDir'
go run ./app/auction/service/cmd/server *> '$backendLog'
"@
$backend = Start-Process -FilePath 'powershell.exe' -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-Command', $backendCommand) -WindowStyle Hidden -PassThru
$backend.Id | Set-Content -LiteralPath (Join-Path $runDir 'loadtest-backend.pid') -Encoding ascii

Wait-HttpOk "http://127.0.0.1:$backendPort/healthz" 'Loadtest backend health' 180
Wait-HttpOk "http://127.0.0.1:$backendPort/readyz" 'Loadtest backend readiness' 180

Write-Host ''
Write-Host 'Isolated LiveAuction loadtest backend is running.'
Write-Host "Backend API:      http://127.0.0.1:$backendPort"
Write-Host "MySQL database:   127.0.0.1:$mysqlPort / $databaseName / auction / auction_dev"
Write-Host "Redis:            127.0.0.1:$redisPort"
Write-Host 'RabbitMQ exchange: auction.loadtest.events'
Write-Host 'RabbitMQ queue:    auction.loadtest.realtime.websocket'
Write-Host 'Stop command:      powershell -ExecutionPolicy Bypass -File .\stop-loadtest-local.ps1'
