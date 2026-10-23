import { useEffect, useMemo, useRef, useState } from 'react';
import { AlertTriangle, ArrowDown, ArrowUp, CheckCircle2, FileCheck2, ShieldAlert } from 'lucide-react';
import { StudioBadge, StudioButton, StudioCard } from '../../pages/host-console/components/studio-ui';
import { ApiResultError, AuthExpiredError, HttpError } from '../../shared/api/httpClient';
import type { AgentDraftCandidate, AgentFieldSource, AgentSession, ApplyInput, ApplyTarget, Execution, RecoveredExecution, ReviewInput, ReviewSnapshot } from './model/types';

export type AgentReviewPanelProps = {
  session: AgentSession;
  candidates: AgentDraftCandidate[];
  activeSnapshot?: ReviewSnapshot;
  recoveredExecution?: RecoveredExecution;
  onCreateSnapshot(input: ReviewInput): Promise<ReviewSnapshot>;
  onApply(input: ApplyInput): Promise<Execution>;
  disabled?: boolean;
  onBusyChange?(busy: boolean): void;
  onSnapshotCreated?(snapshot: ReviewSnapshot, nextSessionVersion: number): void;
  onApplied?(execution: Execution): void;
  onRevisionRequired?(): void | Promise<void>;
  onNavigate?(path: string, execution: Execution): void;
  snapshotRecoveryNotice?: string;
};

const sourceLabels: Record<AgentFieldSource, string> = { USER: '用户填写', AI: 'AI 建议', SYSTEM: '系统默认', EXISTING: '已有拍品' };
const fieldLabels: Record<string, string> = { title: '拍品名称', startPriceAmount: '起拍价', durationSeconds: '竞拍时长', mainImageAssetId: '主图', minIncrementAmount: '最低加价', capPriceAmount: '封顶价', category: '分类', description: '介绍', tags: '标签', stock: '库存', antiSnipeWindowSeconds: '反狙击窗口', antiSnipeExtendSeconds: '每次延长', maxExtendCount: '延长次数', depositAmount: '保证金' };
const requiredLabels: Record<string, string> = { TITLE: '拍品名称', START_PRICE: '起拍价', DURATION: '竞拍时长', MAIN_IMAGE: '真实主图', AMBIGUOUS_CANDIDATE: '候选项归属' };

