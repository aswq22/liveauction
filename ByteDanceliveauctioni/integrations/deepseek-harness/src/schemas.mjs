export const userSchema = {
  type: 'object', additionalProperties: false,
  properties: {
    id: { type: 'string' }, username: { type: 'string' }, mainAccountId: { type: 'string' },
    roleCodes: { type: 'array', items: { type: 'string' } },
    permissionCodes: { type: 'array', items: { type: 'string' } },
  }, required: ['id', 'username', 'mainAccountId', 'roleCodes', 'permissionCodes'],
};

export const authOutput = {
  schema: {
    type: 'object', additionalProperties: false,
    properties: {
      authenticated: { type: 'boolean' }, user: userSchema,
      accessExpiresAtUnixMs: { type: 'integer' }, message: { type: 'string' },
    }, required: ['authenticated'],
  },
  render: (_args, value) => [{ type: 'text', text: JSON.stringify(value) }],
};

export const loginParameters = {
  type: 'object', additionalProperties: false,
  properties: { username: { type: 'string', description: '用户提供的登录账号' }, password: { type: 'string', description: '用户提供的密码，不得猜测' } },
  required: ['username', 'password'],
};

export const teamMemberSchema = {
  type: 'object', additionalProperties: false,
  properties: {
    id: { type: 'string' }, username: { type: 'string' }, nickname: { type: 'string' },
    status: { type: 'string' }, mainAccountId: { type: 'string' }, createdByUserId: { type: 'string' },
    roleCodes: { type: 'array', items: { type: 'string' } },
    permissionCodes: { type: 'array', items: { type: 'string' } },
    createdAtUnixMs: { oneOf: [{ type: 'integer' }, { type: 'string' }] },
    updatedAtUnixMs: { oneOf: [{ type: 'integer' }, { type: 'string' }] },
  },
  required: ['id', 'username', 'nickname', 'status', 'mainAccountId', 'createdByUserId', 'roleCodes', 'permissionCodes', 'createdAtUnixMs', 'updatedAtUnixMs'],
};

export const createOutput = {
  schema: {
    type: 'object', additionalProperties: false,
    properties: { created: { type: 'boolean' }, user: teamMemberSchema },
    required: ['created', 'user'],
  },
  render: (_args, value) => [{ type: 'text', text: JSON.stringify(value) }],
};

export const teamMemberParameters = {
  type: 'object', additionalProperties: false,
  properties: {
    mainAccountUsername: { type: 'string', description: '用户指定的目标商家主账号用户名，不是成员用户名' },
    username: { type: 'string', description: '新成员登录用户名；“账号/用户名/登录名”均对应此字段' },
    nickname: { type: 'string', description: '新成员昵称；可从“昵称/姓名/怎么称呼”提取' },
    password: { type: 'string', minLength: 8, maxLength: 128, description: '用户提供的新成员初始密码，不得猜测或复述' },
    roleCode: { type: 'string', enum: ['anchor', 'operator'], description: 'anchor=主播/场控/中控/直播控制；operator=运营/商品助理/拍品助理/订单客服' },
  },
  required: ['mainAccountUsername', 'username', 'nickname', 'password', 'roleCode'],
};

export const dashboardParameters = {
  type: 'object', additionalProperties: false,
  properties: {
    range: { type: 'string', enum: ['today', 'live', '7d', '30d'], description: '统计范围；省略时为 today（今日）' },
    sections: { type: 'array', maxItems: 7, items: { type: 'string', enum: ['summary', 'funnel', 'trend', 'topLots', 'categoryPerformance', 'orderRisks', 'lotPerformance'] }, description: '要加载的工作台面板；空数组或省略代表全部面板' },
  },
};

const ratioSchema = {
  type: 'object', additionalProperties: false,
  properties: { numerator: { type: 'integer' }, denominator: { type: 'integer' }, rate: { type: 'number' } },
  required: ['numerator', 'denominator', 'rate'],
};

