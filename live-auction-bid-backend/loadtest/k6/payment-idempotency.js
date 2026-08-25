import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Rate } from 'k6/metrics';
import { SharedArray } from 'k6/data';

const data = new SharedArray('payment-loadtest-data', () => [JSON.parse(open(__ENV.DATA_FILE))])[0];
const baseUrl = (__ENV.BASE_URL || data.baseUrl || 'http://127.0.0.1:18081').replace(/\/+$/, '');
const vus = Number(__ENV.VUS || 100);
const duration = __ENV.DURATION || '1m';
const idempotencyKey = __ENV.IDEMPOTENCY_KEY || `k6-payment-same-key-${data.runId}`;

export const options = {
  scenarios: {
    same_order_same_key_retries: {
      executor: 'constant-vus',
      vus,
      duration,
      gracefulStop: '10s'
    }
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<600', 'p(99)<1200'],
    payment_unexpected_error_rate: ['rate<0.01']
  }
};

const paymentOk = new Counter('payment_ok_total');
const paymentUnexpectedErrorRate = new Rate('payment_unexpected_error_rate');

export default function () {
  assertLoadtestTarget(baseUrl);
  if (!data.order?.id || !data.winner?.token) {
    throw new Error('DATA_FILE must be prepared with LOADTEST_MODE=payment or mixed.');
  }
  const payload = JSON.stringify({ idempotency_key: idempotencyKey });
  const response = http.post(`${baseUrl}/api/orders/${encodeURIComponent(data.order.id)}/mock-pay`, payload, {
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
      Authorization: `Bearer ${data.winner.token}`,
      'Idempotency-Key': idempotencyKey
    },
    tags: { endpoint: 'mock_pay_order' }
  });
  const body = safeJson(response);
  const ok = check(response, {
    'payment http status is 2xx': (r) => r.status >= 200 && r.status < 300,
    'payment body is json': () => body !== null,
    'payment has result or payment': () => Boolean(body?.result || body?.payment || body?.order)
  });
  paymentUnexpectedErrorRate.add(!ok);
  if (ok) paymentOk.add(1);
  sleep(Number(__ENV.SLEEP_SECONDS || 0));
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
