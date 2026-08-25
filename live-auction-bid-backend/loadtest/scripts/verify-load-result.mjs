#!/usr/bin/env node

import { readFile } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const configPath = process.env.LOADTEST_CONFIG || path.join(repoRoot, 'loadtest', 'config.local.json');
const config = JSON.parse(await readFile(configPath, 'utf8'));
const dataFile = process.env.DATA_FILE;
if (!dataFile) throw new Error('DATA_FILE is required.');
const data = JSON.parse(await readFile(dataFile, 'utf8'));
const baseUrl = (process.env.BASE_URL || data.baseUrl || config.baseUrl || 'http://127.0.0.1:18081').replace(/\/+$/, '');

assertLoadtestTarget(baseUrl, config);
await request('/readyz');

const report = {
  runId: data.runId,
  baseUrl,
  ready: true,
  order: null,
  mysql: null
};

if (data.order?.id && data.winner?.token) {
  const orderReply = await request(`/api/orders/${encodeURIComponent(data.order.id)}`, { token: data.winner.token });
  report.order = {
    id: data.order.id,
    paymentStatus: orderReply.order?.paymentStatus || orderReply.order?.payment_status || '',
    status: orderReply.order?.status || ''
  };
}

report.mysql = runMysqlChecks(config.mysql, data);
console.log(JSON.stringify(report, null, 2));

function runMysqlChecks(mysql, loadData) {
  if (!mysql?.database) return { skipped: true, reason: 'mysql config is missing' };
  if (!mysql.database.includes('loadtest')) {
    throw new Error(`Refusing to verify non-loadtest database: ${mysql.database}`);
  }
  const nativeMysql = 'D:\\Program Files\\MySQL\\MySQL Server 8.0\\bin\\mysql.exe';
  const mysqlExe = process.env.MYSQL_EXE || (existsSync(nativeMysql) ? nativeMysql : 'mysql');
  const orderId = loadData.order?.id || '';
  const sql = [
    "SELECT COUNT(*) AS pending_outbox FROM auction_events WHERE streamed_at_unix_ms = 0;",
    "SELECT reason, COUNT(*) AS cnt FROM auction_events GROUP BY reason ORDER BY cnt DESC LIMIT 10;",
    orderId ? `SELECT COUNT(*) AS payment_rows, COUNT(DISTINCT idempotency_key) AS idempotency_keys, COUNT(DISTINCT id) AS payment_ids FROM user_order_payments WHERE order_id='${escapeSql(orderId)}';` : "SELECT 0 AS payment_rows, 0 AS idempotency_keys, 0 AS payment_ids;"
  ].join(' ');
  const env = { ...process.env, MYSQL_PWD: mysql.password || '' };
  const args = [
    '--protocol=tcp',
    `--host=${mysql.host || '127.0.0.1'}`,
    `--port=${mysql.port || 3306}`,
    `--user=${mysql.user || 'auction'}`,
    mysql.database,
    '-e',
    sql
  ];
  const result = spawnSync(mysqlExe, args, { env, encoding: 'utf8' });
  if (result.status !== 0) {
    return { skipped: true, reason: 'mysql cli failed', stderr: result.stderr.trim() };
  }
  return { skipped: false, output: result.stdout.trim() };
}

async function request(urlPath, options = {}) {
  const headers = { Accept: 'application/json' };
  if (options.token) headers.Authorization = `Bearer ${options.token}`;
  const response = await fetch(`${baseUrl}${urlPath}`, { headers });
  const text = await response.text();
  let data = {};
  if (text) data = JSON.parse(text);
  if (!response.ok) throw new Error(`GET ${urlPath} HTTP ${response.status}: ${text.slice(0, 240)}`);
  return data;
}

function assertLoadtestTarget(url, cfg) {
  if (url.includes(':18080')) throw new Error(`Refusing to verify non-loadtest target: ${url}`);
  if (!String(cfg.mysql?.database || '').includes('loadtest')) {
    throw new Error(`Refusing to verify non-loadtest database: ${cfg.mysql?.database || ''}`);
  }
}

function escapeSql(value) {
  return String(value).replaceAll("'", "''");
}
