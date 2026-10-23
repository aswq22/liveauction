import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { AlertTriangle, Bot, CheckCircle2, ImagePlus, LoaderCircle, MessageSquareText, RefreshCw, Save, Sparkles } from 'lucide-react';
import { StudioBadge, StudioButton, StudioCard, StudioEmptyState, StudioPageHeader } from '../../pages/host-console/components/studio-ui';
import { getRoomSnapshot, startLot, uploadImage } from '../auction/api/auctionApi';
import { ApiResultError, AuthExpiredError, HttpError } from '../../shared/api/httpClient';
import { agentApi } from './api/agentApi';
import { AgentReviewPanel } from './AgentReviewPanel';
import { SessionPlanPanel } from './SessionPlanPanel';
import type { AgentApi, AgentDraftCandidate, AgentFieldSource, AgentRunEvent, AgentRunStatus, AgentSessionAggregate, CandidateDraft, CandidatePatchFields, Execution, ReviewSnapshot } from './model/types';
import './agent-console.css';

const terminalEvents = new Set(['run.completed', 'run.failed', 'run.cancelled']);
const runLabels: Record<string, string> = { CREATED: '准备中', PLANNING: '理解需求', COLLECTING_CONTEXT: '查询库存', GENERATING_PROPOSAL: '生成内容', VERIFYING: '校验内容', COMPLETED: '本轮完成', FAILED: '本轮失败', CANCELLED: '已取消' };
const sessionLabels: Record<string, string> = { COLLECTING_INPUT: '等待补充', READY_FOR_REVIEW: '可以确认', NEEDS_REVISION: '需要修改', APPLYING: '正在保存', APPLIED: '已完成', CANCELLED: '已取消' };
const sourceLabels: Record<AgentFieldSource, string> = { USER: '用户填写', AI: 'AI 建议', SYSTEM: '系统默认', EXISTING: '已有拍品' };
const requiredInputLabels: Record<string, string> = { TITLE: '拍品名称', START_PRICE: '起拍价', DURATION: '竞拍时长', MAIN_IMAGE: '真实主图', AMBIGUOUS_CANDIDATE: '目标拍品' };
type AgentMode = 'AUTO' | 'LOT_DRAFT_ASSIST' | 'SESSION_PLAN';
type AgentRunType = Exclude<AgentMode, 'AUTO'>;
type PendingAdmission = { sessionId: string; expectedVersion: number; content: string; runType: AgentRunType; idempotencyKey: string; messageSeqFloor: number };

