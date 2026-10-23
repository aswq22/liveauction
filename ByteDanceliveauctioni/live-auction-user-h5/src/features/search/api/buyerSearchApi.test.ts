import { describe, expect, it } from 'vitest';
import { normalizeBuyerSearchReply } from './buyerSearchApi';
describe('buyer search API contract', () => {
 it('normalizes protobuf strings and makes lexical degradation explicit', () => {
  const page = normalizeBuyerSearchReply({mode:'LEXICAL',degradedReason:'EMBEDDING_UNAVAILABLE',hits:[{lot:{id:'a',title:'翡翠',roomId:'r',status:'LOT_STATUS_LIVE',currentPrice:{amount:'301',currency:'CNY'}},factsReadAtUnixMs:'123'}],appliedFilters:{maxPriceAmount:'300000'},nextCursor:'signed'});
  expect(page.results[0].currentPrice?.amount).toBe('301');expect(page.mode).toBe('LEXICAL');expect(page.nextCursor).toBe('signed');expect(page.applied.maxPriceAmount).toBe('300000');
 });
 it('does not silently show drafts or malformed modes', () => {
  expect(() => normalizeBuyerSearchReply({mode:'UNKNOWN'})).toThrow();
  expect(() => normalizeBuyerSearchReply({mode:'HYBRID',hits:[{lot:{id:'draft',status:'LOT_STATUS_DRAFT'}}]})).toThrow();
 });
 it('rejects missing required facts and malformed prices instead of manufacturing zero', () => {
  const lot = {id:'a',title:'玉',roomId:'r',status:'LOT_STATUS_LIVE',currentPrice:{amount:'100',currency:'CNY'}};
  for (const invalid of [{...lot,currentPrice:undefined},{...lot,id:undefined},{...lot,title:undefined},{...lot,roomId:undefined},
    {...lot,currentPrice:{amount:'-1',currency:'CNY'}},{...lot,currentPrice:{amount:'abc',currency:'CNY'}},{...lot,currentPrice:{amount:'100',currency:'USD'}}]) {
    expect(() => normalizeBuyerSearchReply({mode:'HYBRID',hits:[{lot:invalid}]})).toThrow();
  }
 });
});
