import http from 'k6/http';
import ws from 'k6/ws';
import { check } from 'k6';
import { Counter, Rate } from 'k6/metrics';
import { SharedArray } from 'k6/data';

const data = new SharedArray('ws-loadtest-data', () => [JSON.parse(open(__ENV.DATA_FILE))])[0];
const baseUrl = (__ENV.BASE_URL || data.baseUrl || 'http://127.0.0.1:18081').replace(/\/+$/, '');
const wsBaseUrl = baseUrl.replace(/^http/, 'ws');

export const options = {
  scenarios: {
    websocket_watchers: {
      executor: 'constant-vus',
      vus: Number(__ENV.WS_VUS || 100),
      duration: __ENV.DURATION || '1m',
      gracefulStop: '10s'
    }
  },
  thresholds: {
    ws_connecting: ['p(95)<500'],
    websocket_error_rate: ['rate<0.02']
  }
};

const wsMessages = new Counter('websocket_messages_total');
const websocketErrorRate = new Rate('websocket_error_rate');

export default function () {
  assertLoadtestTarget(baseUrl);
  const buyer = data.buyers[(__VU - 1) % data.buyers.length];
  const ticketResponse = http.post(`${baseUrl}/api/realtime/ws-ticket`, JSON.stringify({
    roomId: data.room.id
  }), {
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
      Authorization: `Bearer ${buyer.token}`
    },
    tags: { endpoint: 'ws_ticket' }
  });
  const ticketBody = safeJson(ticketResponse);
  const ticket = ticketBody?.ticket || ticketBody?.wsTicket || ticketBody?.ws_ticket || '';
  const okTicket = check(ticketResponse, {
    'ws ticket status is 2xx': (r) => r.status >= 200 && r.status < 300,
    'ws ticket returned': () => Boolean(ticket)
  });
  if (!okTicket) {
    websocketErrorRate.add(true);
    return;
  }

  const url = `${wsBaseUrl}/ws/rooms/${encodeURIComponent(data.room.id)}?scope=public&ticket=${encodeURIComponent(ticket)}`;
  const response = ws.connect(url, {}, (socket) => {
    socket.on('message', () => wsMessages.add(1));
    socket.on('error', () => websocketErrorRate.add(true));
    socket.setTimeout(() => socket.close(), Number(__ENV.WS_HOLD_MS || 10000));
  });
  websocketErrorRate.add(Boolean(response?.status && response.status !== 101));
}

function safeJson(response) {
  try {
    return response.json();
  } catch (_) {
    return null;
  }
}

function assertLoadtestTarget(url) {
  if (url.includes('127.0.0.1:18080') || url.includes('localhost:18080')) {
    throw new Error(`Refusing to run loadtest against normal backend: ${url}`);
  }
}
