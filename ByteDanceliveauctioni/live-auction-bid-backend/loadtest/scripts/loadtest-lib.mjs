export function assertLoadtestTarget(baseUrl, config = {}) {
  const url = String(baseUrl || '');
  if (/https?:\/\/(127\.0\.0\.1|localhost):18080(?:\/|$)/i.test(url)) {
    throw new Error(`Refusing to run loadtest against normal backend: ${url}`);
  }
  if (/\/prod(?:\/|$)/i.test(url)) {
    throw new Error(`Refusing to run loadtest against production-looking target: ${url}`);
  }
  const database = String(config.mysql?.database || '');
  if (database && !database.toLowerCase().includes('loadtest')) {
    throw new Error(`Refusing to run loadtest against non-loadtest database: ${database}`);
  }
  const exchange = String(config.rabbitmq?.exchange || '');
  if (exchange && !exchange.toLowerCase().includes('loadtest')) {
    throw new Error(`Refusing to run loadtest against non-loadtest RabbitMQ exchange: ${exchange}`);
  }
  const queue = String(config.rabbitmq?.queue || '');
  if (queue && !queue.toLowerCase().includes('loadtest')) {
    throw new Error(`Refusing to run loadtest against non-loadtest RabbitMQ queue: ${queue}`);
  }
}

export function summarizeK6Metrics(file, summary) {
  const metrics = summary?.metrics || {};
  return {
    file,
    httpReqs: numberValue(metrics.http_reqs?.count),
    qps: numberValue(metrics.http_reqs?.rate),
    failedRate: numberValue(metrics.http_req_failed?.value ?? metrics.http_req_failed?.rate),
    p95Ms: trendValue(metrics.http_req_duration, 'p(95)'),
    p99Ms: trendValue(metrics.http_req_duration, 'p(99)'),
    maxMs: trendValue(metrics.http_req_duration, 'max'),
    iterations: numberValue(metrics.iterations?.count),
    raw: summary
  };
}

export function formatMarkdownReport(report) {
  const lines = [];
  lines.push('# Production Load Test Report');
  lines.push('');
  lines.push(`- Run ID: \`${report.runId}\``);
  lines.push(`- Target: \`${report.baseUrl}\``);
  lines.push(`- Started: \`${report.startedAt}\``);
  lines.push(`- Completed: \`${report.completedAt}\``);
  lines.push(`- Conclusion: **${report.conclusion || 'UNKNOWN'}**`);
  lines.push('');

  lines.push('## Performance Summary');
  lines.push('');
  lines.push('| Stage | Script | VUs | Duration | Requests | QPS | Failed | p95 | p99 | Max |');
  lines.push('|---|---|---:|---:|---:|---:|---:|---:|---:|---:|');
  for (const stage of report.stages || []) {
    const summary = stage.summary || {};
    lines.push([
      `| ${stage.name}`,
      stage.script || '',
      stage.vus ?? '',
      stage.duration || '',
      formatInteger(summary.httpReqs),
      formatDecimal(summary.qps),
      formatPercent(summary.failedRate),
      formatMs(summary.p95Ms),
      formatMs(summary.p99Ms),
      `${formatMs(summary.maxMs)} |`
    ].join(' | '));
  }
  lines.push('');

  lines.push('## 支付幂等');
  lines.push('');
  appendVerification(lines, report.verification?.idempotency);

  lines.push('## 最终一致');
  lines.push('');
  appendVerification(lines, report.verification?.eventualConsistency);

  lines.push('## 资源与瓶颈');
  lines.push('');
  appendVerification(lines, report.verification?.resources);

  if (report.artifacts?.length) {
    lines.push('## Artifacts');
    lines.push('');
    for (const artifact of report.artifacts) {
      lines.push(`- ${artifact}`);
    }
    lines.push('');
  }

  return `${lines.join('\n').trimEnd()}\n`;
}

export function makeConclusion({ stages = [], verification = {} }) {
  if (stages.some((stage) => Number(stage.exitStatus || 0) !== 0)) return 'FAIL';
  const failedStage = stages.find((stage) => {
    const summary = stage.summary || {};
    return numberValue(summary.failedRate) > numberValue(stage.maxFailedRate ?? 0.03);
  });
  if (failedStage) return 'FAIL';
  if (verification.idempotency && !verification.idempotency.ok) return 'FAIL';
  if (verification.eventualConsistency && !verification.eventualConsistency.ok) return 'RISK';
  if (verification.resources && !verification.resources.ok) return 'RISK';
  return 'PASS';
}

export function parsePrometheusSamples(text, metricNames) {
  const names = new Set(metricNames);
  const samples = {};
  for (const line of String(text || '').split(/\r?\n/)) {
    if (!line || line.startsWith('#')) continue;
    const match = line.match(/^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{[^}]*\})?\s+(.+)$/);
    if (!match) continue;
    const [, name, rawValue] = match;
    if (names.size && !names.has(name)) continue;
    if (!samples[name]) samples[name] = [];
    samples[name].push(Number(rawValue));
  }
  return samples;
}

export function latestSample(samples, name) {
  const values = samples?.[name] || [];
  return values.length ? values[values.length - 1] : 0;
}

export function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function appendVerification(lines, result) {
  if (!result) {
    lines.push('- 未执行');
    lines.push('');
    return;
  }
  lines.push(`- Result: **${result.ok ? 'OK' : 'NOT OK'}**`);
  for (const note of result.notes || []) {
    lines.push(`- ${note}`);
  }
  lines.push('');
}

function trendValue(metric, key) {
  return numberValue(metric?.percentiles?.[key] ?? metric?.[key]);
}

function numberValue(raw) {
  const value = Number(raw ?? 0);
  return Number.isFinite(value) ? value : 0;
}

function formatInteger(raw) {
  return Math.round(numberValue(raw)).toLocaleString('en-US');
}

function formatDecimal(raw) {
  return numberValue(raw).toFixed(2);
}

function formatPercent(raw) {
  return `${(numberValue(raw) * 100).toFixed(2)}%`;
}

function formatMs(raw) {
  return `${numberValue(raw).toFixed(2)}ms`;
}
