import { describe, expect, it } from 'vitest';
import type { AuctionSocketEvent } from '../../../shared/api/types';
import { auctionRoomReducer, createInitialAuctionRoomState } from './auctionRoomReducer';

describe('auctionRoomReducer barrage feed', () => {
  it('prepends a batch, deduplicates echoed local messages, and bounds the feed', () => {
    let state = createInitialAuctionRoomState('room-1') as ReturnType<typeof createInitialAuctionRoomState> & {
      barrages: Array<{ id: string; content: string }>;
    };
    state = {
      ...state,
      barrages: Array.from({ length: 49 }, (_, index) => ({
        id: `old-${index}`,
        roomId: 'room-1',
        displayName: '拍友',
        content: `旧弹幕${index}`,
        priority: 0,
        createdAtMs: index,
        count: 1,
      })),
    };

    const next = auctionRoomReducer(state, {
      type: 'eventReceived',
      event: {
        type: 'BARRAGE_BATCH',
        roomId: 'room-1',
        messages: [
          { id: 'new-1', roomId: 'room-1', displayName: '拍友', content: '新弹幕', priority: 0, createdAtMs: 100, count: 1 },
          { id: 'old-0', roomId: 'room-1', displayName: '拍友', content: '重复弹幕', priority: 0, createdAtMs: 99, count: 1 },
        ],
      } as unknown as AuctionSocketEvent,
    }) as typeof state;

    expect(next.barrages).toHaveLength(50);
    expect(next.barrages.slice(0, 2).map((item) => item.id)).toEqual(['new-1', 'old-0']);
    expect(next.barrages.filter((item) => item.id === 'old-0')).toHaveLength(1);
  });
});
