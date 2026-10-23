function checkedInteger(value, label) {
  if (!Number.isSafeInteger(value) || value < 0) throw new Error(`${label} 必须是非负安全整数`);
  return value;
}

function add(left, right, label) {
  const value = checkedInteger(left, label) + checkedInteger(right, label);
  if (!Number.isSafeInteger(value)) throw new Error(`${label} 超出安全整数范围`);
  return value;
}

function ratio(numerator, denominator) {
  return { numerator, denominator, rate: denominator === 0 ? 0 : numerator / denominator };
}

function compareNumberDesc(left, right) {
  return left === right ? 0 : left > right ? -1 : 1;
}

function ranking(accounts, field) {
  return accounts.map(account => ({
    mainAccountId: account.mainAccountId,
    accountLabel: account.accountLabel,
    value: account.summary[field],
  })).sort((a, b) => compareNumberDesc(a.value, b.value) || a.mainAccountId.localeCompare(b.mainAccountId, 'en'));
}

function ratioRanking(accounts, field) {
  return accounts.map(account => ({
    mainAccountId: account.mainAccountId,
    accountLabel: account.accountLabel,
    ...account.summary[field],
  })).sort((a, b) => compareNumberDesc(a.rate, b.rate) || compareNumberDesc(a.denominator, b.denominator) || a.mainAccountId.localeCompare(b.mainAccountId, 'en'));
}

function sumFields(accounts, property, fields) {
  return Object.fromEntries(fields.map(field => [field, accounts.reduce(
    (total, account) => add(total, account[property][field], `${property}.${field}`), 0,
  )]));
}

function mergeTrend(accounts) {
  for (const account of accounts) if (!Array.isArray(account.trend)) throw new Error('portfolio trend 无效');
  const populated = accounts.filter(account => account.trend.length > 0);
  if (populated.length === 0) return [];

  const first = populated[0].trend[0];
  const width = checkedInteger(first.endUnixMs, 'trend.endUnixMs') - checkedInteger(first.startUnixMs, 'trend.startUnixMs');
  if (width <= 0) throw new Error('INCOMPATIBLE_BUCKETS');
  const alignment = first.startUnixMs % width;
  const merged = new Map();
  for (const account of populated) {
    let previousEnd;
    for (const bucket of account.trend) {
      const startUnixMs = checkedInteger(bucket.startUnixMs, 'trend.startUnixMs');
      const endUnixMs = checkedInteger(bucket.endUnixMs, 'trend.endUnixMs');
      if (endUnixMs - startUnixMs !== width || startUnixMs % width !== alignment || (previousEnd !== undefined && startUnixMs !== previousEnd)) {
        throw new Error('INCOMPATIBLE_BUCKETS');
      }
      previousEnd = endUnixMs;
      const current = merged.get(startUnixMs) ?? { startUnixMs, endUnixMs, paidAmount: 0, paidOrders: 0 };
      if (current.endUnixMs !== endUnixMs) throw new Error('INCOMPATIBLE_BUCKETS');
      current.paidAmount = add(current.paidAmount, bucket.paidAmount, 'trend.paidAmount');
      current.paidOrders = add(current.paidOrders, bucket.paidOrders, 'trend.paidOrders');
      merged.set(startUnixMs, current);
    }
  }
  return [...merged.values()].sort((a, b) => a.startUnixMs - b.startUnixMs);
}

function mergeTopLots(accounts) {
  return accounts.flatMap(account => account.topLots.map(lot => ({
    mainAccountId: account.mainAccountId,
    accountLabel: account.accountLabel,
    lotId: lot.lotId,
    title: lot.title,
    category: lot.category,
    paidAmount: checkedInteger(lot.paidAmount, 'topLots.paidAmount'),
    finalAmount: checkedInteger(lot.finalAmount, 'topLots.finalAmount'),
    bidCount: checkedInteger(lot.bidCount, 'topLots.bidCount'),
    participantCount: checkedInteger(lot.participantCount, 'topLots.participantCount'),
  }))).sort((a, b) => compareNumberDesc(a.paidAmount, b.paidAmount)
    || a.mainAccountId.localeCompare(b.mainAccountId, 'en')
    || a.lotId.localeCompare(b.lotId, 'en')).slice(0, 10);
}

