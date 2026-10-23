import { AppApiError } from '../../../shared/api/errors';
import { searchBuyerLots, type BuyerSearchPage } from '../api/buyerSearchApi';
export type SubmittedSearch = { query: string; roomId: string };
type Loader = typeof searchBuyerLots;
export async function loadSubmittedSearchPage(identity: SubmittedSearch, cursor: string, signal?: AbortSignal, load: Loader = searchBuyerLots): Promise<{ page: BuyerSearchPage; append: boolean }> {
  try {
    const page = await load(identity.query, { roomId: identity.roomId, cursor, signal });
    return { page, append: Boolean(cursor) };
  } catch (error) {
    if (!cursor || signal?.aborted || !(error instanceof AppApiError) || error.code !== 410301) throw error;
    // Exactly one refresh; discard the previous page's facts and IDs.
    const page = await load(identity.query, { roomId: identity.roomId, cursor: '', signal });
    return { page, append: false };
  }
}
