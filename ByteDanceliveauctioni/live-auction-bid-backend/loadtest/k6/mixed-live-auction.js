import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate } from 'k6/metrics';
import { SharedArray } from 'k6/data';

const data = new SharedArray('mixed-loadtest-data', () => [JSON.parse(open(__ENV.DATA_FILE))])[0];
const baseUrl = (__ENV.BASE_URL || data.baseUrl || 'http://127.0.0.1:18081').replace(/\/+$/, '');

export const options = {
  scenarios: {
    viewers: {
      executor: 'constant-vus',
      exec: 'viewer',
      vus: Number(__ENV.VIEWER_VUS || 300),
      duration: __ENV.DURATION || '3m',
      gracefulStop: '10s'
    },
    bidders: {
      executor: 'constant-vus',
      exec: 'bidder',
      vus: Number(__ENV.BIDDER_VUS || 50),
      duration: __ENV.DURATION || '3m',
      gracefulStop: '10s'
    },
    payment_retries: {
      executor: 'constant-vus',
      exec: 'payer',
      vus: Number(__ENV.PAYER_VUS || 20),
      duration: __ENV.PAY_DURATION || '1m',
      startTime: __ENV.PAY_START_TIME || '15s',
      gracefulStop: '10s'
    }
  },
  thresholds: {
    http_req_failed: ['rate<0.03'],
    http_req_duration: ['p(95)<1000', 'p(99)<2000'],
    mixed_unexpected_error_rate: ['rate<0.03']
  }
};

const unexpectedErrorRate = new Rate('mixed_unexpected_error_rate');

export function viewer() {
  assertLoadtestTarget(baseUrl);
  const response = http.get(`${baseUrl}/api/rooms/${encodeURIComponent(data.room.id)}/snapshot`, {
    headers: { Accept: 'application/json' },
    tags: { endpoint: 'room_snapshot' }
  });
  const ok = check(response, {
    'snapshot status is 2xx': (r) => r.status >= 200 && r.status < 300,
    'snapshot json': (r) => safeJson(r) !== null
  });
  unexpectedErrorRate.add(!ok);
  sleep(Number(__ENV.VIEWER_SLEEP_SECONDS || 1));
}

export function bidder() {
  assertLoadtestTarget(baseUrl);
  const buyer = data.buyers[(__VU - 1) % data.buyers.length];
  const sequence = (__ITER * data.buyers.length) + (__VU - 1) + 1;
  const amount = Number(data.money.startPrice) + sequence * Number(data.money.minIncrement);
  const idempotencyKey = `k6-mixed-bid-${data.runId}-${__VU}-${__ITER}`;
  const response = http.post(`${baseUrl}/api/lots/${encodeURIComponent(data.lot.id)}/bid`, JSON.stringify({
    amount: { amount, currency: 'CNY' },
    idempotency_key: idempotencyKey
  }), {
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
      Authorization: `Bearer ${buyer.token}`,
      'Idempotency-Key': idempotencyKey
    },
    tags: { endpoint: 'place_bid' }
  });
  const ok = check(response, {
    'mixed bid status is 2xx': (r) => r.status >= 200 && r.status < 300,
    'mixed bid json': (r) => safeJson(r) !== null
  });
  unexpectedErrorRate.add(!ok);
  sleep(Number(__ENV.BIDDER_SLEEP_SECONDS || 0.2));
}

export function payer() {
  assertLoadtestTarget(baseUrl);
  if (!data.order?.id || !data.winner?.token) {
    sleep(1);
    return;
  }
  const idempotencyKey = `k6-mixed-payment-same-key-${data.runId}`;
  const response = http.post(`${baseUrl}/api/orders/${encodeURIComponent(data.order.id)}/mock-pay`, JSON.stringify({
    idempotency_key: idempotencyKey
  }), {
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
      Authorization: `Bearer ${data.winner.token}`,
      'Idempotency-Key': idempotencyKey
    },
    tags: { endpoint: 'mock_pay_order' }
  });
  const ok = check(response, {
    'mixed pay status is 2xx': (r) => r.status >= 200 && r.status < 300,
    'mixed pay json': (r) => safeJson(r) !== null
  });
  unexpectedErrorRate.add(!ok);
  sleep(Number(__ENV.PAYER_SLEEP_SECONDS || 0.5));
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
