# AI 直播竞拍 Agent 对话审批演示

## 启动

Windows PowerShell：

```powershell
cd D:\Go\Auc\ByteDanceliveauctioni\live-auction-bid-backend\deploy
if (-not (Test-Path .\.env)) { Copy-Item .\.env.example .\.env }
docker compose --profile agent up -d --build
docker compose --profile agent ps

cd D:\Go\Auc\ByteDanceliveauctioni\live-auction-bid-frontend
npm ci
npm run dev -- --host 0.0.0.0 --port 5173
```

打开 <http://127.0.0.1:5173/login>，使用本地示例账号 `main / main_dev_password`，进入 <http://127.0.0.1:5173/admin/agent>。Go API 为 <http://127.0.0.1:18080>；MySQL、Redis、PostgreSQL 分别监听 `13306`、`16379`、`15432`。Agent gRPC 仅在 Compose 网络内监听 `auction-agent:19081`。

仓库的 `.env.example` 与 Compose 插值默认均为 `AGENT_MODEL_PROVIDER=mock`，无需外部密钥；被 Git 忽略的本机 `deploy/.env` 若设置其他值会显式覆盖默认。启用 DeepSeek 时，只在该本机文件中填写：

```env
AGENT_MODEL_PROVIDER=deepseek
AGENT_MODEL_BASE_URL=https://api.deepseek.com
AGENT_MODEL_NAME=deepseek-chat
AGENT_MODEL_API_KEY=<本机密钥>
AGENT_MODEL_TOKEN_LIMIT_FIELD=max_tokens
```

密钥不得写入源码、`.env.example`、README、fixture、命令参数、截图或日志。若曾经在聊天或日志中暴露，应立即在供应商控制台轮换。

## 演示一：创建“海洋之心”

输入：

```text
海洋之心，价格10000，时长50分钟，封顶价格100000
```

预期行为：

- 金额按人民币元理解，确认页显示 10,000 元，持久层保存 1,000,000 分。
- Agent 不猜测主图，只追问 `MAIN_IMAGE`。
- 点击候选项的上传按钮，选择真实 PNG/JPEG/WebP/GIF 图片。
- 分类、介绍、标签、最低加价、库存和反狙击规则是可编辑建议，并显示来源。
- 上传完成前不能生成可执行确认快照；最终确认前 `auction_lots` 中不存在“海洋之心”。
- 点击“确认并加入队列”后才创建 lot；结果页保留 execution 和队列位置，点击“前往拍品队列”。
- 新 lot 为待开拍，必须由运营手动点击“开始竞拍”。

如果输入没有价格或时长，Agent 会继续追问对应字段，不会静默猜值。

## 演示二：现有库存批量排品

输入“为当前库存生成竞拍方案”。确认页默认勾选全部候选项。取消任意一项并拖动批内顺序，再创建新快照并确认。预期新批次按确认顺序连续追加到旧队尾，旧队列顺序保持不变。

## 恢复与重试

会话 ID 保存在 URL。刷新页面或重新登录后重新打开该 URL，可恢复消息、候选草稿、图片绑定、当前 Run 和活动快照；已经完成的会话会从持久化 execution 恢复 lot/队列位置和“前往拍品队列”按钮，不会误调仅适用于待确认会话的快照接口。网络中断后的相同消息重试复用同一个幂等键；同一 Apply 重放返回相同 execution 和 lot IDs，不会重复创建。

字段来源使用闭合枚举 `USER / AI / SYSTEM / EXISTING`。数据库中的 `provenance_version=0` 只标记迁移前历史候选，允许对少量 optional 零值做兼容补全；所有新候选明确写入版本 1，缺失来源或未知来源均拒绝生成快照和 Apply。

## 原子回滚演示

自动化用例会在确认快照后使其中一个测试资产失效，然后执行包含 EXISTING 与 NEW 候选项的批次。预期：零新增 lot、零入队、零 APPLIED 执行记录，已有队列不变。

跨服务 E2E 请使用 [Agent Service README](../live-auction-agent-service/README.md) 中已验证的 Python 3.12 容器命令；不要用宿主机 Python 3.14 直接运行。

该测试为每个场景注册专用商家/房间并只删除自己拥有的记录，不会清空共享数据。

## 排查

```powershell
cd D:\Go\Auc\ByteDanceliveauctioni\live-auction-bid-backend\deploy
docker compose --profile agent ps
docker compose --profile agent logs --tail 100 auction-backend
docker compose --profile agent logs --tail 100 auction-agent
Invoke-RestMethod http://127.0.0.1:18080/readyz
```

出现 `dockerDesktopLinuxEngine` 管道不存在时，先启动 Docker Desktop 并等待 Engine running。Agent 失败不影响后端 readiness；恢复 Agent 可执行 `docker compose --profile agent up -d --force-recreate auction-agent`。停止并保留数据使用 `docker compose --profile agent stop`，不要误用 `down -v`。