export function AgentConsolePage({ api = agentApi, roomId = '' }: { api?: AgentApi; roomId?: string }) {
  const [aggregate, setAggregate] = useState<AgentSessionAggregate | null>(null);
  const [events, setEvents] = useState<AgentRunEvent[]>([]);
  const [input, setInput] = useState('');
  const [agentMode, setAgentMode] = useState<AgentMode>('AUTO');
  const [error, setError] = useState('');
  const [turnFailed, setTurnFailed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [syncRequired, setSyncRequired] = useState(false);
  const [syncing, setSyncing] = useState(false);
  const [versionedWriteBusy, setVersionedWriteBusy] = useState(false);
  const [reviewBusy, setReviewBusy] = useState(false);
  const [activeSnapshot, setActiveSnapshot] = useState<ReviewSnapshot>();
  const [snapshotRecoveryNotice, setSnapshotRecoveryNotice] = useState('');
  const [completedExecution, setCompletedExecution] = useState<Execution | null>(null);
  const [continuationRoomID, setContinuationRoomID] = useState('');
  const [startingContinuation, setStartingContinuation] = useState(false);
  const [planBusy, setPlanBusy] = useState<'start' | 'close' | null>(null);
  const [startedPlanTitle, setStartedPlanTitle] = useState('');
  const [dirtyCandidateIDs, setDirtyCandidateIDs] = useState<Set<string>>(() => new Set());
  const [uploadCandidateIDs, setUploadCandidateIDs] = useState<Set<string>>(() => new Set());
  const loadController = useRef<AbortController | null>(null);
  const actionController = useRef<AbortController | null>(null);
  const continuationController = useRef<AbortController | null>(null);
  const continuationGate = useRef(false);
  const loadGeneration = useRef(0);
  const streamCursor = useRef(0);
  const lastSubmitted = useRef('');
  const syncRequiredRef = useRef(false);
  const pendingAdmission = useRef<PendingAdmission | null>(null);
  const versionedWriteBusyRef = useRef(false);
  const reviewBusyRef = useRef(false);
  const dirtyCandidateIDsRef = useRef(new Set<string>());
  const uploadCandidateIDsRef = useRef(new Set<string>());
  const planIdempotency = useRef<{ signature: string; key: string } | null>(null);
  const sessionID = aggregate?.session.id ?? '';

  const loadTarget = useCallback(async (requested: string | null, options: { clear: boolean; successMessage?: string; fallback: string }) => {
    loadController.current?.abort();
    const controller = new AbortController(); loadController.current = controller;
    const generation = ++loadGeneration.current;
    if (options.clear) { actionController.current?.abort(); continuationController.current?.abort(); continuationGate.current = false; planIdempotency.current = null; setBusy(false); setPlanBusy(null); setStartedPlanTitle(''); setAggregate(null); setEvents([]); streamCursor.current = 0; setTurnFailed(false); versionedWriteBusyRef.current = false; reviewBusyRef.current = false; dirtyCandidateIDsRef.current = new Set(); uploadCandidateIDsRef.current = new Set(); setVersionedWriteBusy(false); setReviewBusy(false); setDirtyCandidateIDs(new Set()); setUploadCandidateIDs(new Set()); setActiveSnapshot(undefined); setSnapshotRecoveryNotice(''); setCompletedExecution(null); setContinuationRoomID(''); setStartingContinuation(false); }
    setError('');
    try {
      let next: AgentSessionAggregate;
      if (requested) next = await api.getSession(requested, controller.signal);
      else {
        if (!roomId) throw new Error('当前没有可用直播间');
        const session = await api.createSession(roomId, controller.signal);
        next = { session, messages: [], candidates: [], activeRun: null, appliedExecution: null };
      }
      if (controller.signal.aborted || generation !== loadGeneration.current) return;
      let recoveredSnapshot: ReviewSnapshot | undefined; let recoveryNotice = '';
      if (next.session.status === 'READY_FOR_REVIEW' && next.session.activeSnapshotVersion > 0) {
        try { recoveredSnapshot = await api.getReviewSnapshot(next.session.id, next.session.activeSnapshotVersion, controller.signal); }
        catch { if (!controller.signal.aborted) recoveryNotice = '当前确认快照无法安全恢复，请重新生成确认预览。'; }
      }
      if (controller.signal.aborted || generation !== loadGeneration.current) return;
      const pending = pendingAdmission.current;
      if (pending) {
        const admitted = next.session.id === pending.sessionId && next.messages.some((message) => message.role === 'USER' && message.seq > pending.messageSeqFloor && message.content.trim() === pending.content);
        const identityChanged = next.session.id !== pending.sessionId || next.session.version !== pending.expectedVersion;
        if (admitted) {
          pendingAdmission.current = null;
          setInput((current) => current.trim() === pending.content ? '' : current);
        } else if (identityChanged) pendingAdmission.current = null;
      }
      setAggregate(next);
      setActiveSnapshot(recoveredSnapshot); setSnapshotRecoveryNotice(recoveryNotice);
      syncRequiredRef.current = false; setSyncRequired(false);
      if (!requested) { const url = new URL(location.href); url.searchParams.set('session', next.session.id); history.replaceState(history.state, '', `${url.pathname}${url.search}${url.hash}`); }
      if (options.successMessage) setError(options.successMessage);
    } catch (cause) { if (!controller.signal.aborted && generation === loadGeneration.current) setError(syncRequiredRef.current ? '会话终态已收到，但最新版本同步失败。请重新同步后继续。' : errorMessage(cause, options.fallback)); }
  }, [api, roomId]);

  useEffect(() => {
    let disposed = false;
    const open = () => { if (disposed) return; const requested = new URLSearchParams(location.search).get('session')?.trim() || null; void loadTarget(requested, { clear: true, fallback: requested ? '无法恢复 Agent 会话' : '无法打开 Agent 会话' }); };
    const timer = window.setTimeout(open, 0);
    const popstate = () => open();
    window.addEventListener('popstate', popstate);
    return () => { disposed = true; window.clearTimeout(timer); loadController.current?.abort(); actionController.current?.abort(); continuationController.current?.abort(); loadGeneration.current += 1; window.removeEventListener('popstate', popstate); };
  }, [loadTarget]);

  const refresh = useCallback(async (message?: string) => {
    if (!sessionID) return;
    const recovering = syncRequiredRef.current; if (recovering) setSyncing(true);
    try { await loadTarget(sessionID, { clear: false, successMessage: message, fallback: '无法刷新会话' }); }
    finally { if (recovering) setSyncing(false); }
  }, [loadTarget, sessionID]);

  useEffect(() => {
    if (!sessionID || !aggregate?.activeRun || ['FAILED', 'COMPLETED', 'CANCELLED'].includes(aggregate.activeRun.status)) return;
    const controller = new AbortController();
    void api.streamSessionEvents(sessionID, {
      afterSeq: streamCursor.current, activeRunId: aggregate.activeRun.runId, signal: controller.signal,
      onEvent: (event) => {
        if (event.seq <= streamCursor.current) return;
        streamCursor.current = event.seq;
        setEvents((current) => current.some((item) => item.seq === event.seq) ? current : [...current, event]);
        if (event.runId !== aggregate.activeRun?.runId) return;
        setAggregate((current) => current?.activeRun ? { ...current, activeRun: { ...current.activeRun, status: statusForEvent(event.type, current.activeRun.status) } } : current);
        if (terminalEvents.has(event.type)) {
          syncRequiredRef.current = true; setSyncRequired(true);
          actionController.current?.abort(); setBusy(false);
          setTurnFailed(event.type === 'run.failed');
          void refresh();
        }
      },
    }).catch((cause) => { if (!controller.signal.aborted) setError(errorMessage(cause, '实时状态已断开，请刷新重试')); });
    return () => controller.abort();
  }, [aggregate?.activeRun?.runId, aggregate?.activeRun?.status, api, refresh, sessionID]);

  async function send(content = input) {
    const normalized = content.trim(); if (!aggregate || !normalized || busy || reviewBusyRef.current || versionedWriteBusyRef.current || uploadCandidateIDsRef.current.size > 0 || syncRequiredRef.current || isActiveRun(aggregate.activeRun?.status)) return;
    const runType = resolveAgentRunType(normalized, agentMode);
    const controller = new AbortController(); actionController.current?.abort(); actionController.current = controller; setBusy(true); setError(''); setTurnFailed(false);
    try {
      lastSubmitted.current = normalized;
      const existing = pendingAdmission.current;
      const pending = existing && existing.sessionId === aggregate.session.id && existing.expectedVersion === aggregate.session.version && existing.content === normalized && existing.runType === runType ? existing : { sessionId: aggregate.session.id, expectedVersion: aggregate.session.version, content: normalized, runType, idempotencyKey: crypto.randomUUID(), messageSeqFloor: Math.max(0, ...aggregate.messages.map((message) => message.seq)) };
      pendingAdmission.current = pending;
      const run = await api.sendMessage(aggregate.session.id, { expectedVersion: pending.expectedVersion, content: pending.content, runType: pending.runType, idempotencyKey: pending.idempotencyKey }, controller.signal);
      pendingAdmission.current = null;
      setAggregate((current) => current ? { ...current, activeRun: run, lastRunType: run.runType ?? runType } : current);
      setInput((current) => current.trim() === normalized ? '' : current);
    } catch (cause) { if (!shouldRetainAdmission(cause)) pendingAdmission.current = null; if (!controller.signal.aborted) setError(errorMessage(cause, '本轮生成失败，现有内容已保留')); }
    finally { if (actionController.current === controller) setBusy(false); }
  }

  async function mutate(action: (signal: AbortSignal) => Promise<AgentSessionAggregate>, options: { allowDuringUpload?: boolean } = {}) {
    if (syncRequiredRef.current || reviewBusyRef.current || versionedWriteBusyRef.current || uploadCandidateIDsRef.current.size > 0 && !options.allowDuringUpload) { const cause = new Error('当前写入尚未完成，请稍后重试'); setError(cause.message); throw cause; }
    const controller = new AbortController(); actionController.current?.abort(); actionController.current = controller; versionedWriteBusyRef.current = true; setVersionedWriteBusy(true);
    try { setError(''); const next = await action(controller.signal); if (syncRequiredRef.current || controller.signal.aborted) throw new Error('请先重新同步最新会话版本'); setAggregate(next); if (next.session.activeSnapshotVersion === 0) { setActiveSnapshot(undefined); setSnapshotRecoveryNotice(''); } }
    catch (cause) {
      if (controller.signal.aborted) return;
      if (/conflict|版本|stale/i.test(errorMessage(cause, ''))) await refresh('内容已在其他位置更新，已载入最新版本，请确认后重试。');
      else setError(errorMessage(cause, '保存失败，请重试'));
      throw cause;
    } finally { versionedWriteBusyRef.current = false; setVersionedWriteBusy(false); }
  }

  function changeReviewBusy(next: boolean) { reviewBusyRef.current = next; setReviewBusy(next); }
  async function startContinuation(nextRoomID: string) {
    if (!nextRoomID || continuationGate.current) return;
    continuationGate.current = true; setStartingContinuation(true); setError('');
    const controller = new AbortController(); continuationController.current?.abort(); continuationController.current = controller;
    try {
      const session = await api.createSession(nextRoomID, controller.signal);
      if (controller.signal.aborted) return;
      setAggregate({ session, messages: [], candidates: [], activeRun: null, appliedExecution: null });
      setEvents([]); streamCursor.current = 0; setTurnFailed(false); setInput('');
      setActiveSnapshot(undefined); setSnapshotRecoveryNotice(''); setContinuationRoomID('');
      const url = new URL(location.href); url.searchParams.set('session', session.id); history.replaceState(history.state, '', `${url.pathname}${url.search}${url.hash}`);
      window.setTimeout(() => document.getElementById('agent-message')?.focus(), 0);
    } catch (cause) {
      if (!controller.signal.aborted) setError(errorMessage(cause, '本批拍品已成功保存，但新的添加工作区创建失败，请重试。'));
    } finally {
      if (continuationController.current === controller) { continuationGate.current = false; setStartingContinuation(false); }
    }
  }

  function continueAfterApplied(execution: Execution) {
    const nextRoomID = aggregate?.session.roomId || roomId;
    setCompletedExecution(execution); setContinuationRoomID(nextRoomID);
    setActiveSnapshot(undefined); setSnapshotRecoveryNotice('');
    setAggregate((current) => current ? { ...current, candidates: [], activeRun: null, appliedExecution: null } : current);
    void startContinuation(nextRoomID);
  }

  async function startPlan() {
    if (!aggregate || planBusy || aggregate.session.status !== 'READY_FOR_REVIEW') return;
    const planned = aggregate.candidates.filter((item) => item.candidate.sourceType === 'EXISTING').sort((a, b) => a.position - b.position);
    const candidateIDs = planned.map((item) => item.candidate.id);
    if (!candidateIDs.length) { setError('当前方案没有可开拍的库存拍品，请重新生成。'); return; }
    setPlanBusy('start'); setError('');
    try {
      let snapshot = activeSnapshot;
      const snapshotIDs = snapshot?.candidates.map((item) => item.candidateId) ?? [];
      if (!snapshot || snapshotIDs.join('\u0000') !== candidateIDs.join('\u0000')) {
        snapshot = await api.createReviewSnapshot(aggregate.session.id, { expectedSessionVersion: aggregate.session.version, selectedCandidateIds: candidateIDs, orderedCandidateIds: candidateIDs });
        setActiveSnapshot(snapshot);
        setAggregate((current) => current ? { ...current, session: { ...current.session, version: current.session.version + 1, activeSnapshotVersion: snapshot!.snapshotVersion } } : current);
      }
      const signature = `${aggregate.session.id}\u0000${snapshot.snapshotVersion}\u0000${snapshot.contentHash}\u0000${candidateIDs.join('\u0000')}`;
      if (!planIdempotency.current || planIdempotency.current.signature !== signature) planIdempotency.current = { signature, key: crypto.randomUUID() };
      const execution = await api.apply(aggregate.session.id, { snapshotVersion: snapshot.snapshotVersion, contentHash: snapshot.contentHash, selectedCandidateIds: candidateIDs, orderedCandidateIds: candidateIDs, idempotencyKey: planIdempotency.current.key, target: 'QUEUE' });
      if (execution.queuedLots.length !== candidateIDs.length) throw new Error('方案已提交，但服务端返回的入队拍品数量不完整');
      const firstQueued = [...execution.queuedLots].sort((a, b) => a.queuePosition - b.queuePosition)[0];
      const room = await getRoomSnapshot(aggregate.session.roomId).catch(() => null);
      if (firstQueued && room && !room.currentLot) {
        try {
          const started = await startLot(firstQueued.lotId);
          setStartedPlanTitle(started.title);
        } catch (cause) {
          const latest = await getRoomSnapshot(aggregate.session.roomId).catch(() => null);
          if (latest?.currentLot?.id === firstQueued.lotId) setStartedPlanTitle(latest.currentLot.title);
          else if (!latest?.currentLot) setError(`方案已加入队列，但首件自动开拍失败：${errorMessage(cause, '请前往拍品队列手动开拍')}`);
        }
      }
      planIdempotency.current = null;
      continueAfterApplied(execution);
    } catch (cause) {
      setError(errorMessage(cause, '执行排品方案失败；可安全重试，系统不会重复入队。'));
    } finally { setPlanBusy(null); }
  }

  async function closePlan() {
    if (!aggregate || planBusy || !window.confirm('关闭当前排品方案？库存拍品不会被删除或入队。')) return;
    setPlanBusy('close'); setError('');
    try {
      await api.cancelSession(aggregate.session.id, aggregate.session.version);
      planIdempotency.current = null; setActiveSnapshot(undefined);
      await startContinuation(aggregate.session.roomId);
    } catch (cause) { setError(errorMessage(cause, '关闭方案失败，请刷新后重试')); }
    finally { setPlanBusy(null); }
  }

  const changeCandidateDirty = useCallback((candidateID: string, next: boolean) => { const values = new Set(dirtyCandidateIDsRef.current); if (next) values.add(candidateID); else values.delete(candidateID); dirtyCandidateIDsRef.current = values; setDirtyCandidateIDs(values); }, []);
  const changeCandidateUpload = useCallback((candidateID: string, next: boolean) => {
    if (next && (uploadCandidateIDsRef.current.size > 0 || versionedWriteBusyRef.current || reviewBusyRef.current || syncRequiredRef.current)) return false;
    const values = new Set(uploadCandidateIDsRef.current); if (next) values.add(candidateID); else values.delete(candidateID); uploadCandidateIDsRef.current = values; setUploadCandidateIDs(values); return true;
  }, []);

  const run = aggregate?.activeRun;
  const isPlanningWorkspace = (run?.runType ?? aggregate?.lastRunType) === 'SESSION_PLAN';
  const planCandidates = aggregate?.candidates.filter((item) => item.candidate.sourceType === 'EXISTING') ?? [];
  return <section className="agentConsolePage">
    <StudioCard padding="lg" className="agentConsoleHero">
      <StudioPageHeader eyebrow={<span className="agentConsoleEyebrow"><Bot size={15} /> Recoverable workspace</span>} title="AI 竞拍工作台" description="用自然语言创建库存拍品或生成直播排品方案；未开拍的拍品进入库存，开拍后不再参与排品。" actions={aggregate ? <StudioBadge tone={aggregate.session.status === 'READY_FOR_REVIEW' ? 'success' : 'info'}>{sessionLabels[aggregate.session.status]}</StudioBadge> : undefined} />
    </StudioCard>
    <nav className="agentWorkspaceNav" aria-label="工作区导航"><a href="#agent-conversation">对话</a><a href="#agent-candidates">{isPlanningWorkspace ? '排品方案' : '待创建拍品'} {isPlanningWorkspace ? planCandidates.length : aggregate?.candidates.length ?? 0}</a>{!isPlanningWorkspace ? <a href="#agent-review">确认创建</a> : null}</nav>
    {syncRequired && !error ? <div className="agentInlineAlert" role="status" aria-live="polite"><LoaderCircle size={18} className={syncing ? 'agentSpin' : undefined} /><span>正在同步最新会话版本，完成前已暂停所有写入操作。</span></div> : null}
    {error ? <div className="agentInlineAlert" role="alert"><AlertTriangle size={18} /><span>{error}</span>{continuationRoomID ? <button type="button" disabled={startingContinuation} onClick={() => void startContinuation(continuationRoomID)}><RefreshCw size={15} className={startingContinuation ? 'agentSpin' : undefined} />重试继续添加</button> : sessionID ? <button type="button" disabled={syncing} onClick={() => void refresh()}><RefreshCw size={15} className={syncing ? 'agentSpin' : undefined} />{syncRequired ? '重新同步' : '刷新会话'}</button> : null}</div> : null}
    <div className="agentDraftWorkspace">
      <section id="agent-conversation" className="agentConversationColumn" aria-labelledby="agent-conversation-title">
        <StudioCard title="与 Agent 对话" subtitle={run ? runLabels[run.status] : 'Conversation'} padding="lg">
          <h3 id="agent-conversation-title" className="agentVisuallyHidden">会话消息</h3>
          <div className="agentMessages" aria-live="polite">
            {aggregate?.messages.length ? aggregate.messages.map((message) => <article key={message.seq} className={`agentMessage agentMessage-${message.role.toLowerCase()}`}><small>{message.role === 'USER' ? '你' : 'Agent'}</small><p>{message.content}</p></article>) : <StudioEmptyState compact icon={<MessageSquareText size={26} />} title="从一句需求开始" description="例如：拍卖海洋之心，起拍价 10000 元，时长 50 分钟。" />}
            {events.map((event) => <div className="agentRunEvent" key={event.seq}><Sparkles size={14} /><span>{event.message}</span><small>#{event.seq}</small></div>)}
          </div>
          {turnFailed || run?.status === 'FAILED' ? <div className="agentRunFailure"><strong>本轮生成失败</strong><span>现有对话和内容均已保留。</span><StudioButton type="button" variant="secondary" disabled={syncRequired || reviewBusy || versionedWriteBusy || uploadCandidateIDs.size > 0} onClick={() => void send(lastSubmitted.current)}>重试本轮</StudioButton></div> : null}
          <div className="agentComposer">
            <fieldset className="agentModeSelector">
              <legend>Agent 模式</legend>
              <label><input type="radio" name="agent-mode" value="AUTO" checked={agentMode === 'AUTO'} onChange={() => setAgentMode('AUTO')} /><span><strong>自动识别</strong><small>按消息内容选择能力</small></span></label>
              <label><input type="radio" name="agent-mode" value="LOT_DRAFT_ASSIST" checked={agentMode === 'LOT_DRAFT_ASSIST'} onChange={() => setAgentMode('LOT_DRAFT_ASSIST')} /><span><strong>创建拍品</strong><small>创建、补充或修改库存拍品</small></span></label>
              <label><input type="radio" name="agent-mode" value="SESSION_PLAN" checked={agentMode === 'SESSION_PLAN'} onChange={() => setAgentMode('SESSION_PLAN')} /><span><strong>直播排品</strong><small>读取库存并生成方案</small></span></label>
            </fieldset>
            <label htmlFor="agent-message">给 Agent 发消息</label>
            <textarea id="agent-message" aria-describedby="agent-message-help agent-mode-result" value={input} onChange={(event) => setInput(event.target.value)} rows={4} placeholder="描述要拍卖的商品，Agent 会只追问必要信息" disabled={!aggregate || startingContinuation || Boolean(continuationRoomID)} />
            <p id="agent-mode-result" className="agentModeResult" aria-live="polite">{modeResultText(agentMode, input)}</p>
            <div><small id="agent-message-help">创建库存拍品需要名称、起拍价、时长和真实主图；直播排品会读取现有库存。</small><StudioButton type="button" variant="primary" loading={busy || startingContinuation} disabled={!input.trim() || !aggregate || Boolean(continuationRoomID) || syncRequired || reviewBusy || versionedWriteBusy || uploadCandidateIDs.size > 0 || isActiveRun(run?.status)} onClick={() => void send()}>发送</StudioButton></div>
          </div>
        </StudioCard>
      </section>
      <aside id="agent-candidates" className="agentCandidatesColumn" aria-labelledby="agent-candidates-title">
        {isPlanningWorkspace ? <SessionPlanPanel candidates={planCandidates} busy={planBusy} disabled={syncRequired || busy || reviewBusy || isActiveRun(run?.status)} onStart={() => void startPlan()} onClose={() => void closePlan()} /> : <><header><div><span>Pending creation</span><h2 id="agent-candidates-title">待创建拍品</h2></div><StudioBadge tone="neutral">{aggregate?.candidates.length ?? 0} 件</StudioBadge></header>{aggregate?.candidates.length ? aggregate.candidates.map((item) => <CandidateCard key={`${item.candidate.id}-${item.version}`} item={item} session={aggregate} api={api} mutate={mutate} onDirtyChange={changeCandidateDirty} onUploadChange={changeCandidateUpload} writeBlocked={syncRequired || busy || versionedWriteBusy || reviewBusy || uploadCandidateIDs.size > 0 || isActiveRun(run?.status)} />) : <StudioCard padding="lg"><StudioEmptyState compact icon={<Sparkles size={26} />} title="尚无待创建拍品" description="发送拍品信息后，Agent 会在这里生成可编辑内容。" /></StudioCard>}</>}
      </aside>
    </div>
    {completedExecution ? <section className="agentApplySuccess agentCompletedBatch" role="status" aria-live="polite">
      <header><CheckCircle2 size={22} /><div><span>Completed batch</span><h2>{completedExecution.target === 'INVENTORY_ONLY' ? '本次拍品已创建' : startedPlanTitle ? `${startedPlanTitle}已开拍，其余拍品排队中` : '排品方案已加入等待队列'}</h2></div></header>
      <p>执行 ID：{completedExecution.executionId}</p>
      {completedExecution.createdLotIds.length ? <p>新建拍品 ID：{completedExecution.createdLotIds.join('、')}</p> : <p>本批次未创建新拍品</p>}
      {completedExecution.queuedLots.length ? <ul>{completedExecution.queuedLots.map((lot) => <li key={lot.lotId}>{lot.lotId} · 队列位置 {lot.queuePosition}</li>)}</ul> : <p>当前状态：待开拍，可供 Agent 生成直播排品方案。</p>}
      <p>{startingContinuation ? '右侧候选区已清空，正在准备下一件拍品的添加工作区。' : continuationRoomID ? '右侧候选区已清空；请重试创建下一件拍品的添加工作区。' : '右侧候选区已清空，可以继续描述下一件拍品。'}</p>
      <StudioButton type="button" variant="secondary" onClick={() => { location.href = '/admin/auctions'; }}>{completedExecution.target === 'INVENTORY_ONLY' ? '查看待开拍拍品' : '前往拍品队列'}</StudioButton>
    </section> : null}
    {!isPlanningWorkspace && aggregate?.candidates.length ? <section id="agent-review" className="agentReviewSection" aria-label="确认创建拍品">
      {dirtyCandidateIDs.size ? <div className="agentInlineAlert" role="status" aria-live="polite"><AlertTriangle size={18} /><span>有未保存修改，请先保存或恢复为服务器中的值后再确认。</span></div> : uploadCandidateIDs.size ? <div className="agentInlineAlert" role="status" aria-live="polite"><LoaderCircle size={18} className="agentSpin" /><span>主图上传并绑定中，完成前已暂停确认和其他写入。</span></div> : null}
      <AgentReviewPanel session={aggregate.session} candidates={aggregate.candidates} activeSnapshot={activeSnapshot} recoveredExecution={aggregate.appliedExecution ?? undefined} snapshotRecoveryNotice={snapshotRecoveryNotice} disabled={syncRequired || busy || versionedWriteBusy || reviewBusy || dirtyCandidateIDs.size > 0 || uploadCandidateIDs.size > 0 || isActiveRun(run?.status)} onBusyChange={changeReviewBusy} onCreateSnapshot={(input) => api.createReviewSnapshot(aggregate.session.id, input)} onSnapshotCreated={(snapshot, nextVersion) => { setActiveSnapshot(snapshot); setSnapshotRecoveryNotice(''); setAggregate((current) => current ? { ...current, session: { ...current.session, version: nextVersion, activeSnapshotVersion: snapshot.snapshotVersion } } : current); }} onApply={(input) => api.apply(aggregate.session.id, input)} onApplied={continueAfterApplied} onRevisionRequired={() => refresh()} />
    </section> : null}
  </section>;
}

function CandidateCard({ item, session, api, mutate, writeBlocked, onDirtyChange, onUploadChange }: { item: AgentDraftCandidate; session: AgentSessionAggregate; api: AgentApi; mutate: (action: (signal: AbortSignal) => Promise<AgentSessionAggregate>, options?: { allowDuringUpload?: boolean }) => Promise<void>; writeBlocked: boolean; onDirtyChange(candidateID: string, dirty: boolean): void; onUploadChange(candidateID: string, uploading: boolean): boolean }) {
  const draft = item.candidate.draft; const [form, setForm] = useState(() => draftForm(draft, item.candidate.startPriceSpecified)); const [dirty, setDirty] = useState<Set<keyof DraftForm>>(() => new Set()); const [fieldErrors, setFieldErrors] = useState<DraftFormErrors>({}); const [localError, setLocalError] = useState(''); const [saving, setSaving] = useState(false); const [uploading, setUploading] = useState(false);
  const cardMounted = useRef(true);
  const sources = parseSources(item.fieldSourcesJSON); const required = parseRequired(item.validationJSON); const title = form.title.trim() || '未命名拍品';
  const missingRequired = Array.from(required).filter((input) => input !== 'MAIN_IMAGE' || !draft.mainImageAssetId);
  const missingRequiredText = missingRequired.map((input) => requiredInputLabels[input] ?? input).join('、');
  const prefix = `agent-candidate-${item.candidate.id.replace(/[^a-zA-Z0-9_-]/g, '-')}`;
  const serverError = (name: keyof DraftForm) => name === 'title' && !form.title.trim() ? '请填写拍品名称' : name === 'startPrice' && !form.startPrice.trim() ? '请填写起拍价' : name === 'durationMinutes' && !form.durationMinutes.trim() ? '请填写竞拍时长' : '';
  const errorFor = (name: keyof DraftForm) => fieldErrors[name] || serverError(name);
  const inputProps = (name: keyof DraftForm) => { const fieldError = errorFor(name); return { id: `${prefix}-${name}`, disabled: writeBlocked, 'aria-invalid': Boolean(fieldError), 'aria-describedby': fieldError ? `${prefix}-${name}-error` : undefined }; };
  useEffect(() => { cardMounted.current = true; return () => { cardMounted.current = false; onDirtyChange(item.candidate.id, false); onUploadChange(item.candidate.id, false); }; }, [item.candidate.id, onDirtyChange, onUploadChange]);
  const change = (name: keyof DraftForm, value: string) => { const nextForm = { ...form, [name]: value }; const nextDirty = new Set((Object.keys(nextForm) as Array<keyof DraftForm>).filter((field) => draftFieldDiffers(field, nextForm[field], item))); setForm(nextForm); setDirty(nextDirty); onDirtyChange(item.candidate.id, nextDirty.size > 0); setFieldErrors((current) => { const validated = validateDraftForm(nextForm); const next: DraftFormErrors = {}; for (const field of Object.keys(current) as Array<keyof DraftForm>) if (validated[field]) next[field] = validated[field]; return next; }); setLocalError(''); };
  async function save() {
    if (!dirty.size || writeBlocked) return;
    const invalid = validateDraftForm(form); const firstInvalid = Object.keys(invalid)[0] as keyof DraftForm | undefined;
    if (firstInvalid) { setFieldErrors(invalid); setLocalError('请修正标记的字段后再保存'); window.setTimeout(() => document.getElementById(`${prefix}-${firstInvalid}`)?.focus(), 0); return; }
    setFieldErrors({});
    setSaving(true); setLocalError('');
    try {
      const fields: CandidatePatchFields = {};
      if (dirty.has('title')) fields.title = form.title.trim();
      if (dirty.has('startPrice')) fields.startPriceAmount = form.startPrice.trim() ? parseYuanToCents(form.startPrice) : null;
      if (dirty.has('durationMinutes')) fields.durationSeconds = parseDurationSeconds(form.durationMinutes);
      if (dirty.has('minIncrement')) fields.minIncrementAmount = parseYuanToCents(form.minIncrement);
      if (dirty.has('capPrice')) fields.capPriceAmount = form.capPrice.trim() ? parseYuanToCents(form.capPrice) : null;
      if (dirty.has('category')) fields.category = form.category.trim();
      if (dirty.has('description')) fields.description = form.description.trim();
      if (dirty.has('tags')) fields.tags = form.tags.split(/[，,]/).map((tag) => tag.trim()).filter(Boolean);
      if (dirty.has('stock')) fields.stock = parsePositiveInteger(form.stock);
      if (dirty.has('antiSnipeWindow')) fields.antiSnipeWindowSeconds = parsePositiveInteger(form.antiSnipeWindow);
      if (dirty.has('antiSnipeExtend')) fields.antiSnipeExtendSeconds = parseIntegerInRange(form.antiSnipeExtend, 10, 30, '每次延长秒数必须在 10 到 30 之间');
      if (dirty.has('maxExtendCount')) fields.maxExtendCount = parsePositiveInteger(form.maxExtendCount);
      if (dirty.has('deposit')) fields.depositAmount = parseYuanToCents(form.deposit);
      await mutate((signal) => api.patchCandidate(session.session.id, item.candidate.id, { expectedSessionVersion: session.session.version, expectedCandidateVersion: item.version, fields }, signal));
    } catch (cause) { setLocalError(errorMessage(cause, '请检查输入后重试')); }
    finally { setSaving(false); }
  }
  async function upload(file?: File) {
    if (!file || writeBlocked || !onUploadChange(item.candidate.id, true)) return; setUploading(true); setLocalError('');
    try { const asset = await uploadImage(file, { roomId: session.session.roomId, bizType: 'agent_lot_image' }); if (!cardMounted.current) return; await mutate((signal) => api.bindCandidateImage(session.session.id, item.candidate.id, { expectedSessionVersion: session.session.version, expectedCandidateVersion: item.version, assetId: asset.id }, signal), { allowDuringUpload: true }); }
    catch (cause) { if (cardMounted.current) setLocalError(errorMessage(cause, '图片上传失败，请重试')); }
    finally { if (cardMounted.current) { setUploading(false); onUploadChange(item.candidate.id, false); } }
  }
  return <StudioCard padding="lg" className="agentCandidateCard">
    <header className="agentCandidateHeader"><div><StudioBadge tone={item.candidate.sourceType === 'NEW' ? 'info' : 'neutral'}>{item.candidate.sourceType === 'NEW' ? '待创建' : '已有拍品'}</StudioBadge><h3>{title}</h3></div><span>版本 {item.version}</span></header>
    <div className={`agentCandidateCompleteness${missingRequired.length ? ' agentCandidateCompleteness-missing' : ''}`} role="status" aria-live="polite">
      <strong>{missingRequired.length ? `还需补充：${missingRequiredText}` : '必填信息已完整'}</strong>
      <span>{missingRequired.length ? '其余非必填参数已由 AI 自动补全，可在下方编辑。' : 'AI 建议参数已自动补全，仍可在下方调整。'}</span>
    </div>
    <div className="agentCandidateImage">
      {draft.mainImageUrl ? <img src={draft.mainImageUrl} alt={`${title}主图`} /> : <div><ImagePlus size={25} /><span>缺少真实主图</span></div>}
      <label className="agentUploadButton">{uploading ? <LoaderCircle size={16} className="agentSpin" /> : <ImagePlus size={16} />}<span>{uploading ? '上传中' : draft.mainImageUrl ? '替换主图' : '选择主图'}</span><input type="file" accept="image/jpeg,image/png,image/webp" aria-label={`上传${title}的主图`} disabled={uploading || writeBlocked} onChange={(event) => void upload(event.target.files?.[0])} /></label>
    </div>
    <div className="agentCandidateFields">
      <DraftField label="拍品名称" source={sources.title} error={errorFor('title')} errorId={`${prefix}-title-error`}><input {...inputProps('title')} aria-label={`${title}的拍品名称`} value={form.title} onChange={(event) => change('title', event.target.value)} /></DraftField>
      <DraftField label="起拍价（元）" source={sources.startPriceAmount} error={errorFor('startPrice')} errorId={`${prefix}-startPrice-error`}><input {...inputProps('startPrice')} inputMode="decimal" aria-label={`${title}的起拍价（元）`} value={form.startPrice} onChange={(event) => change('startPrice', event.target.value)} /></DraftField>
      <DraftField label="竞拍时长（分钟）" source={sources.durationSeconds} error={errorFor('durationMinutes')} errorId={`${prefix}-durationMinutes-error`}><input {...inputProps('durationMinutes')} inputMode="numeric" aria-label={`${title}的竞拍时长（分钟）`} value={form.durationMinutes} onChange={(event) => change('durationMinutes', event.target.value)} /></DraftField>
      <DraftField label="最低加价（元）" source={sources.minIncrementAmount} error={errorFor('minIncrement')} errorId={`${prefix}-minIncrement-error`}><input {...inputProps('minIncrement')} inputMode="decimal" aria-label={`${title}的最低加价（元）`} value={form.minIncrement} onChange={(event) => change('minIncrement', event.target.value)} /></DraftField>
      <DraftField label="封顶价（元）" source={sources.capPriceAmount} error={errorFor('capPrice')} errorId={`${prefix}-capPrice-error`}><input {...inputProps('capPrice')} inputMode="decimal" aria-label={`${title}的封顶价（元）`} value={form.capPrice} onChange={(event) => change('capPrice', event.target.value)} /></DraftField>
      <DraftField label="库存" source={sources.stock} error={errorFor('stock')} errorId={`${prefix}-stock-error`}><input {...inputProps('stock')} inputMode="numeric" aria-label={`${title}的库存`} value={form.stock} onChange={(event) => change('stock', event.target.value)} /></DraftField>
      <DraftField label="反狙击窗口（秒）" source={sources.antiSnipeWindowSeconds} error={errorFor('antiSnipeWindow')} errorId={`${prefix}-antiSnipeWindow-error`}><input {...inputProps('antiSnipeWindow')} inputMode="numeric" aria-label={`${title}的反狙击窗口（秒）`} value={form.antiSnipeWindow} onChange={(event) => change('antiSnipeWindow', event.target.value)} /><small>必须大于 0 秒</small></DraftField>
      <DraftField label="每次延长（秒）" source={sources.antiSnipeExtendSeconds} error={errorFor('antiSnipeExtend')} errorId={`${prefix}-antiSnipeExtend-error`}><input {...inputProps('antiSnipeExtend')} inputMode="numeric" aria-label={`${title}的每次延长（秒）`} value={form.antiSnipeExtend} onChange={(event) => change('antiSnipeExtend', event.target.value)} /><small>允许 10–30 秒</small></DraftField>
      <DraftField label="最多延长次数" source={sources.maxExtendCount} error={errorFor('maxExtendCount')} errorId={`${prefix}-maxExtendCount-error`}><input {...inputProps('maxExtendCount')} inputMode="numeric" aria-label={`${title}的最多延长次数`} value={form.maxExtendCount} onChange={(event) => change('maxExtendCount', event.target.value)} /></DraftField>
      <DraftField label="保证金（元）" source={sources.depositAmount} error={errorFor('deposit')} errorId={`${prefix}-deposit-error`}><input {...inputProps('deposit')} inputMode="decimal" aria-label={`${title}的保证金（元）`} value={form.deposit} onChange={(event) => change('deposit', event.target.value)} /><small>可为 0，提交时精确换算为分</small></DraftField>
      <DraftField label="分类" source={sources.category}><input disabled={writeBlocked} aria-label={`${title}的分类`} value={form.category} onChange={(event) => change('category', event.target.value)} /></DraftField>
      <DraftField label="标签" source={sources.tags}><input disabled={writeBlocked} aria-label={`${title}的标签`} value={form.tags} onChange={(event) => change('tags', event.target.value)} /></DraftField>
      <DraftField label="介绍" source={sources.description} wide><textarea disabled={writeBlocked} aria-label={`${title}的介绍`} rows={4} value={form.description} onChange={(event) => change('description', event.target.value)} /></DraftField>
    </div>
    {localError ? <p className="agentFieldError" role="alert">{localError}</p> : null}
    <div className="agentCandidateActions"><span>{writeBlocked ? '等待会话版本同步' : dirty.size ? `有 ${dirty.size} 项未保存` : '修改后会使旧确认快照失效'}</span><StudioButton type="button" variant="secondary" icon={<Save size={16} />} loading={saving} disabled={writeBlocked || !dirty.size} onClick={() => void save()}>保存{title}</StudioButton></div>
  </StudioCard>;
}

export function resolveAgentRunType(content: string, mode: AgentMode): AgentRunType {
  if (mode !== 'AUTO') return mode;
  const normalized = content.replace(/\s+/g, ' ').trim();
  // Strong planning language wins even when the request also contains price or
  // duration constraints. Product copy such as “直播间展示的介绍” is not a plan.
  const planningIntent = /(?:排品|排期|商品顺序|拍卖顺序|开场.{0,16}压轴|压轴.{0,16}开场|主推商品|根据.{0,20}库存|库存.{0,20}(?:方案|计划|策略|安排)|(?:方案|计划|策略|安排).{0,20}(?:库存|拍品顺序)|(?:均衡|保守|激进)型.{0,16}(?:直播|排品|方案)|(?:生成|制定|安排).{0,12}(?:直播方案|直播计划))/;
  if (planningIntent.test(normalized)) return 'SESSION_PLAN';
  return 'LOT_DRAFT_ASSIST';
}
function modeResultText(mode: AgentMode, content: string) {
  if (mode === 'AUTO' && !content.trim()) return '自动模式会在“创建拍品”和“直播排品”之间识别；发送前可手动指定。';
  const resolved = resolveAgentRunType(content, mode);
  return `${mode === 'AUTO' ? '自动识别为' : '当前指定'}：${resolved === 'SESSION_PLAN' ? '直播排品（查询库存、历史与场次）' : '创建拍品（生成、补充或修改库存拍品）'}`;
}
function DraftField({ label, source, error, errorId, wide, children }: { label: string; source?: AgentFieldSource; error?: string; errorId?: string; wide?: boolean; children: ReactNode }) { return <label className={`agentDraftField${wide ? ' agentDraftField-wide' : ''}`}><span>{label}{source ? <em className={`agentSource agentSource-${source.toLowerCase()}`}>{sourceLabels[source] ?? '来源未知'}</em> : null}</span>{children}{error ? <small id={errorId} className="agentFieldError" role="alert">{error}</small> : null}</label>; }
function draftForm(draft: CandidateDraft, startPriceSpecified: boolean) { return { title: draft.title, startPrice: formatStartPrice(draft.startPriceAmount, startPriceSpecified), durationMinutes: draft.durationSeconds > 0 ? String(draft.durationSeconds / 60) : '', minIncrement: formatYuanFromCents(draft.minIncrementAmount), capPrice: draft.capPriceAmount == null ? '' : formatYuanFromCents(draft.capPriceAmount), category: draft.category, description: draft.description, tags: draft.tags.join('，'), stock: String(draft.stock || 1), antiSnipeWindow: String(draft.antiSnipeWindowSeconds), antiSnipeExtend: String(draft.antiSnipeExtendSeconds), maxExtendCount: String(draft.maxExtendCount), deposit: formatYuanFromCents(draft.depositAmount) }; }
function formatStartPrice(cents: number, specified: boolean) { if (!specified) return ''; return cents === 0 ? '0.00' : formatYuanFromCents(cents); }
type DraftForm = ReturnType<typeof draftForm>;
type DraftFormErrors = Partial<Record<keyof DraftForm, string>>;
function draftFieldDiffers(field: keyof DraftForm, value: string, item: AgentDraftCandidate) {
  const draft = item.candidate.draft;
  try {
    switch (field) {
      case 'title': return value.trim() !== draft.title;
      case 'startPrice': return value.trim() ? !item.candidate.startPriceSpecified || parseYuanToCents(value) !== draft.startPriceAmount : item.candidate.startPriceSpecified;
      case 'durationMinutes': return parseDurationSeconds(value) !== draft.durationSeconds;
      case 'minIncrement': return parseYuanToCents(value) !== draft.minIncrementAmount;
      case 'capPrice': return value.trim() ? parseYuanToCents(value) !== draft.capPriceAmount : draft.capPriceAmount != null;
      case 'category': return value.trim() !== draft.category;
      case 'description': return value.trim() !== draft.description;
      case 'tags': return JSON.stringify(value.split(/[，,]/).map((tag) => tag.trim()).filter(Boolean)) !== JSON.stringify(draft.tags);
      case 'stock': return parsePositiveInteger(value) !== draft.stock;
      case 'antiSnipeWindow': return parsePositiveInteger(value) !== draft.antiSnipeWindowSeconds;
      case 'antiSnipeExtend': return parseIntegerInRange(value, 10, 30, '') !== draft.antiSnipeExtendSeconds;
      case 'maxExtendCount': return parsePositiveInteger(value) !== draft.maxExtendCount;
      case 'deposit': return parseYuanToCents(value) !== draft.depositAmount;
    }
  } catch { return true; }
}
function parseSources(raw: string) { try { return JSON.parse(raw) as Partial<Record<keyof CandidateDraft, AgentFieldSource>>; } catch { return {}; } }
function parseRequired(raw: string) { try { const value = JSON.parse(raw) as { requiredInputs?: string[] }; return new Set(value.requiredInputs ?? []); } catch { return new Set<string>(); } }
function parseDurationSeconds(value: string) { const minutes = Number(value); const seconds = minutes * 60; if (!Number.isFinite(minutes) || !Number.isSafeInteger(seconds) || seconds < 60) throw new Error('竞拍时长必须至少为 1 分钟，且能精确换算为整秒'); return seconds; }
function parsePositiveInteger(value: string) { const number = Number(value); if (!Number.isSafeInteger(number) || number <= 0) throw new Error('库存必须是正整数'); return number; }
function parseIntegerInRange(value: string, min: number, max: number, message: string) { const number = Number(value); if (!Number.isInteger(number) || number < min || number > max) throw new Error(message); return number; }
function validateDraftForm(form: DraftForm): DraftFormErrors {
  const errors: DraftFormErrors = {};
  if (!form.title.trim()) errors.title = '请填写拍品名称';
  let startPrice: number | null = null;
  if (form.startPrice.trim()) { try { startPrice = parseYuanToCents(form.startPrice); } catch { errors.startPrice = '起拍价格式无效，最多保留两位小数且不能为负数'; } }
  try { parseDurationSeconds(form.durationMinutes); } catch { errors.durationMinutes = '竞拍时长必须至少为 1 分钟，且能精确换算为整秒'; }
  try { if (parseYuanToCents(form.minIncrement) <= 0) errors.minIncrement = '最低加价必须大于 0 元'; } catch { errors.minIncrement = '最低加价格式无效，最多保留两位小数且不能为负数'; }
  if (form.capPrice.trim()) { try { const capPrice = parseYuanToCents(form.capPrice); if (startPrice !== null && capPrice <= startPrice) errors.capPrice = '封顶价必须严格大于起拍价'; } catch { errors.capPrice = '封顶价格式无效，最多保留两位小数且不能为负数'; } }
  try { parsePositiveInteger(form.stock); } catch { errors.stock = '库存必须是正整数'; }
  try { parsePositiveInteger(form.antiSnipeWindow); } catch { errors.antiSnipeWindow = '反狙击窗口必须是正整数秒'; }
  try { parseIntegerInRange(form.antiSnipeExtend, 10, 30, ''); } catch { errors.antiSnipeExtend = '每次延长秒数必须在 10 到 30 之间'; }
  try { parsePositiveInteger(form.maxExtendCount); } catch { errors.maxExtendCount = '最多延长次数必须是正整数'; }
  try { parseYuanToCents(form.deposit); } catch { errors.deposit = '保证金格式无效，最多保留两位小数且不能为负数'; }
  return errors;
}
export function parseYuanToCents(value: string) { const normalized = value.trim(); const match = /^(0|[1-9]\d*)(?:\.(\d{1,2}))?$/.exec(normalized); if (!match) throw new Error('金额最多保留两位小数且不能为负数'); const yuan = BigInt(match[1]); const decimals = (match[2] ?? '').padEnd(2, '0'); const cents = yuan * 100n + BigInt(decimals || '0'); if (cents > BigInt(Number.MAX_SAFE_INTEGER)) throw new Error('金额超出安全范围'); return Number(cents); }
export function formatYuanFromCents(cents: number) { if (!Number.isSafeInteger(cents) || cents < 0) return ''; const yuan = Math.floor(cents / 100); const remainder = cents % 100; return remainder ? `${yuan}.${String(remainder).padStart(2, '0').replace(/0$/, '')}` : String(yuan); }
function errorMessage(cause: unknown, fallback: string) { return cause instanceof Error && cause.message.trim() ? cause.message : fallback; }
function shouldRetainAdmission(cause: unknown) { if (cause instanceof HttpError) return cause.status === 408 || cause.status === 425 || cause.status === 429 || cause.status >= 500; if (cause instanceof ApiResultError || cause instanceof AuthExpiredError) return false; return true; }
function statusForEvent(type: string, fallback: AgentRunStatus): AgentRunStatus { if (type === 'run.completed') return 'COMPLETED'; if (type === 'run.failed') return 'FAILED'; if (type === 'run.cancelled') return 'CANCELLED'; if (type === 'tool.started' || type === 'tool.completed') return 'COLLECTING_CONTEXT'; if (type === 'run.planning') return 'PLANNING'; return fallback; }
function isActiveRun(status?: AgentRunStatus) { return Boolean(status && !['COMPLETED', 'FAILED', 'CANCELLED'].includes(status)); }
