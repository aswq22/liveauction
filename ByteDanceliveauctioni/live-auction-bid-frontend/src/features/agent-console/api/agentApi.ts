import { apiRequest, waitForAbortable } from '../../../shared/api/httpClient';
import { API_BASE } from '../../../shared/config/env';
import { authSession } from '../../../shared/auth/authSession';
import type { AgentApi, AgentDraftCandidate, AgentMessage, AgentRun, AgentRunEvent, AgentRunStatus, AgentSession, AgentSessionAggregate, AgentSessionStatus, ApplyInput, Execution, RecoveredExecution, ReviewSnapshot } from '../model/types';

const sessionsPath = '/api/admin/agent/sessions';
const terminalEvents = new Set(['run.completed', 'run.failed', 'run.cancelled']);
const runStatuses = new Set<AgentRunStatus>(['CREATED', 'PLANNING', 'COLLECTING_CONTEXT', 'GENERATING_PROPOSAL', 'VERIFYING', 'COMPLETED', 'FAILED', 'CANCELLED']);
const sessionStatuses = new Set<AgentSessionStatus>(['COLLECTING_INPUT', 'READY_FOR_REVIEW', 'APPLYING', 'APPLIED', 'NEEDS_REVISION', 'CANCELLED']);
const fieldSources = new Set(['USER', 'AI', 'SYSTEM', 'EXISTING']);

type Dependencies = { fetcher?: typeof fetch; getAccessToken?: () => Promise<string | null>; refreshAccessToken?: () => Promise<string | null>; wait?: (ms: number, signal: AbortSignal) => Promise<void> };

