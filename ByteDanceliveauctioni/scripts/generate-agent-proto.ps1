$ErrorActionPreference = 'Stop'

if (-not (Get-Command protoc-gen-go -ErrorAction SilentlyContinue)) {
    Write-Error 'protoc-gen-go is required. Install it with: go install google.golang.org/protobuf/cmd/protoc-gen-go@latest'
    exit 1
}

if (-not (Get-Command protoc-gen-go-grpc -ErrorAction SilentlyContinue)) {
    Write-Error 'protoc-gen-go-grpc is required. Install it with: go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest'
    exit 1
}

$pythonPackageDirectories = @(
    'live-auction-agent-service/app/generated',
    'live-auction-agent-service/app/generated/proto',
    'live-auction-agent-service/app/generated/proto/agent',
    'live-auction-agent-service/app/generated/proto/agent/v1'
)

foreach ($directory in $pythonPackageDirectories) {
    New-Item -ItemType Directory -Force -Path $directory | Out-Null
}

python -m grpc_tools.protoc `
  -I . `
  --python_out=live-auction-agent-service/app/generated `
  --grpc_python_out=live-auction-agent-service/app/generated `
  --go_out=live-auction-bid-backend `
  --go_opt=module=live-auction-bid/backend `
  --go-grpc_out=live-auction-bid-backend `
  --go-grpc_opt=module=live-auction-bid/backend `
  proto/agent/v1/common.proto proto/agent/v1/runtime.proto proto/agent/v1/tool.proto

if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
}

$generatedPythonFiles = Get-ChildItem 'live-auction-agent-service/app/generated/proto/agent/v1' -Filter '*_pb2*.py'
foreach ($file in $generatedPythonFiles) {
    $content = Get-Content -Raw $file.FullName
    $content = $content.Replace('from proto.agent.v1 import', 'from app.generated.proto.agent.v1 import')
    Set-Content -NoNewline -Path $file.FullName -Value $content
}

foreach ($directory in $pythonPackageDirectories) {
    New-Item -ItemType File -Force -Path (Join-Path $directory '__init__.py') | Out-Null
}