const dashboardSummarySchema = {
  type: 'object', additionalProperties: false,
  properties: {
    gmvAmount: { type: 'integer' }, paidAmount: { type: 'integer' }, pendingAmount: { type: 'integer' },
    orderCount: { type: 'integer' }, cancelledOrderCount: { type: 'integer' }, participantCount: { type: 'integer' },
    averageDealAmount: { type: 'integer' }, paymentRate: ratioSchema, dealRate: ratioSchema,
  },
  required: ['gmvAmount', 'paidAmount', 'pendingAmount', 'orderCount', 'cancelledOrderCount', 'participantCount', 'averageDealAmount', 'paymentRate', 'dealRate'],
};

const portfolioSummarySchema = {
  ...dashboardSummarySchema,
  properties: Object.fromEntries(Object.entries(dashboardSummarySchema.properties).filter(([key]) => key !== 'participantCount')),
  required: dashboardSummarySchema.required.filter(key => key !== 'participantCount'),
};

const funnelSchema = {
  type: 'object', additionalProperties: false,
  properties: Object.fromEntries(['createdLotCount', 'listedLotCount', 'startedLotCount', 'bidLotCount', 'settledLotCount', 'paidLotCount'].map(key => [key, { type: 'integer' }])),
  required: ['createdLotCount', 'listedLotCount', 'startedLotCount', 'bidLotCount', 'settledLotCount', 'paidLotCount'],
};

const orderRisksSchema = {
  type: 'object', additionalProperties: false,
  properties: Object.fromEntries(['pendingPaymentCount', 'expiringSoonCount', 'expiredCount', 'paymentFailedCount'].map(key => [key, { type: 'integer' }])),
  required: ['pendingPaymentCount', 'expiringSoonCount', 'expiredCount', 'paymentFailedCount'],
};

const dashboardPartialErrorSchema = {
  type: 'object', additionalProperties: false,
  properties: { section: { type: 'string' }, code: { type: 'string' } }, required: ['section', 'code'],
};

const coverageSchema = {
  type: 'object', additionalProperties: false,
  properties: Object.fromEntries(['requested', 'authenticated', 'included', 'failed'].map(key => [key, { type: 'integer' }])),
  required: ['requested', 'authenticated', 'included', 'failed'],
};

const portfolioErrorSchema = {
  type: 'object', additionalProperties: false,
  properties: {
    inputIndex: { type: 'integer' }, alias: { type: 'string' },
    code: { type: 'string', enum: ['AUTH_FAILED', 'PERMISSION_DENIED', 'BACKEND_UNAVAILABLE', 'INVALID_BACKEND_RESPONSE', 'ACCOUNT_QUERY_FAILED', 'DUPLICATE_ACCOUNT'] },
  }, required: ['inputIndex', 'alias', 'code'],
};

const rankingValueSchema = {
  type: 'object', additionalProperties: false,
  properties: { mainAccountId: { type: 'string' }, accountLabel: { type: 'string' }, value: { type: 'integer' } },
  required: ['mainAccountId', 'accountLabel', 'value'],
};

const ratioRankingSchema = {
  type: 'object', additionalProperties: false,
  properties: { mainAccountId: { type: 'string' }, accountLabel: { type: 'string' }, numerator: { type: 'integer' }, denominator: { type: 'integer' }, rate: { type: 'number' } },
  required: ['mainAccountId', 'accountLabel', 'numerator', 'denominator', 'rate'],
};

const accountSummarySchema = {
  type: 'object', additionalProperties: false,
  properties: {
    inputIndex: { type: 'integer' }, mainAccountId: { type: 'string' }, username: { type: 'string' }, accountLabel: { type: 'string' },
    snapshotAtUnixMs: { type: 'integer' }, summary: dashboardSummarySchema, funnel: funnelSchema, orderRisks: orderRisksSchema,
    partialErrors: { type: 'array', items: dashboardPartialErrorSchema },
  },
  required: ['inputIndex', 'mainAccountId', 'username', 'accountLabel', 'snapshotAtUnixMs', 'summary', 'funnel', 'orderRisks', 'partialErrors'],
};

