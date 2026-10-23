# 竞拍后台 Harness 插件：账户与经营查询（0.3.0）

本目录为独立的 Cordis 插件，通过竞拍后端 API 登录、创建团队成员，并查询商家工作台、拍品价格和直播间当前拍品。它还提供多商家组合查询 Tool 与经营分析 Skill；单账号统计由后端聚合，跨账号统计由插件按后端返回的分子、分母和整数金额确定性合并。

## 七个工具与一个 Skill

| 工具 | 用途与参数 |
| --- | --- |
| `auction_login` | 登录或切换账号，必须提供 `username`、`password`。 |
| `auction_login_status` | 向后端验证当前账号，无参数。 |
| `auction_create_team_member` | 创建团队成员，提供目标主账号 `mainAccountUsername`、成员 `username`、`nickname`、`password`、`roleCode`。 |
| `auction_get_dashboard` | 工作台；`range` 为 `today`（默认）、`live`、`7d`、`30d`；`sections` 省略或为空时加载全部面板。 |
| `auction_get_lot_price` | 拍品价格；提供 `lotId` 或 `keyword`，关键词查询可用 `roomId` 缩小范围。 |
| `auction_get_live_lots` | 正在拍什么；默认查全部启用直播间，可用 `roomId` 或 `roomName` 二选一；`includeRecentBids=true` 返回最近十条出价。 |
| `auction_get_multi_account_report` | 一次性汇总 2-10 个独立商家主账号；每项提供 `username`、`password` 和可选 `alias`，可选 `range`、`sections` 与工作台一致。 |

工作台面板为 `summary`、`funnel`、`trend`、`topLots`、`categoryPerformance`、`orderRisks`、`lotPerformance`。三个单账号查询工具均不接受账号、密码、令牌或租户 ID；组合查询只接受一次性账号密码，不接受 Token 或租户 ID。

运行时 Skill `analyzing-auction-portfolio` 只面向多个独立商家账号的汇总、比较和经营诊断。它要求模型调用一次组合 Tool、检查覆盖率与部分错误，并把回答区分为统计事实、风险信号、分析推断和建议动作。普通单账号查询不应加载该 Skill。

## 启动

准备 Node.js 22.19.0 或以上版本、pnpm，以及单独获取的 DeepSeek Harness 源码仓库；Harness 不包含在本仓库中。下面的 PowerShell 命令要求从竞拍仓库根目录执行，且两个仓库为相邻目录，名称分别为 `ByteDanceliveauctioni` 和 `deepseek-harness`（runtime smoke 也使用这个相邻布局）。若放在其他位置，先调整路径及 smoke 的 Harness 根路径。

先按竞拍仓库根目录的 [STARTUP.md](../../STARTUP.md) 启动后端，确认 `http://127.0.0.1:18080/healthz` 返回成功。然后安装和构建 Harness：

```powershell
$auctionRepo = (Resolve-Path -LiteralPath '.').Path
$auctionHarnessRoot = (Resolve-Path -LiteralPath (Join-Path (Split-Path $auctionRepo -Parent) 'deepseek-harness')).Path
$auctionPluginPath = (Resolve-Path -LiteralPath (Join-Path $auctionRepo 'integrations/deepseek-harness/src/plugin.mjs')).Path
Push-Location $auctionHarnessRoot
try {
    pnpm install --frozen-lockfile
    if ($LASTEXITCODE -ne 0) { throw 'Harness 依赖安装失败' }
    pnpm run build
    if ($LASTEXITCODE -ne 0) { throw 'Harness 构建失败' }
} finally { Pop-Location }
```

本插件自身没有 npm 依赖，入口为 ESM JavaScript。干净 checkout 不包含部署用 `cordis.yml`；在同一个 PowerShell 会话继续执行下面的完整最小配置创建命令。配置写入新建临时目录，不覆盖现有配置，也不需要密码或令牌。

```powershell
$auctionPatchDir = Join-Path ([System.IO.Path]::GetTempPath()) ('auction-harness-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $auctionPatchDir | Out-Null
$auctionPatchPath = Join-Path $auctionPatchDir 'cordis.yml'
$auctionPluginUrl = ([System.Uri]$auctionPluginPath).AbsoluteUri
$auctionPatch = @(
    @{ insert = @(
        @{ id = 'auction-backoffice'; name = $auctionPluginUrl; config = @{ apiBaseUrl = 'http://127.0.0.1:18080' } }
    ) }
)
ConvertTo-Json -InputObject $auctionPatch -Depth 6 | Set-Content -LiteralPath $auctionPatchPath -Encoding utf8
Push-Location $auctionHarnessRoot
try {
    pnpm dsh web --patch $auctionPatchPath --no-open
    if ($LASTEXITCODE -ne 0) { throw 'Harness 启动失败' }
} finally { Pop-Location }
```