export function createAgentApi(deps: Dependencies = {}): AgentApi {
  const fetcher = deps.fetcher ?? fetch;
  const getToken = deps.getAccessToken ?? (() => authSession.getValidAccessToken());
  const refreshToken = deps.refreshAccessToken ?? (async () => (await authSession.refreshOnce())?.accessToken ?? null);
  const wait = deps.wait ?? waitForReconnect;
  return {
    async createSession(roomId, signal) {
      const reply = await apiRequest<Record<string, unknown>>({ path: sessionsPath, method: 'POST', body: { roomId }, operation: 'agent-create-session', signal });
      return sessionOf(field(reply, 'session'));
    },
    async getSession(id, signal) {
      const reply = await apiRequest<Record<string, unknown>>({ path: `${sessionsPath}/${encodeURIComponent(id)}`, method: 'GET', operation: 'agent-get-session', signal });
      return decodeSessionAggregate(reply);
    },
    async sendMessage(id, input, signal) {
      const reply = await apiRequest<Record<string, unknown>>({ path: `${sessionsPath}/${encodeURIComponent(id)}/messages`, method: 'POST', body: { expectedVersion: input.expectedVersion, content: input.content, runType: input.runType }, headers: { 'Idempotency-Key': input.idempotencyKey }, operation: 'agent-send-message', signal });
      return runOf(field(reply, 'run'));
    },
    async cancelSession(id, expectedVersion, signal) {
      await apiRequest<unknown>({ path: `${sessionsPath}/${encodeURIComponent(id)}/cancel`, method: 'POST', body: { expectedVersion }, operation: 'agent-cancel-session', signal });
    },
    async patchCandidate(sessionId, candidateId, input, signal) {
      const reply = await apiRequest<Record<string, unknown>>({ path: `${sessionsPath}/${encodeURIComponent(sessionId)}/candidates/${encodeURIComponent(candidateId)}`, method: 'PATCH', body: { expectedSessionVersion: input.expectedSessionVersion, fields: input.fields }, headers: { 'If-Match': String(input.expectedCandidateVersion) }, operation: 'agent-patch-candidate', signal });
      return decodeSessionAggregate(reply);
    },
    async bindCandidateImage(sessionId, candidateId, input, signal) {
      const reply = await apiRequest<Record<string, unknown>>({ path: `${sessionsPath}/${encodeURIComponent(sessionId)}/candidates/${encodeURIComponent(candidateId)}/images`, method: 'POST', body: input, operation: 'agent-bind-candidate-image', signal });
      return decodeSessionAggregate(reply);
    },
    async createReviewSnapshot(sessionId, input, signal) {
      return decodeReviewSnapshot(await apiRequest<unknown>({ path: `${sessionsPath}/${encodeURIComponent(sessionId)}/review-snapshots`, method: 'POST', body: input, operation: 'agent-review-snapshot', signal }));
    },
    async getReviewSnapshot(sessionId, snapshotVersion, signal) {
      return decodeReviewSnapshot(await apiRequest<unknown>({ path: `${sessionsPath}/${encodeURIComponent(sessionId)}/review-snapshots/${snapshotVersion}`, method: 'GET', operation: 'agent-get-review-snapshot', signal }));
    },
    async apply(sessionId, input: ApplyInput, signal) {
      const { idempotencyKey, ...body } = input;
      return decodeExecution(await apiRequest<unknown>({ path: `${sessionsPath}/${encodeURIComponent(sessionId)}/apply`, method: 'POST', body, headers: { 'Idempotency-Key': idempotencyKey }, operation: 'agent-apply', signal }));
    },
    async streamSessionEvents(sessionId, options) {
      let cursor = Math.max(0, options.afterSeq ?? 0);
      let failures = 0;
      let refreshed = false;
      let tokenOverride: string | null = null;
      while (!options.signal.aborted) {
        options.signal.throwIfAborted();
        const token = tokenOverride ?? await waitForAbortable(getToken(), options.signal);
        if (!token) throw new Error('登录已过期，请重新登录');
        const url = new URL(`${API_BASE}${sessionsPath}/${encodeURIComponent(sessionId)}/events`, location.origin);
        url.searchParams.set('after_seq', String(cursor));
        const headers = new Headers({ Accept: 'text/event-stream', Authorization: `Bearer ${token}`, 'Cache-Control': 'no-cache' });
        if (cursor > 0) headers.set('Last-Event-ID', String(cursor));
        let response: Response;
        try { response = await fetcher(url, { method: 'GET', headers, signal: options.signal }); }
        catch (cause) {
          if (options.signal.aborted) return;
          if (!(cause instanceof TypeError)) throw cause;
          failures = await backoff(failures, wait, options.signal); continue;
        }
        if (response.status === 401 && !refreshed) {
          await response.body?.cancel().catch(() => undefined); refreshed = true;
          tokenOverride = await waitForAbortable(refreshToken(), options.signal);
          if (!tokenOverride) throw new Error('登录已过期，请重新登录');
          continue;
        }
        if (!response.ok) {
          await response.body?.cancel().catch(() => undefined);
          if (response.status === 429 || response.status >= 500) { failures = await backoff(failures, wait, options.signal); continue; }
          throw new Error(`Agent 事件流连接失败（HTTP ${response.status}）`);
        }
        if (!response.headers.get('Content-Type')?.toLowerCase().startsWith('text/event-stream') || !response.body) { await response.body?.cancel().catch(() => undefined); throw new Error('Agent 事件流返回了无效内容'); }
        let terminal = false;
        try {
          await parseSSEStream(response.body, (message) => {
            if (message.event === 'agent.run.error') throw streamError(message.data);
            if (!message.data || message.event && message.event !== 'agent.run.event') return;
            const event = eventOf(parseEventJSON(message.data));
            const eventID = message.id ? Number(message.id) : event.seq;
            if (!Number.isSafeInteger(eventID) || eventID !== event.seq) throw new Error('Agent 事件流序号不匹配');
            if (event.seq <= cursor) return;
            if (event.seq !== cursor + 1) throw new Error('Agent 事件流全局序号不连续');
            options.onEvent(event); cursor = event.seq; failures = 0;
            terminal = terminalEvents.has(event.type) && (!options.activeRunId || event.runId === options.activeRunId);
          }, options.signal, () => terminal);
        } catch (cause) {
          if (options.signal.aborted) return;
          if (cause instanceof RetriableStreamError) { failures = await backoff(failures, wait, options.signal, cause.retryAfterMs); continue; }
          if (!(cause instanceof SSETransportError)) throw cause;
          failures = await backoff(failures, wait, options.signal); continue;
        }
        if (terminal || options.signal.aborted) return;
        failures = await backoff(failures, wait, options.signal);
      }
    },
  };
}

