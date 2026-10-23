import { normalizeLot } from '../../../shared/api/adapters';
import { apiRequest } from '../../../shared/api/httpClient';
import { LOT_STATUS, type AIBuyerResult } from '../../../shared/api/types';

type SearchFilters = { roomId?: string; minPriceAmount?: string; maxPriceAmount?: string; statuses?: string[] };
export type BuyerSearchPage = {
  results: AIBuyerResult[];
  mode: 'HYBRID' | 'LEXICAL';
  degradedReason: string;
  nextCursor: string;
  applied: SearchFilters;
  warnings: string[];
};
type WireReply = {
  mode?: string; degradedReason?: string; nextCursor?: string;
  appliedFilters?: SearchFilters; warnings?: string[];
  hits?: Array<{ lot?: unknown; matchReason?: string; factsReadAtUnixMs?: string }>;
};
function validAmount(value: unknown): boolean {
  if (typeof value === 'number') return Number.isSafeInteger(value) && value >= 0;
  return typeof value === 'string' && /^\d+$/.test(value) && value.length <= 19 && BigInt(value) <= 9223372036854775807n;
}
function requiredLot(raw: unknown) {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) throw new Error('搜索拍品数据无效');
  const value = raw as Record<string, unknown>;
  for (const field of ['id', 'roomId', 'title']) {
    if (typeof value[field] !== 'string' || !(value[field] as string).trim()) throw new Error('搜索拍品字段缺失，请重新搜索');
  }
  if (!new Set<string>([LOT_STATUS.LIVE, LOT_STATUS.QUEUED, LOT_STATUS.EXTENDED]).has(String(value.status))) throw new Error('搜索返回了非公开拍品');
  const price = value.currentPrice as { amount?: unknown; currency?: unknown } | undefined;
  if (!price || price.currency !== 'CNY' || !validAmount(price.amount)) throw new Error('搜索拍品价格无效，请重新搜索');
  return normalizeLot(raw);
}
export function normalizeBuyerSearchReply(raw: WireReply): BuyerSearchPage {
  if (raw.mode !== 'HYBRID' && raw.mode !== 'LEXICAL') throw new Error('搜索响应无效，请稍后重试');
  const results = (raw.hits || []).map((hit) => {
    const lot = requiredLot(hit.lot);
    return { type: 'lot', title: lot.title, lotId: lot.id, roomId: lot.roomId, status: lot.status,
      currentPrice: lot.currentPrice, imageUrl: lot.imageUrl, href: `/m/room/${encodeURIComponent(lot.roomId)}`,
      reason: hit.matchReason || '与搜索描述相关' };
  });
  return { results, mode: raw.mode, degradedReason: raw.degradedReason || '', nextCursor: raw.nextCursor || '',
    applied: raw.appliedFilters || {}, warnings: raw.warnings || [] };
}
export async function searchBuyerLots(query: string, options: {roomId?: string; cursor?: string; signal?: AbortSignal} = {}): Promise<BuyerSearchPage> {
  const raw = await apiRequest<WireReply>({ path: '/api/buyer/lots/search', method: 'POST', auth: 'none',
    body: { query, roomId: options.roomId || '', cursor: options.cursor || '', pageSize: 20 }, signal: options.signal,
    operation: 'buyer-search' });
  return normalizeBuyerSearchReply(raw);
}
