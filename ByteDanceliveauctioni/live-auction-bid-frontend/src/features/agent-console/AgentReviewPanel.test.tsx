import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { StrictMode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
// @ts-expect-error Vitest runs with Node; production TypeScript intentionally omits Node globals.
import { readFileSync } from 'node:fs';
// @ts-expect-error Vitest runs with Node; production TypeScript intentionally omits Node globals.
import { resolve } from 'node:path';
import { AgentReviewPanel, executionMatchesSnapshot } from './AgentReviewPanel';
import type { AgentDraftCandidate, AgentSession, Execution, ReviewSnapshot } from './model/types';
const testProcess = globalThis as typeof globalThis & { process: { cwd(): string } };
const agentConsoleCSS = readFileSync(resolve(testProcess.process.cwd(), 'src/features/agent-console/agent-console.css'), 'utf8');

const session: AgentSession = { id: 'session-1', roomId: 'room-1', status: 'READY_FOR_REVIEW', version: 7, activeSnapshotVersion: 0, conversationSummary: '', createdAtUnixMs: 1, updatedAtUnixMs: 2 };
const sourceFields = ['antiSnipeExtendSeconds', 'antiSnipeWindowSeconds', 'capPriceAmount', 'category', 'depositAmount', 'description', 'durationSeconds', 'mainImageAssetId', 'maxExtendCount', 'minIncrementAmount', 'startPriceAmount', 'stock', 'tags', 'title'];
const candidates = [
  candidate('new-1', '海洋之心', 'NEW', 1, { title: 'USER', startPriceAmount: 'USER', durationSeconds: 'USER', mainImageAssetId: 'USER', description: 'AI' }),
  candidate('existing-1', '典藏腕表', 'EXISTING', 2, { title: 'EXISTING', startPriceAmount: 'EXISTING', durationSeconds: 'AI', mainImageAssetId: 'EXISTING' }),
  candidate('new-2', '青花瓷瓶', 'NEW', 3, { title: 'AI', startPriceAmount: 'USER', durationSeconds: 'SYSTEM', mainImageAssetId: 'USER' }),
];

describe('AgentReviewPanel immutable confirmation', () => {
  beforeEach(() => { history.replaceState(null, '', '/admin/agent?session=session-1'); vi.useRealTimers(); });

  it('defaults every candidate selected and exposes type, risk, validation and provenance as text', () => {
    renderPanel();
    expect(screen.getAllByRole('checkbox')).toHaveLength(3);
    screen.getAllByRole('checkbox').forEach((box) => expect(box).toBeChecked());
    expect(screen.getAllByText('NEW')).toHaveLength(2);
    expect(screen.getByText('EXISTING')).toBeInTheDocument();
    expect(screen.getByText((_content, node) => node?.tagName === 'P' && Boolean(node.textContent?.startsWith('来源摘要：拍品名称：用户填写')))).toBeInTheDocument();
    expect(screen.getAllByText((_content, node) => node?.tagName === 'P' && Boolean(node.textContent?.startsWith('风险提示：AI 建议字段 1 项')))).toHaveLength(3);
    expect(screen.getAllByText((_content, node) => node?.tagName === 'P' && Boolean(node.textContent?.endsWith('校验通过')))).toHaveLength(3);
    expect(screen.getByRole('button', { name: '生成确认预览（3 件）' })).toBeEnabled();
    expect(screen.getByRole('heading', { name: '确认创建拍品' }).closest('article')).toHaveClass('agentReviewPanel');
  });

  it('deselects candidates, preserves exact set equality, and reorders with accessible disabled controls', async () => {
    const user = userEvent.setup(); const create = vi.fn().mockResolvedValue(snapshot(['new-2', 'new-1']));
    renderPanel({ onCreateSnapshot: create });
    await user.click(screen.getByRole('checkbox', { name: /选择典藏腕表/ }));
    expect(screen.getByRole('button', { name: '上移海洋之心' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '下移青花瓷瓶' })).toBeDisabled();
    screen.getByRole('button', { name: '上移青花瓷瓶' }).focus();
    await user.keyboard('{Enter}');
    await user.click(screen.getByRole('button', { name: '生成确认预览（2 件）' }));
    expect(create).toHaveBeenCalledWith({ expectedSessionVersion: 7, selectedCandidateIds: ['new-2', 'new-1'], orderedCandidateIds: ['new-2', 'new-1'] });
    expect(screen.getByText('快照版本 4')).toBeInTheDocument();
    expect(screen.getByText(/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/)).toBeInTheDocument();
    expect(screen.getByText((_content, node) => node?.tagName === 'STRONG' && node.textContent === '1. 青花瓷瓶')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '确认创建（2 件）' })).toBeEnabled();
  });

  it('renders every immutable approval field and identity from the snapshot with exact units', async () => {
    renderPanel(); await userEvent.click(screen.getByRole('button', { name: '生成确认预览（3 件）' }));
    const review = await screen.findByRole('list', { name: '不可变快照拍品明细' });
    expect(review).toHaveTextContent('候选 ID：new-1'); expect(review).toHaveTextContent('候选版本：3'); expect(review).toHaveTextContent('主图资产：asset-new-1');
    expect(review).toHaveTextContent('起拍价¥100.00'); expect(review).toHaveTextContent('最低加价¥1.00'); expect(review).toHaveTextContent('封顶价¥200.00');
    expect(review).toHaveTextContent('竞拍时长600 秒'); expect(review).toHaveTextContent('反狙击窗口 10 秒 / 延长 15 秒 / 最多 5 次'); expect(review).toHaveTextContent('保证金¥0.00');
    expect(review).toHaveTextContent('已有拍品 ID：lot-existing-1'); expect(review).toHaveTextContent('库存读取版本：2'); expect(review).toHaveTextContent('介绍介绍'); expect(review).toHaveTextContent('标签精选');
  });

  it('requires at least one valid candidate, links errors and focuses the first invalid choice', async () => {
    const user = userEvent.setup(); const invalid = [candidate('bad', '缺图拍品', 'NEW', 1, {}, { requiredInputs: ['MAIN_IMAGE'], errors: ['最低加价必须大于 0'] })];
    renderPanel({ candidates: invalid });
    await user.click(screen.getByRole('checkbox', { name: /选择缺图拍品/ }));
    await user.click(screen.getByRole('button', { name: '生成确认预览（0 件）' }));
    expect(screen.getByRole('alert')).toHaveTextContent('至少选择一件拍品');
    expect(screen.getByRole('checkbox', { name: /选择缺图拍品/ })).toHaveFocus();
    await user.click(screen.getByRole('checkbox', { name: /选择缺图拍品/ }));
    await user.click(screen.getByRole('button', { name: '生成确认预览（1 件）' }));
    expect(screen.getByRole('alert')).toHaveTextContent('缺图拍品还不能确认');
    expect(screen.getByText(/缺少：真实主图/)).toBeInTheDocument();
    expect(screen.getByText(/校验错误：最低加价必须大于 0/)).toBeInTheDocument();
  });

  it('invalidates preview when selection, order, session, candidate identity or image changes', async () => {
    const user = userEvent.setup(); const { rerender } = renderPanel({ onCreateSnapshot: vi.fn(async (input) => snapshot(input.orderedCandidateIds)) });
    await user.click(screen.getByRole('button', { name: '生成确认预览（3 件）' }));
    expect(screen.getByRole('button', { name: '确认创建（3 件）' })).toBeEnabled();
    await user.click(screen.getByRole('checkbox', { name: /选择青花瓷瓶/ }));
    expect(screen.queryByRole('button', { name: /确认创建/ })).not.toBeInTheDocument();
    expect(screen.getByText(/预览已失效/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '生成确认预览（2 件）' }));
    rerender(<AgentReviewPanel session={{ ...session, version: 8 }} candidates={candidates.map((item) => item.candidate.id === 'new-1' ? { ...item, version: 4, candidate: { ...item.candidate, draft: { ...item.candidate.draft, mainImageAssetId: 'asset-replaced' } } } : item)} onCreateSnapshot={vi.fn().mockResolvedValue(snapshot(['new-1', 'existing-1']))} onApply={vi.fn()} />);
    expect(screen.queryByRole('button', { name: /确认创建/ })).not.toBeInTheDocument();
    expect(screen.getByText(/候选项或会话已变化/)).toBeInTheDocument();
  });

  it('rejects a recovered snapshot when any approval draft field, asset or canonical provenance differs', () => {
    const changedDraft = candidates.map((item) => item.candidate.id === 'new-1' ? { ...item, candidate: { ...item.candidate, draft: { ...item.candidate.draft, description: '已被会话修改' } } } : item);
    renderPanel({ session: { ...session, activeSnapshotVersion: 4 }, candidates: changedDraft, activeSnapshot: snapshot(candidates.map((item) => item.candidate.id)) });
    expect(screen.getByText('已有确认快照无法安全恢复，请重新生成确认预览。')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /确认创建（/ })).not.toBeInTheDocument();

    const changedSource = candidates.map((item) => item.candidate.id === 'new-1' ? { ...item, fieldSourcesJSON: item.fieldSourcesJSON.replace('"description":"AI"', '"description":"USER"') } : item);
    renderPanel({ session: { ...session, activeSnapshotVersion: 4 }, candidates: changedSource, activeSnapshot: snapshot(candidates.map((item) => item.candidate.id)) });
    expect(screen.getAllByText('已有确认快照无法安全恢复，请重新生成确认预览。')).toHaveLength(2);
  });

  it('preserves retained deselections and stable order while safely reconciling additions, removals and duplicates', async () => {
    const user = userEvent.setup(); const { rerender } = renderPanel();
    await user.click(screen.getByRole('checkbox', { name: '选择典藏腕表' }));
    screen.getByRole('button', { name: '上移青花瓷瓶' }).focus(); await user.keyboard('{Enter}');
    const added = candidate('new-3', '新到玉佩', 'NEW', 1, { title: 'USER' });
    rerender(<AgentReviewPanel session={{ ...session, version: 8 }} candidates={[added, candidates[2], candidates[1]]} onCreateSnapshot={vi.fn()} onApply={vi.fn()} />);
    expect(screen.getByRole('checkbox', { name: '选择典藏腕表' })).not.toBeChecked();
    expect(screen.getByRole('checkbox', { name: '选择新到玉佩' })).toBeChecked();
    const labels = screen.getAllByRole('checkbox').map((box) => box.getAttribute('aria-label'));
    expect(labels).toEqual(['选择青花瓷瓶', '选择典藏腕表', '选择新到玉佩']);
    const duplicate = { ...added, position: 9 };
    rerender(<AgentReviewPanel session={{ ...session, version: 9 }} candidates={[added, duplicate]} onCreateSnapshot={vi.fn()} onApply={vi.fn()} />);
    expect(screen.getByRole('alert')).toHaveTextContent('候选项 ID 重复');
    expect(screen.getByRole('button', { name: '生成确认预览（1 件）' })).toBeDisabled();
  });

  it('ignores deferred create/apply completions after unmount or session identity change', async () => {
    const createPending = deferred<ReviewSnapshot>(); const applyPending = deferred<Execution>(); const snapshotCreated = vi.fn(); const navigate = vi.fn(); const busy = vi.fn();
    const createView = renderPanel({ onCreateSnapshot: () => createPending.promise, onSnapshotCreated: snapshotCreated, onBusyChange: busy });
    await userEvent.click(screen.getByRole('button', { name: '生成确认预览（3 件）' }));
    createView.unmount(); createPending.resolve(snapshot(candidates.map((item) => item.candidate.id)));
    await Promise.resolve();
    expect(snapshotCreated).not.toHaveBeenCalled(); expect(busy).toHaveBeenLastCalledWith(false);

    const applyView = renderPanel({ onApply: () => applyPending.promise, onNavigate: navigate, onBusyChange: busy });
    await userEvent.click(screen.getByRole('button', { name: '生成确认预览（3 件）' }));
    await userEvent.click(screen.getByRole('button', { name: '确认创建（3 件）' }));
    applyView.rerender(<AgentReviewPanel session={{ ...session, id: 'session-2' }} candidates={candidates} onCreateSnapshot={vi.fn()} onApply={vi.fn()} onNavigate={navigate} onBusyChange={busy} />);
    applyPending.resolve(execution); await Promise.resolve();
    expect(navigate).not.toHaveBeenCalled();
    expect(screen.queryByRole('heading', { name: '已成功加入队列' })).not.toBeInTheDocument();
  });

  it('remains live under StrictMode and installs deferred create/apply results exactly once', async () => {
    const createPending = deferred<ReviewSnapshot>(); const create = vi.fn(() => createPending.promise); const created = vi.fn(); const busy = vi.fn();
    const props: React.ComponentProps<typeof AgentReviewPanel> = { session, candidates, onCreateSnapshot: create, onApply: vi.fn().mockResolvedValue(execution), onSnapshotCreated: created, onBusyChange: busy };
    const view = render(<StrictMode><AgentReviewPanel {...props} /></StrictMode>);
    await userEvent.click(screen.getByRole('button', { name: '生成确认预览（3 件）' })); expect(create).toHaveBeenCalledTimes(1);
    createPending.resolve(snapshot(candidates.map((item) => item.candidate.id))); expect(await screen.findByRole('button', { name: '确认创建（3 件）' })).toBeEnabled();
    expect(created).toHaveBeenCalledTimes(1); expect(busy).toHaveBeenLastCalledWith(false); view.unmount();

    const applyPending = deferred<Execution>(); const apply = vi.fn(() => applyPending.promise); const applyProps = { ...props, onSnapshotCreated: undefined, onCreateSnapshot: vi.fn().mockResolvedValue(snapshot(candidates.map((item) => item.candidate.id))), onApply: apply };
    render(<StrictMode><AgentReviewPanel {...applyProps} /></StrictMode>); await userEvent.click(screen.getByRole('button', { name: '生成确认预览（3 件）' }));
    await userEvent.click(await screen.findByRole('button', { name: '确认创建（3 件）' })); expect(apply).toHaveBeenCalledTimes(1);
    applyPending.resolve(execution); expect(await screen.findByRole('heading', { name: '已成功加入队列' })).toBeInTheDocument(); expect(apply).toHaveBeenCalledTimes(1); expect(busy).toHaveBeenLastCalledWith(false);
  });

  it('atomically releases create busy on session switch, ignores stale completion, and tracks the new create independently', async () => {
    const oldPending = deferred<ReviewSnapshot>(); const nextPending = deferred<ReviewSnapshot>(); const busy = vi.fn(); const oldCreated = vi.fn(); const nextCreated = vi.fn();
    const view = renderPanel({ onCreateSnapshot: () => oldPending.promise, onSnapshotCreated: oldCreated, onBusyChange: busy });
    await userEvent.click(screen.getByRole('button', { name: '生成确认预览（3 件）' })); expect(busy.mock.calls.map(([value]) => value)).toEqual([true]);
    view.rerender(<AgentReviewPanel session={{ ...session, id: 'session-2' }} candidates={candidates} onCreateSnapshot={() => nextPending.promise} onApply={vi.fn()} onSnapshotCreated={nextCreated} onBusyChange={busy} />);
    await waitFor(() => expect(busy.mock.calls.map(([value]) => value)).toEqual([true, false])); oldPending.resolve(snapshot(candidates.map((item) => item.candidate.id))); await Promise.resolve();
    expect(oldCreated).not.toHaveBeenCalled(); expect(screen.queryByText('确认预览已生成。请核对不可变快照后再加入队列。')).not.toBeInTheDocument(); expect(busy.mock.calls.map(([value]) => value)).toEqual([true, false]);
    await userEvent.click(screen.getByRole('button', { name: '生成确认预览（3 件）' })); expect(busy.mock.calls.map(([value]) => value)).toEqual([true, false, true]);
    nextPending.resolve(snapshot(candidates.map((item) => item.candidate.id))); await screen.findByRole('button', { name: '确认创建（3 件）' });
    expect(nextCreated).toHaveBeenCalledTimes(1); expect(busy.mock.calls.map(([value]) => value)).toEqual([true, false, true, false]);
  });

  it('atomically releases apply busy on session switch, ignores stale execution, and tracks the new apply independently', async () => {
    const oldPending = deferred<Execution>(); const nextPending = deferred<Execution>(); const busy = vi.fn(); const firstSnapshot = snapshot(candidates.map((item) => item.candidate.id)); const nextSnapshot = snapshot(candidates.map((item) => item.candidate.id));
    const activeSession = { ...session, activeSnapshotVersion: firstSnapshot.snapshotVersion };
    const view = renderPanel({ session: activeSession, activeSnapshot: firstSnapshot, onApply: () => oldPending.promise, onBusyChange: busy });
    await userEvent.click(screen.getByRole('button', { name: '确认创建（3 件）' })); expect(busy.mock.calls.map(([value]) => value)).toEqual([true]);
    view.rerender(<AgentReviewPanel session={{ ...activeSession, id: 'session-2' }} candidates={candidates} activeSnapshot={nextSnapshot} onCreateSnapshot={vi.fn()} onApply={() => nextPending.promise} onBusyChange={busy} />);
    await waitFor(() => expect(busy.mock.calls.map(([value]) => value)).toEqual([true, false])); oldPending.resolve(execution); await Promise.resolve();
    expect(screen.queryByRole('heading', { name: '已成功加入队列' })).not.toBeInTheDocument(); expect(busy.mock.calls.map(([value]) => value)).toEqual([true, false]);
    await userEvent.click(await screen.findByRole('button', { name: '确认创建（3 件）' })); expect(busy.mock.calls.map(([value]) => value)).toEqual([true, false, true]);
    nextPending.resolve(execution); expect(await screen.findByRole('heading', { name: '已成功加入队列' })).toBeInTheDocument(); expect(busy.mock.calls.map(([value]) => value)).toEqual([true, false, true, false]);
  });

  it('allows exactly one in-flight apply and reuses the same key for an ambiguous retry', async () => {
    const user = userEvent.setup(); const pending = deferred<Execution>(); const apply = vi.fn().mockReturnValueOnce(pending.promise).mockRejectedValueOnce(new TypeError('network')).mockResolvedValueOnce(execution);
    renderPanel({ onApply: apply });
    await user.click(screen.getByRole('button', { name: '生成确认预览（3 件）' }));
    const confirm = screen.getByRole('button', { name: '确认创建（3 件）' });
    await user.dblClick(confirm);
    expect(apply).toHaveBeenCalledTimes(1);
    expect(confirm).toBeDisabled();
    await act(async () => pending.resolve(Promise.reject(new TypeError('network')) as never));
    expect(await screen.findByRole('alert')).toHaveTextContent('确认结果暂时未知');
    expect(screen.queryByText('network')).not.toBeInTheDocument();
    expect(await screen.findByRole('button', { name: '确认创建（3 件）' })).toBeEnabled();
    const firstKey = apply.mock.calls[0][0].idempotencyKey;
    await user.click(screen.getByRole('button', { name: '确认创建（3 件）' }));
    await screen.findByRole('button', { name: '确认创建（3 件）' });
    await user.click(screen.getByRole('button', { name: '确认创建（3 件）' }));
    expect(apply.mock.calls[1][0].idempotencyKey).toBe(firstKey);
    expect(apply.mock.calls[2][0].idempotencyKey).toBe(firstKey);
  });

  it('renders exact assigned IDs and positions, then navigates explicitly without starting a lot', async () => {
    const user = userEvent.setup(); const apply = vi.fn().mockResolvedValue(execution); const navigate = vi.fn();
    renderPanel({ onApply: apply, onNavigate: navigate });
    await user.click(screen.getByRole('button', { name: '生成确认预览（3 件）' }));
    await user.click(screen.getByRole('button', { name: '确认创建（3 件）' }));
    expect(await screen.findByRole('heading', { name: '已成功加入队列' })).toBeInTheDocument();
    expect(screen.getByText('lot-new-1 · 队列位置 11')).toBeInTheDocument();
    expect(screen.getByText('lot-existing-1 · 队列位置 12')).toBeInTheDocument();
    expect(screen.getByText(/新建拍品 ID：lot-new-1、lot-new-2/)).toBeInTheDocument();
    expect(apply).toHaveBeenCalledWith(expect.objectContaining({ snapshotVersion: 4, contentHash: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', selectedCandidateIds: ['new-1', 'existing-1', 'new-2'], orderedCandidateIds: ['new-1', 'existing-1', 'new-2'], idempotencyKey: expect.any(String) }));
    await new Promise((resolve) => window.setTimeout(resolve, 750));
    expect(navigate).not.toHaveBeenCalled();
    const navigation = screen.getByRole('button', { name: '前往拍品队列' });
    expect(navigation).toHaveClass('studioButton-primary');
    navigation.focus(); await user.keyboard('{Enter}');
    expect(navigate).toHaveBeenCalledWith('/admin/auctions', execution);
    expect(document.body.textContent).not.toContain('开始竞拍');
  });

  it('saves an all-new immutable snapshot to inventory without queueing', async () => {
    const inventoryCandidates = [candidates[0], candidates[2]];
    const inventoryExecution: Execution = { executionId: 'inventory-execution', createdLotIds: ['lot-new-1', 'lot-new-2'], queuedLots: [], target: 'INVENTORY_ONLY' };
    const apply = vi.fn().mockResolvedValue(inventoryExecution);
    renderPanel({ candidates: inventoryCandidates, onCreateSnapshot: vi.fn().mockResolvedValue(snapshot(['new-1', 'new-2'])), onApply: apply });
    await userEvent.click(screen.getByRole('button', { name: '生成确认预览（2 件）' }));

    await userEvent.click(await screen.findByRole('button', { name: '确认创建（2 件）' }));

    expect(apply).toHaveBeenCalledWith(expect.objectContaining({ target: 'INVENTORY_ONLY', selectedCandidateIds: ['new-1', 'new-2'] }));
    expect(await screen.findByRole('heading', { name: '拍品创建成功' })).toBeInTheDocument();
    expect(screen.getByText('当前状态：待开拍，可用于后续直播排品。')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '查看待开拍拍品' })).toBeEnabled();
  });

  it('notifies the workspace exactly once after a verified apply succeeds', async () => {
    const applied = vi.fn();
    renderPanel({ onApplied: applied });
    await userEvent.click(screen.getByRole('button', { name: '生成确认预览（3 件）' }));
    await userEvent.click(screen.getByRole('button', { name: '确认创建（3 件）' }));
    await screen.findByRole('heading', { name: '已成功加入队列' });
    expect(applied).toHaveBeenCalledTimes(1);
    expect(applied).toHaveBeenCalledWith(execution);
  });

  it('has an explicit non-overlapping tablet grid rule and semantic ordering controls', () => {
    renderPanel();
    expect(agentConsoleCSS).toMatch(/@media \(min-width: 481px\) and \(max-width: 768px\)/);
    expect(agentConsoleCSS).toMatch(/\.agentReviewItem\s*\{[^}]*grid-template-areas:/s);
    expect(agentConsoleCSS).toMatch(/\.agentSnapshotFields\s*\{[^}]*repeat\(3, minmax\(0, 1fr\)\)/s);
    expect(agentConsoleCSS).toMatch(/@media \(max-width: 480px\)[\s\S]*\.agentSnapshotMeta, \.agentSnapshotFields\s*\{\s*grid-template-columns: 1fr;/);
    expect(screen.getByRole('button', { name: '上移典藏腕表' })).toBeEnabled();
  });

  it.each([
    ['reversed order', { ...execution, queuedLots: [...execution.queuedLots].reverse() }],
    ['missing lot', { ...execution, queuedLots: execution.queuedLots.slice(0, 2) }],
    ['extra lot', { ...execution, queuedLots: [...execution.queuedLots, { lotId: 'extra', queuePosition: 14 }] }],
    ['wrong existing identity', { ...execution, queuedLots: execution.queuedLots.map((lot) => lot.lotId === 'lot-existing-1' ? { ...lot, lotId: 'wrong-existing' } : lot) }],
    ['created mismatch', { ...execution, createdLotIds: ['lot-new-2', 'lot-new-1'] }],
    ['position gap', { ...execution, queuedLots: execution.queuedLots.map((lot, index) => index === 2 ? { ...lot, queuePosition: 15 } : lot) }],
    ['duplicate position', { ...execution, queuedLots: execution.queuedLots.map((lot, index) => index === 2 ? { ...lot, queuePosition: 12 } : lot) }],
  ])('rejects semantically inconsistent execution: %s', (_name, value) => expect(executionMatchesSnapshot(snapshot(['new-1', 'existing-1', 'new-2']), value)).toBe(false));

  it('accepts a valid mixed NEW/EXISTING execution in snapshot order', () => expect(executionMatchesSnapshot(snapshot(['new-1', 'existing-1', 'new-2']), execution)).toBe(true));

  it('keeps a semantic execution mismatch recoverable without success or navigation and retains the idempotency key', async () => {
    const apply = vi.fn().mockResolvedValue({ ...execution, queuedLots: [...execution.queuedLots].reverse() }); const navigate = vi.fn(); renderPanel({ onApply: apply, onNavigate: navigate });
    await userEvent.click(screen.getByRole('button', { name: '生成确认预览（3 件）' })); await userEvent.click(screen.getByRole('button', { name: '确认创建（3 件）' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('入队结果与确认快照不一致'); expect(screen.queryByRole('heading', { name: '已成功加入队列' })).not.toBeInTheDocument(); expect(navigate).not.toHaveBeenCalled();
    const key = apply.mock.calls[0][0].idempotencyKey; await userEvent.click(screen.getByRole('button', { name: '确认创建（3 件）' })); expect(apply.mock.calls[1][0].idempotencyKey).toBe(key);
  });
});

const execution: Execution = { executionId: 'execution-1', createdLotIds: ['lot-new-1', 'lot-new-2'], queuedLots: [{ lotId: 'lot-new-1', queuePosition: 11 }, { lotId: 'lot-existing-1', queuePosition: 12 }, { lotId: 'lot-new-2', queuePosition: 13 }], target: 'QUEUE' };
function candidate(id: string, title: string, sourceType: 'NEW' | 'EXISTING', position: number, sources: Record<string, string>, validation: object = { requiredInputs: [], errors: [] }): AgentDraftCandidate { const complete = Object.fromEntries(sourceFields.map((field) => [field, sources[field] ?? 'SYSTEM'])); return { candidate: { id, sourceType, ...(sourceType === 'EXISTING' ? { existingLotId: `lot-${id}`, existingLotReadVersion: 2 } : {}), startPriceSpecified: true, draft: { title, startPriceAmount: 10_000, durationSeconds: 600, mainImageAssetId: sourceType === 'NEW' ? `asset-${id}` : '', mainImageUrl: `https://img/${id}.webp`, minIncrementAmount: 100, capPriceAmount: 20_000, category: '收藏', description: '介绍', tags: ['精选'], stock: 1, antiSnipeWindowSeconds: 10, antiSnipeExtendSeconds: 15, maxExtendCount: 5, depositAmount: 0 } }, position, selectedByDefault: true, fieldSourcesJSON: JSON.stringify(complete), validationJSON: JSON.stringify(validation), provenanceVersion: 1, version: 3, createdAtUnixMs: 1, updatedAtUnixMs: 2 }; }
function snapshot(ids: string[]): ReviewSnapshot { return { snapshotVersion: 4, contentHash: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', candidates: ids.map((id) => { const item = candidates.find((candidate) => candidate.candidate.id === id)!; const current = item.candidate; const { mainImageUrl: _url, ...draft } = current.draft; const sources = JSON.parse(item.fieldSourcesJSON) as Record<string, 'USER' | 'AI' | 'SYSTEM' | 'EXISTING'>; return { candidateId: id, candidateVersion: item.version, sourceType: current.sourceType, ...(current.existingLotId ? { existingLotId: current.existingLotId, existingLotReadVersion: current.existingLotReadVersion } : {}), draft, fieldSources: sourceFields.map((field) => ({ field: field as keyof typeof draft, source: sources[field] })), assetIds: current.draft.mainImageAssetId ? [current.draft.mainImageAssetId] : [] }; }) as ReviewSnapshot['candidates'] }; }
function renderPanel(overrides: Partial<React.ComponentProps<typeof AgentReviewPanel>> = {}) { const props: React.ComponentProps<typeof AgentReviewPanel> = { session, candidates, onCreateSnapshot: vi.fn().mockResolvedValue(snapshot(candidates.map((item) => item.candidate.id))), onApply: vi.fn().mockResolvedValue(execution), ...overrides }; return render(<AgentReviewPanel {...props} />); }
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>((next) => { resolve = next; }); return { promise, resolve }; }