export const agentApi = createAgentApi();
type SSEMessage = { id?: string; event?: string; data: string };
export async function parseSSEStream(body: ReadableStream<Uint8Array>, onMessage: (message: SSEMessage) => void, signal?: AbortSignal, shouldStop?: () => boolean) {
  const reader = body.getReader(); const decoder = new TextDecoder(); let buffer = ''; let id: string | undefined; let event: string | undefined; let data: string[] = [];
  const cancel = () => { void reader.cancel().catch(() => undefined); }; signal?.addEventListener('abort', cancel, { once: true });
  const line = (raw: string) => {
    const value = raw.endsWith('\r') ? raw.slice(0, -1) : raw;
    if (!value) { if (data.length) onMessage({ ...(id ? { id } : {}), ...(event ? { event } : {}), data: data.join('\n') }); id = undefined; event = undefined; data = []; return !shouldStop?.(); }
    if (value.startsWith(':')) return true;
    const split = value.indexOf(':'); const key = split < 0 ? value : value.slice(0, split); const content = (split < 0 ? '' : value.slice(split + 1)).replace(/^ /, '');
    if (key === 'id') id = content; else if (key === 'event') event = content; else if (key === 'data') data.push(content); return true;
  };
  try {
    while (!signal?.aborted) {
      let chunk: ReadableStreamReadResult<Uint8Array>; try { chunk = await reader.read(); } catch (cause) { throw new SSETransportError(cause); }
      if (chunk.done) break; buffer += decoder.decode(chunk.value, { stream: true });
      let newline = buffer.indexOf('\n'); while (newline >= 0) { if (!line(buffer.slice(0, newline))) { await reader.cancel().catch(() => undefined); return; } buffer = buffer.slice(newline + 1); newline = buffer.indexOf('\n'); }
    }
  } catch (cause) { await reader.cancel().catch(() => undefined); throw cause; }
  finally { signal?.removeEventListener('abort', cancel); reader.releaseLock(); }
}
class SSETransportError extends Error { constructor(readonly cause: unknown) { super('Agent SSE transport disconnected'); } }
class RetriableStreamError extends Error { constructor(message: string, readonly retryAfterMs?: number) { super(message); } }
async function backoff(failures: number, wait: (ms: number, signal: AbortSignal) => Promise<void>, signal: AbortSignal, hint?: number) { const next = failures + 1; if (next >= 5) throw new Error('Agent 事件流多次断开，请重试'); const exponential = Math.min(30_000, 1_000 * 2 ** failures); await wait(hint && hint > 0 ? Math.min(30_000, Math.max(500, hint)) : exponential, signal); return next; }
function waitForReconnect(ms: number, signal: AbortSignal) { return new Promise<void>((resolve, reject) => { if (signal.aborted) { reject(signal.reason ?? new DOMException('Aborted', 'AbortError')); return; } const cleanup = () => signal.removeEventListener('abort', abort); const timer = window.setTimeout(() => { cleanup(); resolve(); }, ms); const abort = () => { clearTimeout(timer); cleanup(); reject(signal.reason ?? new DOMException('Aborted', 'AbortError')); }; signal.addEventListener('abort', abort, { once: true }); }); }
function record(value: unknown): Record<string, unknown> { if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Agent 响应格式无效'); return value as Record<string, unknown>; }
const initialismWireNames: Readonly<Record<string, string>> = {
  runId: 'RunID',
  sessionId: 'SessionID',
};
function field(value: unknown, name: string): unknown { const item = record(value); return item[name] ?? item[name[0].toUpperCase() + name.slice(1)] ?? item[initialismWireNames[name]]; }
function text(value: unknown, name: string) { const result = String(field(value, name) ?? ''); if (!result.trim()) throw new Error(`Agent 响应缺少 ${name}`); return result; }
function number(value: unknown, name: string) { const result = Number(field(value, name)); if (!Number.isFinite(result)) throw new Error(`Agent 响应缺少 ${name}`); return result; }
function sessionOf(value: unknown): AgentSession { const status = text(value, 'status') as AgentSessionStatus; if (!sessionStatuses.has(status)) throw new Error('Agent 会话状态无效'); return { id: text(value, 'id'), roomId: text(value, 'roomId'), status, version: number(value, 'version'), activeSnapshotVersion: number(value, 'activeSnapshotVersion'), conversationSummary: String(field(value, 'conversationSummary') ?? ''), createdAtUnixMs: number(value, 'createdAtUnixMs'), updatedAtUnixMs: number(value, 'updatedAtUnixMs') }; }
function runOf(value: unknown): AgentRun { const status = text(value, 'status') as AgentRunStatus; if (!runStatuses.has(status)) throw new Error('Agent Run 状态无效'); const rawRunType = String(field(value, 'runType') ?? ''); const runType = rawRunType === 'SESSION_PLAN' || rawRunType === 'LOT_DRAFT_ASSIST' ? rawRunType : undefined; return { runId: text(value, 'runId'), ...(runType ? { runType } : {}), goal: text(value, 'goal'), status, ...(field(value, 'errorCode') ? { errorCode: String(field(value, 'errorCode')) } : {}), ...(field(value, 'errorMessage') ? { errorMessage: String(field(value, 'errorMessage')) } : {}), createdAtUnixMs: number(value, 'createdAtUnixMs'), updatedAtUnixMs: number(value, 'updatedAtUnixMs') }; }
function messageOf(value: unknown): AgentMessage { return { sessionId: text(value, 'sessionId'), seq: number(value, 'seq'), role: text(value, 'role') as AgentMessage['role'], content: text(value, 'content'), runId: String(field(value, 'runId') ?? ''), createdAtUnixMs: number(value, 'createdAtUnixMs') }; }
function candidateOf(value: unknown): AgentDraftCandidate { const outer = record(value); const candidate = record(field(outer, 'candidate')); const draft = record(field(candidate, 'draft')); const provenanceVersion = number(outer, 'provenanceVersion'); if (!Number.isSafeInteger(provenanceVersion) || provenanceVersion < 0 || provenanceVersion > 1) invalidResponse(); return { candidate: { id: text(candidate, 'id'), sourceType: text(candidate, 'sourceType') as 'NEW' | 'EXISTING', ...(field(candidate, 'existingLotId') ? { existingLotId: String(field(candidate, 'existingLotId')) } : {}), ...(field(candidate, 'existingLotReadVersion') != null ? { existingLotReadVersion: Number(field(candidate, 'existingLotReadVersion')) } : {}), startPriceSpecified: Boolean(field(candidate, 'startPriceSpecified')), draft: { title: String(field(draft, 'title') ?? ''), startPriceAmount: Number(field(draft, 'startPriceAmount') ?? 0), durationSeconds: Number(field(draft, 'durationSeconds') ?? 0), mainImageAssetId: String(field(draft, 'mainImageAssetId') ?? ''), mainImageUrl: String(field(draft, 'mainImageUrl') ?? ''), minIncrementAmount: Number(field(draft, 'minIncrementAmount') ?? 0), capPriceAmount: field(draft, 'capPriceAmount') == null ? null : Number(field(draft, 'capPriceAmount')), category: String(field(draft, 'category') ?? ''), description: String(field(draft, 'description') ?? ''), tags: Array.isArray(field(draft, 'tags')) ? field(draft, 'tags') as string[] : [], stock: Number(field(draft, 'stock') ?? 0), antiSnipeWindowSeconds: Number(field(draft, 'antiSnipeWindowSeconds') ?? 0), antiSnipeExtendSeconds: Number(field(draft, 'antiSnipeExtendSeconds') ?? 0), maxExtendCount: Number(field(draft, 'maxExtendCount') ?? 0), depositAmount: Number(field(draft, 'depositAmount') ?? 0) } }, position: number(outer, 'position'), selectedByDefault: Boolean(field(outer, 'selectedByDefault')), fieldSourcesJSON: String(field(outer, 'fieldSourcesJSON') ?? '{}'), validationJSON: String(field(outer, 'validationJSON') ?? '{}'), provenanceVersion, version: number(outer, 'version'), createdAtUnixMs: Number(field(outer, 'createdAtUnixMs') ?? 0), updatedAtUnixMs: Number(field(outer, 'updatedAtUnixMs') ?? 0) }; }
export function decodeSessionAggregate(value: unknown): AgentSessionAggregate { const item = record(value); const rawRunType = String(field(item, 'lastRunType') ?? ''); const lastRunType = rawRunType === 'SESSION_PLAN' || rawRunType === 'LOT_DRAFT_ASSIST' ? rawRunType : undefined; return { session: sessionOf(field(item, 'session')), messages: (field(item, 'messages') as unknown[] ?? []).map(messageOf), candidates: (field(item, 'candidates') as unknown[] ?? []).map(candidateOf), activeRun: field(item, 'activeRun') ? runOf(field(item, 'activeRun')) : null, ...(lastRunType ? { lastRunType } : {}), appliedExecution: field(item, 'appliedExecution') ? decodeRecoveredExecution(field(item, 'appliedExecution')) : null }; }
function eventOf(value: unknown): AgentRunEvent { return { runId: text(value, 'runId'), seq: number(value, 'seq'), type: text(value, 'type'), message: text(value, 'message'), payload: field(value, 'payload') && typeof field(value, 'payload') === 'object' ? field(value, 'payload') as Record<string, unknown> : {} }; }
function parseEventJSON(data: string) { try { return JSON.parse(data) as unknown; } catch { throw new Error('Agent 事件流包含无效 JSON'); } }
function streamError(data: string) { let value: { code?: unknown; message?: unknown; retriable?: unknown; retryAfterMs?: unknown }; try { value = JSON.parse(data) as typeof value; } catch { return new Error('Agent 事件流暂时不可用，请重试'); } const message = typeof value.message === 'string' && value.message.trim() ? value.message : 'Agent 事件流暂时不可用，请重试'; if (value.retriable === true) return new RetriableStreamError(message, Number.isFinite(Number(value.retryAfterMs)) ? Number(value.retryAfterMs) : undefined); return new Error(message); }

const snapshotDraftFields = ['antiSnipeExtendSeconds', 'antiSnipeWindowSeconds', 'capPriceAmount', 'category', 'depositAmount', 'description', 'durationSeconds', 'mainImageAssetId', 'maxExtendCount', 'minIncrementAmount', 'startPriceAmount', 'stock', 'tags', 'title'] as const;
function invalidResponse(): never { throw new Error('Agent 响应格式无效'); }
function strictString(value: unknown, allowEmpty = false) { if (typeof value !== 'string' || (!allowEmpty && !value.trim())) invalidResponse(); return value; }
function strictInteger(value: unknown, minimum: number) { if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < minimum) invalidResponse(); return value; }
function strictStringArray(value: unknown) { if (!Array.isArray(value) || value.some((item) => typeof item !== 'string')) invalidResponse(); return [...value] as string[]; }
function unique(values: string[]) { return new Set(values).size === values.length; }

