import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it, vi } from 'vitest';
import type { LiveRoomController } from '../hooks/useLiveRoomController';
import { LiveComposer } from './LiveRoomView';

function controllerFixture(): LiveRoomController {
  return {
    meId: 'buyer-1',
    barrageSending: false,
    actions: {
      showNotice: vi.fn(),
      submitBarrage: vi.fn(async () => true),
      requireBuyerAuth: vi.fn(),
    },
  } as unknown as LiveRoomController;
}

describe('LiveComposer barrage input', () => {
  it('renders an inline barrage editor instead of a comment dialog when expanded', () => {
    const html = renderToStaticMarkup(
      <LiveComposer
        controller={controllerFixture()}
        expanded
        onExpandedChange={vi.fn()}
        onOpenAuction={vi.fn()}
        onOpenGift={vi.fn()}
        onOpenMore={vi.fn()}
      />,
    );

    expect(html).toContain('<form');
    expect(html).toContain('aria-label="发送弹幕"');
    expect(html).toContain('aria-label="输入弹幕"');
    expect(html).not.toContain('role="dialog"');
    expect(html).not.toContain('条评论');
  });
});
