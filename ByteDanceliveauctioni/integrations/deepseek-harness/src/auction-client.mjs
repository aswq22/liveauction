import { InvalidBackendResponseError } from './auth.mjs';

function fail() { throw new InvalidBackendResponseError(); }
function record(v) { if (!v || typeof v !== 'object' || Array.isArray(v)) fail(); return v; }
function string(v) { if (typeof v !== 'string' || !v) fail(); return v; }
function emptyString(v) { if (typeof v !== 'string') fail(); return v; }
function integer(v) {
  if (typeof v === 'number' && Number.isSafeInteger(v)) return v;
  if (typeof v === 'string' && /^-?(0|[1-9]\d*)$/.test(v)) { const n = Number(v); if (Number.isSafeInteger(n)) return n; }
  fail();
}
function number(v) { if (typeof v !== 'number' || !Number.isFinite(v)) fail(); return v; }
function money(v) { const raw = record(v); return { amount: integer(raw.amount), currency: string(raw.currency) }; }
function optionalMoney(v) { return v === undefined || v === null ? null : money(v); }
function optionalString(v) { if (v === undefined || v === null) return ''; return emptyString(v); }
function envelope(v) { const reply = record(v); if (record(reply.result).code !== 0) fail(); return reply; }
function array(v, limit, fn) { if (!Array.isArray(v)) fail(); return v.slice(0, limit).map(fn); }
function boundedArray(v, limit, fn) { if (!Array.isArray(v) || v.length > limit) fail(); return v.map(fn); }
function recentBids(v) { if (!Array.isArray(v)) fail(); return v.slice(-10).map(bid); }
function text(v, required = false, max = 256) { if (v === undefined || v === null || v === '') { if (required) fail(); return ''; } if (typeof v !== 'string' || v.length > max) fail(); return v; }
function lot(v) {
  const raw = record(v); const rule = record(raw.rule); const stats = record(raw.stats);
  const out = { id: string(raw.id), roomId: string(raw.roomId), title: string(raw.title), status: string(raw.status), category: emptyString(raw.category), currentPrice: money(raw.currentPrice), startPrice: money(rule.startPrice), minIncrement: money(rule.minIncrement), capPrice: optionalMoney(rule.capPrice), finalPrice: optionalMoney(raw.finalPrice), leadingNickname: optionalString(raw.leadingNickname), updatedAtUnixMs: integer(raw.updatedAtUnixMs), startedAtUnixMs: integer(raw.startedAtUnixMs), endsAtUnixMs: integer(raw.endsAtUnixMs), queuePosition: integer(raw.queuePosition), bidCount: integer(stats.bidCount), participantCount: integer(stats.participantCount) };
  return out;
}
function bid(v) { const raw = record(v); return { nickname: optionalString(raw.nickname), amount: money(raw.amount), createdAtUnixMs: integer(raw.createdAtUnixMs) }; }
function room(v) { const raw = record(v); return { id: string(raw.id), name: string(raw.name), platform: string(raw.platform), status: string(raw.status), liveStartedAtUnixMs: integer(raw.liveStartedAtUnixMs) }; }
function ratio(v) { const raw = record(v); return { numerator: integer(raw.numerator), denominator: integer(raw.denominator), rate: number(raw.rate) }; }
function summary(v) { const raw = record(v); return { gmvAmount: integer(raw.gmvAmount), paidAmount: integer(raw.paidAmount), pendingAmount: integer(raw.pendingAmount), orderCount: integer(raw.orderCount), cancelledOrderCount: integer(raw.cancelledOrderCount), participantCount: integer(raw.participantCount), averageDealAmount: integer(raw.averageDealAmount), paymentRate: ratio(raw.paymentRate), dealRate: ratio(raw.dealRate) }; }
function funnel(v) { const raw = record(v); return { createdLotCount: integer(raw.createdLotCount), listedLotCount: integer(raw.listedLotCount), startedLotCount: integer(raw.startedLotCount), bidLotCount: integer(raw.bidLotCount), settledLotCount: integer(raw.settledLotCount), paidLotCount: integer(raw.paidLotCount) }; }
function dashboard(v) {
  const raw = record(v); const range = record(raw.range); const risks = record(raw.orderRisks);
return { currency: string(raw.currency), snapshotAtUnixMs: integer(raw.snapshotAtUnixMs), range: { range: string(range.range), timezone: string(range.timezone), startUnixMs: integer(range.startUnixMs), endUnixMs: integer(range.endUnixMs) }, summary: summary(raw.summary), funnel: funnel(raw.funnel), trend: boundedArray(raw.trend, 720, p => { const x = record(p); return { startUnixMs: integer(x.startUnixMs), endUnixMs: integer(x.endUnixMs), paidAmount: integer(x.paidAmount), paidOrders: integer(x.paidOrders), bidders: integer(x.bidders) }; }), topLots: boundedArray(raw.topLots, 10, p => { const x = record(p); return { lotId: string(x.lotId), title: string(x.title), category: string(x.category), paidAmount: integer(x.paidAmount), finalAmount: integer(x.finalAmount), bidCount: integer(x.bidCount), participantCount: integer(x.participantCount) }; }), categoryPerformance: boundedArray(raw.categories, 1000, p => { const x = record(p); return { category: string(x.category), paidAmount: integer(x.paidAmount), startedLotCount: integer(x.startedLotCount), settledLotCount: integer(x.settledLotCount), dealRate: ratio(x.dealRate) }; }), orderRisks: { pendingPaymentCount: integer(risks.pendingPaymentCount), expiringSoonCount: integer(risks.expiringSoonCount), expiredCount: integer(risks.expiredCount), paymentFailedCount: integer(risks.paymentFailedCount) }, lotPerformance: boundedArray(raw.lotPerformance, 100, p => { const x = record(p); return { lotId: string(x.lotId), title: string(x.title), category: string(x.category), status: string(x.status), bidCount: integer(x.bidCount), participantCount: integer(x.participantCount), finalAmount: integer(x.finalAmount), paidAmount: integer(x.paidAmount), conversionRate: ratio(x.conversionRate) }; }), partialErrors: boundedArray(raw.partialErrors, 7, p => { const x = record(p); return { section: string(x.section), code: string(x.code) }; }) };
}

