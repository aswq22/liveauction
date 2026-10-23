import { AuctionAuth, apiOrigin } from './auth.mjs';
import { AuctionApi } from './auction-client.mjs';
import { authOutput, createOutput, dashboardOutput, dashboardParameters, liveLotsOutput, liveLotsParameters, loginParameters, lotPriceOutput, lotPriceParameters, portfolioOutput, portfolioParameters, teamMemberParameters } from './schemas.mjs';
import { getDashboard, getLiveLots, getLotPrice } from './query-tools.mjs';
import { getMultiAccountReport } from './portfolio-tool.mjs';
import { portfolioSkill } from './portfolio-skill.mjs';

export const name = 'auction-backoffice';
export const inject = ['tools', 'skills'];

/** Raw ToolDefinition avoids a second Cordis/DSH runtime in this external plugin. */
export function apply(ctx, config = {}) {
  if (!ctx.skills?.register) throw new Error('auction-backoffice 需要 Harness skills service');
  const origin = apiOrigin(config.apiBaseUrl ?? 'http://127.0.0.1:18080');
  ctx.effect(() => ctx.skills.register(portfolioSkill));
  const sessions = new WeakMap();
  const activeBundles = new Set();
  ctx.effect(() => () => {
    for (const bundle of activeBundles) bundle.auth.clear();
    activeBundles.clear();
  });
  function client(exec) {
    const agent = exec.agent;
    if (!agent?.session || !agent.ctx) throw new Error('该工具需要运行中的 Harness Agent 会话');
    const sessionKey = agent.session;
    let bundle = sessions.get(sessionKey);
    if (!bundle) {
      const auth = new AuctionAuth(origin);
      bundle = { auth, api: new AuctionApi(auth.authorizedRequest.bind(auth)) };
      sessions.set(sessionKey, bundle);
      activeBundles.add(bundle);
      agent.ctx.effect(() => () => {
        bundle.auth.clear();
        if (sessions.get(sessionKey) === bundle) sessions.delete(sessionKey);
        activeBundles.delete(bundle);
      });
    }
    return bundle;
  }
  ctx.tools.register({
    name: 'auction_login',
    description: '登录或切换竞拍商家后台账号。用户要登录、或要在指定主账号下执行操作时使用。账号或密码缺失必须先追问，不能猜测；切换账号必须使用目标账号的密码。不要在回答中复述密码。登录仅对当前 Harness 会话有效，返回账号和权限，不返回令牌。',
    parameters: loginParameters, output: authOutput,
    execute: (args, exec) => client(exec).auth.login(args, exec.signal),
  });
  ctx.tools.register({
    name: 'auction_create_team_member',
    description: '在用户明确指定的商家主账号下创建团队成员。可理解“新建/新增/添加成员/加个人”等表达；主播、场控、中控、直播控制映射为 anchor，运营、运营人员、商品助理、拍品助理、订单客服映射为 operator。必须收集目标主账号用户名、成员用户名、昵称、初始密码和明确角色；“管理员/普通成员”等角色不明确时先追问，任何字段缺失都先追问。调用前确认当前登录账号与目标主账号一致；未登录则先调用 auction_login，登录了其他账号则取得目标账号密码后切换，绝不能在错误商家空间继续创建。密码不得复述，创建结果不返回密码。创建失败或超时时不得自动重试，需向用户说明结果不确定或错误原因。',
    parameters: teamMemberParameters, output: createOutput,
    execute: (args, exec) => client(exec).auth.createTeamMember(args, exec.signal),
  });
  ctx.tools.register({
    name: 'auction_login_status',
    description: '查询当前竞拍后台登录账号，并向后端验证会话是否有效。未登录或凭证过期时提示重新登录。',
    parameters: { type: 'object', properties: {}, additionalProperties: false }, output: authOutput,
    execute: async (args, exec) => {
      if (!args || typeof args !== 'object' || Array.isArray(args) || Object.keys(args).length) throw new Error('此工具不接受参数');
      return client(exec).auth.status(exec.signal);
    },
  });
  ctx.tools.register({
    name: 'auction_get_dashboard',
    description: '查询当前已登录商家后台的今日工作台和经营数据：适用于“今天咋样”、GMV、成交额、付款/支付率、成交率、参拍人数、平均成交价、漏斗、趋势、Top 10、品类、订单风险、拍品表现等问题。查询指定的不同账号前，必须先用 auction_login 登录或切换到该账号；本工具不接受账号、密码或任何凭证。',
    parameters: dashboardParameters, output: dashboardOutput,
    execute: (args, exec) => getDashboard(args, client(exec), exec.signal),
  });
  ctx.tools.register({
    name: 'auction_get_lot_price',
    description: '查询当前已登录商家后台拍品多少钱：支持当前价、起拍价、最低加价、封顶价、成交价、谁领先、有没有成交。可按拍品 ID 直接查询，或按标题关键词并可选直播间查询；同名或多个候选返回 LOT_AMBIGUOUS 时必须向用户追问具体拍品或直播间，不能猜测。查询其他账号前必须先用 auction_login 登录或切换到该账号；本工具不接受账号、密码或任何凭证。',
    parameters: lotPriceParameters, output: lotPriceOutput,
    execute: (args, exec) => getLotPrice(args, client(exec), exec.signal),
  });
  ctx.tools.register({
    name: 'auction_get_live_lots',
    description: '查询当前已登录商家现在正在拍什么：支持所有直播间/所有场子、哪个直播间、目前谁领先、还剩多久、下一件是什么、最近谁出价。省略直播间筛选时查询全部启用的直播间；可按 roomId 或 roomName 精确查询，ROOM_AMBIGUOUS 时必须追问。NO_LIVE_LOT 表示没有当前拍品；partialErrors 表示部分查询失败，不能据此断言全部房间为空。最近出价需 includeRecentBids=true。查询其他账号前必须先登录或切换账号，不接受账号、密码或任何凭证。',
    parameters: liveLotsParameters, output: liveLotsOutput,
    execute: (args, exec) => getLiveLots(args, client(exec), exec.signal),
  });
  ctx.tools.register({
    name: 'auction_get_multi_account_report',
    description: '汇总、比较或诊断 2-10 个彼此独立的竞拍商家主账号。一次提交每个账号的 username、password 和可选 alias；本工具在隔离的临时登录态中查询并确定性聚合，不切换当前单账号会话。适用于总成交额、账号排名、支付/成交率、成交商品、品类和异常风险。缺少任一账号密码时必须追问，不得猜测或复述；PARTIAL 结果必须披露覆盖率。',
    parameters: portfolioParameters,
    output: portfolioOutput,
    execute: (args, exec) => getMultiAccountReport(args, { origin }, exec?.signal),
  });
}
