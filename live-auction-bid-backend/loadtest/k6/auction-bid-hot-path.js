import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import { SharedArray } from 'k6/data';

const data = new SharedArray('auction-loadtest-data', () => [JSON.parse(open(__ENV.DATA_FILE))])[0];
const baseUrl = (__ENV.BASE_URL || data.baseUrl || 'http://127.0.0.1:18081').replace(/\/+$/, '');
const duration = __ENV.DURATION || '1m';
const vus = Number(__ENV.VUS || 100);
const startPrice = Number(data.money?.startPrice || 10000);
const minIncrement = Number(data.money?.minIncrement || 100);

export const options = {
  scenarios: {
    bid_hot_path: {
      executor: 'constant-vus',
      vus,
      duration,
      gracefulStop: '10s'
    }
  },
  thresholds: {
    http_req_failed: ['rate<0.02'],
    http_req_duration: ['p(95)<800', 'p(99)<1500'],
    bid_transport_error_rate: ['rate<0.02']
  }
};

const bidAccepted = new Counter('bid_accepted_total');
const bidRejected = new Counter('bid_rejected_total');
const bidTransportErrorRate = new Rate('bid_transport_error_rate');
const bidBusinessLatency = new Trend('bid_business_latency_ms');

export default function () {
  assertLoadtestTarget(baseUrl);
  const buyer = data.buyers[(__VU - 1) % data.buyers.length];
  const sequence = (__ITER * data.buyers.length) + (__VU - 1) + 1;
  const amount = startPrice + sequence * minIncrement;
  const idempotencyKey = `k6-bid-${data.runId}-${__VU}-${__ITER}`;
  const payload = JSON.stringify({
    amount: { amount, currency: 'CNY' },
    idempotency_key: idempotencyKey
  });
  const started = Date.now();
  const response = http.post(`${baseUrl}/api/lots/${encodeURIComponent(data.lot.id)}/bid`, payload, {
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
      Authorization: `Bearer ${buyer.token}`,
      'Idempotency-Key': idempotencyKey
    },
    tags: { endpoint: 'place_bid' }
  });
  bidBusinessLatency.add(Date.now() - started);
  const ok = check(response, {
    'bid http status is 2xx': (r) => r.status >= 200 && r.status < 300,
    'bid body is json': (r) => safeJson(r) !== null
  });
  bidTransportErrorRate.add(!ok);
  const body = safeJson(response);
  const businessOK = !body?.result || Number(body.result.code || 0) === 0;
  if (businessOK && (body?.accepted || body?.bid?.id)) {
    bidAccepted.add(1);
  } else {
    bidRejected.add(1);
  }
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