function mergeCategories(accounts) {
  const merged = new Map();
  for (const account of accounts) for (const item of account.categoryPerformance) {
    if (typeof item.category !== 'string' || !item.category) throw new Error('portfolio category 无效');
    const current = merged.get(item.category) ?? {
      category: item.category, paidAmount: 0, startedLotCount: 0, settledLotCount: 0,
      dealNumerator: 0, dealDenominator: 0,
    };
    current.paidAmount = add(current.paidAmount, item.paidAmount, 'category.paidAmount');
    current.startedLotCount = add(current.startedLotCount, item.startedLotCount, 'category.startedLotCount');
    current.settledLotCount = add(current.settledLotCount, item.settledLotCount, 'category.settledLotCount');
    current.dealNumerator = add(current.dealNumerator, item.dealRate.numerator, 'category.dealRate numerator');
    current.dealDenominator = add(current.dealDenominator, item.dealRate.denominator, 'category.dealRate denominator');
    merged.set(item.category, current);
  }
  return [...merged.values()].map(item => ({
    category: item.category,
    paidAmount: item.paidAmount,
    startedLotCount: item.startedLotCount,
    settledLotCount: item.settledLotCount,
    dealRate: ratio(item.dealNumerator, item.dealDenominator),
  })).sort((a, b) => compareNumberDesc(a.paidAmount, b.paidAmount) || a.category.localeCompare(b.category, 'zh-CN'));
}

function flag(account, code, evidence) {
  return { code, mainAccountId: account.mainAccountId, accountLabel: account.accountLabel, ...evidence };
}

function buildRiskFlags(accounts, summary) {
  const flags = [];
  for (const account of accounts) {
    const risks = account.orderRisks;
    if (risks.expiredCount > 0) flags.push(flag(account, 'EXPIRED_ORDERS', { count: risks.expiredCount }));
    if (risks.paymentFailedCount > 0) flags.push(flag(account, 'PAYMENT_FAILED', { count: risks.paymentFailedCount }));
    const noBidCount = account.funnel.startedLotCount - account.funnel.bidLotCount;
    if (noBidCount > 0) flags.push(flag(account, 'NO_BID_LOTS', { count: noBidCount }));
    const unpaidCount = account.funnel.settledLotCount - account.funnel.paidLotCount;
    if (unpaidCount > 0) flags.push(flag(account, 'UNPAID_SETTLED_LOTS', { count: unpaidCount }));
    for (const [field, code] of [['paymentRate', 'PAYMENT_RATE_BELOW_PORTFOLIO'], ['dealRate', 'DEAL_RATE_BELOW_PORTFOLIO']]) {
      const actual = account.summary[field];
      if (actual.denominator >= 10 && summary[field].rate - actual.rate >= 0.2) flags.push(flag(account, code, {
        actualRate: actual.rate, portfolioRate: summary[field].rate, denominator: actual.denominator, thresholdPoints: 0.2,
      }));
    }
    if (account.partialErrors.length) flags.push(flag(account, 'DATA_INCOMPLETE', {
      sections: account.partialErrors.map(error => error.section),
    }));
    if (!account.partialErrors.some(error => error.section === 'lotPerformance')) for (const lot of account.lotPerformance) {
      if (lot.bidCount > 0 && lot.conversionRate.numerator === 0) flags.push(flag(account, 'LOT_NO_CONVERSION', {
        lotId: lot.lotId, title: lot.title, bidCount: lot.bidCount,
      }));
    }
  }
  return flags;
}