JSON 是合法的 YAML，以上生成的 patch 可直接交给 Cordis。插件 `name` 由本机实际路径转换为绝对 `file:///` URL；`apiBaseUrl` 是后端源地址，不带 `/api`，远程地址必须使用 HTTPS，本地可使用 loopback HTTP。使用远程后端时，将上面的本地源地址改为实际部署源地址。移动仓库后重新执行配置创建命令即可。

启动命令在当前终端运行，Ctrl+C 停止。通过终端给出的 Harness 访问地址完成页面认证和模型配置；单独访问 `http://127.0.0.1:3080` 可能返回 401。接着用受控测试账号登录，再说“查看我的竞拍登录状态”，模型应调用 `auction_login_status`。不要将账号凭据写入 patch。

创建团队成员时可说“在 merchant_a 主账号下新增运营小王”。模型应追问缺少的主账号密码、成员用户名、初始密码或角色；信息完整且当前登录账号一致后调用 `auction_create_team_member`。角色同义词映射为 `anchor`（主播/场控）或 `operator`（运营/商品助理/订单客服）。以上启动流程不依赖 `start.ps1`；开发机器上若存在该文件，它只是未随仓库交付的本地可选脚本。

## 登录状态与凭证

- 账号密码只发送给配置中的竞拍后端，拒绝 HTTP 重定向。不猜测密码、不重试错误密码。
- 登录成功仅返回账号 ID、用户名、商家主账号 ID、角色、权限和过期时间；访问令牌保存在当前 Agent Session 对应的内存对象中，不进入工具返回值。refresh token 不保存。
- 状态查询携带内存中的 Bearer token 调用 `/api/users/me`，从后端重新校验用户。仅接纳与网页后台一致的 active 账号及后台权限。
- 切换账号开始时清除旧登录；失败后不会继续使用旧账号。账号状态变更或验证失败会清除本地登录。
- 创建成员必须显式携带目标主账号用户名，并与当前真实登录账号一致；不同账号会被插件拒绝，必须先使用目标主账号密码切换登录。商家空间由后端令牌决定，模型不能指定主账号 ID。
- 创建成员只允许 `anchor` 和 `operator`，要求成员用户名 3-64 位、昵称 1-128 个字符、初始密码 8-128 个字符。密码不会出现在工具结果中，失败或超时不会自动重试。
- 会话、Agent 或插件销毁、进程重启、令牌到期后需重新登录。不同会话和分叉不共享令牌。第一版不做自动刷新，也不把登录同步到浏览器网站。
- **聊天明文密码会进入模型输入及 Harness 的 user/message、tool/call 日志。** 本插件不提供输入日志脱敏。请用测试账号体验；若要避免密码进入模型和日志，下一步应增加独立的安全输入页面并向工具提供凭证引用。
- 插件代码运行在 Harness 进程中，必须信任所加载插件。HTTP 请求的目标由部署配置确定，模型无法在参数中指定任意地址。

### 临时多账号分析

- 每次组合查询接受 2-10 组商家主账号凭据，并为每组凭据创建独立 `AuctionAuth`、Token 和 API 客户端；最多三条账号链并发。
- 临时认证不读写当前会话的单账号登录态。组合查询完成、失败或取消时都会清理全部临时认证对象，不落库、不跨调用复用。
- 插件不会主动记录或回显组合查询的密码与 Token；但用户在聊天中提交的明文密码仍可能保留在模型输入、Harness 会话事件和 Tool 调用记录中。生产环境应增加安全输入页面和凭据引用，不能把当前聊天输入方式描述为端到端机密存储。
- 认证成功后按后端返回的 `mainAccountId` 去重；重复账号、密码错误、权限不足和后端失败通过 `partialErrors` 返回，不透传后端消息。
- 只要有一个账号成功就返回 `OK` 或 `PARTIAL` 以及 `included/requested` 覆盖率；全部失败返回 `ALL_ACCOUNTS_FAILED`，不生成零值经营结论。
- 总额与计数直接求和，平均成交额按总已支付金额和总已支付订单重算，支付率与成交率合并分子分母后重算。不同商家可能拥有相同买家，因此不提供跨账号唯一参拍人数。
- 趋势按相同粒度的绝对时间桶合并；独立商家的 `live` 开播时间不同时按时间键取并集，不同粒度或错位时间桶会被拒绝，避免模型猜测对齐。

## 验证

在本目录执行 `npm test`。测试使用本机 HTTP 测试服务覆盖正常登录、令牌复用、错误密码、后台权限、非法返回、重定向、取消、会话隔离和清理。测试不调用模型、不需要真实密码。

真实 Cordis/ToolRuntime 冒烟验证在 Harness 根目录执行（要求其依赖已安装）：

```powershell
pnpm exec tsx --tsconfig tsconfig.json ../ByteDanceliveauctioni/integrations/deepseek-harness/scripts/runtime-smoke.mjs
```

