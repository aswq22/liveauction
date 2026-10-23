import { describe, expect, it } from 'vitest';
import { decodeExecution, decodeRecoveredExecution, decodeReviewSnapshot } from './agentApi';

const fields = ['antiSnipeExtendSeconds', 'antiSnipeWindowSeconds', 'capPriceAmount', 'category', 'depositAmount', 'description', 'durationSeconds', 'mainImageAssetId', 'maxExtendCount', 'minIncrementAmount', 'startPriceAmount', 'stock', 'tags', 'title'];
const draft = { title: '海洋之心', startPriceAmount: 10000, durationSeconds: 600, mainImageAssetId: 'asset-1', minIncrementAmount: 100, capPriceAmount: 20000, category: '珠宝', description: '介绍', tags: ['精选'], stock: 1, antiSnipeWindowSeconds: 10, antiSnipeExtendSeconds: 15, maxExtendCount: 5, depositAmount: 0 };
const snapshot = { snapshotVersion: 2, contentHash: 'a'.repeat(64), candidates: [{ candidateId: 'candidate-1', candidateVersion: 3, sourceType: 'NEW', draft, fieldSources: fields.map((field) => ({ field, source: field === 'description' ? 'AI' : 'USER' })), assetIds: ['asset-1'] }] };

describe('review/apply runtime decoders', () => {
  it('decodes complete immutable snapshots and exact execution results', () => {
    expect(decodeReviewSnapshot(snapshot)).toEqual(snapshot);
    expect(decodeExecution({ executionId: 'execution-1', createdLotIds: ['lot-1'], queuedLots: [{ lotId: 'lot-1', queuePosition: 4 }] })).toEqual({ executionId: 'execution-1', createdLotIds: ['lot-1'], queuedLots: [{ lotId: 'lot-1', queuePosition: 4 }], target: 'QUEUE' });
    expect(decodeExecution({ executionId: 'execution-2', target: 'INVENTORY_ONLY', createdLotIds: ['lot-2'], queuedLots: [] })).toEqual({ executionId: 'execution-2', target: 'INVENTORY_ONLY', createdLotIds: ['lot-2'], queuedLots: [] });
  });

  it('decodes the sanitized real GET response for a recovered legacy snapshot', () => {
    const realWire = {
      result: { message: 'ok', trace_id: 'redacted' }, snapshotVersion: 2, contentHash: 'b'.repeat(64),
      candidates: [{
        candidateId: 'candidate-legacy', candidateVersion: 3, sourceType: 'NEW',
        draft: { title: '海洋之心', startPriceAmount: 1_000_000, durationSeconds: 3_000, mainImageAssetId: 'asset-redacted', minIncrementAmount: 10_000, capPriceAmount: 10_000_000, category: '', description: '', tags: [], stock: 1, antiSnipeWindowSeconds: 30, antiSnipeExtendSeconds: 15, maxExtendCount: 5, depositAmount: 0 },
        fieldSources: fields.map((field) => ({ field, source: ['category', 'description', 'tags', 'depositAmount'].includes(field) ? 'AI' : 'USER' })),
        assetIds: ['asset-redacted'],
      }],
    };
    const decoded = decodeReviewSnapshot(realWire);
    expect(decoded.snapshotVersion).toBe(2);
    expect(decoded.candidates[0].draft.tags).toEqual([]);
    expect(decoded.candidates[0].fieldSources.find((item) => item.field === 'category')?.source).toBe('AI');
  });

  it('rejects an unknown provenance source instead of rendering it as trusted', () => {
    const unknown = { ...snapshot, candidates: [{ ...snapshot.candidates[0], fieldSources: snapshot.candidates[0].fieldSources.map((item) => item.field === 'description' ? { ...item, source: 'VENDOR_IMPORT' } : item) }] };
    expect(() => decodeReviewSnapshot(unknown)).toThrow('Agent 响应格式无效');
  });

  it.each([
    ['bad hash', { ...snapshot, contentHash: 'short' }],
    ['duplicate candidates', { ...snapshot, candidates: [snapshot.candidates[0], snapshot.candidates[0]] }],
    ['missing source', { ...snapshot, candidates: [{ ...snapshot.candidates[0], fieldSources: snapshot.candidates[0].fieldSources.slice(1) }] }],
    ['duplicate source', { ...snapshot, candidates: [{ ...snapshot.candidates[0], fieldSources: [...snapshot.candidates[0].fieldSources, snapshot.candidates[0].fieldSources[0]] }] }],
    ['wrong asset identity', { ...snapshot, candidates: [{ ...snapshot.candidates[0], assetIds: ['asset-other'] }] }],
    ['invalid draft range', { ...snapshot, candidates: [{ ...snapshot.candidates[0], draft: { ...draft, stock: 0 } }] }],
  ])('rejects malformed snapshot: %s', (_name, value) => expect(() => decodeReviewSnapshot(value)).toThrow('Agent 响应格式无效'));

  it.each([
    ['duplicate created IDs', { executionId: 'execution-1', createdLotIds: ['lot-1', 'lot-1'], queuedLots: [{ lotId: 'lot-1', queuePosition: 1 }] }],
    ['duplicate queued IDs', { executionId: 'execution-1', createdLotIds: [], queuedLots: [{ lotId: 'lot-1', queuePosition: 1 }, { lotId: 'lot-1', queuePosition: 2 }] }],
    ['duplicate positions', { executionId: 'execution-1', createdLotIds: [], queuedLots: [{ lotId: 'lot-1', queuePosition: 1 }, { lotId: 'lot-2', queuePosition: 1 }] }],
    ['created missing from queue', { executionId: 'execution-1', createdLotIds: ['lot-2'], queuedLots: [{ lotId: 'lot-1', queuePosition: 1 }] }],
    ['invalid position', { executionId: 'execution-1', createdLotIds: [], queuedLots: [{ lotId: 'lot-1', queuePosition: 0 }] }],
  ])('rejects malformed execution: %s', (_name, value) => expect(() => decodeExecution(value)).toThrow('Agent 响应格式无效'));

  const recovered = { executionId: 'exec-1', snapshotVersion: 4, createdLotIds: ['lot-new'], queuedLots: [{ candidateId: 'candidate-existing', sourceType: 'EXISTING', lotId: 'lot-existing', queuePosition: 8 }, { candidateId: 'candidate-new', sourceType: 'NEW', lotId: 'lot-new', queuePosition: 9 }], target: 'QUEUE' as const };
  it('decodes durable queue and inventory-only executions', () => {
    expect(decodeRecoveredExecution(recovered)).toEqual(recovered);
    const inventory = { executionId: 'execution-2', snapshotVersion: 4, target: 'INVENTORY_ONLY', createdLotIds: ['lot-2'], queuedLots: [] } as const;
    expect(decodeRecoveredExecution(inventory)).toEqual(inventory);
  });
  it.each([
    ['unknown source', { ...recovered, queuedLots: recovered.queuedLots.map((lot, index) => index ? { ...lot, sourceType: 'VENDOR_IMPORT' } : lot) }],
    ['wrong created mapping', { ...recovered, createdLotIds: ['lot-existing'] }],
    ['non-contiguous positions', { ...recovered, queuedLots: recovered.queuedLots.map((lot, index) => index ? { ...lot, queuePosition: 10 } : lot) }],
  ])('rejects malformed recovered execution: %s', (_name, value) => expect(() => decodeRecoveredExecution(value)).toThrow('Agent 响应格式无效'));
});