/** Read-only, allow-listing facade over AuctionAuth.authorizedRequest. */
export class AuctionApi {
  #request;
  constructor(request) { if (typeof request !== 'function') throw new Error('必须提供已授权的请求函数'); this.#request = request; }
  async getDashboard({ range = 'today', sections = [] } = {}, signal) {
    const safeRange = text(range, true, 32); if (!Array.isArray(sections) || sections.length > 7) fail();
    const params = new URLSearchParams(); params.set('range', safeRange); for (const section of sections) params.append('sections', text(section, true, 64));
    return dashboard(envelope(await this.#request(`/api/admin/dashboard?${params}`, { signal })));
  }
  async searchLots({ lotId = '', keyword = '', roomId = '', pageSize = 5 } = {}, signal) {
    const id = text(lotId); if (id) { const item = await this.getLot(id, signal); return { lots: [item], total: 1, page: 1, pageSize: 1 }; }
    const size = Number.isSafeInteger(pageSize) ? Math.min(5, Math.max(1, pageSize)) : 5; const p = new URLSearchParams();
    p.set('keyword', text(keyword)); p.set('roomId', text(roomId)); p.set('pageSize', String(size)); const reply = envelope(await this.#request(`/api/admin/lots?${p}`, { signal }));
    return { lots: array(reply.lots, 5, lot), total: integer(reply.total), page: integer(reply.page), pageSize: Math.min(5, integer(reply.pageSize)) };
  }
  async getLot(lotId, signal) {
    const id = text(lotId, true);
    const value = lot(envelope(await this.#request(`/api/admin/lots/${encodeURIComponent(id)}`, { signal })).lot);
    if (value.id !== id) fail();
    return value;
  }
  async listRooms(signal) {
    const rooms = envelope(await this.#request('/api/admin/rooms', { signal })).rooms;
    if (!Array.isArray(rooms) || rooms.length > 100) fail();
    return rooms.map(room);
  }
  async getNextQueuedLot(roomId, signal) {
    const id = text(roomId, true);
    const params = new URLSearchParams({ roomId: id, status: 'LOT_STATUS_QUEUED', page: '1', pageSize: '1' });
    const reply = envelope(await this.#request(`/api/admin/lots?${params}`, { signal }));
    if (!Array.isArray(reply.lots) || reply.lots.length > 1) fail();
    if (!reply.lots.length) return null;
    const next = lot(reply.lots[0]);
    if (next.roomId !== id) throw new Error('竞拍服务返回了无效的直播间范围');
    if (next.status !== 'LOT_STATUS_QUEUED') fail();
    return next;
  }
  async getRoomSnapshot(roomId, signal) { const id = text(roomId, true); const raw = record(envelope(await this.#request(`/api/rooms/${encodeURIComponent(id)}/snapshot`, { signal })).snapshot); const out = { roomId: string(raw.roomId), serverTimeUnixMs: integer(raw.serverTimeUnixMs), recentBids: recentBids(raw.recentBids) }; if (raw.currentLot !== undefined && raw.currentLot !== null) out.currentLot = lot(raw.currentLot); return out; }
}