const portfolioLotSchema = {
  type: 'object', additionalProperties: false,
  properties: {
    mainAccountId: { type: 'string' }, accountLabel: { type: 'string' }, lotId: { type: 'string' }, title: { type: 'string' }, category: { type: 'string' },
    paidAmount: { type: 'integer' }, finalAmount: { type: 'integer' }, bidCount: { type: 'integer' }, participantCount: { type: 'integer' },
  }, required: ['mainAccountId', 'accountLabel', 'lotId', 'title', 'category', 'paidAmount', 'finalAmount', 'bidCount', 'participantCount'],
};

const categorySchema = {
  type: 'object', additionalProperties: false,
  properties: { category: { type: 'string' }, paidAmount: { type: 'integer' }, startedLotCount: { type: 'integer' }, settledLotCount: { type: 'integer' }, dealRate: ratioSchema },
  required: ['category', 'paidAmount', 'startedLotCount', 'settledLotCount', 'dealRate'],
};

const trendSchema = {
  type: 'object', additionalProperties: false,
  properties: { startUnixMs: { type: 'integer' }, endUnixMs: { type: 'integer' }, paidAmount: { type: 'integer' }, paidOrders: { type: 'integer' } },
  required: ['startUnixMs', 'endUnixMs', 'paidAmount', 'paidOrders'],
};

const riskCommon = { mainAccountId: { type: 'string' }, accountLabel: { type: 'string' } };
const countRiskSchema = {
  type: 'object', additionalProperties: false,
  properties: { code: { type: 'string', enum: ['EXPIRED_ORDERS', 'PAYMENT_FAILED', 'NO_BID_LOTS', 'UNPAID_SETTLED_LOTS'] }, ...riskCommon, count: { type: 'integer' } },
  required: ['code', 'mainAccountId', 'accountLabel', 'count'],
};
const rateRiskSchema = {
  type: 'object', additionalProperties: false,
  properties: { code: { type: 'string', enum: ['PAYMENT_RATE_BELOW_PORTFOLIO', 'DEAL_RATE_BELOW_PORTFOLIO'] }, ...riskCommon, actualRate: { type: 'number' }, portfolioRate: { type: 'number' }, denominator: { type: 'integer' }, thresholdPoints: { type: 'number' } },
  required: ['code', 'mainAccountId', 'accountLabel', 'actualRate', 'portfolioRate', 'denominator', 'thresholdPoints'],
};
const incompleteRiskSchema = {
  type: 'object', additionalProperties: false,
  properties: { code: { type: 'string', enum: ['DATA_INCOMPLETE'] }, ...riskCommon, sections: { type: 'array', items: { type: 'string' } } },
  required: ['code', 'mainAccountId', 'accountLabel', 'sections'],
};
const lotRiskSchema = {
  type: 'object', additionalProperties: false,
  properties: { code: { type: 'string', enum: ['LOT_NO_CONVERSION'] }, ...riskCommon, lotId: { type: 'string' }, title: { type: 'string' }, bidCount: { type: 'integer' } },
  required: ['code', 'mainAccountId', 'accountLabel', 'lotId', 'title', 'bidCount'],
};

const portfolioReportSchema = {
  type: 'object', additionalProperties: false,
  properties: {
    status: { type: 'string', enum: ['OK', 'PARTIAL'] }, coverage: coverageSchema, currency: { type: 'string', enum: ['CNY'] },
    range: dashboardParameters.properties.range, summary: portfolioSummarySchema, funnel: funnelSchema, orderRisks: orderRisksSchema,
    accounts: { type: 'array', items: accountSummarySchema },
    rankings: { type: 'object', additionalProperties: false, properties: {
      paidAmount: { type: 'array', items: rankingValueSchema }, orderCount: { type: 'array', items: rankingValueSchema },
      paymentRate: { type: 'array', items: ratioRankingSchema }, dealRate: { type: 'array', items: ratioRankingSchema },
    }, required: ['paidAmount', 'orderCount', 'paymentRate', 'dealRate'] },
    topLots: { type: 'array', items: portfolioLotSchema },
    categoryPerformance: { type: 'array', items: categorySchema },
    trend: { type: 'array', items: trendSchema },
    riskFlags: { type: 'array', items: { oneOf: [countRiskSchema, rateRiskSchema, incompleteRiskSchema, lotRiskSchema] } },
    partialErrors: { type: 'array', items: portfolioErrorSchema },
  },
  required: ['status', 'coverage', 'currency', 'range', 'summary', 'funnel', 'orderRisks', 'accounts', 'rankings', 'topLots', 'categoryPerformance', 'trend', 'riskFlags', 'partialErrors'],
};

