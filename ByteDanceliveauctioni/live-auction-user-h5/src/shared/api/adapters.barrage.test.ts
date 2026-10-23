import { describe, expect, it } from 'vitest';
import { normalizeAuctionEvent } from './adapters';

describe('normalizeAuctionEvent barrage batches', () => {
  it('preserves the public barrage batch and its display fields', () => {
    const event = normalizeAuctionEvent({
      type: 'BARRAGE_BATCH',
      roomId: 'room-1',
      windowMs: 100,
      messages: [{
        id: 'barrage-1',
        roomId: 'room-1',
        displayName: '小明',
        content: '这个拍品真不错',
        priority: 0,
        createdAtMs: 1_786_000_000_000,
        count: 3,
      }],
    });

    expect(event.type).toBe('BARRAGE_BATCH');
    expect((event as unknown as { windowMs?: number }).windowMs).toBe(100);
    expect((event as unknown as { messages?: unknown[] }).messages).toEqual([{
      id: 'barrage-1',
      roomId: 'room-1',
      displayName: '小明',
      content: '这个拍品真不错',
      priority: 0,
      createdAtMs: 1_786_000_000_000,
      count: 3,
    }]);
  });
});
