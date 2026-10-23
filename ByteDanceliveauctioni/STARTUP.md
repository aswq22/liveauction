# Live Auction 项目启动手册

本文档适用于 Windows PowerShell。项目根目录为：

```text
D:\Go\Auc\ByteDanceliveauctioni
```

完整运行需要三个 PowerShell 窗口：

1. Docker 后端、数据库及 AI Agent
2. PC 商家后台
3. 用户 H5

## 一、启动后端、数据库和 AI Agent

确保 Docker Desktop 已启动，并且使用 Linux Containers。

```powershell
cd D:\Go\Auc\ByteDanceliveauctioni\live-auction-bid-backend\deploy

# 只有 deploy\.env 不存在时才执行，避免覆盖现有配置
if (-not (Test-Path .\.env)) {
    Copy-Item .\.env.example .\.env
}

docker compose --profile agent up -d --build
docker compose --profile agent ps
```

正常情况下，下列核心容器应显示 `healthy`：

- `live-auction-bid-backend`
- `live-auction-agent-service`
- `live-auction-mysql`
- `live-auction-redis`
- `live-auction-rabbitmq`
- `live-auction-consul`
- `live-auction-pgvector`

检查 Go 后端：

```powershell
Invoke-RestMethod http://127.0.0.1:18080/healthz
Invoke-RestMethod http://127.0.0.1:18080/readyz
```

`readyz` 预期返回：

```json
{"ok":true}
```

## 二、启动 PC 商家后台

打开第二个 PowerShell 窗口：

```powershell
cd D:\Go\Auc\ByteDanceliveauctioni\live-auction-bid-frontend

# 首次启动或 package-lock.json 更新后执行
npm ci

npm run dev -- --host 0.0.0.0 --port 5173
```

访问地址：

- 登录页：<http://127.0.0.1:5173/login>
- 商家后台：<http://127.0.0.1:5173/admin>
- AI Agent：<http://127.0.0.1:5173/admin/agent>

本地默认商家主账号：

```text
账号：main
密码：main_dev_password
```

## 三、启动用户 H5

打开第三个 PowerShell 窗口：

```powershell
cd D:\Go\Auc\ByteDanceliveauctioni\live-auction-user-h5

# 只有 .env.local 不存在时才执行
if (-not (Test-Path .\.env.local)) {
    Copy-Item .\.env.example .\.env.local
}

# 首次启动或 package-lock.json 更新后执行
npm ci

npm run dev -- --host 0.0.0.0 --port 5174
```

访问地址：<http://127.0.0.1:5174>

## 四、服务地址

| 服务 | 地址 |
| --- | --- |
| PC 商家后台 | <http://127.0.0.1:5173> |
| 用户 H5 | <http://127.0.0.1:5174> |
| Go 后端 API | <http://127.0.0.1:18080> |
| MySQL | `127.0.0.1:13306` |
| Redis | `127.0.0.1:16379` |
| PostgreSQL / pgvector | `127.0.0.1:15432` |
| RabbitMQ AMQP | `127.0.0.1:5672` |
| RabbitMQ 管理后台 | <http://127.0.0.1:15672> |
| Consul | <http://127.0.0.1:18500> |
| Grafana | <http://127.0.0.1:13000> |
| Prometheus | <http://127.0.0.1:19090> |
| Agent Runtime gRPC | Compose 内部 `auction-agent:19081` |
| Go 只读 Tool gRPC | Compose 内部 `auction-backend:19082` |

`19081` 和 `19082` 不向宿主机公开，这是正常的安全配置。

## 五、模型配置（默认 Fake，DeepSeek 仅显式启用）

模型配置文件：

```text
D:\Go\Auc\ByteDanceliveauctioni\live-auction-bid-backend\deploy\.env
```

回归和本地演示默认使用确定性的 Fake Provider，不需要 API Key：

```env
AGENT_MODEL_PROVIDER=mock
```

需要真实 DeepSeek 时，才把以下配置写入本机 `deploy\.env`：

```env
AGENT_MODEL_PROVIDER=deepseek
AGENT_MODEL_BASE_URL=https://api.deepseek.com
AGENT_MODEL_NAME=deepseek-chat
AGENT_MODEL_API_KEY=<仅放本机环境变量或 deploy/.env>
AGENT_MODEL_TOKEN_LIMIT_FIELD=max_tokens
AGENT_MODEL_TIMEOUT_SECONDS=60
AGENT_TOKEN_BUDGET=8192
```

修改模型配置后，只重新创建 Agent：

```powershell
cd D:\Go\Auc\ByteDanceliveauctioni\live-auction-bid-backend\deploy
docker compose --profile agent up -d --force-recreate auction-agent
```