const allAccountsFailedSchema = {
  type: 'object', additionalProperties: false,
  properties: { status: { type: 'string', enum: ['ALL_ACCOUNTS_FAILED'] }, coverage: coverageSchema, partialErrors: { type: 'array', items: portfolioErrorSchema } },
  required: ['status', 'coverage', 'partialErrors'],
};

const accountCredentialSchema = {
  type: 'object', additionalProperties: false,
  properties: {
    username: { type: 'string', minLength: 1, maxLength: 128, description: '用户提供的商家主账号' },
    password: { type: 'string', minLength: 1, maxLength: 1024, description: '该账号密码，不得猜测、记录或复述' },
    alias: { type: 'string', minLength: 1, maxLength: 64, description: '报告中使用的可选安全别名' },
  }, required: ['username', 'password'],
};

export const portfolioParameters = {
  type: 'object', additionalProperties: false,
  properties: {
    accounts: { type: 'array', minItems: 2, maxItems: 10, items: accountCredentialSchema },
    range: dashboardParameters.properties.range,
    sections: dashboardParameters.properties.sections,
  }, required: ['accounts'],
};

export const portfolioOutput = {
  schema: { oneOf: [portfolioReportSchema, allAccountsFailedSchema] },
  render: (_args, value) => [{ type: 'text', text: JSON.stringify(value) }],
};

export const lotPriceParameters = {
  type: 'object', additionalProperties: false,
  properties: {
    lotId: { type: 'string', description: '拍品 ID；提供时直接查询该拍品' },
    keyword: { type: 'string', description: '拍品标题关键词；结果有歧义时需向用户追问' },
    roomId: { type: 'string', description: '可选直播间 ID，仅用于按标题搜索时缩小范围' },
  },
  anyOf: [{ required: ['lotId'] }, { required: ['keyword'] }],
};

const moneySchema = {
  type: 'object', additionalProperties: false,
  properties: { amount: { type: 'integer' }, currency: { type: 'string' } },
  required: ['amount', 'currency'],
};

const nullableMoneySchema = { oneOf: [moneySchema, { type: 'null' }] };

const lotPriceSuccessSchema = {
  type: 'object', additionalProperties: false,
  properties: {
    lotId: { type: 'string' }, roomId: { type: 'string' }, title: { type: 'string' }, status: { type: 'string' },
    startPrice: moneySchema, currentPrice: moneySchema, minIncrement: moneySchema,
    capPrice: nullableMoneySchema, finalPrice: nullableMoneySchema,
    leadingNickname: { type: 'string' }, bidCount: { type: 'integer' }, updatedAtUnixMs: { type: 'integer' },
  },
  required: ['lotId', 'roomId', 'title', 'status', 'startPrice', 'currentPrice', 'minIncrement', 'capPrice', 'finalPrice', 'leadingNickname', 'bidCount', 'updatedAtUnixMs'],
};

const lotPriceCandidateSchema = {
  type: 'object', additionalProperties: false,
  properties: { lotId: { type: 'string' }, roomId: { type: 'string' }, title: { type: 'string' }, status: { type: 'string' } },
  required: ['lotId', 'roomId', 'title', 'status'],
};

export const lotPriceOutput = {
  schema: {
    oneOf: [
      lotPriceSuccessSchema,
      {
        type: 'object', additionalProperties: false,
        properties: { status: { type: 'string', enum: ['LOT_AMBIGUOUS'] }, candidates: { type: 'array', items: lotPriceCandidateSchema } },
        required: ['status', 'candidates'],
      },
      {
        type: 'object', additionalProperties: false,
        properties: { status: { type: 'string', enum: ['LOT_NOT_FOUND'] }, keyword: { type: 'string' } },
        required: ['status', 'keyword'],
      },
    ],
  },
  render: (_args, value) => [{ type: 'text', text: JSON.stringify(value) }],
};

