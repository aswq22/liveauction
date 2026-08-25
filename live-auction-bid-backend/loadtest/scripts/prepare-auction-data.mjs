#!/usr/bin/env node

import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const configPath = process.env.LOADTEST_CONFIG || path.join(repoRoot, 'loadtest', 'config.local.json');
const config = JSON.parse(await readFile(configPath, 'utf8'));
const baseUrl = (process.env.BASE_URL || config.baseUrl || 'http://127.0.0.1:18081').replace(/\/+$/, '');
const mode = process.env.LOADTEST_MODE || 'mixed';
const runId = process.env.RUN_ID || `lt-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
const buyersCount = positiveInt(process.env.BUYERS, config.profiles?.baseline?.buyers || 100);
const startPrice = positiveInt(process.env.START_PRICE_CENTS, 10000);
const minIncrement = positiveInt(process.env.MIN_INCREMENT_CENTS, 100);
const capPrice = positiveInt(process.env.CAP_PRICE_CENTS, startPrice + (buyersCount + 1000) * minIncrement);
const durationSeconds = positiveInt(process.env.DURATION_SECONDS, 900);
const outputFile = process.env.OUTPUT_FILE || path.join(repoRoot, 'loadtest', '.runs', `${runId}.json`);

assertLoadtestTarget(baseUrl);

await request('/readyz');

const merchant = await ensureUser('/api/merchants/register', `merchant_${runId}`, 'LoadTestPass123!', `loadtest merchant ${runId}`);
const merchantToken = tokenOf(merchant);
const room = normalizeRoom((await request('/api/admin/rooms', { token: merchantToken })).rooms?.[0]);
if (!room?.id) throw new Error('admin room list is empty after merchant registration');

const buyers = [];
for (let index = 0; index < buyersCount; index += 1) {
  const username = `buyer_${runId}_${index}`;
  const reply = await ensureUser('/api/users/register', username, 'BuyerPass123!', `loadtest buyer ${index}`);
  buyers.push({ index, user: normalizeUser(reply.user), token: tokenOf(reply), username });
}

const lot = await createStartedLot(merchantToken, room.id);
let order = null;
let winner = null;
if (mode === 'payment' || mode === 'mixed') {
  winner = buyers[buyers.length - 1];
  if (process.env.PAYMENT_SOURCE === 'shop') {
    order = await createShopOrder(winner.token);
  } else {
  const bidKey = `prepare-cap-bid-${runId}`;
  const bidReply = await request(`/api/lots/${encodeURIComponent(lot.id)}/bid`, {
    method: 'POST',
    token: winner.token,
    idempotencyKey: bidKey,
    body: {
      amount: money(capPrice),
      idempotency_key: bidKey
    }
  });
  if (!bidReply.accepted && !bidReply.bid?.id) {
    throw new Error(`prepare cap bid was not accepted: ${JSON.stringify(bidReply.result || bidReply).slice(0, 240)}`);
  }
  let result = await waitForOrderResult(lot.id, winner.token, 60000);
  order = normalizeOrder(result.order || result.unifiedOrder || result.unified_order);
  if (!order?.id) {
    await request(`/api/lots/${encodeURIComponent(lot.id)}/settle`, {
      method: 'POST',
      token: merchantToken,
      body: { operator_id: normalizeUser(merchant.user)?.id || 'loadtest-operator' }
    });
    result = await waitForOrderResult(lot.id, winner.token, 60000);
    order = normalizeOrder(result.order || result.unifiedOrder || result.unified_order);
  }
  if (!order?.id) throw new Error(`cap settlement did not return a winner order: ${JSON.stringify(result).slice(0, 240)}`);
  }
}

async function waitForOrderResult(lotId, token, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  let last = {};
  do {
    last = await request(`/api/lots/${encodeURIComponent(lotId)}/result`, { token });
    const order = normalizeOrder(last.order || last.unifiedOrder || last.unified_order);
    if (order?.id) return last;
    await sleep(1000);
  } while (Date.now() < deadline);
  return last;
}

const data = {
  runId,
  mode,
  baseUrl,
  createdAtUnixMs: Date.now(),
  room,
  lot,
  merchant: { user: normalizeUser(merchant.user), token: merchantToken },
  buyers,
  winner,
  order,
  money: {
    startPrice,
    minIncrement,
    capPrice
  },
  safety: {
    database: config.mysql?.database || '',
    rabbitmqExchange: config.rabbitmq?.exchange || '',
    rabbitmqQueue: config.rabbitmq?.queue || ''
  }
};

await mkdir(path.dirname(outputFile), { recursive: true });
await writeFile(outputFile, `${JSON.stringify(data, null, 2)}\n`, 'utf8');
console.log(JSON.stringify({ runId, mode, baseUrl, buyers: buyers.length, roomId: room.id, lotId: lot.id, orderId: order?.id || '', outputFile }, null, 2));

async function createStartedLot(token, roomId) {
  const create = await request('/api/lots', {
    method: 'POST',
    token,
    body: {
      room_id: roomId,
      title: `Loadtest lot ${runId}`,
      description: 'isolated loadtest auction lot',
      image_url: 'https://liveauction.tos-cn-beijing.volces.com/douyin-h5/images/live-anchor-01.jpg',
      category: 'loadtest',
      tags: ['loadtest', runId],
      rule: {
        start_price: money(startPrice),
        min_increment: money(minIncrement),
        duration_seconds: durationSeconds,
        anti_snipe_window_seconds: 15,
        anti_snipe_extend_seconds: 15,
        max_extend_count: 20,
        cap_price: money(capPrice)
      }
    }
  });
  const lot = normalizeLot(create.lot);
  await request(`/api/lots/${encodeURIComponent(lot.id)}/queue`, { method: 'POST', token, body: {} });
  const started = await request(`/api/lots/${encodeURIComponent(lot.id)}/start`, { method: 'POST', token, body: {} });
  return normalizeLot(started.lot || lot);
}

async function createShopOrder(token) {
  const productsReply = await request('/api/shop/products?page_size=20', { token });
  const product = (productsReply.products || []).find((item) => Array.isArray(item.skus) && item.skus.length > 0);
  const sku = product?.skus?.[0];
  if (!sku?.id) throw new Error('no shop SKU is available for payment loadtest data');
  const addressReply = await request('/api/shop/addresses', {
    method: 'POST',
    token,
    body: {
      address: {
        receiver_name: 'Load Test Buyer',
        phone: '13800000000',
        province: 'Shanghai',
        city: 'Shanghai',
        district: 'Pudong',
        street: 'Loadtest Road',
        detail: `Room ${runId}`,
        postal_code: '200000',
        tag: 'loadtest',
        is_default: true
      }
    }
  });
  const address = addressReply.address;
  if (!address?.id) throw new Error(`failed to create shop address: ${JSON.stringify(addressReply).slice(0, 240)}`);
  const orderReply = await request('/api/shop/orders', {
    method: 'POST',
    token,
    body: {
      sku_id: sku.id,
      quantity: 1,
      address_id: address.id,
      idempotency_key: `prepare-shop-order-${runId}`
    }
  });
  const order = normalizeOrder(orderReply.order);
  if (!order?.id) throw new Error(`failed to create shop order: ${JSON.stringify(orderReply).slice(0, 240)}`);
  return order;
}

async function ensureUser(registerPath, username, password, nickname) {
  try {
    return await request(registerPath, { method: 'POST', body: { username, password, nickname } });
  } catch (error) {
    if (String(error.message || error).includes('network failed')) throw error;
    return request('/api/users/login', { method: 'POST', body: { username, password } });
  }
}

async function request(urlPath, options = {}) {
  const headers = { Accept: 'application/json' };
  if (options.body !== undefined) headers['Content-Type'] = 'application/json';
  if (options.token) headers.Authorization = `Bearer ${options.token}`;
  if (options.idempotencyKey) headers['Idempotency-Key'] = options.idempotencyKey;
  const method = options.method || 'GET';
  let response;
  try {
    response = await fetch(`${baseUrl}${urlPath}`, {
      method,
      headers,
      body: options.body !== undefined ? JSON.stringify(options.body) : undefined
    });
  } catch (error) {
    throw new Error(`${method} ${baseUrl}${urlPath} network failed: ${error instanceof Error ? error.message : String(error)}`);
  }
  const text = await response.text();
  let data = {};
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      throw new Error(`${method} ${urlPath} returned non-json ${response.status}: ${text.slice(0, 160)}`);
    }
  }
  if (!response.ok) throw new Error(`${method} ${urlPath} HTTP ${response.status}: ${text.slice(0, 240)}`);
  const result = data.result;
  if (result && Number(result.code || 0) !== 0 && !options.allowResultError) {
    throw new Error(`${method} ${urlPath} failed: ${result.message || result.code}`);
  }
  return data;
}

function assertLoadtestTarget(url) {
  if (url.includes(':18080') || /\/prod(\/|$)/i.test(url)) {
    throw new Error(`Refusing to prepare loadtest data against non-loadtest target: ${url}`);
  }
  if (process.env.LOADTEST_CONFIRM !== 'YES') {
    throw new Error('Set LOADTEST_CONFIRM=YES before preparing loadtest data.');
  }
}

function tokenOf(reply) {
  return reply.tokens?.accessToken || reply.tokens?.access_token || reply.accessToken || reply.access_token || '';
}

function normalizeRoom(room) {
  if (!room) return null;
  return { ...room, id: room.id || room.roomId || room.room_id };
}

function normalizeLot(lot) {
  if (!lot) return null;
  return { ...lot, id: lot.id || lot.lotId || lot.lot_id };
}

function normalizeUser(user) {
  if (!user) return null;
  return { ...user, id: user.id || user.userId || user.user_id };
}

function normalizeOrder(order) {
  if (!order) return null;
  return { ...order, id: order.id || order.orderId || order.order_id };
}

function money(amount) {
  return { amount, currency: 'CNY' };
}

function positiveInt(raw, fallback) {
  const value = Number.parseInt(String(raw ?? ''), 10);
  return Number.isFinite(value) && value > 0 ? value : fallback;
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