function projectAccount(item, expectedRange) {
  const { dashboard, user } = item;
  if (!dashboard || dashboard.currency !== 'CNY' || dashboard.range?.range !== expectedRange || dashboard.range?.timezone !== 'Asia/Shanghai') {
    throw new Error('portfolio 账号统计口径不兼容');
  }
  if (!user || typeof user.mainAccountId !== 'string' || !user.mainAccountId) throw new Error('portfolio 账号身份无效');
  return {
    inputIndex: checkedInteger(item.inputIndex, 'inputIndex'),
    mainAccountId: user.mainAccountId,
    username: user.username,
    accountLabel: item.alias || user.username,
    snapshotAtUnixMs: checkedInteger(dashboard.snapshotAtUnixMs, 'snapshotAtUnixMs'),
    summary: dashboard.summary,
    funnel: dashboard.funnel,
    orderRisks: dashboard.orderRisks,
    partialErrors: dashboard.partialErrors,
    trend: dashboard.trend,
    topLots: dashboard.topLots,
    categoryPerformance: dashboard.categoryPerformance,
    lotPerformance: dashboard.lotPerformance,
  };
}

export function aggregatePortfolio(successes, { range } = {}) {
  if (!Array.isArray(successes) || successes.length === 0) throw new Error('portfolio 至少需要一个成功账号');
  const accounts = successes.map(item => projectAccount(item, range));
  const totals = accounts.reduce((sum, account) => {
    const summary = account.summary;
    for (const field of ['gmvAmount', 'paidAmount', 'pendingAmount', 'orderCount', 'cancelledOrderCount']) {
      sum[field] = add(sum[field], summary[field], field);
    }
    sum.paymentNumerator = add(sum.paymentNumerator, summary.paymentRate.numerator, 'paymentRate numerator');
    sum.paymentDenominator = add(sum.paymentDenominator, summary.paymentRate.denominator, 'paymentRate denominator');
    sum.dealNumerator = add(sum.dealNumerator, summary.dealRate.numerator, 'dealRate numerator');
    sum.dealDenominator = add(sum.dealDenominator, summary.dealRate.denominator, 'dealRate denominator');
    return sum;
  }, {
    gmvAmount: 0, paidAmount: 0, pendingAmount: 0, orderCount: 0, cancelledOrderCount: 0,
    paymentNumerator: 0, paymentDenominator: 0, dealNumerator: 0, dealDenominator: 0,
  });
  const summary = {
    gmvAmount: totals.gmvAmount,
    paidAmount: totals.paidAmount,
    pendingAmount: totals.pendingAmount,
    orderCount: totals.orderCount,
    cancelledOrderCount: totals.cancelledOrderCount,
    averageDealAmount: totals.paymentNumerator === 0 ? 0 : Math.trunc(totals.paidAmount / totals.paymentNumerator),
    paymentRate: ratio(totals.paymentNumerator, totals.paymentDenominator),
    dealRate: ratio(totals.dealNumerator, totals.dealDenominator),
  };
  const funnel = sumFields(accounts, 'funnel', [
    'createdLotCount', 'listedLotCount', 'startedLotCount', 'bidLotCount', 'settledLotCount', 'paidLotCount',
  ]);
  const orderRisks = sumFields(accounts, 'orderRisks', [
    'pendingPaymentCount', 'expiringSoonCount', 'expiredCount', 'paymentFailedCount',
  ]);
  const topLots = mergeTopLots(accounts);
  const categoryPerformance = mergeCategories(accounts);
  const trend = mergeTrend(accounts);
  const riskFlags = buildRiskFlags(accounts, summary);
  const publicAccounts = accounts.map(({
    trend: _trend, topLots: _topLots, categoryPerformance: _categories,
    lotPerformance: _lots, ...account
  }) => account);
  return {
    currency: 'CNY', range, summary, funnel, orderRisks, accounts: publicAccounts,
    rankings: {
      paidAmount: ranking(accounts, 'paidAmount'),
      orderCount: ranking(accounts, 'orderCount'),
      paymentRate: ratioRanking(accounts, 'paymentRate'),
      dealRate: ratioRanking(accounts, 'dealRate'),
    },
    topLots, categoryPerformance, trend, riskFlags,
  };
}
