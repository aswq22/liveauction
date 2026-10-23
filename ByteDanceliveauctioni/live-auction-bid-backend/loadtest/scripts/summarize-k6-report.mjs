#!/usr/bin/env node

import { readFile } from 'node:fs/promises';

const files = process.argv.slice(2);
if (!files.length) {
  throw new Error('Usage: node loadtest/scripts/summarize-k6-report.mjs <summary.json>...');
}

const rows = [];
for (const file of files) {
  const summary = JSON.parse(await readFile(file, 'utf8'));
  const metrics = summary.metrics || {};
  rows.push({
    file,
    http_reqs: value(metrics.http_reqs?.count),
    qps: rate(metrics.http_reqs),
    failedRate: rateValue(metrics.http_req_failed),
    p95: trend(metrics.http_req_duration, 'p(95)'),
    p99: trend(metrics.http_req_duration, 'p(99)'),
    max: trend(metrics.http_req_duration, 'max'),
    iterations: value(metrics.iterations?.count)
  });
}

console.table(rows.map((row) => ({
  file: row.file.replace(/^.*[\\/](?=[^\\/]+$)/, ''),
  http_reqs: row.http_reqs,
  qps: row.qps.toFixed(2),
  failed_rate: `${(row.failedRate * 100).toFixed(2)}%`,
  p95_ms: row.p95.toFixed(2),
  p99_ms: row.p99.toFixed(2),
  max_ms: row.max.toFixed(2),
  iterations: row.iterations
})));

function value(raw) {
  return Number(raw || 0);
}

function rate(metric) {
  return Number(metric?.rate || 0);
}

function rateValue(metric) {
  return Number(metric?.value || metric?.rate || 0);
}

function trend(metric, key) {
  return Number(metric?.percentiles?.[key] ?? metric?.[key] ?? 0);
}
