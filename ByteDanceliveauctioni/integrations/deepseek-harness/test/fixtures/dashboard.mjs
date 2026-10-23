// Complete protojson dashboard shape, including every bounded panel at capacity.
export function dashboardReply({ trendCount = 720, categoryCount = 1000, lotCount = 100, topCount = 10, range = 'live' } = {}) {
  const ratio = () => ({ numerator: '1', denominator: '2', rate: 0.5 });
  return {
    result: { code: 0, message: 'ok' }, currency: 'CNY', snapshotAtUnixMs: '1788667200000',
    range: { range, timezone: 'Asia/Shanghai', startUnixMs: '1786075200000', endUnixMs: '1788667200000' },
    summary: { gmvAmount: '2000', paidAmount: '2000', pendingAmount: '1000', orderCount: '2', cancelledOrderCount: '1', participantCount: '2', averageDealAmount: '2000', paymentRate: ratio(), dealRate: ratio() },
    funnel: { createdLotCount: '2', listedLotCount: '2', startedLotCount: '2', bidLotCount: '2', settledLotCount: '1', paidLotCount: '1' },
    trend: Array.from({ length: trendCount }, (_, i) => ({ startUnixMs: String(1786075200000 + i * 3600000), endUnixMs: String(1786078800000 + i * 3600000), paidAmount: String(i), paidOrders: '1', bidders: '2' })),
    topLots: Array.from({ length: topCount }, (_, i) => ({ lotId: `top-${i}`, title: `Top ${i}`, category: 'jade', paidAmount: String(1000 - i), finalAmount: '1000', bidCount: '2', participantCount: '2' })),
    categories: Array.from({ length: categoryCount }, (_, i) => ({ category: `Category ${i}`, paidAmount: String(i), startedLotCount: '2', settledLotCount: '1', dealRate: ratio() })),
    orderRisks: { pendingPaymentCount: '1', expiringSoonCount: '1', expiredCount: '0', paymentFailedCount: '0' },
    lotPerformance: Array.from({ length: lotCount }, (_, i) => ({ lotId: `lot-${i}`, title: `Lot ${i}`, category: 'jade', status: 'LOT_STATUS_SETTLED', bidCount: '2', participantCount: '2', finalAmount: '1000', paidAmount: String(1000 - i), conversionRate: ratio() })),
    partialErrors: Array.from({ length: 6 }, (_, i) => ({ section: ['funnel', 'trend', 'topLots', 'categoryPerformance', 'orderRisks', 'lotPerformance'][i], code: 'DATA_INCOMPLETE', message: 'This dashboard section could not be loaded.' })),
  };
}