export function AgentReviewPanel({ session, candidates, activeSnapshot, recoveredExecution, onCreateSnapshot, onApply, disabled = false, onBusyChange, onSnapshotCreated, onApplied, onRevisionRequired, onNavigate = navigateToAuctions, snapshotRecoveryNotice = '' }: AgentReviewPanelProps) {
  const sorted = useMemo(() => [...candidates].sort((a, b) => a.position - b.position || a.candidate.id.localeCompare(b.candidate.id)), [candidates]);
  const candidateIDs = sorted.map((item) => item.candidate.id);
  const identityKey = candidateIDs.join('\u0000');
  const duplicateIDs = useMemo(() => candidateIDs.filter((id, index) => candidateIDs.indexOf(id) !== index), [identityKey]);
  const aggregateKey = aggregateSignature(session, sorted);
  const [orderedIDs, setOrderedIDs] = useState<string[]>(() => activeSnapshot?.candidates.map((item) => item.candidateId) ?? candidateIDs);
  const [selected, setSelected] = useState<Set<string>>(() => new Set(activeSnapshot?.candidates.map((item) => item.candidateId) ?? candidateIDs));
  const [snapshot, setSnapshot] = useState<ReviewSnapshot | undefined>(activeSnapshot);
  const [snapshotBaseline, setSnapshotBaseline] = useState(activeSnapshot ? aggregateKey : '');
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const [creating, setCreating] = useState(false);
  const [applying, setApplying] = useState(false);
  const [execution, setExecution] = useState<Execution | null>(() => recoveredExecution && recoveredExecutionMatchesCandidates(recoveredExecution, candidates) ? recoveredExecution : null);
  const knownIDs = useRef(new Set(candidateIDs));
  const previousAggregate = useRef(aggregateKey);
  const applyGate = useRef(false);
  const idempotency = useRef<{ signature: string; key: string } | null>(null);
  const currentAggregate = useRef(aggregateKey);
  const mounted = useRef(true);
  const operationGeneration = useRef(0);
  const currentSessionID = useRef(session.id);
  const successHeading = useRef<HTMLHeadingElement>(null);
  const busyNotified = useRef(false);
  const busyCallback = useRef(onBusyChange);
  busyCallback.current = onBusyChange;
  currentAggregate.current = aggregateKey;
  const byID = useMemo(() => new Map(sorted.map((item) => [item.candidate.id, item])), [sorted]);
  const selectedOrder = orderedIDs.filter((id) => selected.has(id) && byID.has(id));
  const writeBlocked = disabled || creating || applying || session.status !== 'READY_FOR_REVIEW' || duplicateIDs.length > 0;

  useEffect(() => {
    const available = new Set(candidateIDs);
    const newlySeen = candidateIDs.filter((id) => !knownIDs.current.has(id));
    setSelected((current) => new Set([...current].filter((id) => available.has(id)).concat(newlySeen)));
    setOrderedIDs((current) => current.filter((id) => available.has(id)).concat(newlySeen.filter((id) => !current.includes(id))));
    knownIDs.current = available;
    if (previousAggregate.current !== aggregateKey && snapshotBaseline && snapshotBaseline !== aggregateKey) invalidate('候选项或会话已变化，请重新生成确认预览。');
    previousAggregate.current = aggregateKey;
  // candidateIDs is represented by identityKey; using the array itself would reconcile every render.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [aggregateKey, identityKey, snapshotBaseline]);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false; operationGeneration.current += 1; applyGate.current = false;
      releaseBusy();
    };
  }, []);

  useEffect(() => {
    if (currentSessionID.current === session.id) return;
    currentSessionID.current = session.id; operationGeneration.current += 1; applyGate.current = false; idempotency.current = null;
    releaseBusy();
    knownIDs.current = new Set(candidateIDs); setSelected(new Set(candidateIDs)); setOrderedIDs(candidateIDs);
    setSnapshot(undefined); setSnapshotBaseline(''); setNotice('会话已切换，请重新生成确认预览。'); setError(''); setCreating(false); setApplying(false); setExecution(null);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [session.id]);

  useEffect(() => {
    if (!activeSnapshot) return;
    const order = activeSnapshot.candidates.map((item) => item.candidateId);
    if (session.activeSnapshotVersion === activeSnapshot.snapshotVersion && snapshotMatches(activeSnapshot, order, byID)) {
      setSelected(new Set(order)); setOrderedIDs(order.concat(candidateIDs.filter((id) => !order.includes(id))));
      setSnapshot(activeSnapshot); setSnapshotBaseline(aggregateKey); setNotice('已恢复当前不可变确认快照。'); setError('');
    } else {
      setSnapshot(undefined); setSnapshotBaseline(''); setNotice('已有确认快照无法安全恢复，请重新生成确认预览。');
    }
  // activeSnapshot is intentionally synchronized when recovery completes after the aggregate.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeSnapshot]);

  useEffect(() => {
    if (!recoveredExecution) return;
    if (session.status === 'APPLIED' && recoveredExecutionMatchesCandidates(recoveredExecution, candidates)) {
      setExecution(recoveredExecution); setSnapshot(undefined); setSnapshotBaseline(''); setNotice(recoveredExecution.target === 'INVENTORY_ONLY' ? '已恢复成功创建的待开拍拍品。' : '已恢复成功入队结果。拍品不会自动开拍。'); setError('');
    } else {
      setExecution(null); setError('已应用结果无法安全恢复，请刷新后核对拍品队列。');
    }
  }, [candidates, recoveredExecution, session.status]);

  useEffect(() => { if (execution) successHeading.current?.focus(); }, [execution]);

  function invalidate(message: string) {
    if (snapshot || snapshotBaseline) { setSnapshot(undefined); setSnapshotBaseline(''); setNotice(message); setExecution(null); }
  }

  function toggle(id: string) {
    if (writeBlocked) return;
    setSelected((current) => { const next = new Set(current); if (next.has(id)) next.delete(id); else next.add(id); return next; });
    invalidate('选择已变化，预览已失效，请重新生成。'); setError('');
  }

  function move(id: string, direction: -1 | 1) {
    if (writeBlocked) return;
    const current = selectedOrder; const index = current.indexOf(id); const swap = index + direction;
    if (index < 0 || swap < 0 || swap >= current.length) return;
    const nextSelected = [...current]; [nextSelected[index], nextSelected[swap]] = [nextSelected[swap], nextSelected[index]];
    const unselected = orderedIDs.filter((candidateID) => !selected.has(candidateID));
    setOrderedIDs([...nextSelected, ...unselected]); invalidate('顺序已变化，预览已失效，请重新生成。'); setError('');
  }

  async function createSnapshot() {
    if (writeBlocked) return;
    if (duplicateIDs.length) { fail('候选项 ID 重复，无法安全生成确认预览。'); return; }
    if (!selectedOrder.length) { fail('至少选择一件拍品才能生成确认预览。', candidateIDs[0]); return; }
    const invalid = selectedOrder.map((id) => byID.get(id)).find((item) => item && validationOf(item).invalid);
    if (invalid) { fail(`${invalid.candidate.draft.title || '未命名拍品'}还不能确认，请先补齐必填项并修正校验错误。`, invalid.candidate.id); return; }
    const operation = ++operationGeneration.current; const operationSessionID = session.id;
    setCreating(true); notifyBusy(true); setError(''); setNotice(''); setExecution(null);
    try {
      const input = { expectedSessionVersion: session.version, selectedCandidateIds: [...selectedOrder], orderedCandidateIds: [...selectedOrder] };
      const next = await onCreateSnapshot(input);
      if (!operationIsCurrent(operation, operationSessionID)) return;
      if (currentAggregate.current !== aggregateKey) throw new Error('候选项已变化，请重新生成确认预览');
      if (!snapshotMatches(next, selectedOrder, byID)) throw new Error('确认预览与当前候选项不一致');
      const nextBaseline = onSnapshotCreated ? aggregateSignature({ ...session, version: session.version + 1, activeSnapshotVersion: next.snapshotVersion }, sorted) : aggregateKey;
      setSnapshot(next); setSnapshotBaseline(nextBaseline); setNotice('确认预览已生成。请核对不可变快照后创建拍品。');
      onSnapshotCreated?.(next, session.version + 1);
      idempotency.current = null;
    } catch (cause) { if (operationIsCurrent(operation, operationSessionID)) setError(safeError(cause, '确认预览生成失败，请检查拍品信息后重试。')); }
    finally { if (operationIsCurrent(operation, operationSessionID)) { setCreating(false); notifyBusy(false); } }
  }

  async function applySnapshot(target: ApplyTarget) {
    if (!snapshot || writeBlocked || applyGate.current) return;
    if (snapshotBaseline !== aggregateKey || !snapshotMatches(snapshot, selectedOrder, byID)) { invalidate('候选项或会话已变化，请重新生成确认预览。'); return; }
    const operation = ++operationGeneration.current; const operationSessionID = session.id;
    if (target === 'INVENTORY_ONLY' && snapshot.candidates.some((candidate) => candidate.sourceType !== 'NEW')) { setError('当前确认流程只创建新拍品；已有待开拍拍品无需重复创建。'); return; }
    applyGate.current = true; setApplying(true); notifyBusy(true); setError('');
    const signature = `${session.id}\u0000${snapshot.snapshotVersion}\u0000${snapshot.contentHash}\u0000${target}\u0000${selectedOrder.join('\u0000')}`;
    if (!idempotency.current || idempotency.current.signature !== signature) idempotency.current = { signature, key: crypto.randomUUID() };
    try {
      const result = await onApply({ snapshotVersion: snapshot.snapshotVersion, contentHash: snapshot.contentHash, selectedCandidateIds: [...selectedOrder], orderedCandidateIds: [...selectedOrder], idempotencyKey: idempotency.current.key, target });
      if (!operationIsCurrent(operation, operationSessionID)) return;
      if (!executionMatchesSnapshot(snapshot, result)) { setError('服务端返回的入队结果与确认快照不一致。请先刷新队列核对；再次确认会复用同一幂等键。'); return; }
      setExecution(result); setNotice(target === 'INVENTORY_ONLY' ? '拍品已创建，当前为待开拍状态。' : '已成功加入队列。拍品不会自动开拍，你可以前往队列手动开拍。'); onApplied?.(result);
    } catch (cause) {
      if (!operationIsCurrent(operation, operationSessionID)) return;
      const message = safeError(cause, '确认结果暂时未知，可使用同一请求安全重试。'); setError(message);
      if (/stale|changed|revision|快照|候选|版本|需要修改/i.test(message)) { invalidate('服务端拍品信息已变化，请返回编辑并重新生成确认预览。'); await onRevisionRequired?.(); }
    } finally { if (operationIsCurrent(operation, operationSessionID)) { applyGate.current = false; setApplying(false); notifyBusy(false); } }
  }

  function operationIsCurrent(operation: number, operationSessionID: string) { return mounted.current && operationGeneration.current === operation && currentSessionID.current === operationSessionID; }
  function notifyBusy(next: boolean) { if (busyNotified.current === next) return; busyNotified.current = next; busyCallback.current?.(next); }
  function releaseBusy() { notifyBusy(false); }
  function fail(message: string, candidateID?: string) { setError(message); document.getElementById(`agent-review-select-${safeID(candidateID ?? '')}`)?.focus(); }

  return <StudioCard padding="lg" className="agentReviewPanel">
    <header className="agentReviewHeader"><div><span>Final review</span><h2>确认创建拍品</h2><p>确认后创建为待开拍拍品，可继续用于直播排品；最终确认前不会创建真实拍品。</p></div><StudioBadge tone={snapshot ? 'success' : 'neutral'}>{snapshot ? '预览已锁定' : `${selectedOrder.length} / ${sorted.length} 已选择`}</StudioBadge></header>
    {snapshotRecoveryNotice ? <div className="agentReviewNotice" role="status"><FileCheck2 size={18} /><span>{snapshotRecoveryNotice}</span></div> : null}
    {notice ? <div className="agentReviewNotice" role="status" aria-live="polite"><FileCheck2 size={18} /><span>{notice}</span></div> : null}
    {duplicateIDs.length ? <div className="agentReviewError" role="alert"><AlertTriangle size={18} /><span>候选项 ID 重复，无法安全确认，请刷新会话。</span></div> : error ? <div className="agentReviewError" role="alert"><AlertTriangle size={18} /><span>{error}</span></div> : null}
    <ol className="agentReviewList" aria-label="待确认拍品及批内顺序">
      {orderedIDs.map((id) => { const item = byID.get(id); if (!item) return null; const title = item.candidate.draft.title || '未命名拍品'; const isSelected = selected.has(id); const selectedIndex = selectedOrder.indexOf(id); const validation = validationOf(item); const sources = sourcesOf(item); const aiCount = Object.values(sources).filter((source) => source === 'AI').length;
        return <li key={id} className={!isSelected ? 'agentReviewItem agentReviewItem-unselected' : 'agentReviewItem'}>
          <label className="agentReviewSelect"><input id={`agent-review-select-${safeID(id)}`} type="checkbox" checked={isSelected} disabled={writeBlocked} aria-label={`选择${title}`} aria-describedby={`agent-review-detail-${safeID(id)}`} onChange={() => toggle(id)} /><span><strong>{title}</strong><small>{isSelected ? '已选择' : '未选择'}</small></span></label>
          <div className="agentReviewIdentity"><StudioBadge tone={item.candidate.sourceType === 'NEW' ? 'info' : 'neutral'}>{item.candidate.sourceType}</StudioBadge><span>候选版本 {item.version}</span>{item.candidate.sourceType === 'EXISTING' ? <span>库存读取版本 {item.candidate.existingLotReadVersion}</span> : <span>确认后创建真实拍品</span>}</div>
          <div id={`agent-review-detail-${safeID(id)}`} className="agentReviewDetails">
            <p><strong>来源摘要：</strong>{sourceSummary(sources)}</p>
            <p><strong>风险提示：</strong>{aiCount ? `AI 建议字段 ${aiCount} 项，确认前请人工复核` : '没有 AI 建议字段'}；{hasRequiredImage(item) ? '主图已绑定' : '缺少真实主图'}；{item.candidate.sourceType === 'EXISTING' ? `将按库存版本 ${item.candidate.existingLotReadVersion ?? '未知'} 重新校验` : '将创建新拍品'}</p>
            <p className={validation.invalid ? 'agentReviewValidation agentReviewValidation-error' : 'agentReviewValidation'}>{validation.invalid ? <ShieldAlert size={15} /> : <CheckCircle2 size={15} />}{validation.missing.length ? `缺少：${validation.missing.map((value) => requiredLabels[value] ?? value).join('、')}` : '必填项完整'}；{validation.errors.length ? `校验错误：${validation.errors.join('；')}` : '校验通过'}</p>
          </div>
          <div className="agentReviewOrder" aria-label={`${title}批内排序`}><StudioButton type="button" variant="ghost" icon={<ArrowUp size={16} />} disabled={writeBlocked || !isSelected || selectedIndex <= 0} aria-label={`上移${title}`} onClick={() => move(id, -1)}>上移</StudioButton><StudioButton type="button" variant="ghost" icon={<ArrowDown size={16} />} disabled={writeBlocked || !isSelected || selectedIndex < 0 || selectedIndex === selectedOrder.length - 1} aria-label={`下移${title}`} onClick={() => move(id, 1)}>下移</StudioButton></div>
        </li>; })}
    </ol>
    {snapshot ? <SnapshotReview snapshot={snapshot} /> : null}
    {execution ? <section className="agentApplySuccess" role="status" aria-live="polite"><h3 ref={successHeading} tabIndex={-1}>{execution.target === 'INVENTORY_ONLY' ? '拍品创建成功' : '已成功加入队列'}</h3><p>执行 ID：{execution.executionId}</p>{execution.createdLotIds.length ? <p>新建拍品 ID：{execution.createdLotIds.join('、')}</p> : <p>本批次未创建新拍品</p>}{execution.queuedLots.length ? <ul>{execution.queuedLots.map((lot) => <li key={lot.lotId}>{lot.lotId} · 队列位置 {lot.queuePosition}</li>)}</ul> : <p>当前状态：待开拍，可用于后续直播排品。</p>}<StudioButton type="button" variant="primary" onClick={() => onNavigate('/admin/auctions', execution)}>{execution.target === 'INVENTORY_ONLY' ? '查看待开拍拍品' : '前往拍品队列'}</StudioButton></section> : null}
    {!execution ? <footer className="agentReviewActions"><p>{snapshot ? error ? '上次确认未得到成功结果；再次确认会复用对应操作的幂等键。' : '确认后统一创建为待开拍状态，不会直接加入有序直播队列。' : '选择或排序变化后必须重新生成预览。'}</p>{snapshot ? <div className="agentReviewActionButtons"><StudioButton type="button" variant="primary" loading={applying} disabled={writeBlocked} onClick={() => void applySnapshot(snapshot.candidates.every((candidate) => candidate.sourceType === 'NEW') ? 'INVENTORY_ONLY' : 'QUEUE')}>确认创建（{selectedOrder.length} 件）</StudioButton></div> : <StudioButton type="button" variant="primary" loading={creating} disabled={writeBlocked} onClick={() => void createSnapshot()}>生成确认预览（{selectedOrder.length} 件）</StudioButton>}</footer> : null}
  </StudioCard>;
}

function SnapshotReview({ snapshot }: { snapshot: ReviewSnapshot }) {
  return <section className="agentSnapshotSummary" aria-labelledby="agent-snapshot-title">
    <header><div><span>Immutable snapshot</span><h3 id="agent-snapshot-title">确认快照</h3></div><strong>快照版本 {snapshot.snapshotVersion}</strong></header>
    <dl className="agentSnapshotMeta"><div><dt>内容哈希</dt><dd>{snapshot.contentHash}</dd></div><div><dt>拍品数量</dt><dd>{snapshot.candidates.length} 件</dd></div></dl>
    <ol className="agentSnapshotCandidates" aria-label="不可变快照拍品明细">
      {snapshot.candidates.map((candidate, index) => {
        const sources = new Map<string, AgentFieldSource>(candidate.fieldSources.map((item) => [item.field, item.source]));
        const aiCount = candidate.fieldSources.filter((item) => item.source === 'AI').length;
        const unknownCount = candidate.fieldSources.filter((item) => !sourceLabels[item.source]).length;
        return <li key={candidate.candidateId} className="agentSnapshotCandidate">
          <header><div><strong>{index + 1}. {candidate.draft.title}</strong><StudioBadge tone={candidate.sourceType === 'NEW' ? 'info' : 'neutral'}>{candidate.sourceType}</StudioBadge></div><span>候选 ID：{candidate.candidateId} · 候选版本：{candidate.candidateVersion}</span></header>
          <p className="agentSnapshotIdentity">{candidate.sourceType === 'EXISTING' ? `已有拍品 ID：${candidate.existingLotId} · 库存读取版本：${candidate.existingLotReadVersion}` : '确认后创建真实拍品'} · 主图资产：{candidate.draft.mainImageAssetId || '沿用已有拍品主图'} · 资产 ID：{candidate.assetIds.length ? candidate.assetIds.join('、') : '无新增资产'}</p>
          <dl className="agentSnapshotFields">
            <div><dt>起拍价</dt><dd>¥{formatSnapshotCents(candidate.draft.startPriceAmount)}</dd></div><div><dt>最低加价</dt><dd>¥{formatSnapshotCents(candidate.draft.minIncrementAmount)}</dd></div><div><dt>封顶价</dt><dd>{candidate.draft.capPriceAmount == null ? '不封顶' : `¥${formatSnapshotCents(candidate.draft.capPriceAmount)}`}</dd></div>
            <div><dt>竞拍时长</dt><dd>{candidate.draft.durationSeconds} 秒</dd></div><div><dt>分类</dt><dd>{candidate.draft.category || '未填写'}</dd></div><div><dt>库存</dt><dd>{candidate.draft.stock} 件</dd></div>
            <div><dt>标签</dt><dd>{candidate.draft.tags.length ? candidate.draft.tags.join('、') : '无'}</dd></div><div><dt>保证金</dt><dd>¥{formatSnapshotCents(candidate.draft.depositAmount)}</dd></div><div><dt>反狙击</dt><dd>窗口 {candidate.draft.antiSnipeWindowSeconds} 秒 / 延长 {candidate.draft.antiSnipeExtendSeconds} 秒 / 最多 {candidate.draft.maxExtendCount} 次</dd></div>
            <div className="agentSnapshotFieldWide"><dt>介绍</dt><dd>{candidate.draft.description || '未填写'}</dd></div>
          </dl>
          <p><strong>完整来源：</strong>{approvalFields.map((field) => `${fieldLabels[field]}：${sourceLabels[sources.get(field)!] ?? '来源未知'}`).join('；')}</p>
          <p><strong>风险提示：</strong>{aiCount ? `AI 建议字段 ${aiCount} 项，需人工复核` : '无 AI 建议字段'}；{unknownCount ? `来源未知字段 ${unknownCount} 项` : '字段来源完整'}；{candidate.sourceType === 'EXISTING' ? `应用时重新校验库存版本 ${candidate.existingLotReadVersion}` : '将创建新拍品'}</p>
        </li>;
      })}
    </ol>
  </section>;
}

function validationOf(item: AgentDraftCandidate) { try { const parsed = JSON.parse(item.validationJSON) as { requiredInputs?: string[]; errors?: string[] }; const missing = Array.isArray(parsed.requiredInputs) ? parsed.requiredInputs : []; const errors = Array.isArray(parsed.errors) ? parsed.errors : []; return { missing, errors, invalid: missing.length > 0 || errors.length > 0 || !hasRequiredImage(item) }; } catch { return { missing: [], errors: ['校验状态无法读取'], invalid: true }; } }
function sourcesOf(item: AgentDraftCandidate) { try { return JSON.parse(item.fieldSourcesJSON) as Partial<Record<string, AgentFieldSource>>; } catch { return {}; } }
function sourceSummary(sources: Partial<Record<string, AgentFieldSource>>) { const entries = Object.entries(fieldLabels).map(([field, label]) => `${label}：${sources[field] ? sourceLabels[sources[field]!] ?? '来源未知' : '来源未知'}`); return entries.join('；'); }
function hasRequiredImage(item: AgentDraftCandidate) { return item.candidate.sourceType === 'NEW' ? Boolean(item.candidate.draft.mainImageAssetId) : Boolean(item.candidate.draft.mainImageUrl || item.candidate.draft.mainImageAssetId); }
function aggregateSignature(session: AgentSession, candidates: AgentDraftCandidate[]) { return JSON.stringify({ session: [session.id, session.version, session.status, session.activeSnapshotVersion], candidates: candidates.map((item) => [item.candidate.id, item.version, item.candidate.sourceType, item.candidate.existingLotId, item.candidate.existingLotReadVersion, item.candidate.draft, canonicalCurrentSources(item)]) }); }
function safeID(value: string) { return value.replace(/[^a-zA-Z0-9_-]/g, '-'); }
const approvalFields = Object.keys(fieldLabels).sort();
function normalizedSource(source: AgentFieldSource | undefined) { return source; }
function canonicalCurrentSources(item: AgentDraftCandidate) {
  const sources = sourcesOf(item);
  return approvalFields.map((field) => [field, normalizedSource(sources[field]) ?? (item.provenanceVersion === 0 ? legacyOptionalDefaultSource(item.candidate.draft, field) : undefined) ?? 'UNKNOWN']);
}
function legacyOptionalDefaultSource(draft: AgentDraftCandidate['candidate']['draft'], field: string) {
  if (field === 'capPriceAmount' && draft.capPriceAmount == null) return 'AI';
  if (field === 'category' && !draft.category.trim()) return 'AI';
  if (field === 'description' && !draft.description.trim()) return 'AI';
  if (field === 'tags' && draft.tags.length === 0) return 'AI';
  if (field === 'depositAmount' && draft.depositAmount === 0) return 'AI';
  return undefined;
}
function canonicalSnapshotSources(fields: ReviewSnapshot['candidates'][number]['fieldSources']) {
  const seen = new Set<string>(); const mapped = new Map<string, AgentFieldSource>();
  for (const entry of fields) { if (seen.has(entry.field)) return null; seen.add(entry.field); mapped.set(entry.field, normalizedSource(entry.source) ?? entry.source); }
  if (seen.size !== approvalFields.length || approvalFields.some((field) => !seen.has(field))) return null;
  return approvalFields.map((field) => [field, mapped.get(field)]);
}
function currentSnapshotDraft(item: AgentDraftCandidate) { const { mainImageUrl: _mainImageUrl, ...draft } = item.candidate.draft; return draft; }
function snapshotMatches(snapshot: ReviewSnapshot, selectedOrder: string[], byID: Map<string, AgentDraftCandidate>) {
  if (snapshot.candidates.length !== selectedOrder.length || new Set(selectedOrder).size !== selectedOrder.length) return false;
  return snapshot.candidates.every((candidate, index) => {
    const current = byID.get(candidate.candidateId); const snapshotSources = canonicalSnapshotSources(candidate.fieldSources);
    const expectedAssets = current?.candidate.sourceType === 'NEW' && current.candidate.draft.mainImageAssetId ? [current.candidate.draft.mainImageAssetId] : [];
    return Boolean(current) && candidate.candidateId === selectedOrder[index] && current!.version === candidate.candidateVersion
      && current!.candidate.sourceType === candidate.sourceType && current!.candidate.existingLotId === candidate.existingLotId
      && current!.candidate.existingLotReadVersion === candidate.existingLotReadVersion
      && JSON.stringify(currentSnapshotDraft(current!)) === JSON.stringify(candidate.draft)
      && JSON.stringify(expectedAssets) === JSON.stringify(candidate.assetIds)
      && snapshotSources !== null && JSON.stringify(canonicalCurrentSources(current!)) === JSON.stringify(snapshotSources);
  });
}
function formatSnapshotCents(cents: number) { return `${Math.floor(cents / 100)}.${String(cents % 100).padStart(2, '0')}`; }
export function executionMatchesSnapshot(snapshot: ReviewSnapshot, execution: Execution) {
  if (execution.createdLotIds.length !== snapshot.candidates.filter((item) => item.sourceType === 'NEW').length) return false;
  if (execution.target === 'INVENTORY_ONLY') return execution.queuedLots.length === 0 && snapshot.candidates.every((item) => item.sourceType === 'NEW');
  if (execution.queuedLots.length !== snapshot.candidates.length) return false;
  const queuedIDs = execution.queuedLots.map((item) => item.lotId); const positions = execution.queuedLots.map((item) => item.queuePosition);
  if (new Set(queuedIDs).size !== queuedIDs.length || new Set(execution.createdLotIds).size !== execution.createdLotIds.length || positions.some((position) => !Number.isSafeInteger(position) || position <= 0)) return false;
  if (positions.some((position, index) => index > 0 && position !== positions[index - 1] + 1)) return false;
  const newQueuedIDs: string[] = [];
  for (let index = 0; index < snapshot.candidates.length; index += 1) {
    const candidate = snapshot.candidates[index]; const queuedID = queuedIDs[index];
    if (candidate.sourceType === 'EXISTING') { if (!candidate.existingLotId || queuedID !== candidate.existingLotId) return false; }
    else newQueuedIDs.push(queuedID);
  }
  return newQueuedIDs.every((id, index) => id === execution.createdLotIds[index]);
}
export function recoveredExecutionMatchesCandidates(execution: RecoveredExecution, candidates: AgentDraftCandidate[]) {
  if (execution.target === 'INVENTORY_ONLY') return execution.queuedLots.length === 0 && execution.createdLotIds.length > 0 && execution.createdLotIds.length <= candidates.filter((candidate) => candidate.candidate.sourceType === 'NEW').length;
  if (!execution.queuedLots.length || execution.queuedLots.length > candidates.length) return false;
  const byID = new Map(candidates.map((candidate) => [candidate.candidate.id, candidate]));
  return execution.queuedLots.every((queued) => {
    const candidate = byID.get(queued.candidateId);
    return Boolean(candidate) && candidate!.candidate.sourceType === queued.sourceType
      && (queued.sourceType === 'NEW' || candidate!.candidate.existingLotId === queued.lotId);
  });
}
function safeError(cause: unknown, fallback: string) { return cause instanceof HttpError || cause instanceof ApiResultError || cause instanceof AuthExpiredError ? cause.message : fallback; }
function navigateToAuctions(path: string, _execution: Execution) { location.href = path; }
