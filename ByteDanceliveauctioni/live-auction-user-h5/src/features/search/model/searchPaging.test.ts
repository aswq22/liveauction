import { describe, expect, it, vi } from 'vitest';
import { AppApiError } from '../../../shared/api/errors';
import { loadSubmittedSearchPage } from './searchPaging';
const page = { results: [], mode: 'HYBRID' as const, degradedReason: '', nextCursor: '', applied: {}, warnings: [] };
describe('submitted search paging', () => {
 it('uses submitted identity rather than an edited draft', async () => {
  const submitted = {query:'玉',roomId:'room-a'}; const draft = '相机';
  const load = vi.fn().mockResolvedValue(page);
  await loadSubmittedSearchPage(submitted,'cursor-a',undefined,load);
  expect(draft).toBe('相机');expect(load).toHaveBeenCalledWith('玉',{roomId:'room-a',cursor:'cursor-a',signal:undefined});
 });
 it('refreshes expired cursor once and replaces the old result list', async () => {
  const load = vi.fn().mockRejectedValueOnce(new AppApiError('SEARCH_CURSOR_EXPIRED',{code:410301,kind:'result'})).mockResolvedValueOnce(page);
  const result = await loadSubmittedSearchPage({query:'玉',roomId:''},'expired',undefined,load);
  expect(load).toHaveBeenCalledTimes(2);expect(result.append).toBe(false);expect(load.mock.calls[1][1].cursor).toBe('');
 });
});