export function decodeReviewSnapshot(value: unknown): ReviewSnapshot {
  const outer = record(value); const snapshotVersion = strictInteger(outer.snapshotVersion, 1); const contentHash = strictString(outer.contentHash);
  if (!/^[a-f0-9]{64}$/.test(contentHash) || !Array.isArray(outer.candidates) || outer.candidates.length === 0) invalidResponse();
  const candidates = outer.candidates.map((raw) => {
    const item = record(raw); const candidateId = strictString(item.candidateId); const candidateVersion = strictInteger(item.candidateVersion, 1);
    const sourceType = strictString(item.sourceType); if (sourceType !== 'NEW' && sourceType !== 'EXISTING') invalidResponse();
    const draftValue = record(item.draft); const tags = strictStringArray(draftValue.tags); const capRaw = draftValue.capPriceAmount;
    const draft = {
      title: strictString(draftValue.title), startPriceAmount: strictInteger(draftValue.startPriceAmount, 0), durationSeconds: strictInteger(draftValue.durationSeconds, 60),
      mainImageAssetId: strictString(draftValue.mainImageAssetId, sourceType === 'EXISTING'), minIncrementAmount: strictInteger(draftValue.minIncrementAmount, 1),
      capPriceAmount: capRaw == null ? null : strictInteger(capRaw, 0), category: strictString(draftValue.category, true), description: strictString(draftValue.description, true), tags,
      stock: strictInteger(draftValue.stock, 1), antiSnipeWindowSeconds: strictInteger(draftValue.antiSnipeWindowSeconds, 1), antiSnipeExtendSeconds: strictInteger(draftValue.antiSnipeExtendSeconds, 10),
      maxExtendCount: strictInteger(draftValue.maxExtendCount, 1), depositAmount: strictInteger(draftValue.depositAmount, 0),
    };
    if (draft.antiSnipeExtendSeconds > 30 || draft.capPriceAmount != null && draft.capPriceAmount <= draft.startPriceAmount) invalidResponse();
    if (!Array.isArray(item.fieldSources) || item.fieldSources.length !== snapshotDraftFields.length) invalidResponse();
    const decodedSources = item.fieldSources.map((rawSource) => { const source = record(rawSource); const fieldName = strictString(source.field); const sourceName = strictString(source.source); if (!snapshotDraftFields.includes(fieldName as typeof snapshotDraftFields[number]) || !fieldSources.has(sourceName)) invalidResponse(); return { field: fieldName, source: sourceName as ReviewSnapshot['candidates'][number]['fieldSources'][number]['source'] }; });
    const sourceNames = decodedSources.map((source) => source.field); if (!unique(sourceNames) || snapshotDraftFields.some((name) => !sourceNames.includes(name))) invalidResponse();
    const assetIds = strictStringArray(item.assetIds); if (!unique(assetIds)) invalidResponse();
    const existingLotId = item.existingLotId == null ? undefined : strictString(item.existingLotId); const existingLotReadVersion = item.existingLotReadVersion == null ? undefined : strictInteger(item.existingLotReadVersion, 1);
    if (sourceType === 'NEW' && (existingLotId || existingLotReadVersion || assetIds.length !== 1 || assetIds[0] !== draft.mainImageAssetId)) invalidResponse();
    if (sourceType === 'EXISTING' && (!existingLotId || !existingLotReadVersion || assetIds.length !== 0 || draft.mainImageAssetId !== '')) invalidResponse();
    return { candidateId, candidateVersion, sourceType, ...(existingLotId ? { existingLotId } : {}), ...(existingLotReadVersion ? { existingLotReadVersion } : {}), draft, fieldSources: decodedSources, assetIds } as ReviewSnapshot['candidates'][number];
  });
  if (!unique(candidates.map((item) => item.candidateId))) invalidResponse();
  return { snapshotVersion, contentHash, candidates };
}

