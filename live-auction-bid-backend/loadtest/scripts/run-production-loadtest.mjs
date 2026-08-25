#!/usr/bin/env node

import { existsSync } from 'node:fs';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  assertLoadtestTarget,
  formatMarkdownReport,
  latestSample,
  makeConclusion,
  parsePrometheusSamples,
  sleep,
  summarizeK6Metrics
} from './loadtest-lib.mjs';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const configPath = process.env.LOADTEST_CONFIG || path.join(repoRoot, 'loadtest', 'config.local.json');
const config = JSON.parse(await readFile(configPath, 'utf8'));
const baseUrl = (process.env.BASE_URL || config.baseUrl || 'http://127.0.0.1:18081').replace(/\/+$/, '');
const dockerBaseUrl = (process.env.LOADTEST_DOCKER_BASE_URL || 'http://auction-backend-loadtest:18080').replace(/\/+$/, '');
const runId = process.env.RUN_ID || `prod-lt-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
const reportsDir = path.join(repoRoot, 'loadtest', '.runs', 'reports');
const profile = process.env.LOADTEST_PROFILE || 'standard';

assertLoadtestTarget(baseUrl, config);
if (process.env.LOADTEST_CONFIRM !== 'YES') {
  throw new Error('Set LOADTEST_CONFIRM=YES before running the production load-test suite.');
}

await mkdir(reportsDir, { recursive: true });
await ensureReady();

const startedAt = new Date().toISOString();
const stages = [];
const bidRuns = [];
const artifacts = [];

const bidStageVus = intList(process.env.BID_STAGE_VUS, profile === 'quick' ? [5, 10] : [50, 100, 200, 500]);
const bidStageDuration = process.env.BID_STAGE_DURATION || (profile === 'quick' ? '10s' : '30s');
const soakVus = intValue(process.env.SOAK_VUS, profile === 'quick' ? 10 : 200);
const soakDuration = process.env.SOAK_DURATION || (profile === 'quick' ? '30s' : '10m');
const paymentVus = intValue(process.env.PAYMENT_VUS, profile === 'quick' ? 10 : 100);
const paymentDuration = process.env.PAYMENT_DURATION || (profile === 'quick' ? '10s' : '1m');
const websocketVus = intValue(process.env.WEBSOCKET_VUS, profile === 'quick' ? 5 : 100);
const websocketDuration = process.env.WEBSOCKET_DURATION || (profile === 'quick' ? '10s' : '1m');
const consistencyWaitSeconds = intValue(process.env.CONSISTENCY_WAIT_SECONDS, profile === 'quick' ? 10 : 60);
const runWebsocket = envBool('RUN_WEBSOCKET', true);

console.log(`Production load test runId=${runId} profile=${profile}`);

for (const vus of bidStageVus) {
  const data = await prepareData({
    mode: 'bid',
    buyers: Math.max(vus * 2, profile === 'quick' ? 20 : 1000),
    runId: `${runId}-bid-${vus}`,
    durationSeconds: intValue(process.env.AUCTION_DURATION_SECONDS, profile === 'quick' ? 600 : 3600),
    capPriceCents: intValue(process.env.CAP_PRICE_CENTS, 999999999)
  });
  artifacts.push(data.outputFile);
  const bidRun = { name: `bid-${vus}`, data };
  bidRuns.push(bidRun);
  const stage = await runK6({
    name: `bid-${vus}`,
    script: 'auction-bid-hot-path.js',
    dataFile: data.outputFile,
    vus,
    duration: bidStageDuration,
    env: { VUS: String(vus), DURATION: bidStageDuration },
    maxFailedRate: 0.02
  });
  bidRun.stage = stage;
  stages.push(stage);
}

const soakData = await prepareData({
  mode: 'bid',
  buyers: Math.max(soakVus * 2, profile === 'quick' ? 20 : 1000),
  runId: `${runId}-bid-soak`,
  durationSeconds: intValue(process.env.AUCTION_DURATION_SECONDS, profile === 'quick' ? 600 : 3600),
  capPriceCents: intValue(process.env.CAP_PRICE_CENTS, 999999999)
});
artifacts.push(soakData.outputFile);
const soakRun = { name: `bid-soak-${soakVus}`, data: soakData };
bidRuns.push(soakRun);
const soakStage = await runK6({
  name: `bid-soak-${soakVus}`,
  script: 'auction-bid-hot-path.js',
  dataFile: soakData.outputFile,
  vus: soakVus,
  duration: soakDuration,
  env: { VUS: String(soakVus), DURATION: soakDuration },
  maxFailedRate: 0.02
});
soakRun.stage = soakStage;
stages.push(soakStage);

const paymentData = await prepareData({
  mode: 'payment',
  buyers: Math.max(paymentVus, profile === 'quick' ? 20 : 100),
  runId: `${runId}-payment`,
  durationSeconds: intValue(process.env.AUCTION_DURATION_SECONDS, profile === 'quick' ? 600 : 3600),
  capPriceCents: intValue(process.env.PAYMENT_CAP_PRICE_CENTS, 120000)
});
artifacts.push(paymentData.outputFile);
stages.push(await runK6({
  name: `payment-idempotency-${paymentVus}`,
  script: 'payment-idempotency.js',
  dataFile: paymentData.outputFile,
  vus: paymentVus,
  duration: paymentDuration,
  env: { VUS: String(paymentVus), DURATION: paymentDuration },
  maxFailedRate: 0.01
}));

if (runWebsocket) {
  stages.push(await runK6({
    name: `websocket-${websocketVus}`,
    script: 'websocket-realtime.js',
    dataFile: soakData.outputFile,
    vus: websocketVus,
    duration: websocketDuration,
    env: { WS_VUS: String(websocketVus), DURATION: websocketDuration, WS_HOLD_MS: '5000' },
    maxFailedRate: 0.02
  }));
}

console.log(`Waiting up to ${consistencyWaitSeconds}s for eventual consistency checks...`);
const verification = await waitForVerification({ bidRuns, paymentData, waitSeconds: consistencyWaitSeconds });
const completedAt = new Date().toISOString();
const conclusion = makeConclusion({ stages, verification });
const report = {
  runId,
  profile,
  baseUrl,
  startedAt,
  completedAt,
  stages,
  verification,
  conclusion,
  artifacts
};
const reportPath = path.join(reportsDir, `${runId}.md`);
const jsonPath = path.join(reportsDir, `${runId}.json`);
await writeFile(reportPath, formatMarkdownReport(report), 'utf8');
await writeFile(jsonPath, `${JSON.stringify(report, null, 2)}\n`, 'utf8');
console.log(`Report: ${reportPath}`);
console.log(`Report JSON: ${jsonPath}`);
console.log(`Conclusion: ${conclusion}`);

async function prepareData({ mode, buyers, runId: dataRunId, durationSeconds, capPriceCents }) {
  const outputFile = path.join(repoRoot, 'loadtest', '.runs', `${dataRunId}.json`);
  console.log(`Preparing data mode=${mode} buyers=${buyers} runId=${dataRunId}`);
  runCommand(process.execPath, [path.join(repoRoot, 'loadtest', 'scripts', 'prepare-auction-data.mjs')], {
    env: {
      ...process.env,
      LOADTEST_CONFIG: configPath,
      LOADTEST_CONFIRM: 'YES',
      LOADTEST_MODE: mode,
      RUN_ID: dataRunId,
      BUYERS: String(buyers),
      DURATION_SECONDS: String(durationSeconds),
      CAP_PRICE_CENTS: String(capPriceCents),
      PAYMENT_SOURCE: mode === 'payment' ? 'shop' : (process.env.PAYMENT_SOURCE || ''),
      OUTPUT_FILE: outputFile,
      BASE_URL: baseUrl
    },
    timeoutMs: Math.max(180000, buyers * 5000)
  });
  const data = JSON.parse(await readFile(outputFile, 'utf8'));
  return { ...data, outputFile };
}

async function runK6({ name, script, dataFile, vus, duration, env, maxFailedRate }) {
  const summaryPath = path.join(reportsDir, `${runId}-${name}.json`);
  const args = [
    'run',
    '--rm',
    '--network',
    process.env.LOADTEST_DOCKER_NETWORK || 'live-auction-loadtest_live-auction-loadtest',
    '-v',
    `${repoRoot}:/work`,
    '-w',
    '/work',
    '-e',
    `DATA_FILE=${toDockerPath(dataFile)}`,
    '-e',
    `BASE_URL=${dockerBaseUrl}`
  ];
  for (const [key, value] of Object.entries(env || {})) {
    args.push('-e', `${key}=${value}`);
  }
  args.push(
    process.env.K6_IMAGE || 'grafana/k6',
    'run',
    '--quiet',
    '--summary-trend-stats',
    'avg,min,med,p(90),p(95),p(99),max',
    '--summary-export',
    toDockerPath(summaryPath),
    `/work/loadtest/k6/${script}`
  );

  console.log(`Running ${name}: script=${script} vus=${vus} duration=${duration}`);
  const result = runCommand('docker', args, { timeoutMs: durationToMs(duration) + 180000, allowFailure: true });
  if (result.status !== 0 && !existsSync(summaryPath)) {
    throw new Error(`k6 stage ${name} failed before writing summary: ${result.stderr || result.stdout}`);
  }
  const summary = summarizeK6Metrics(path.basename(summaryPath), JSON.parse(await readFile(summaryPath, 'utf8')));
  return { name, script, vus, duration, summary, summaryPath, maxFailedRate, exitStatus: result.status };
}

async function waitForVerification({ bidRuns, paymentData, waitSeconds }) {
  const deadline = Date.now() + waitSeconds * 1000;
  let last;
  do {
    last = await verifyOnce({ bidRuns, paymentData });
    if (last.idempotency.ok && last.eventualConsistency.ok && last.resources.ok) return last;
    await sleep(Math.min(5000, Math.max(0, deadline - Date.now())));
  } while (Date.now() < deadline);
  return last;
}

async function verifyOnce({ bidRuns, paymentData }) {
  const mysqlRows = runMysqlJson(buildVerificationSql(bidRuns, paymentData));
  const byKind = groupByKind(mysqlRows);
  const rabbit = await rabbitQueueSnapshot();
  const metrics = await prometheusSnapshot();
  const dockerStats = runDockerStats();
  const idempotency = verifyPayment(byKind.payment?.[0]);
  const eventualConsistency = verifyBidConsistency(bidRuns, byKind.bidConsistency || [], byKind.outbox?.[0], rabbit);
  const resources = verifyResources(byKind.mysqlStatus?.[0], metrics, rabbit, dockerStats);
  return { idempotency, eventualConsistency, resources };
}

function buildVerificationSql(bidRuns, paymentData) {
  const lotIds = bidRuns.map((run) => run.data.lot?.id).filter(Boolean);
  const quotedLotIds = lotIds.map(sqlString).join(',');
  const orderId = paymentData.order?.id || '';
  const statements = [
    `SELECT JSON_OBJECT('kind','outbox','pending',COUNT(*)) FROM auction_events WHERE streamed_at_unix_ms = 0`,
    `SELECT JSON_OBJECT(
      'kind','mysqlStatus',
      'threadsConnected', CAST((SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Threads_connected') AS UNSIGNED),
      'threadsRunning', CAST((SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Threads_running') AS UNSIGNED),
      'slowQueries', CAST((SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Slow_queries') AS UNSIGNED),
      'innodbRowLockWaits', CAST((SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Innodb_row_lock_waits') AS UNSIGNED),
      'innodbRowLockTime', CAST((SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Innodb_row_lock_time') AS UNSIGNED)
    )`
  ];
  if (lotIds.length) {
    statements.push(`SELECT JSON_OBJECT(
      'kind','bidConsistency',
      'lotId', l.id,
      'status', l.status,
      'version', l.version,
      'currentPriceAmount', l.current_price_amount,
      'bidRows', COALESCE(b.bid_rows, 0),
      'distinctIdempotencyKeys', COALESCE(b.distinct_idem, 0),
      'duplicateIdempotencyKeys', COALESCE(b.duplicate_idem, 0),
      'maxBidAmount', COALESCE(b.max_amount, 0),
      'priceMatchesMaxBid', IF(COALESCE(b.bid_rows, 0) = 0, TRUE, l.current_price_amount = COALESCE(b.max_amount, 0)),
      'lastProjectedVersion', COALESCE(o.last_projected_version, 0)
    )
    FROM auction_lots l
    LEFT JOIN (
      SELECT lot_id, COUNT(*) AS bid_rows, COUNT(DISTINCT idempotency_key) AS distinct_idem,
             COUNT(*) - COUNT(DISTINCT idempotency_key) AS duplicate_idem, MAX(amount) AS max_amount
      FROM auction_bids
      WHERE lot_id IN (${quotedLotIds})
      GROUP BY lot_id
    ) b ON b.lot_id = l.id
    LEFT JOIN auction_runtime_projection_offsets o ON o.lot_id = l.id
    WHERE l.id IN (${quotedLotIds})`);
  }
  if (orderId) {
    statements.push(`SELECT JSON_OBJECT(
      'kind','payment',
      'orderId', o.id,
      'status', o.status,
      'paymentStatus', o.payment_status,
      'paymentRows', COALESCE(p.payment_rows, 0),
      'distinctIdempotencyKeys', COALESCE(p.distinct_idem, 0),
      'distinctPaymentIds', COALESCE(p.distinct_payment_ids, 0),
      'duplicateIdempotencyKeys', COALESCE(p.duplicate_idem, 0)
    )
    FROM user_orders o
    LEFT JOIN (
      SELECT order_id, COUNT(*) AS payment_rows, COUNT(DISTINCT idempotency_key) AS distinct_idem,
             COUNT(DISTINCT id) AS distinct_payment_ids,
             COUNT(*) - COUNT(DISTINCT idempotency_key) AS duplicate_idem
      FROM user_order_payments
      WHERE order_id = ${sqlString(orderId)}
      GROUP BY order_id
    ) p ON p.order_id = o.id
    WHERE o.id = ${sqlString(orderId)}`);
  }
  return `${statements.join('; ')};`;
}

function verifyPayment(row) {
  if (!row) return { ok: false, notes: ['payment verification row is missing'] };
  const ok = row.paymentRows === 1 &&
    row.distinctIdempotencyKeys === 1 &&
    row.distinctPaymentIds === 1 &&
    row.duplicateIdempotencyKeys === 0 &&
    String(row.paymentStatus).toLowerCase() === 'success';
  return {
    ok,
    notes: [
      `order ${row.orderId}: status=${row.status}, payment_status=${row.paymentStatus}`,
      `payment rows=${row.paymentRows}, distinct idempotency keys=${row.distinctIdempotencyKeys}, duplicate idempotency keys=${row.duplicateIdempotencyKeys}`
    ]
  };
}

function verifyBidConsistency(bidRuns, rows, outbox, rabbit) {
  const expectedByLot = new Map();
  for (const run of bidRuns) {
    const accepted = run.stage?.summary?.raw?.metrics?.bid_accepted_total?.count || 0;
    expectedByLot.set(run.data.lot.id, { name: run.name, accepted });
  }
  const notes = [];
  let ok = Number(outbox?.pending || 0) === 0 && Number(rabbit.messages || 0) === 0;
  notes.push(`outbox pending=${Number(outbox?.pending || 0)}`);
  notes.push(`RabbitMQ queue messages=${Number(rabbit.messages || 0)}, ready=${Number(rabbit.messages_ready || 0)}, unacked=${Number(rabbit.messages_unacknowledged || 0)}`);
  for (const row of rows) {
    const expected = expectedByLot.get(row.lotId);
    const rowOk = row.duplicateIdempotencyKeys === 0 && Boolean(row.priceMatchesMaxBid);
    if (!rowOk) ok = false;
    notes.push(`lot ${row.lotId}: bids=${row.bidRows}, duplicate idempotency keys=${row.duplicateIdempotencyKeys}, current=${row.currentPriceAmount}, maxBid=${row.maxBidAmount}, priceMatches=${Boolean(row.priceMatchesMaxBid)}, projectedVersion=${row.lastProjectedVersion}`);
    if (expected?.accepted && Number(row.bidRows) < Number(expected.accepted)) {
      ok = false;
      notes.push(`lot ${row.lotId}: accepted by k6=${expected.accepted}, durable MySQL bids=${row.bidRows}`);
    }
  }
  return { ok, notes };
}

function verifyResources(mysqlStatus, metrics, rabbit, dockerStats) {
  const redisTimeouts = latestSample(metrics.samples, 'auction_redis_pool_timeouts_total');
  const outboxPendingMetric = latestSample(metrics.samples, 'auction_outbox_pending_count');
  const goroutines = latestSample(metrics.samples, 'go_goroutines');
  const dbWaitCount = latestSample(metrics.samples, 'auction_db_pool_wait_count_total');
  const ok = redisTimeouts === 0 && Number(rabbit.messages || 0) === 0;
  return {
    ok,
    notes: [
      `MySQL slow queries=${mysqlStatus?.slowQueries ?? 'unknown'}, row lock waits=${mysqlStatus?.innodbRowLockWaits ?? 'unknown'}, row lock time=${mysqlStatus?.innodbRowLockTime ?? 'unknown'}ms`,
      `DB pool wait count=${dbWaitCount}, Redis timeouts=${redisTimeouts}, outbox pending metric=${outboxPendingMetric}, goroutines=${goroutines}`,
      dockerStats ? `Docker stats: ${dockerStats.replace(/\r?\n/g, ' | ')}` : 'Docker stats unavailable'
    ]
  };
}

function runMysqlJson(sql) {
  const mysql = config.mysql || {};
  const nativeMysql = 'D:\\Program Files\\MySQL\\MySQL Server 8.0\\bin\\mysql.exe';
  const mysqlExe = process.env.MYSQL_EXE || (existsSync(nativeMysql) ? nativeMysql : 'mysql');
  const result = spawnSync(mysqlExe, [
    '--protocol=tcp',
    `--host=${mysql.host || '127.0.0.1'}`,
    `--port=${mysql.port || 3306}`,
    `--user=${mysql.user || 'auction'}`,
    '--batch',
    '--raw',
    '--skip-column-names',
    mysql.database,
    '--execute',
    sql
  ], {
    env: { ...process.env, MYSQL_PWD: mysql.password || '' },
    encoding: 'utf8',
    timeout: 30000
  });
  if (result.status !== 0 && !options.allowFailure) {
    throw new Error(`mysql verification failed: ${result.stderr || result.stdout}`);
  }
  return result.stdout.split(/\r?\n/).filter(Boolean).map((line) => JSON.parse(line));
}

async function rabbitQueueSnapshot() {
  const url = new URL(process.env.RABBITMQ_HTTP_URL || 'http://127.0.0.1:25673');
  const amqpUrl = new URL(config.rabbitmq?.url || 'amqp://auction_loadtest:auction_mq_loadtest@127.0.0.1:25672/');
  const queue = encodeURIComponent(config.rabbitmq?.queue || 'auction.loadtest.realtime.websocket');
  const response = await fetch(`${url.origin}/api/queues/%2F/${queue}`, {
    headers: {
      Authorization: `Basic ${Buffer.from(`${decodeURIComponent(amqpUrl.username)}:${decodeURIComponent(amqpUrl.password)}`).toString('base64')}`
    }
  });
  if (!response.ok) {
    return { messages: -1, messages_ready: -1, messages_unacknowledged: -1, error: await response.text() };
  }
  return response.json();
}

async function prometheusSnapshot() {
  const response = await fetch(`${baseUrl}/metrics`);
  const text = await response.text();
  return {
    text,
    samples: parsePrometheusSamples(text, [
      'auction_db_pool_wait_count_total',
      'auction_outbox_pending_count',
      'auction_redis_pool_timeouts_total',
      'go_goroutines',
      'process_resident_memory_bytes',
      'process_cpu_seconds_total'
    ])
  };
}

function runDockerStats() {
  const result = spawnSync('docker', [
    'stats',
    '--no-stream',
    '--format',
    'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.NetIO}}\t{{.BlockIO}}\t{{.PIDs}}',
    'live-auction-loadtest-backend',
    'live-auction-loadtest-mysql',
    'live-auction-loadtest-redis',
    'live-auction-loadtest-rabbitmq'
  ], { encoding: 'utf8', timeout: 30000 });
  if (result.status !== 0) return '';
  return result.stdout.trim();
}

async function ensureReady() {
  const response = await fetch(`${baseUrl}/readyz`);
  if (!response.ok) throw new Error(`loadtest backend is not ready: HTTP ${response.status}`);
}

function runCommand(command, args, options = {}) {
  const result = spawnSync(command, args, {
    env: options.env || process.env,
    cwd: repoRoot,
    encoding: 'utf8',
    timeout: options.timeoutMs || 600000,
    maxBuffer: 1024 * 1024 * 20
  });
  if (result.status !== 0) {
    throw new Error([
      `Command failed: ${command} ${args.join(' ')}`,
      result.stdout,
      result.stderr
    ].filter(Boolean).join('\n'));
  }
  if (result.stdout.trim()) console.log(result.stdout.trim());
  if (result.stderr.trim()) console.error(result.stderr.trim());
  return result;
}

function groupByKind(rows) {
  const grouped = {};
  for (const row of rows) {
    const kind = row.kind || 'unknown';
    if (!grouped[kind]) grouped[kind] = [];
    grouped[kind].push(row);
  }
  return grouped;
}

function toDockerPath(localPath) {
  const relative = path.relative(repoRoot, localPath).replaceAll(path.sep, '/');
  return `/work/${relative}`;
}

function durationToMs(duration) {
  const match = String(duration || '').trim().match(/^(\d+)(ms|s|m|h)$/);
  if (!match) return 600000;
  const value = Number(match[1]);
  const unit = match[2];
  if (unit === 'ms') return value;
  if (unit === 's') return value * 1000;
  if (unit === 'm') return value * 60 * 1000;
  return value * 60 * 60 * 1000;
}

function intList(raw, fallback) {
  if (!raw) return fallback;
  const values = String(raw).split(',').map((item) => Number.parseInt(item.trim(), 10)).filter((value) => Number.isFinite(value) && value > 0);
  return values.length ? values : fallback;
}

function intValue(raw, fallback) {
  const value = Number.parseInt(String(raw ?? ''), 10);
  return Number.isFinite(value) && value > 0 ? value : fallback;
}

function envBool(name, fallback) {
  const raw = process.env[name];
  if (raw === undefined) return fallback;
  return /^(1|true|yes|on)$/i.test(raw);
}

function sqlString(value) {
  return `'${String(value).replaceAll("'", "''")}'`;
}