查看 Agent 日志：

```powershell
docker compose --profile agent logs -f --tail 100 auction-agent
```

`.env` 已被 Git 忽略，不应把真实 API Key 写入 `.env.example`、README 或提交记录。若密钥曾发送到聊天、截图或公开日志，请在 DeepSeek 控制台轮换。

不要把 Key 放进 PowerShell 历史、测试参数、fixture、截图或日志。真实模型烟测必须显式设置 `AGENT_RUN_REAL_MODEL_SMOKE=1`；默认测试永远使用 Fake Provider。

## 六、AI Agent 对话审批演示

1. 登录后打开 <http://127.0.0.1:5173/admin/agent>，选择当前直播间。
2. 输入：`海洋之心，价格10000，时长50分钟，封顶价格100000`。
3. Agent 只追问真实主图；上传图片后检查 AI 建议的分类、介绍、标签和规则，可直接编辑。
4. 进入确认页。默认全选，可取消拍品并调整本批顺序。
5. 点击“确认并加入队列”。此时才创建 NEW 拍品，并整体追加到旧队尾。
6. 前往拍品队列，确认拍品处于待开拍状态，再由运营手动点击“开始竞拍”。

最终确认前，Python 只能读取库存并保存 Agent 草稿，不能写真实 `auction_lots`。刷新页面或重新登录后，可通过 URL 中的会话 ID 恢复消息、候选项、图片和确认版本。若确认失败，整批事务回滚，可修订后重新生成快照。

## 七、停止项目

先在两个前端窗口分别按 `Ctrl+C`，停止 PC 商家后台和用户 H5。

然后停止全部 Compose 服务并保留数据库数据：

```powershell
cd D:\Go\Auc\ByteDanceliveauctioni\live-auction-bid-backend\deploy
docker compose --profile agent stop
```

上述命令不会删除容器或数据卷。不要使用 `docker compose down -v`，除非确实要永久删除本地数据库数据。

## 八、常用排查命令

### 查看容器状态

```powershell
docker compose --profile agent ps
```

### 查看后端日志

```powershell
docker compose --profile agent logs -f --tail 100 auction-backend
```

### 查看 Agent 日志

```powershell
docker compose --profile agent logs -f --tail 100 auction-agent
```

### 检查项目端口

```powershell
Get-NetTCPConnection -State Listen |
    Where-Object LocalPort -In 5173,5174,18080,19081,19082,13306,16379,15432,18500,5672,15672,13000,19090 |
    Sort-Object LocalPort
```

### Docker Desktop 未启动

如果出现以下错误：

```text
failed to connect to the docker API at npipe:////./pipe/dockerDesktopLinuxEngine
```

先启动 Docker Desktop，等待状态显示 Engine running，再执行 Compose 启动命令。

### 端口 5672 被占用

Windows 本机 RabbitMQ 服务可能和 Docker RabbitMQ 冲突。使用管理员 PowerShell 检查并停止：

```powershell
Get-Service RabbitMQ
Stop-Service RabbitMQ
```

然后重新启动 Compose：

```powershell
cd D:\Go\Auc\ByteDanceliveauctioni\live-auction-bid-backend\deploy
docker compose --profile agent up -d
```

### Agent 无法启动

依次检查：

```powershell
docker compose --profile agent ps auction-agent
docker compose --profile agent logs --tail 200 auction-agent
```

常见原因包括：

- DeepSeek API Key 缺失或已失效
- `AGENT_MODEL_BASE_URL` 或 `AGENT_MODEL_NAME` 未配置
- PostgreSQL 尚未健康
- Go Tool gRPC 服务尚未就绪

若暂时不使用真实模型，可在 `deploy/.env` 中切换为：

```env
AGENT_MODEL_PROVIDER=mock
```

然后重新创建 Agent 容器。

### 原子回滚验收

确保 Compose 健康后执行隔离 E2E。测试使用唯一商家/房间，只清理自己的记录，并覆盖失效资产整批零写入、并发队列位置、幂等重放和“入队但未开拍”：

使用项目固定的 Python 3.12 测试镜像，不要直接使用当前宿主机 Python 3.14：

```powershell
cd D:\Go\Auc\ByteDanceliveauctioni
docker build -t live-auction-agent-service:test -f live-auction-agent-service\Dockerfile.test .
docker run --rm --add-host host.docker.internal:host-gateway `
  -v "${PWD}:/workspace" `
  -v /var/run/docker.sock:/var/run/docker.sock `
  -w /workspace/live-auction-agent-service `
  -e AUCTION_E2E_BASE_URL=http://host.docker.internal:18080 `
  live-auction-agent-service:test `
  python -m pytest tests/e2e/test_conversation_approval.py -v
```
