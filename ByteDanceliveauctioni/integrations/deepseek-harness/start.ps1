param(
    [string]$HarnessRoot = (Join-Path $PSScriptRoot '..\..\..\deepseek-harness')
)
$ErrorActionPreference = 'Stop'
$resolvedHarness = (Resolve-Path -LiteralPath $HarnessRoot).Path
$patchFile = Join-Path $PSScriptRoot 'cordis.yml'
if (-not (Test-Path -LiteralPath (Join-Path $resolvedHarness 'node_modules\tsx'))) {
    throw 'Harness 依赖未安装，请先在 Harness 目录运行 pnpm install --frozen-lockfile'
}
Push-Location $resolvedHarness
try {
    & pnpm dsh web --patch $patchFile --no-open
    if ($LASTEXITCODE -ne 0) { throw "Harness 启动失败，退出码 $LASTEXITCODE" }
} finally {
    Pop-Location
}