此检查通过真实 Cordis、ToolRuntime 与 SkillRegistry 注册七个工具和一个运行时 Skill；它会加载 Skill 正文，并实际使用两个隔离的临时账号执行组合查询，覆盖 Bearer token 隔离、确定性聚合、当前登录态不变、关键词歧义、整数金额、会话隔离、非法参数、401/403 和卸载。测试用本机临时 HTTP fixture 验证结果及渲染文本不含测试密码、令牌、买家 ID、电话和地址。它不调用模型，不等同于自然语言端到端验收。聊天输入日志仍受上面的凭证说明约束。

后续业务工具应使用同一会话的认证客户端，由后端执行真实权限及租户检查；不要接受模型提供的任意用户 ID 或商家 ID 作为授权依据。

## 统计口径与回答边界

- 日期按 `Asia/Shanghai`（上海时区）解释，后端返回统计起止与快照时间；时间窗口为左闭右开。`live` 按当前直播窗口统计。
- 原始金额保留人民币分的安全整数，例如 `1200` 表示 12.00 元；工作台 `display` 提供人民币显示文本。不要把原始金额直接当成元，或用显示字符串参与汇总。
- GMV 按窗口内支付时间统计；有效订单数排除取消订单，取消数量单列；支付率按同一创建窗口内的订单群体计算，排除取消和退款订单，因此两者并非同一口径。
- 价格查询优先 `lotId`，通过 `/api/admin/lots/{lot_id}` 校验当前 `lot.view_admin` 权限和租户；否则使用关键词。只有搜索返回完整第一页，唯一精确标题或唯一候选才能返回详情。分页仍隐藏候选时返回 `LOT_AMBIGUOUS`，最多展示五个候选，必须追问具体拍品 ID 或更精确的关键词、直播间；`LOT_NOT_FOUND` 表示完整搜索没有结果。
- 同名直播间返回 `ROOM_AMBIGUOUS`，必须追问；`ROOM_NOT_FOUND` 表示房间不存在。`NO_LIVE_LOT` 表示当前没有拍品。`partialErrors` 表示部分房间查询失败，不能断言全部房间为空；401 需重新登录，403 表示权限不足。
- 当前价、起拍价、最低加价、封顶价和成交价各自独立；可选价格为 `null` 时不要编造数值。直播剩余时间由快照时间计算，回答时应说明快照可能滞后。
- 看板每次从账户存储读取当前启用状态及 `realtime.view`、`lot.view_admin`、`order.manage` 权限；已签发令牌不能绕过撤权或停用，账户租户必须仍与令牌一致。
- 工作台保留完整趋势（今日 24 桶、7 日 7 桶、30 日 30 桶、直播窗口最多 720 桶；长场次自动加宽桶），成交榜保留 Top 10。拍品表现明确为按已支付金额降序、拍品 ID 升序选出的前 100 件，不代表全部明细；工具 `coverage` 同时标明选择方式和上限。
- 品类按全部合格拍品聚合，最多支持 1,000 个不同品类；超过上限时整个品类面板留空并返回 `partialErrors` 中的 `DATA_INCOMPLETE`，其余成功板块和核心汇总保留。客户端不会进一步截断这些板块，超出协议上限的响应会明确失败。
- 传输边界保留 20,000 节点、2 MiB UTF-8 解码响应和 32 层嵌套限制（包含分块响应）；最大面板基准样本为 14,768 节点、253,191 字节，字符串特别长时仍可能达到字节限制。房间超过 100 个会明确失败，避免给出不完整的全部房间结论。

## 自然语言示例与验收

在配置好模型的 Harness 中，先用测试账号登录，再按下面的话术检查工具选择与回答。切换账号缺少目标密码时必须先追问；只有用户名时不能猜密码或继续用旧账号查询。

```text
今天咋样？
只看今天支付率和待付款。
最近七天成交额最高的拍品有哪些？
青花瓷现在多少钱？
所有直播间目前分别在拍什么？
换到 merchant_b 账号查今天工作台。
汇总 merchant_a 和 merchant_b 最近 30 天的成交额、支付率和异常商品。
```

预期查询计划：“今天咋样”调用 `auction_get_dashboard` 默认全量面板；“只看今天支付率和待付款”请求 `summary` 与 `orderRisks`；“最近七天成交额最高的拍品”使用 `range=7d`、`sections=["topLots"]`，并说明按后端支付金额排序；价格问题走关键词查询，直播问题走全部启用房间查询。账号切换先调用 `auction_login`，成功后才执行查询。多个账号的汇总或诊断加载 `analyzing-auction-portfolio`，并调用一次 `auction_get_multi_account_report`，不得循环切换单账号后由模型自行算数。

使用有重复拍品标题、重复房间名称的测试数据继续验收：

```text
查一下茅台现在多少钱
看看一号直播间现在拍什么
今天咋样
付款情况呢
```

前两句应追问候选，最后一句应保留上一句的今日范围并只请求付款相关面板。任何回答都不应复述密码或令牌。自然语言验收需要可用模型配置和受控测试账号；仅凭单元测试和 runtime smoke 无法证明模型完成这些话术。
