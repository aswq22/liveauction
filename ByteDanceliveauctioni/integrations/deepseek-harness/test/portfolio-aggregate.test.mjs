import assert from 'node:assert/strict';
import test from 'node:test';
import { aggregatePortfolio } from '../src/portfolio-aggregate.mjs';

function rate(numerator, denominator) {
  return { numerator, denominator, rate: denominator ? numerator / denominator : 0 };
}

function dashboard(overrides = {}) {
  const paidOrders = overrides.paidOrders ?? 1;
  const paidAmount = overrides.paidAmount ?? 10_000;
  return {
    currency: overrides.currency ?? 'CNY',
    snapshotAtUnixMs: overrides.snapshotAtUnixMs ?? 1_789_000_000_000,
    range: {
      range: overrides.range ?? '30d', timezone: overrides.timezone ?? 'Asia/Shanghai',
      startUnixMs: 1_786_000_000_000, endUnixMs: 1_789_000_000_000,
    },
    summary: {
      gmvAmount: overrides.gmvAmount ?? paidAmount,
      paidAmount,
      pendingAmount: overrides.pendingAmount ?? 0,
      orderCount: overrides.orderCount ?? paidOrders,
      cancelledOrderCount: overrides.cancelledOrderCount ?? 0,
      participantCount: overrides.participantCount ?? 7,
      averageDealAmount: paidOrders ? Math.trunc(paidAmount / paidOrders) : 0,
      paymentRate: rate(...(overrides.payment ?? [paidOrders, paidOrders])),
      dealRate: rate(...(overrides.deal ?? [paidOrders, paidOrders])),
    },
    funnel: {
      createdLotCount: overrides.createdLotCount ?? 1,
      listedLotCount: overrides.listedLotCount ?? 1,
      startedLotCount: overrides.startedLotCount ?? 1,
      bidLotCount: overrides.bidLotCount ?? 1,
      settledLotCount: overrides.settledLotCount ?? 1,
      paidLotCount: overrides.paidLotCount ?? 1,
    },
    trend: overrides.trend ?? [],
    topLots: overrides.topLots ?? [],
    categoryPerformance: overrides.categoryPerformance ?? [],
    orderRisks: {
      pendingPaymentCount: overrides.pendingPaymentCount ?? 0,
      expiringSoonCount: overrides.expiringSoonCount ?? 0,
      expiredCount: overrides.expiredCount ?? 0,
      paymentFailedCount: overrides.paymentFailedCount ?? 0,
    },
    lotPerformance: overrides.lotPerformance ?? [],
    partialErrors: overrides.partialErrors ?? [],
  };
}

function success(mainAccountId, alias, value) {
  return {
    inputIndex: mainAccountId === 'tenant-a' ? 0 : 1,
    alias,
    user: {
      id: `${mainAccountId}-owner`, username: `${mainAccountId}-user`, mainAccountId,
      roleCodes: ['merchant_owner'], permissionCodes: ['lot.view_admin', 'order.manage', 'realtime.view'],
    },
    dashboard: value,
  };
}

test('portfolio recomputes weighted totals and never invents a global participant count', () => {
  const report = aggregatePortfolio([
    success('tenant-a', '店铺A', dashboard({ paidAmount: 10_000, paidOrders: 1, payment: [1, 2], deal: [2, 4] })),
    success('tenant-b', '店铺B', dashboard({ paidAmount: 90_000, paidOrders: 9, payment: [9, 10], deal: [8, 10] })),
  ], { range: '30d' });

  assert.equal(report.summary.paidAmount, 100_000);
  assert.equal(report.summary.averageDealAmount, 10_000);
  assert.deepEqual(report.summary.paymentRate, { numerator: 10, denominator: 12, rate: 10 / 12 });
  assert.deepEqual(report.summary.dealRate, { numerator: 10, denominator: 14, rate: 10 / 14 });
  assert.equal('participantCount' in report.summary, false);
  assert.deepEqual(report.rankings.paidAmount.map(item => item.mainAccountId), ['tenant-b', 'tenant-a']);
  assert.deepEqual(Object.keys(report.accounts[0]).sort(), [
    'accountLabel', 'funnel', 'inputIndex', 'mainAccountId', 'orderRisks',
    'partialErrors', 'snapshotAtUnixMs', 'summary', 'username',
  ]);
});