export const dashboardOutput = {
  schema: { type: 'object', additionalProperties: true },
  render: (_args, value) => [{ type: 'text', text: JSON.stringify(value) }],
};

export const liveLotsParameters = {
  type: 'object', additionalProperties: false,
  properties: {
    roomId: { type: 'string', description: '直播间 ID，与 roomName 二选一；都省略时查询全部启用的直播间' },
    roomName: { type: 'string', description: '直播间名称，忽略大小写精确匹配；重名时需追问' },
    includeRecentBids: { type: 'boolean', description: '是否包含最近十条出价的昵称、金额与时间，默认不包含' },
  },
};

const liveLotSchema = {
  type: 'object', additionalProperties: false,
  properties: {
    lotId: { type: 'string' }, title: { type: 'string' }, status: { type: 'string' },
    startPrice: moneySchema, currentPrice: moneySchema, minIncrement: moneySchema,
    leadingNickname: { type: 'string' }, participantCount: { type: 'integer' }, bidCount: { type: 'integer' },
    startedAtUnixMs: { type: 'integer' }, endsAtUnixMs: { type: 'integer' }, remainingMs: { type: 'integer' },
  },
  required: ['lotId', 'title', 'status', 'startPrice', 'currentPrice', 'minIncrement', 'leadingNickname', 'participantCount', 'bidCount', 'startedAtUnixMs', 'endsAtUnixMs', 'remainingMs'],
};

const nextLotSchema = {
  type: 'object', additionalProperties: false,
  properties: { lotId: { type: 'string' }, title: { type: 'string' }, queuePosition: { type: 'integer' }, startPrice: moneySchema },
  required: ['lotId', 'title', 'queuePosition', 'startPrice'],
};

const liveRoomSchema = {
  type: 'object', additionalProperties: false,
  properties: {
    roomId: { type: 'string' }, roomName: { type: 'string' }, snapshotAtUnixMs: { type: 'integer' },
    currentLot: { oneOf: [liveLotSchema, { type: 'null' }] }, nextLot: { oneOf: [nextLotSchema, { type: 'null' }] },
    recentBids: { type: 'array', items: {
      type: 'object', additionalProperties: false,
      properties: { nickname: { type: 'string' }, amount: moneySchema, createdAtUnixMs: { type: 'integer' } },
      required: ['nickname', 'amount', 'createdAtUnixMs'],
    } },
  },
  required: ['roomId', 'roomName', 'snapshotAtUnixMs', 'currentLot', 'nextLot'],
};

export const liveLotsOutput = {
  schema: { oneOf: [
    {
      type: 'object', additionalProperties: false,
      properties: {
        status: { type: 'string', enum: ['OK', 'NO_LIVE_LOT', 'BACKEND_UNAVAILABLE'] },
        rooms: { type: 'array', items: liveRoomSchema },
        partialErrors: { type: 'array', items: {
          type: 'object', additionalProperties: false,
          properties: { roomId: { type: 'string' }, code: { type: 'string', enum: ['BACKEND_UNAVAILABLE'] } },
          required: ['roomId', 'code'],
        } },
      }, required: ['status', 'rooms', 'partialErrors'],
    },
    {
      type: 'object', additionalProperties: false,
      properties: {
        status: { type: 'string', enum: ['ROOM_AMBIGUOUS'] },
        candidates: { type: 'array', items: {
          type: 'object', additionalProperties: false,
          properties: { roomId: { type: 'string' }, name: { type: 'string' }, status: { type: 'string' } },
          required: ['roomId', 'name', 'status'],
        } },
      }, required: ['status', 'candidates'],
    },
    {
      type: 'object', additionalProperties: false,
      properties: { status: { type: 'string', enum: ['ROOM_NOT_FOUND'] } }, required: ['status'],
    },
  ] },
  render: (_args, value) => [{ type: 'text', text: JSON.stringify(value) }],
};