export function decodeExecution(value: unknown): Execution {
  const item = record(value); const executionId = strictString(item.executionId); const createdLotIds = strictStringArray(item.createdLotIds);
  if (!Array.isArray(item.queuedLots) || !unique(createdLotIds)) invalidResponse();
  const queuedLots = item.queuedLots.map((raw) => { const lot = record(raw); return { lotId: strictString(lot.lotId), queuePosition: strictInteger(lot.queuePosition, 1) }; });
  const queuedIDs = queuedLots.map((lot) => lot.lotId); const positions = queuedLots.map((lot) => String(lot.queuePosition));
  const target = String(item.target ?? 'QUEUE') as Execution['target']; if (target !== 'QUEUE' && target !== 'INVENTORY_ONLY') invalidResponse();
  if (!unique(queuedIDs) || !unique(positions)) invalidResponse();
  if ((target === 'QUEUE' && (queuedLots.length === 0 || createdLotIds.some((id) => !queuedIDs.includes(id)))) || (target === 'INVENTORY_ONLY' && (queuedLots.length !== 0 || createdLotIds.length === 0))) invalidResponse();
  return { executionId, createdLotIds, queuedLots, target };
}

export function decodeRecoveredExecution(value: unknown): RecoveredExecution {
  const item = record(value); const executionId = strictString(item.executionId); const snapshotVersion = strictInteger(item.snapshotVersion, 1); const createdLotIds = strictStringArray(item.createdLotIds);
  if (!Array.isArray(item.queuedLots) || !unique(createdLotIds)) invalidResponse();
  const queuedLots = item.queuedLots.map((raw) => { const lot = record(raw); const sourceType = strictString(lot.sourceType); if (sourceType !== 'NEW' && sourceType !== 'EXISTING') invalidResponse(); return { candidateId: strictString(lot.candidateId), sourceType, lotId: strictString(lot.lotId), queuePosition: strictInteger(lot.queuePosition, 1) } as RecoveredExecution['queuedLots'][number]; });
  const lotIDs = queuedLots.map((lot) => lot.lotId); const candidateIDs = queuedLots.map((lot) => lot.candidateId); const positions = queuedLots.map((lot) => lot.queuePosition);
  if (!unique(lotIDs) || !unique(candidateIDs) || !unique(positions.map(String)) || positions.some((position, index) => index > 0 && position !== positions[index - 1] + 1)) invalidResponse();
  const target = String(item.target ?? 'QUEUE') as RecoveredExecution['target']; if (target !== 'QUEUE' && target !== 'INVENTORY_ONLY') invalidResponse();
  const newLots = queuedLots.filter((lot) => lot.sourceType === 'NEW').map((lot) => lot.lotId);
  if ((target === 'QUEUE' && (queuedLots.length === 0 || JSON.stringify(newLots) !== JSON.stringify(createdLotIds))) || (target === 'INVENTORY_ONLY' && (queuedLots.length !== 0 || createdLotIds.length === 0))) invalidResponse();
  return { executionId, snapshotVersion, createdLotIds, queuedLots, target };
}