test('portfolio merges comparable panels and emits evidence-backed risk flags', () => {
  const bucket = { startUnixMs: 100, endUnixMs: 200 };
  const report = aggregatePortfolio([
    success('tenant-a', '店铺A', dashboard({
      paidAmount: 10_000, paidOrders: 1, payment: [1, 20], deal: [2, 20],
      createdLotCount: 9, listedLotCount: 8, startedLotCount: 7, bidLotCount: 4,
      settledLotCount: 3, paidLotCount: 1, expiredCount: 2, paymentFailedCount: 1,
      trend: [{ ...bucket, paidAmount: 10_000, paidOrders: 1, bidders: 4 }],
      topLots: [{ lotId: 'lot-a', title: 'A', category: '瓷器', paidAmount: 10_000, finalAmount: 10_000, bidCount: 3, participantCount: 2 }],
      categoryPerformance: [{ category: '瓷器', paidAmount: 10_000, startedLotCount: 2, settledLotCount: 1, dealRate: rate(1, 2) }],
      lotPerformance: [{ lotId: 'lot-a', title: 'A', category: '瓷器', status: 'LOT_STATUS_SETTLED', bidCount: 3, participantCount: 2, finalAmount: 10_000, paidAmount: 0, conversionRate: rate(0, 1) }],
    })),
    success('tenant-b', '店铺B', dashboard({
      paidAmount: 90_000, paidOrders: 19, payment: [19, 20], deal: [18, 20],
      createdLotCount: 21, listedLotCount: 20, startedLotCount: 20, bidLotCount: 20,
      settledLotCount: 18, paidLotCount: 18,
      trend: [{ ...bucket, paidAmount: 90_000, paidOrders: 19, bidders: 8 }],
      topLots: [{ lotId: 'lot-b', title: 'B', category: '瓷器', paidAmount: 90_000, finalAmount: 90_000, bidCount: 8, participantCount: 5 }],
      categoryPerformance: [{ category: '瓷器', paidAmount: 90_000, startedLotCount: 20, settledLotCount: 18, dealRate: rate(18, 20) }],
    })),
  ], { range: '30d' });

  assert.deepEqual(report.funnel, {
    createdLotCount: 30, listedLotCount: 28, startedLotCount: 27,
    bidLotCount: 24, settledLotCount: 21, paidLotCount: 19,
  });
  assert.deepEqual(report.orderRisks, {
    pendingPaymentCount: 0, expiringSoonCount: 0, expiredCount: 2, paymentFailedCount: 1,
  });
  assert.deepEqual(report.trend, [{ ...bucket, paidAmount: 100_000, paidOrders: 20 }]);
  assert.equal(report.topLots[0].mainAccountId, 'tenant-b');
  assert.equal(report.topLots[0].accountLabel, '店铺B');
  assert.deepEqual(report.categoryPerformance, [{
    category: '瓷器', paidAmount: 100_000, startedLotCount: 22, settledLotCount: 19,
    dealRate: rate(19, 22),
  }]);
  const codes = report.riskFlags.map(flag => flag.code);
  for (const code of ['EXPIRED_ORDERS', 'PAYMENT_FAILED', 'NO_BID_LOTS', 'UNPAID_SETTLED_LOTS', 'PAYMENT_RATE_BELOW_PORTFOLIO', 'DEAL_RATE_BELOW_PORTFOLIO', 'LOT_NO_CONVERSION']) {
    assert.ok(codes.includes(code), `missing ${code}`);
  }
});

test('portfolio merges same-width trend buckets across different live windows', () => {
  const report = aggregatePortfolio([
    success('tenant-a', '店铺A', dashboard({
      range: 'live',
      trend: [
        { startUnixMs: 100, endUnixMs: 200, paidAmount: 10_000, paidOrders: 1, bidders: 2 },
        { startUnixMs: 200, endUnixMs: 300, paidAmount: 20_000, paidOrders: 2, bidders: 3 },
      ],
    })),
    success('tenant-b', '店铺B', dashboard({
      range: 'live',
      trend: [
        { startUnixMs: 200, endUnixMs: 300, paidAmount: 30_000, paidOrders: 3, bidders: 4 },
        { startUnixMs: 300, endUnixMs: 400, paidAmount: 40_000, paidOrders: 4, bidders: 5 },
      ],
    })),
  ], { range: 'live' });

  assert.deepEqual(report.trend, [
    { startUnixMs: 100, endUnixMs: 200, paidAmount: 10_000, paidOrders: 1 },
    { startUnixMs: 200, endUnixMs: 300, paidAmount: 50_000, paidOrders: 5 },
    { startUnixMs: 300, endUnixMs: 400, paidAmount: 40_000, paidOrders: 4 },
  ]);
});

test('portfolio rejects incompatible snapshots and unsafe integer totals', () => {
  assert.throws(() => aggregatePortfolio([
    success('tenant-a', 'A', dashboard()),
    success('tenant-b', 'B', dashboard({ timezone: 'UTC' })),
  ], { range: '30d' }), /口径不兼容/);
  assert.throws(() => aggregatePortfolio([
    success('tenant-a', 'A', dashboard({ gmvAmount: Number.MAX_SAFE_INTEGER })),
    success('tenant-b', 'B', dashboard({ gmvAmount: 1 })),
  ], { range: '30d' }), /超出安全整数范围/);
  assert.throws(() => aggregatePortfolio([
    success('tenant-a', 'A', dashboard({
      trend: [{ startUnixMs: 100, endUnixMs: 200, paidAmount: 1, paidOrders: 1, bidders: 1 }],
    })),
    success('tenant-b', 'B', dashboard({
      trend: [{ startUnixMs: 101, endUnixMs: 200, paidAmount: 1, paidOrders: 1, bidders: 1 }],
    })),
  ], { range: '30d' }), /INCOMPATIBLE_BUCKETS/);
});
