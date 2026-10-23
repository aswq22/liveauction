import { afterEach, describe, expect, it, vi } from 'vitest';
import { setAuthProvider } from '../../../shared/api/httpClient';
import * as auctionApi from './auctionApi';

describe('submitBarrage', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('posts authenticated content to the room barrage admission endpoint', async () => {
    setAuthProvider({
      getValidAccessToken: async () => 'buyer-token',
      refreshOnce: async () => null,
      expire: () => undefined,
    });
    const fetchSpy = vi.fn(async () => new Response(JSON.stringify({ accepted: true, id: 'barrage-7' }), {
      status: 202,
      headers: { 'Content-Type': 'application/json' },
    }));
    vi.stubGlobal('fetch', fetchSpy);

    expect(auctionApi).toHaveProperty('submitBarrage');
    const submitBarrage = (auctionApi as unknown as {
      submitBarrage: (roomId: string, content: string) => Promise<{ accepted: boolean; id: string }>;
    }).submitBarrage;
    await expect(submitBarrage('room/a', '  好看！  ')).resolves.toEqual({ accepted: true, id: 'barrage-7' });

    const [url, init] = fetchSpy.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe('/api/realtime/rooms/room%2Fa/barrages');
    expect(init.method).toBe('POST');
    expect(init.body).toBe(JSON.stringify({ content: '好看！' }));
    expect(new Headers(init.headers).get('Authorization')).toBe('Bearer buyer-token');
  });

  it.each([
    ['', '请输入弹幕内容'],
    ['   ', '请输入弹幕内容'],
    ['弹'.repeat(101), '弹幕最多 100 个字'],
  ])('rejects invalid content before sending: %s', async (content, message) => {
    setAuthProvider({
      getValidAccessToken: async () => 'buyer-token',
      refreshOnce: async () => null,
      expire: () => undefined,
    });
    const fetchSpy = vi.fn();
    vi.stubGlobal('fetch', fetchSpy);

    const submitBarrage = (auctionApi as unknown as {
      submitBarrage: (roomId: string, content: string) => Promise<{ accepted: boolean; id: string }>;
    }).submitBarrage;
    await expect(submitBarrage('room-1', content)).rejects.toThrow(message);
    expect(fetchSpy).not.toHaveBeenCalled();
  });
});
