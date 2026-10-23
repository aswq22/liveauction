import { dashboardParameters } from './schemas.mjs';
import { BackendUnavailableError, InvalidBackendResponseError } from './auth.mjs';

const ranges = new Set(dashboardParameters.properties.range.enum);
const sections = new Set(dashboardParameters.properties.sections.items.enum);

function invalid(message) { throw new Error(`dashboard ${message}`); }

export function normalizeDashboardArgs(args = {}) {
  if (!args || typeof args !== 'object' || Array.isArray(args)) invalid('参数必须是对象');
  if (Object.keys(args).some(key => key !== 'range' && key !== 'sections')) invalid('参数包含未知字段');
  const range = args.range === undefined ? 'today' : args.range;
  if (typeof range !== 'string' || !ranges.has(range)) invalid('range 无效');
  const inputSections = args.sections === undefined ? [] : args.sections;
  if (!Array.isArray(inputSections) || inputSections.length > 7) invalid('sections 无效');
  const deduplicated = [];
  for (const section of inputSections) {
    if (typeof section !== 'string' || !sections.has(section)) invalid('sections 无效');
    if (!deduplicated.includes(section)) deduplicated.push(section);
  }
  return { range, sections: deduplicated };
}

function cny(amount, locale) {
  if (!Number.isSafeInteger(amount)) throw new Error('dashboard 金额必须是安全整数');
  return new Intl.NumberFormat(locale, { style: 'currency', currency: 'CNY', minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(amount / 100);
}

function moneyFields(source, fields, locale) {
  return Object.fromEntries(fields.map(field => [field, cny(source[field], locale)]));
}

/** Adds display-only CNY labels; raw integer amounts remain on the snapshot. */
export function formatDashboardMoney(snapshot, locale = 'zh-CN') {
  return {
    currency: 'CNY',
    summary: moneyFields(snapshot.summary, ['gmvAmount', 'paidAmount', 'pendingAmount', 'averageDealAmount'], locale),
    trend: snapshot.trend.map(item => moneyFields(item, ['paidAmount'], locale)),
    topLots: snapshot.topLots.map(item => moneyFields(item, ['paidAmount', 'finalAmount'], locale)),
    categoryPerformance: snapshot.categoryPerformance.map(item => moneyFields(item, ['paidAmount'], locale)),
    lotPerformance: snapshot.lotPerformance.map(item => moneyFields(item, ['finalAmount', 'paidAmount'], locale)),
  };
}

export async function getDashboard(args, { api }, signal) {
  const input = normalizeDashboardArgs(args);
  if (!api || typeof api.getDashboard !== 'function') throw new Error('dashboard API 不可用');
  const snapshot = await api.getDashboard(input, signal);
  return {
    ...snapshot,
    coverage: {
      trend: { selection: 'all_buckets', limit: 720 },
      topLots: { selection: 'top_by_paid_amount_then_lot_id', limit: 10 },
      categoryPerformance: { selection: 'all_or_partial_error', limit: 1000 },
      lotPerformance: { selection: 'top_by_paid_amount_then_lot_id', limit: 100 },
    },
    display: formatDashboardMoney(snapshot, 'zh-CN'),
  };
}

function invalidLotPrice(message) { throw new Error(`lot price ${message}`); }

function optionalText(value, name) {
  if (value === undefined) return '';
  if (typeof value !== 'string') invalidLotPrice(`${name} 无效`);
  const trimmed = value.trim();
  if (!trimmed) invalidLotPrice(`${name} 不能为空`);
  return trimmed;
}

export function normalizeLotPriceArgs(args = {}) {
  if (!args || typeof args !== 'object' || Array.isArray(args)) invalidLotPrice('参数必须是对象');
  if (Object.keys(args).some(key => key !== 'lotId' && key !== 'keyword' && key !== 'roomId')) invalidLotPrice('参数包含未知字段');
  const lotId = optionalText(args.lotId, 'lotId');
  const keyword = optionalText(args.keyword, 'keyword');
  const roomId = optionalText(args.roomId, 'roomId');
  if (!lotId && !keyword) invalidLotPrice('lotId 或 keyword 必填');
  return { lotId, keyword, roomId };
}

function lotIdOf(lot) {
  const id = lot?.lotId ?? lot?.id;
  if (typeof id !== 'string' || !id) throw new Error('lot price 拍品数据无效');
  return id;
}

function candidateSummary(lot) {
  if (!lot || typeof lot !== 'object' || Array.isArray(lot)
    || typeof lot.roomId !== 'string' || typeof lot.title !== 'string' || typeof lot.status !== 'string') {
    throw new Error('lot price 拍品数据无效');
  }
  return { lotId: lotIdOf(lot), roomId: lot.roomId, title: lot.title, status: lot.status };
}

/** Restricts lot output to price-facing fields and never passes through account or bidder identifiers. */
function safePrice(lot) {
  const candidate = candidateSummary(lot);
  const rule = lot.rule && typeof lot.rule === 'object' && !Array.isArray(lot.rule) ? lot.rule : {};
  return {
    ...candidate,
    startPrice: lot.startPrice ?? rule.startPrice,
    currentPrice: lot.currentPrice,
    minIncrement: lot.minIncrement ?? rule.minIncrement,
    capPrice: lot.capPrice !== undefined ? lot.capPrice : (rule.capPrice ?? null),
    finalPrice: lot.finalPrice,
    leadingNickname: lot.leadingNickname,
    bidCount: lot.bidCount ?? lot.stats?.bidCount,
    updatedAtUnixMs: lot.updatedAtUnixMs,
  };
}

function searchResults(reply) {
  const lots = Array.isArray(reply) ? reply : reply?.lots;
  if (!Array.isArray(lots)) throw new Error('lot price 搜索结果无效');
  const found = lots.slice(0, 5);
  // Only a complete first page can prove that an exact title is unique.
  const complete = Array.isArray(reply) ? lots.length <= 5
    : Number.isSafeInteger(reply.total) && reply.total === found.length && reply.page === 1;
  return { found, complete };
}

export async function getLotPrice(args, { api }, signal) {
  const input = normalizeLotPriceArgs(args);
  if (!api || typeof api.getLot !== 'function' || typeof api.searchLots !== 'function') throw new Error('lot price API 不可用');
  if (input.lotId) return safePrice(await api.getLot(input.lotId, signal));
  const { found, complete } = searchResults(await api.searchLots({ keyword: input.keyword, roomId: input.roomId, pageSize: 5 }, signal));
  const exact = found.filter(lot => typeof lot?.title === 'string'
    && lot.title.localeCompare(input.keyword, 'zh-CN', { sensitivity: 'base' }) === 0);
  if (complete && exact.length === 1) return safePrice(await api.getLot(lotIdOf(exact[0]), signal));
  if (complete && found.length === 1) return safePrice(await api.getLot(lotIdOf(found[0]), signal));
  if (complete && found.length === 0) return { status: 'LOT_NOT_FOUND', keyword: input.keyword };
  return { status: 'LOT_AMBIGUOUS', candidates: found.map(candidateSummary) };
}

function normalizeLiveLotsArgs(args = {}) {
  if (!args || typeof args !== 'object' || Array.isArray(args)
    || Object.keys(args).some(key => !['roomId', 'roomName', 'includeRecentBids'].includes(key))) throw new Error('live lots 参数无效');
  for (const key of ['roomId', 'roomName']) {
    if (args[key] !== undefined && (typeof args[key] !== 'string' || !args[key].trim() || args[key].length > 256)) throw new Error(`live lots ${key} 无效`);
  }
  if (args.roomId !== undefined && args.roomName !== undefined) throw new Error('live lots roomId 和 roomName 只能提供一个');
  if (args.includeRecentBids !== undefined && typeof args.includeRecentBids !== 'boolean') throw new Error('live lots includeRecentBids 无效');
  return { roomId: args.roomId?.trim(), roomName: args.roomName?.trim(), includeRecentBids: args.includeRecentBids ?? false };
}

function liveRoom(snapshot, room, includeRecentBids) {
  if (snapshot.roomId !== room.id) throw new Error('live lots 房间快照无效');
  const current = snapshot.currentLot;
  const value = {
    roomId: room.id, roomName: room.name, snapshotAtUnixMs: snapshot.serverTimeUnixMs,
    currentLot: current ? {
      lotId: current.id, title: current.title, status: current.status,
      startPrice: current.startPrice, currentPrice: current.currentPrice, minIncrement: current.minIncrement,
      leadingNickname: current.leadingNickname, participantCount: current.participantCount, bidCount: current.bidCount,
      startedAtUnixMs: current.startedAtUnixMs, endsAtUnixMs: current.endsAtUnixMs,
      remainingMs: Math.max(0, current.endsAtUnixMs - snapshot.serverTimeUnixMs),
    } : null,
    nextLot: null,
  };
  if (includeRecentBids) value.recentBids = snapshot.recentBids.slice(-10).map(bid => ({ nickname: bid.nickname, amount: bid.amount, createdAtUnixMs: bid.createdAtUnixMs }));
  return value;
}

export async function getLiveLots(args, { api }, signal) {
  const input = normalizeLiveLotsArgs(args);
  signal?.throwIfAborted();
  const allRooms = await api.listRooms(signal);
  signal?.throwIfAborted();
  if (!Array.isArray(allRooms) || allRooms.length > 100) throw new Error('live lots 房间列表超过安全限制或无效');
  let selected = allRooms;
  if (input.roomId) selected = allRooms.filter(room => room.id === input.roomId);
  else if (input.roomName) selected = allRooms.filter(room => room.name.toLocaleLowerCase() === input.roomName.toLocaleLowerCase());
  else selected = allRooms.filter(room => room.status === 'ACTIVE');
  selected = [...selected].sort((a, b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
  if ((input.roomId || input.roomName) && selected.length === 0) return { status: 'ROOM_NOT_FOUND' };
  if (input.roomName && selected.length > 1) return { status: 'ROOM_AMBIGUOUS', candidates: selected.slice(0, 5).map(room => ({ roomId: room.id, name: room.name, status: room.status })) };

  const results = new Array(selected.length);
  let cursor = 0;
  async function worker() {
    while (cursor < selected.length) {
      signal?.throwIfAborted();
      const index = cursor++;
      const room = selected[index];
      let value;
      try {
        const snapshot = await api.getRoomSnapshot(room.id, signal);
        signal?.throwIfAborted();
        value = liveRoom(snapshot, room, input.includeRecentBids);
        // Sequential calls per worker keep total HTTP concurrency at four.
        const next = await api.getNextQueuedLot(room.id, signal);
        signal?.throwIfAborted();
        if (next) value.nextLot = { lotId: next.id, title: next.title, queuePosition: next.queuePosition, startPrice: next.startPrice };
        results[index] = { value };
      } catch (error) {
        signal?.throwIfAborted();
        if (!(error instanceof BackendUnavailableError || error instanceof InvalidBackendResponseError)) throw error;
        results[index] = { value, error: { roomId: room.id, code: 'BACKEND_UNAVAILABLE' } };
      }
    }
  }
  await Promise.all(Array.from({ length: Math.min(4, selected.length) }, () => worker()));
  signal?.throwIfAborted();
  const rooms = results.filter(result => result.value).map(result => result.value);
  const partialErrors = results.filter(result => result.error).map(result => result.error);
  const status = rooms.some(room => room.currentLot) ? 'OK' : partialErrors.length ? 'BACKEND_UNAVAILABLE' : 'NO_LIVE_LOT';
  return { status, rooms, partialErrors };
}
