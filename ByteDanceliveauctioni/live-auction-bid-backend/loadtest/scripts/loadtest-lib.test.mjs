import test from 'node:test';
import assert from 'node:assert/strict';

import {
  assertLoadtestTarget,
  formatMarkdownReport,
  summarizeK6Metrics
} from './loadtest-lib.mjs';

test('assertLoadtestTarget rejects the normal native backend and non-loadtest databases', () => {
  assert.throws(
    () => assertLoadtestTarget('http://127.0.0.1:18080', { mysql: { database: 'live_auction_loadtest' } }),
    /normal backend/
  );
  assert.throws(
    () => assertLoadtestTarget('http://127.0.0.1:18081', { mysql: { database: 'live_auction' } }),
    /non-loadtest database/
  );
});

test('summarizeK6Metrics extracts qps, p95, p99, max, and failed rate from k6 summary json', () => {
  const summary = summarizeK6Metrics('bid-stage-50.json', {
    metrics: {
      http_reqs: { count: 1200, rate: 400 },
      http_req_failed: { value: 0.0025 },
      http_req_duration: {
        percentiles: {
          'p(95)': 72.25,
          'p(99)': 131.5
        },
        max: 420.9
      },
      iterations: { count: 1198 }
    }
  });

  assert.equal(summary.file, 'bid-stage-50.json');
  assert.equal(summary.httpReqs, 1200);
  assert.equal(summary.qps, 400);
  assert.equal(summary.failedRate, 0.0025);
  assert.equal(summary.p95Ms, 72.25);
  assert.equal(summary.p99Ms, 131.5);
  assert.equal(summary.maxMs, 420.9);
  assert.equal(summary.iterations, 1198);
});

test('formatMarkdownReport includes performance, idempotency, and eventual consistency sections', () => {
  const markdown = formatMarkdownReport({
    runId: 'prod-lt-1',
    startedAt: '2026-06-21T00:00:00.000Z',
    completedAt: '2026-06-21T00:15:00.000Z',
    baseUrl: 'http://127.0.0.1:18081',
    stages: [
      {
        name: 'bid-50',
        script: 'auction-bid-hot-path.js',
        vus: 50,
        duration: '30s',
        summary: {
          httpReqs: 1200,
          qps: 400,
          failedRate: 0,
          p95Ms: 72.25,
          p99Ms: 131.5,
          maxMs: 420.9
        }
      }
    ],
    verification: {
      idempotency: { ok: true, notes: ['payment rows: 1'] },
      eventualConsistency: { ok: true, notes: ['outbox pending: 0'] },
      resources: { ok: true, notes: ['redis timeouts: 0'] }
    },
    conclusion: 'PASS'
  });

  assert.match(markdown, /# Production Load Test Report/);
  assert.match(markdown, /bid-50/);
  assert.match(markdown, /支付幂等/);
  assert.match(markdown, /最终一致/);
  assert.match(markdown, /PASS/);
});
