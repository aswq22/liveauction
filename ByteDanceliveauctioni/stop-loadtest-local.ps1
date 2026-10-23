$ErrorActionPreference = 'Continue'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$runDir = Join-Path $root '.loadtest-run'

function Stop-ProcessTree($processId, $name) {
  $children = Get-CimInstance Win32_Process -Filter "ParentProcessId = $processId" -ErrorAction SilentlyContinue
  foreach ($child in $children) {
    Stop-ProcessTree ([int]$child.ProcessId) $name
  }
  Stop-Process -Id $processId -Force -ErrorAction SilentlyContinue
}

foreach ($name in @('loadtest-backend', 'loadtest-redis', 'loadtest-consul')) {
  $pidFile = Join-Path $runDir "$name.pid"
  if (Test-Path -LiteralPath $pidFile) {
    $rawPid = (Get-Content -LiteralPath $pidFile -ErrorAction SilentlyContinue | Select-Object -First 1)
    if ($rawPid -match '^\d+$') {
      Stop-ProcessTree ([int]$rawPid) $name
      Write-Host "Stopped $name process $rawPid"
    }
    Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
  }
}

foreach ($port in @(18081, 6380)) {
  $listeners = netstat -ano | Select-String ":$port\s+.*LISTENING\s+(\d+)"
  foreach ($listener in $listeners) {
    if ($listener.Line -match 'LISTENING\s+(\d+)\s*$') {
      $listenerPid = [int]$matches[1]
      Stop-ProcessTree $listenerPid "loadtest-port-$port"
      Write-Host "Stopped leftover loadtest process on port ${port}: $listenerPid"
    }
  }
}

Write-Host 'Isolated LiveAuction loadtest services stopped.'
