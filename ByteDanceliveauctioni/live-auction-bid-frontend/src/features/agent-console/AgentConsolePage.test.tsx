import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { StrictMode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { AgentConsolePage, formatYuanFromCents, parseYuanToCents } from './AgentConsolePage';
import { createAgentApi } from './api/agentApi';
import type { AgentApi, AgentDraftCandidate, AgentRunEvent, AgentSessionAggregate } from './model/types';
import { canNavigateToAgentConsole, isAgentConsolePath } from '../../pages/host-console/HostConsolePage';
import { HttpError } from '../../shared/api/httpClient';

const uploadImage = vi.fn();
const startLot = vi.fn();
const getRoomSnapshot = vi.fn();
vi.mock('../auction/api/auctionApi', () => ({ uploadImage: (...args: unknown[]) => uploadImage(...args), startLot: (...args: unknown[]) => startLot(...args), getRoomSnapshot: (...args: unknown[]) => getRoomSnapshot(...args) }));

describe('AgentConsolePage recoverable workspace', () => {
  beforeEach(() => { history.replaceState(null, '', '/admin/agent'); uploadImage.mockReset(); startLot.mockReset(); getRoomSnapshot.mockReset(); });

  it('creates one session for the selected room and replaces the URL without navigation', async () => {
    const api = sessionApi();
    render(<AgentConsolePage api={api} roomId="room-7" />);
    await screen.findByRole('heading', { name: 'AI 竞拍工作台' });
    expect(api.createSession).toHaveBeenCalledTimes(1);
    expect(api.createSession).toHaveBeenCalledWith('room-7', expect.any(AbortSignal));
    expect(api.getSession).not.toHaveBeenCalled();
    expect(location.pathname + location.search).toBe('/admin/agent?session=session-1');
  });

  it('bootstraps exactly once under StrictMode and still reaches a usable session', async () => {
    const api = sessionApi();
    render(<StrictMode><AgentConsolePage api={api} roomId="room-7" /></StrictMode>);
    expect(await screen.findByText('等待补充')).toBeInTheDocument();
    expect(api.createSession).toHaveBeenCalledTimes(1);
    expect(location.search).toBe('?session=session-1');
  });

  it('restores exactly the deep-linked session, history, active run and server drafts', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-restored');
    const aggregate = sessionAggregate({
      session: { id: 'session-restored', roomId: 'room-old', status: 'COLLECTING_INPUT', version: 6, activeSnapshotVersion: 0, conversationSummary: '', createdAtUnixMs: 1, updatedAtUnixMs: 2 },
      messages: [
        { sessionId: 'session-restored', seq: 1, role: 'USER', content: '拍卖海洋之心', runId: 'run-1', createdAtUnixMs: 1 },
        { sessionId: 'session-restored', seq: 2, role: 'ASSISTANT', content: '还需要起拍价和时长', runId: 'run-1', createdAtUnixMs: 2 },
      ], activeRun: { runId: 'run-2', goal: '继续补全', status: 'GENERATING_PROPOSAL', createdAtUnixMs: 2, updatedAtUnixMs: 2 },
    });
    const api = sessionApi({ getSession: vi.fn().mockResolvedValue(aggregate) });
    render(<AgentConsolePage api={api} roomId="room-new" />);
    expect(await screen.findByText('拍卖海洋之心')).toBeInTheDocument();
    expect(screen.getByText('还需要起拍价和时长')).toBeInTheDocument();
    expect(screen.getByText('生成内容')).toBeInTheDocument();
    expect(screen.getByDisplayValue('海洋之心')).toBeInTheDocument();
    expect(api.getSession).toHaveBeenCalledTimes(1);
    expect(api.createSession).not.toHaveBeenCalled();
  });

  it('integrates snapshot creation with the authoritative next session version without staling its own preview', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const readyCandidate = { ...baseCandidate, candidate: { ...baseCandidate.candidate, draft: { ...baseCandidate.candidate.draft, mainImageAssetId: 'asset-1', mainImageUrl: 'https://img/asset-1.webp' } }, validationJSON: '{"requiredInputs":[],"errors":[]}' };
    const ready = sessionAggregate({ session: { ...baseSession, status: 'READY_FOR_REVIEW' }, candidates: [readyCandidate] });
    const review = reviewSnapshot(readyCandidate);
    const api = sessionApi({ getSession: vi.fn().mockResolvedValue(ready), createReviewSnapshot: vi.fn().mockResolvedValue(review) });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    await userEvent.click(await screen.findByRole('button', { name: '生成确认预览（1 件）' }));
    expect(api.createReviewSnapshot).toHaveBeenCalledWith('session-1', { expectedSessionVersion: 2, selectedCandidateIds: ['candidate-1'], orderedCandidateIds: ['candidate-1'] });
    expect(await screen.findByRole('button', { name: '确认创建（1 件）' })).toBeEnabled();
    expect(screen.queryByText(/候选项或会话已变化/)).not.toBeInTheDocument();
  });

  it('moves an applied batch below and opens an empty continuation workspace', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const candidate = readyCandidateValue();
    const ready = sessionAggregate({ session: { ...baseSession, status: 'READY_FOR_REVIEW' }, candidates: [candidate] });
    const nextSession = { ...baseSession, id: 'session-2', status: 'COLLECTING_INPUT' as const, version: 1, activeSnapshotVersion: 0 };
    const execution = { executionId: 'execution-continued', createdLotIds: ['lot-aa'], queuedLots: [], target: 'INVENTORY_ONLY' as const };
    const api = sessionApi({
      getSession: vi.fn().mockResolvedValue(ready),
      createReviewSnapshot: vi.fn().mockResolvedValue(reviewSnapshot(candidate)),
      apply: vi.fn().mockResolvedValue(execution),
      createSession: vi.fn().mockResolvedValue(nextSession),
      sendMessage: vi.fn().mockResolvedValue({ runId: 'run-next', goal: '继续添加', status: 'CREATED', createdAtUnixMs: 3, updatedAtUnixMs: 3 }),
    });
    render(<AgentConsolePage api={api} roomId="room-1" />);

    await userEvent.click(await screen.findByRole('button', { name: '生成确认预览（1 件）' }));
    await userEvent.click(await screen.findByRole('button', { name: '确认创建（1 件）' }));

    expect(await screen.findByRole('heading', { name: '本次拍品已创建' })).toBeInTheDocument();
    expect(screen.getByText('当前状态：待开拍，可供 Agent 生成直播排品方案。')).toBeInTheDocument();
    expect(await screen.findByText('尚无待创建拍品')).toBeInTheDocument();
    expect(screen.queryByDisplayValue('海洋之心')).not.toBeInTheDocument();
    expect(api.createSession).toHaveBeenCalledWith('room-1', expect.any(AbortSignal));
    expect(location.search).toBe('?session=session-2');

    const composer = screen.getByLabelText('给 Agent 发消息');
    expect(composer).toHaveFocus();
    await userEvent.type(composer, '继续添加');
    await userEvent.click(screen.getByRole('button', { name: '发送' }));
    expect(api.sendMessage).toHaveBeenCalledWith('session-2', expect.objectContaining({ expectedVersion: 1, content: '继续添加' }), expect.any(AbortSignal));
  });

  it('recovers only the server-declared active immutable snapshot and safely falls back when recovery fails', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const readyCandidate = readyCandidateValue(); const active = sessionAggregate({ session: { ...baseSession, status: 'READY_FOR_REVIEW', activeSnapshotVersion: 5 }, candidates: [readyCandidate] });
    const recovered = reviewSnapshot(readyCandidate); const api = sessionApi({ getSession: vi.fn().mockResolvedValue(active), getReviewSnapshot: vi.fn().mockResolvedValue(recovered) });
    const view = render(<AgentConsolePage api={api} roomId="room-1" />);
    expect(await screen.findByText('已恢复当前不可变确认快照。')).toBeInTheDocument();
    expect(api.getReviewSnapshot).toHaveBeenCalledWith('session-1', 5, expect.any(AbortSignal));
    expect(screen.getByRole('button', { name: '确认创建（1 件）' })).toBeEnabled();
    view.unmount();

    const failed = sessionApi({ getSession: vi.fn().mockResolvedValue(active), getReviewSnapshot: vi.fn().mockRejectedValue(new TypeError('offline')) });
    render(<AgentConsolePage api={failed} roomId="room-1" />);
    expect(await screen.findByText('当前确认快照无法安全恢复，请重新生成确认预览。')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /确认创建（/ })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: '生成确认预览（1 件）' })).toBeEnabled();
  });

  it('recovers an APPLIED result and queue CTA without calling the READY-only snapshot endpoint', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-applied');
    const candidate = readyCandidateValue();
    const omittedCandidate = { ...candidate, candidate: { ...candidate.candidate, id: 'candidate-omitted', draft: { ...candidate.candidate.draft, title: '未勾选拍品' } }, position: 2 };
    const applied = sessionAggregate({
      session: { ...baseSession, id: 'session-applied', status: 'APPLIED', activeSnapshotVersion: 5 }, candidates: [candidate, omittedCandidate],
      appliedExecution: { executionId: 'execution-applied', snapshotVersion: 5, createdLotIds: ['lot-created'], queuedLots: [{ candidateId: candidate.candidate.id, sourceType: 'NEW', lotId: 'lot-created', queuePosition: 12 }], target: 'QUEUE' },
    });
    const api = sessionApi({ getSession: vi.fn().mockResolvedValue(applied), getReviewSnapshot: vi.fn() });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    expect(await screen.findByRole('button', { name: '前往拍品队列' })).toBeInTheDocument();
    expect(screen.getByText(/execution-applied/)).toBeInTheDocument();
    expect(api.getReviewSnapshot).not.toHaveBeenCalled();
    expect(screen.queryByText(/快照无法安全恢复/)).not.toBeInTheDocument();
  });

  it('recovers a real-wire legacy snapshot when only optional zero-value provenance was backfilled', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-legacy');
    const readyCandidate = readyCandidateValue();
    const legacyDraft = { ...readyCandidate.candidate.draft, capPriceAmount: null, category: '', description: '', tags: [], depositAmount: 0 };
    const legacySources = { ...completeSources };
    for (const field of ['capPriceAmount', 'category', 'description', 'tags', 'depositAmount']) delete legacySources[field];
    const legacyCandidate = { ...readyCandidate, provenanceVersion: 0, candidate: { ...readyCandidate.candidate, draft: legacyDraft }, fieldSourcesJSON: JSON.stringify(legacySources) };
    const active = sessionAggregate({ session: { ...baseSession, id: 'session-legacy', status: 'READY_FOR_REVIEW', activeSnapshotVersion: 2 }, candidates: [legacyCandidate] });
    const { mainImageUrl: _url, ...immutableDraft } = legacyDraft;
    const recovered = { snapshotVersion: 2, contentHash: 'b'.repeat(64), candidates: [{ candidateId: 'candidate-1', candidateVersion: legacyCandidate.version, sourceType: 'NEW' as const, draft: immutableDraft, fieldSources: approvalFields.map((field) => ({ field: field as keyof typeof immutableDraft, source: (legacySources[field] ?? 'AI') as 'USER' | 'AI' | 'SYSTEM' })), assetIds: ['asset-1'] }] };
    const api = sessionApi({ getSession: vi.fn().mockResolvedValue(active), getReviewSnapshot: vi.fn().mockResolvedValue(recovered) });

    render(<AgentConsolePage api={api} roomId="room-1" />);

    expect(await screen.findByText('已恢复当前不可变确认快照。')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '确认创建（1 件）' })).toBeEnabled();
    expect(screen.queryByText(/无法安全恢复/)).not.toBeInTheDocument();
  });

  it('blocks an existing snapshot while local edits are unsaved and re-enables it after an exact revert', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const readyCandidate = readyCandidateValue(); const active = sessionAggregate({ session: { ...baseSession, status: 'READY_FOR_REVIEW', activeSnapshotVersion: 5 }, candidates: [readyCandidate] });
    const api = sessionApi({ getSession: vi.fn().mockResolvedValue(active), getReviewSnapshot: vi.fn().mockResolvedValue(reviewSnapshot(readyCandidate)), apply: vi.fn() });
    render(<AgentConsolePage api={api} roomId="room-1" />); const confirm = await screen.findByRole('button', { name: '确认创建（1 件）' }); expect(confirm).toBeEnabled();
    const description = screen.getByLabelText('海洋之心的介绍'); await userEvent.type(description, '（本地修改）');
    expect(screen.getByText(/有未保存修改/)).toBeInTheDocument(); expect(confirm).toBeDisabled(); await userEvent.click(confirm); expect(api.apply).not.toHaveBeenCalled();
    await userEvent.clear(description); await userEvent.type(description, readyCandidate.candidate.draft.description);
    await waitFor(() => expect(confirm).toBeEnabled()); expect(screen.queryByText(/有未保存修改，请先保存/)).not.toBeInTheDocument();
  });

  it('gates review and every write as soon as image upload starts, then never confirms the old snapshot after bind', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const readyCandidate = readyCandidateValue(); const active = sessionAggregate({ session: { ...baseSession, status: 'READY_FOR_REVIEW', activeSnapshotVersion: 5 }, candidates: [readyCandidate] });
    const uploadPending = deferred<{ id: string }>(); uploadImage.mockImplementation(() => uploadPending.promise);
    const rebound = sessionAggregate({ session: { ...baseSession, status: 'READY_FOR_REVIEW', version: 4, activeSnapshotVersion: 0 }, candidates: [{ ...readyCandidate, version: 3, candidate: { ...readyCandidate.candidate, draft: { ...readyCandidate.candidate.draft, mainImageAssetId: 'asset-new', mainImageUrl: 'https://img/new.webp' } } }] });
    const api = sessionApi({ getSession: vi.fn().mockResolvedValue(active), getReviewSnapshot: vi.fn().mockResolvedValue(reviewSnapshot(readyCandidate)), bindCandidateImage: vi.fn().mockResolvedValue(rebound), apply: vi.fn(), sendMessage: vi.fn() });
    render(<AgentConsolePage api={api} roomId="room-1" />); const confirm = await screen.findByRole('button', { name: '确认创建（1 件）' });
    fireEvent.change(screen.getByLabelText('上传海洋之心的主图'), { target: { files: [new File(['image'], 'new.webp', { type: 'image/webp' })] } });
    expect(confirm).toBeDisabled(); expect(screen.getByRole('button', { name: '保存海洋之心' })).toBeDisabled();
    const composer = screen.getByLabelText('给 Agent 发消息'); await userEvent.type(composer, '上传期间保留'); expect(screen.getByRole('button', { name: '发送' })).toBeDisabled();
    await userEvent.click(confirm); expect(api.apply).not.toHaveBeenCalled(); uploadPending.resolve({ id: 'asset-new' });
    expect(await screen.findByRole('button', { name: '生成确认预览（1 件）' })).toBeEnabled(); expect(screen.queryByRole('button', { name: '确认创建（1 件）' })).not.toBeInTheDocument();
    expect(api.bindCandidateImage).toHaveBeenCalledTimes(1); expect(composer).toHaveValue('上传期间保留');
  });

  it('clears the global upload gate after upload failure without discarding the active snapshot', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1'); const readyCandidate = readyCandidateValue(); const active = sessionAggregate({ session: { ...baseSession, status: 'READY_FOR_REVIEW', activeSnapshotVersion: 5 }, candidates: [readyCandidate] });
    const uploadPending = deferred<{ id: string }>(); uploadImage.mockImplementation(() => uploadPending.promise); const api = sessionApi({ getSession: vi.fn().mockResolvedValue(active), getReviewSnapshot: vi.fn().mockResolvedValue(reviewSnapshot(readyCandidate)) });
    render(<AgentConsolePage api={api} roomId="room-1" />); const confirm = await screen.findByRole('button', { name: '确认创建（1 件）' });
    fireEvent.change(screen.getByLabelText('上传海洋之心的主图'), { target: { files: [new File(['bad'], 'bad.webp', { type: 'image/webp' })] } }); expect(confirm).toBeDisabled();
    uploadPending.resolve(Promise.reject(new TypeError('offline')) as never); expect(await screen.findByRole('alert')).toHaveTextContent('offline'); await waitFor(() => expect(confirm).toBeEnabled()); expect(api.bindCandidateImage).not.toHaveBeenCalled();
  });

  it('mutually gates message and candidate writes during deferred review and candidate CAS writes while preserving composer text', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const readyCandidate = readyCandidateValue(); const ready = sessionAggregate({ session: { ...baseSession, status: 'READY_FOR_REVIEW' }, candidates: [readyCandidate] });
    const reviewPending = deferred<ReturnType<typeof reviewSnapshot>>(); const applyPending = deferred<{ executionId: string; target: 'QUEUE'; createdLotIds: string[]; queuedLots: Array<{ lotId: string; queuePosition: number }> }>(); const reviewApi = sessionApi({ getSession: vi.fn().mockResolvedValue(ready), createReviewSnapshot: vi.fn(() => reviewPending.promise), apply: vi.fn(() => applyPending.promise), sendMessage: vi.fn() });
    const first = render(<AgentConsolePage api={reviewApi} roomId="room-1" />); const createReview = await screen.findByRole('button', { name: '生成确认预览（1 件）' }); const composer = screen.getByLabelText('给 Agent 发消息');
    await userEvent.click(createReview); await userEvent.type(composer, '保留这条消息');
    expect(composer).toHaveValue('保留这条消息'); expect(screen.getByRole('button', { name: '发送' })).toBeDisabled(); expect(screen.getByRole('button', { name: '保存海洋之心' })).toBeDisabled(); expect(reviewApi.sendMessage).not.toHaveBeenCalled();
    reviewPending.resolve(reviewSnapshot(readyCandidate)); const confirm = await screen.findByRole('button', { name: '确认创建（1 件）' }); await userEvent.click(confirm);
    expect(screen.getByRole('button', { name: '发送' })).toBeDisabled(); expect(screen.getByRole('button', { name: '保存海洋之心' })).toBeDisabled(); expect(composer).toHaveValue('保留这条消息');
    applyPending.resolve({ executionId: 'execution-1', createdLotIds: ['lot-1'], queuedLots: [{ lotId: 'lot-1', queuePosition: 7 }], target: 'QUEUE' }); await screen.findByRole('button', { name: '前往拍品队列' }); first.unmount();

    const patchPending = deferred<AgentSessionAggregate>(); const patchApi = sessionApi({ getSession: vi.fn().mockResolvedValue(ready), patchCandidate: vi.fn(() => patchPending.promise), sendMessage: vi.fn() });
    render(<AgentConsolePage api={patchApi} roomId="room-1" />); const title = await screen.findByLabelText('海洋之心的拍品名称'); await userEvent.clear(title); await userEvent.type(title, '新名称');
    await userEvent.click(screen.getByRole('button', { name: '保存新名称' })); const secondComposer = screen.getByLabelText('给 Agent 发消息'); await userEvent.type(secondComposer, '也保留');
    expect(secondComposer).toHaveValue('也保留'); expect(screen.getByRole('button', { name: '发送' })).toBeDisabled(); expect(screen.getByRole('button', { name: '生成确认预览（1 件）' })).toBeDisabled(); expect(patchApi.sendMessage).not.toHaveBeenCalled();
    patchPending.resolve(ready); await waitFor(() => expect(screen.getByRole('button', { name: '发送' })).toBeEnabled());
  });

  it('keeps input and draft after a model failure and offers recovery', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const failed = sessionAggregate({ session: { ...baseSession, version: 3 }, activeRun: { runId: 'run-failed', goal: '生成方案', status: 'FAILED', errorCode: 'MODEL_ERROR', errorMessage: '安全错误', createdAtUnixMs: 2, updatedAtUnixMs: 3 } });
    const api = sessionApi({ sendMessage: vi.fn().mockResolvedValue({ runId: 'run-failed', goal: '生成方案', status: 'CREATED', createdAtUnixMs: 2, updatedAtUnixMs: 2 }), streamSessionEvents: vi.fn(async (_id, options) => { options.onEvent({ ...event(1, 'run.failed', '生成失败'), runId: 'run-failed' }); }), getSession: vi.fn().mockResolvedValueOnce(sessionAggregate()).mockResolvedValueOnce(failed) });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    await screen.findByDisplayValue('海洋之心');
    const input = screen.getByLabelText('给 Agent 发消息');
    await userEvent.type(input, '请继续补全介绍');
    await userEvent.click(screen.getByRole('button', { name: '发送' }));
    expect(await screen.findByText('本轮生成失败')).toBeInTheDocument();
    expect(screen.getByDisplayValue('海洋之心')).toBeInTheDocument();
    expect(input).toHaveValue('');
    expect(screen.getByRole('button', { name: '重试本轮' })).toBeEnabled();
  });

  it('keeps the next message typed while the active run is still working', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    let streamOptions: Parameters<AgentApi['streamSessionEvents']>[1] | undefined;
    const api = sessionApi({
      sendMessage: vi.fn().mockResolvedValue({ runId: 'run-current', goal: '当前消息', status: 'CREATED', createdAtUnixMs: 2, updatedAtUnixMs: 2 }),
      streamSessionEvents: vi.fn(async (_id, options) => { streamOptions = options; await new Promise<void>(() => undefined); }),
    });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    await screen.findByDisplayValue('海洋之心');
    const input = await screen.findByLabelText('给 Agent 发消息');
    await userEvent.type(input, '当前消息');
    await userEvent.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(streamOptions).toBeDefined());
    await userEvent.type(input, '下一条消息');
    act(() => streamOptions?.onEvent({ runId: 'run-current', seq: 1, type: 'run.completed', message: '完成', payload: {} }));
    expect(input).toHaveValue('下一条消息');
  });

  it('gates every versioned action until terminal recovery installs the latest aggregate', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const recovery = deferred<AgentSessionAggregate>();
    let streamOptions: Parameters<AgentApi['streamSessionEvents']>[1] | undefined;
    const active = sessionAggregate({ activeRun: { runId: 'run-current', goal: '生成', status: 'PLANNING', createdAtUnixMs: 1, updatedAtUnixMs: 1 } });
    const latest = sessionAggregate({ session: { ...baseSession, version: 3 }, activeRun: null });
    const api = sessionApi({
      getSession: vi.fn().mockResolvedValueOnce(active).mockImplementationOnce(() => recovery.promise),
      streamSessionEvents: vi.fn(async (_id, options) => { streamOptions = options; await new Promise<void>(() => undefined); }),
      sendMessage: vi.fn().mockResolvedValue({ runId: 'run-next', goal: '下一轮', status: 'CREATED', createdAtUnixMs: 3, updatedAtUnixMs: 3 }),
    });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    await waitFor(() => expect(streamOptions).toBeDefined());
    act(() => streamOptions?.onEvent({ ...event(1, 'run.failed', '生成失败'), runId: 'run-current' }));
    const composer = screen.getByLabelText('给 Agent 发消息');
    await userEvent.type(composer, '下一轮');
    expect(screen.getByRole('button', { name: '发送' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '重试本轮' })).toBeDisabled();
    expect(screen.getByLabelText('海洋之心的拍品名称')).toBeDisabled();
    expect(screen.getByRole('button', { name: '保存海洋之心' })).toBeDisabled();
    expect(screen.getByLabelText('上传海洋之心的主图')).toBeDisabled();
    recovery.resolve(latest);
    await waitFor(() => expect(screen.getByRole('button', { name: '发送' })).toBeEnabled());
    await userEvent.click(screen.getByRole('button', { name: '发送' }));
    expect(api.sendMessage).toHaveBeenCalledWith('session-1', expect.objectContaining({ expectedVersion: 3, content: '下一轮' }), expect.any(AbortSignal));
  });

  it('stays gated after terminal recovery failure and offers refetch-only resynchronization', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    let streamOptions: Parameters<AgentApi['streamSessionEvents']>[1] | undefined;
    const active = sessionAggregate({ activeRun: { runId: 'run-current', goal: '生成', status: 'PLANNING', createdAtUnixMs: 1, updatedAtUnixMs: 1 } });
    const api = sessionApi({
      getSession: vi.fn().mockResolvedValueOnce(active).mockRejectedValueOnce(new TypeError('offline')).mockResolvedValueOnce(sessionAggregate({ session: { ...baseSession, version: 3 } })),
      streamSessionEvents: vi.fn(async (_id, options) => { streamOptions = options; await new Promise<void>(() => undefined); }),
      sendMessage: vi.fn(),
    });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    await waitFor(() => expect(streamOptions).toBeDefined());
    act(() => streamOptions?.onEvent({ ...event(1, 'run.completed', '完成'), runId: 'run-current' }));
    const resync = await screen.findByRole('button', { name: '重新同步' });
    expect(screen.getByText(/最新版本同步失败/)).toBeInTheDocument();
    await userEvent.click(resync);
    expect(api.sendMessage).not.toHaveBeenCalled();
    await waitFor(() => expect(api.getSession).toHaveBeenCalledTimes(3));
    expect(screen.queryByRole('button', { name: '重新同步' })).not.toBeInTheDocument();
  });

  it('reuses an ambiguous admission key, rotates it for edited content and definitive rejection', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const sendMessage = vi.fn()
      .mockRejectedValueOnce(new TypeError('lost response'))
      .mockRejectedValueOnce(new TypeError('lost response again'))
      .mockRejectedValueOnce(new TypeError('edited response lost'))
      .mockRejectedValueOnce(new HttpError(400, '请求被拒绝', 'request-1'))
      .mockRejectedValueOnce(new HttpError(400, '请求被拒绝', 'request-2'));
    render(<AgentConsolePage api={sessionApi({ sendMessage })} roomId="room-1" />);
    await screen.findByDisplayValue('海洋之心');
    const composer = await screen.findByLabelText('给 Agent 发消息');
    await userEvent.type(composer, '生成方案');
    await userEvent.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(sendMessage).toHaveBeenCalledTimes(1));
    await userEvent.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(sendMessage).toHaveBeenCalledTimes(2));
    const first = sendMessage.mock.calls[0][1]; const second = sendMessage.mock.calls[1][1];
    expect(second).toEqual(first);
    await userEvent.type(composer, '（新版）');
    await userEvent.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(sendMessage).toHaveBeenCalledTimes(3));
    expect(sendMessage.mock.calls[2][1].idempotencyKey).not.toBe(first.idempotencyKey);
    await userEvent.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(sendMessage).toHaveBeenCalledTimes(4));
    expect(sendMessage.mock.calls[3][1]).toEqual(sendMessage.mock.calls[2][1]);
    await userEvent.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(sendMessage).toHaveBeenCalledTimes(5));
    expect(sendMessage.mock.calls[4][1].idempotencyKey).not.toBe(sendMessage.mock.calls[3][1].idempotencyKey);
  });

  it('routes inventory planning requests to session planning in automatic mode', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const sendMessage = vi.fn().mockResolvedValue({ runId: 'run-plan', goal: 'plan', status: 'CREATED', createdAtUnixMs: 1, updatedAtUnixMs: 1 });
    render(<AgentConsolePage api={sessionApi({ sendMessage })} roomId="room-1" />);
    const composer = await screen.findByLabelText('给 Agent 发消息');
    await waitFor(() => expect(composer).toBeEnabled());
    await userEvent.type(composer, '根据我现在的库存，生成一个60分钟的均衡型直播排品方案。');
    const send = screen.getByRole('button', { name: '发送' });
    await waitFor(() => expect(send).toBeEnabled());
    await userEvent.click(send);
    await waitFor(() => expect(sendMessage).toHaveBeenCalledWith('session-1', expect.objectContaining({ runType: 'SESSION_PLAN' }), expect.any(AbortSignal)));
  });

  it('renders a recovered planning run as a compact executable plan instead of editable candidates', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const existing: AgentDraftCandidate = readyCandidateValue();
    existing.candidate = { ...existing.candidate, sourceType: 'EXISTING', existingLotId: 'lot-ocean', existingLotReadVersion: 4, draft: { ...existing.candidate.draft, mainImageAssetId: '', startPriceAmount: 1_000_000, capPriceAmount: 10_000_000, durationSeconds: 3000 } };
    const planned = sessionAggregate({ session: { ...baseSession, status: 'READY_FOR_REVIEW' }, candidates: [existing], lastRunType: 'SESSION_PLAN' });
    render(<AgentConsolePage api={sessionApi({ getSession: vi.fn().mockResolvedValue(planned) })} roomId="room-1" />);
    expect(await screen.findByRole('heading', { name: '直播排品方案' })).toBeInTheDocument();
    expect(screen.getByText('¥10,000')).toBeInTheDocument();
    expect(screen.getAllByText('50 分钟')).toHaveLength(2);
    expect(screen.getByRole('button', { name: '执行并加入队列' })).toBeEnabled();
    expect(screen.getByRole('button', { name: '取消方案' })).toBeEnabled();
    expect(screen.queryByLabelText('海洋之心的介绍')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /生成确认预览/ })).not.toBeInTheDocument();
  });

  it('queues the whole plan and starts its first lot when the room is idle', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const existing: AgentDraftCandidate = readyCandidateValue();
    existing.candidate = { ...existing.candidate, sourceType: 'EXISTING', existingLotId: 'lot-ocean', existingLotReadVersion: 4, draft: { ...existing.candidate.draft, mainImageAssetId: '', mainImageUrl: '' } };
    const planned = sessionAggregate({ session: { ...baseSession, status: 'READY_FOR_REVIEW' }, candidates: [existing], lastRunType: 'SESSION_PLAN' });
    const snapshot = { ...reviewSnapshot(readyCandidateValue()), candidates: [{ ...reviewSnapshot(readyCandidateValue()).candidates[0], sourceType: 'EXISTING' as const, existingLotId: 'lot-ocean', existingLotReadVersion: 4, draft: { ...reviewSnapshot(readyCandidateValue()).candidates[0].draft, mainImageAssetId: '' }, assetIds: [] }] };
    const apply = vi.fn().mockResolvedValue({ executionId: 'plan-execution', createdLotIds: [], queuedLots: [{ lotId: 'lot-ocean', queuePosition: 19 }], target: 'QUEUE' as const });
    const api = sessionApi({ getSession: vi.fn().mockResolvedValue(planned), createReviewSnapshot: vi.fn().mockResolvedValue(snapshot), apply });
    getRoomSnapshot.mockResolvedValue({ currentLot: null });
    startLot.mockResolvedValue({ id: 'lot-ocean', title: '海洋之心' });
    render(<AgentConsolePage api={api} roomId="room-1" />);

    await userEvent.click(await screen.findByRole('button', { name: '执行并加入队列' }));

    await waitFor(() => expect(apply).toHaveBeenCalledWith('session-1', expect.objectContaining({ target: 'QUEUE', selectedCandidateIds: ['candidate-1'], orderedCandidateIds: ['candidate-1'] })));
    expect(startLot).toHaveBeenCalledWith('lot-ocean');
    expect(await screen.findByRole('heading', { name: '海洋之心已开拍，其余拍品排队中' })).toBeInTheDocument();
    expect(screen.getByText('lot-ocean · 队列位置 19')).toBeInTheDocument();
  });

  it('keeps every planned lot queued when another auction is already live', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const existing: AgentDraftCandidate = readyCandidateValue();
    existing.candidate = { ...existing.candidate, sourceType: 'EXISTING', existingLotId: 'lot-ocean', existingLotReadVersion: 4, draft: { ...existing.candidate.draft, mainImageAssetId: '', mainImageUrl: '' } };
    const planned = sessionAggregate({ session: { ...baseSession, status: 'READY_FOR_REVIEW' }, candidates: [existing], lastRunType: 'SESSION_PLAN' });
    const baseSnapshot = reviewSnapshot(readyCandidateValue());
    const snapshot = { ...baseSnapshot, candidates: [{ ...baseSnapshot.candidates[0], sourceType: 'EXISTING' as const, existingLotId: 'lot-ocean', existingLotReadVersion: 4, draft: { ...baseSnapshot.candidates[0].draft, mainImageAssetId: '' }, assetIds: [] }] };
    const apply = vi.fn().mockResolvedValue({ executionId: 'plan-waiting', createdLotIds: [], queuedLots: [{ lotId: 'lot-ocean', queuePosition: 20 }], target: 'QUEUE' as const });
    getRoomSnapshot.mockResolvedValue({ currentLot: { id: 'lot-live', title: '正在竞拍的商品' } });
    render(<AgentConsolePage api={sessionApi({ getSession: vi.fn().mockResolvedValue(planned), createReviewSnapshot: vi.fn().mockResolvedValue(snapshot), apply })} roomId="room-1" />);

    await userEvent.click(await screen.findByRole('button', { name: '执行并加入队列' }));

    expect(await screen.findByRole('heading', { name: '排品方案已加入等待队列' })).toBeInTheDocument();
    expect(startLot).not.toHaveBeenCalled();
  });

  it('keeps descriptive-copy requests in lot-draft mode and exposes an explicit mode selector', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const sendMessage = vi.fn().mockResolvedValue({ runId: 'run-copy', goal: 'copy', status: 'CREATED', createdAtUnixMs: 1, updatedAtUnixMs: 1 });
    render(<AgentConsolePage api={sessionApi({ sendMessage })} roomId="room-1" />);
    expect(await screen.findByRole('radio', { name: /自动识别/ })).toBeChecked();
    const composer = screen.getByLabelText('给 Agent 发消息');
    await waitFor(() => expect(composer).toBeEnabled());
    await userEvent.type(composer, '给海洋之心生成一个适合直播间展示的介绍，风格高端一些。');
    const send = screen.getByRole('button', { name: '发送' });
    await waitFor(() => expect(send).toBeEnabled());
    await userEvent.click(send);
    await waitFor(() => expect(sendMessage).toHaveBeenCalledWith('session-1', expect.objectContaining({ runType: 'LOT_DRAFT_ASSIST' }), expect.any(AbortSignal)));
  });

  it('clears an ambiguous pending admission when recovery proves the user turn was accepted', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const admitted = sessionAggregate({ session: { ...baseSession, version: 3 }, messages: [{ sessionId: 'session-1', seq: 1, role: 'USER', content: '生成方案', runId: 'run-accepted', createdAtUnixMs: 2 }] });
    const api = sessionApi({ sendMessage: vi.fn().mockRejectedValue(new TypeError('lost response')), getSession: vi.fn().mockResolvedValueOnce(sessionAggregate()).mockResolvedValueOnce(admitted) });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    await screen.findByDisplayValue('海洋之心');
    const composer = await screen.findByLabelText('给 Agent 发消息');
    await userEvent.type(composer, '生成方案');
    await userEvent.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(api.sendMessage).toHaveBeenCalledTimes(1));
    await userEvent.click(await screen.findByRole('button', { name: '刷新会话' }));
    await waitFor(() => expect(composer).toHaveValue(''));
  });

  it('shows missing fields and patches exact integer cents and duration', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const missing = sessionAggregate({ candidates: [{ ...baseCandidate, candidate: { ...baseCandidate.candidate, startPriceSpecified: false, draft: { ...baseCandidate.candidate.draft, startPriceAmount: 0, durationSeconds: 0 } }, validationJSON: '{"requiredInputs":["START_PRICE","DURATION"]}' }] });
    const patched = sessionAggregate({ session: { ...baseSession, version: 3 }, candidates: [{ ...baseCandidate, version: 3, candidate: { ...baseCandidate.candidate, draft: { ...baseCandidate.candidate.draft, startPriceAmount: 1000001, durationSeconds: 3000 } }, fieldSourcesJSON: '{"title":"AI","startPriceAmount":"USER","durationSeconds":"USER"}' }] });
    const api = sessionApi({ getSession: vi.fn().mockResolvedValue(missing), patchCandidate: vi.fn().mockResolvedValue(patched) });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    const price = await screen.findByLabelText('海洋之心的起拍价（元）');
    expect(screen.getByText('还需补充：起拍价、竞拍时长')).toBeInTheDocument();
    expect(screen.getByText('其余非必填参数已由 AI 自动补全，可在下方编辑。')).toBeInTheDocument();
    expect(screen.getByText('请填写起拍价')).toBeInTheDocument();
    expect(screen.getByText('请填写竞拍时长')).toBeInTheDocument();
    await userEvent.clear(price); await userEvent.type(price, '10000.01');
    const duration = screen.getByLabelText('海洋之心的竞拍时长（分钟）');
    await userEvent.clear(duration); await userEvent.type(duration, '50');
    await userEvent.click(screen.getByRole('button', { name: '保存海洋之心' }));
    expect(api.patchCandidate).toHaveBeenCalledWith('session-1', 'candidate-1', { expectedSessionVersion: 2, expectedCandidateVersion: 2, fields: expect.objectContaining({ startPriceAmount: 1000001, durationSeconds: 3000 }) }, expect.any(AbortSignal));
    expect(screen.getAllByText('用户填写').length).toBeGreaterThan(0);
  });

  it('distinguishes the only missing required image from AI-filled editable fields', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const missingImage = sessionAggregate({ candidates: [{ ...baseCandidate, validationJSON: '{"requiredInputs":["MAIN_IMAGE"]}' }] });
    render(<AgentConsolePage api={sessionApi({ getSession: vi.fn().mockResolvedValue(missingImage) })} roomId="room-1" />);
    expect(await screen.findByText('还需补充：真实主图')).toBeInTheDocument();
    expect(screen.getByText('其余非必填参数已由 AI 自动补全，可在下方编辑。')).toBeInTheDocument();
  });

  it('shows that required information is complete after the image is bound', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const ready = sessionAggregate({ candidates: [readyCandidateValue()] });
    render(<AgentConsolePage api={sessionApi({ getSession: vi.fn().mockResolvedValue(ready) })} roomId="room-1" />);
    expect(await screen.findByText('必填信息已完整')).toBeInTheDocument();
    expect(screen.getByText('AI 建议参数已自动补全，仍可在下方调整。')).toBeInTheDocument();
  });

  it('renders an explicitly specified zero start price and saves exact zero cents', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const zero = sessionAggregate({ candidates: [{ ...baseCandidate, candidate: { ...baseCandidate.candidate, startPriceSpecified: true, draft: { ...baseCandidate.candidate.draft, startPriceAmount: 100 } } }] });
    const api = sessionApi({ getSession: vi.fn().mockResolvedValue(zero), patchCandidate: vi.fn().mockResolvedValue(zero) });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    const price = await screen.findByLabelText('海洋之心的起拍价（元）');
    expect(price).toHaveValue('1');
    expect(price).toHaveAttribute('aria-invalid', 'false');
    await userEvent.clear(price); await userEvent.type(price, '0.00');
    await userEvent.click(screen.getByRole('button', { name: '保存海洋之心' }));
    expect(api.patchCandidate).toHaveBeenCalledWith('session-1', 'candidate-1', expect.objectContaining({ fields: { startPriceAmount: 0 } }), expect.any(AbortSignal));
  });

  it('patches an emptied start price as null and marks it as missing', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const zero = sessionAggregate({ candidates: [{ ...baseCandidate, candidate: { ...baseCandidate.candidate, startPriceSpecified: true, draft: { ...baseCandidate.candidate.draft, startPriceAmount: 0 } } }] });
    const missing = sessionAggregate({ candidates: [{ ...baseCandidate, candidate: { ...baseCandidate.candidate, startPriceSpecified: false, draft: { ...baseCandidate.candidate.draft, startPriceAmount: 0 } }, validationJSON: '{"requiredInputs":["START_PRICE"]}' }] });
    const api = sessionApi({ getSession: vi.fn().mockResolvedValue(zero), patchCandidate: vi.fn().mockResolvedValue(missing) });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    const price = await screen.findByLabelText('海洋之心的起拍价（元）');
    await userEvent.clear(price);
    expect(price).toHaveAttribute('aria-invalid', 'true');
    await userEvent.click(screen.getByRole('button', { name: '保存海洋之心' }));
    expect(api.patchCandidate).toHaveBeenCalledWith('session-1', 'candidate-1', expect.objectContaining({ fields: { startPriceAmount: null } }), expect.any(AbortSignal));
  });

  it('rejects a zero minimum increment with a field-linked error and focus', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const api = sessionApi(); render(<AgentConsolePage api={api} roomId="room-1" />);
    const increment = await screen.findByLabelText('海洋之心的最低加价（元）');
    await userEvent.clear(increment); await userEvent.type(increment, '0');
    await userEvent.click(screen.getByRole('button', { name: '保存海洋之心' }));
    expect(increment).toHaveFocus(); expect(increment).toHaveAttribute('aria-invalid', 'true');
    expect(document.getElementById(increment.getAttribute('aria-describedby') || '')).toHaveTextContent('最低加价必须大于 0 元');
    expect(api.patchCandidate).not.toHaveBeenCalled();
  });

  it.each([
    ['equal to zero start', 0, '0', false, 0],
    ['lower than start', 1000, '9.99', false, 999],
    ['equal to start', 1000, '10.00', false, 1000],
    ['greater than zero start', 0, '0.01', true, 1],
  ])('requires cap price to be strictly greater: %s', async (_case, startCents, capYuan, valid, capCents) => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const aggregate = sessionAggregate({ candidates: [{ ...baseCandidate, candidate: { ...baseCandidate.candidate, startPriceSpecified: true, draft: { ...baseCandidate.candidate.draft, startPriceAmount: startCents } } }] });
    const api = sessionApi({ getSession: vi.fn().mockResolvedValue(aggregate), patchCandidate: vi.fn().mockResolvedValue(aggregate) });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    const cap = await screen.findByLabelText('海洋之心的封顶价（元）');
    await userEvent.clear(cap); await userEvent.type(cap, capYuan);
    await userEvent.click(screen.getByRole('button', { name: '保存海洋之心' }));
    if (valid) expect(api.patchCandidate).toHaveBeenCalledWith('session-1', 'candidate-1', expect.objectContaining({ fields: { capPriceAmount: capCents } }), expect.any(AbortSignal));
    else { expect(cap).toHaveFocus(); expect(cap).toHaveAttribute('aria-invalid', 'true'); expect(api.patchCandidate).not.toHaveBeenCalled(); }
  });

  it('accepts an absent cap price and patches a cleared value as null', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const api = sessionApi({ patchCandidate: vi.fn().mockResolvedValue(sessionAggregate()) });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    const cap = await screen.findByLabelText('海洋之心的封顶价（元）');
    await userEvent.clear(cap);
    await userEvent.click(screen.getByRole('button', { name: '保存海洋之心' }));
    expect(cap).toHaveAttribute('aria-invalid', 'false');
    expect(api.patchCandidate).toHaveBeenCalledWith('session-1', 'candidate-1', expect.objectContaining({ fields: { capPriceAmount: null } }), expect.any(AbortSignal));
  });

  it('accepts fractional minutes when they represent whole seconds allowed by Go validation', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const api = sessionApi({ patchCandidate: vi.fn().mockResolvedValue(sessionAggregate()) });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    const duration = await screen.findByLabelText('海洋之心的竞拍时长（分钟）');
    await userEvent.clear(duration); await userEvent.type(duration, '1.5');
    await userEvent.click(screen.getByRole('button', { name: '保存海洋之心' }));
    expect(api.patchCandidate).toHaveBeenCalledWith('session-1', 'candidate-1', expect.objectContaining({ fields: { durationSeconds: 90 } }), expect.any(AbortSignal));
  });

  it('edits all anti-snipe and deposit fields with backend names and USER provenance', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const patched = sessionAggregate({ session: { ...baseSession, version: 3 }, candidates: [{ ...baseCandidate, version: 3, candidate: { ...baseCandidate.candidate, draft: { ...baseCandidate.candidate.draft, antiSnipeWindowSeconds: 20, antiSnipeExtendSeconds: 25, maxExtendCount: 8, depositAmount: 12345 } }, fieldSourcesJSON: '{"antiSnipeWindowSeconds":"USER","antiSnipeExtendSeconds":"USER","maxExtendCount":"USER","depositAmount":"USER"}' }] });
    const api = sessionApi({ patchCandidate: vi.fn().mockResolvedValue(patched) });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    const values: Array<[string, string]> = [['海洋之心的反狙击窗口（秒）', '20'], ['海洋之心的每次延长（秒）', '25'], ['海洋之心的最多延长次数', '8'], ['海洋之心的保证金（元）', '123.45']];
    for (const [label, value] of values) { const field = await screen.findByLabelText(label); await userEvent.clear(field); await userEvent.type(field, value); }
    await userEvent.click(screen.getByRole('button', { name: '保存海洋之心' }));
    expect(api.patchCandidate).toHaveBeenCalledWith('session-1', 'candidate-1', expect.objectContaining({ fields: { antiSnipeWindowSeconds: 20, antiSnipeExtendSeconds: 25, maxExtendCount: 8, depositAmount: 12345 } }), expect.any(AbortSignal));
    expect(screen.getAllByText('用户填写').length).toBeGreaterThanOrEqual(4);
  });

  it('uploads and binds an image using current CAS versions', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    uploadImage.mockResolvedValue({ id: 'asset-9', imageUrl: 'https://cdn.test/ocean.jpg' });
    const bound = sessionAggregate({ session: { ...baseSession, version: 3 }, candidates: [{ ...baseCandidate, version: 3, candidate: { ...baseCandidate.candidate, draft: { ...baseCandidate.candidate.draft, mainImageAssetId: 'asset-9', mainImageUrl: 'https://cdn.test/ocean.jpg' } } }] });
    const api = sessionApi({ bindCandidateImage: vi.fn().mockResolvedValue(bound) });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    const file = new File(['image'], 'ocean.png', { type: 'image/png' });
    fireEvent.change(await screen.findByLabelText('上传海洋之心的主图'), { target: { files: [file] } });
    await waitFor(() => expect(uploadImage).toHaveBeenCalledWith(file, { roomId: 'room-1', bizType: 'agent_lot_image' }));
    expect(api.bindCandidateImage).toHaveBeenCalledWith('session-1', 'candidate-1', { expectedSessionVersion: 2, expectedCandidateVersion: 2, assetId: 'asset-9' }, expect.any(AbortSignal));
    expect(await screen.findByRole('img', { name: '海洋之心主图' })).toHaveAttribute('src', 'https://cdn.test/ocean.jpg');
  });

  it('refreshes on version conflict and explains the recovery', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const latest = sessionAggregate({ session: { ...baseSession, version: 9 }, candidates: [{ ...baseCandidate, version: 6 }] });
    const api = sessionApi({ patchCandidate: vi.fn().mockRejectedValue(new Error('Agent session version conflict')), getSession: vi.fn().mockResolvedValueOnce(sessionAggregate()).mockResolvedValueOnce(latest) });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    await screen.findByDisplayValue('海洋之心');
    await userEvent.type(screen.getByLabelText('海洋之心的介绍'), ' 更新');
    await userEvent.click(screen.getByRole('button', { name: '保存海洋之心' }));
    expect(await screen.findByText('内容已在其他位置更新，已载入最新版本，请确认后重试。')).toBeInTheDocument();
    expect(api.getSession).toHaveBeenCalledTimes(2);
  });

  it('deduplicates resumed events, refreshes terminal state, and cancels on unmount', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    let options: Parameters<AgentApi['streamSessionEvents']>[1] | undefined;
    const api = sessionApi({ streamSessionEvents: vi.fn(async (_id, next) => { options = next; await new Promise<void>(() => undefined); }), getSession: vi.fn().mockResolvedValue(sessionAggregate({ activeRun: { runId: 'run-1', goal: '生成', status: 'PLANNING', createdAtUnixMs: 1, updatedAtUnixMs: 1 } })) });
    const view = render(<AgentConsolePage api={api} roomId="room-1" />);
    await waitFor(() => expect(options).toBeDefined());
    act(() => { options?.onEvent(event(1, 'tool.completed', '库存读取完成')); options?.onEvent(event(1, 'tool.completed', '重复事件')); options?.onEvent(event(2, 'run.completed', '生成完成')); });
    expect(screen.getAllByText('库存读取完成')).toHaveLength(1);
    expect(screen.queryByText('重复事件')).not.toBeInTheDocument();
    await waitFor(() => expect(api.getSession).toHaveBeenCalledTimes(2));
    view.unmount(); expect(options?.signal.aborted).toBe(true);
  });

  it('provides accessible mobile candidate navigation and labelled input', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    render(<AgentConsolePage api={sessionApi()} roomId="room-1" />);
    expect(await screen.findByRole('navigation', { name: '工作区导航' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /待创建拍品/ })).toHaveAttribute('href', '#agent-candidates');
    expect(screen.getByLabelText('给 Agent 发消息')).toHaveAttribute('aria-describedby');
  });

  it('marks missing required fields invalid and connects them to live errors', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const missing = sessionAggregate({ candidates: [{ ...baseCandidate, candidate: { ...baseCandidate.candidate, startPriceSpecified: false, draft: { ...baseCandidate.candidate.draft, title: '', startPriceAmount: 0, durationSeconds: 0 } }, validationJSON: '{"requiredInputs":["TITLE","START_PRICE","DURATION"]}' }] });
    render(<AgentConsolePage api={sessionApi({ getSession: vi.fn().mockResolvedValue(missing) })} roomId="room-1" />);
    const price = await screen.findByLabelText('未命名拍品的起拍价（元）');
    expect(price).toHaveAttribute('aria-invalid', 'true');
    expect(price).toHaveAttribute('aria-describedby');
    expect(document.getElementById(price.getAttribute('aria-describedby') || '')).toHaveTextContent('请填写起拍价');
  });

  it('focuses the first invalid field after an attempted candidate save', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    render(<AgentConsolePage api={sessionApi()} roomId="room-1" />);
    const title = await screen.findByLabelText('海洋之心的拍品名称');
    await userEvent.clear(title);
    await userEvent.click(screen.getByRole('button', { name: '保存未命名拍品' }));
    await waitFor(() => expect(title).toHaveFocus());
    expect(screen.getAllByRole('alert').some((alert) => alert.textContent === '请填写拍品名称')).toBe(true);
  });

  it('reports every invalid candidate field inline, focuses the first, and clears corrected errors', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    render(<AgentConsolePage api={sessionApi()} roomId="room-1" />);
    const values: Array<[string, string]> = [
      ['海洋之心的起拍价（元）', 'abc'], ['海洋之心的反狙击窗口（秒）', '0'], ['海洋之心的每次延长（秒）', '31'], ['海洋之心的最多延长次数', '-1'], ['海洋之心的保证金（元）', '-0.01'],
    ];
    for (const [label, value] of values) { const field = await screen.findByLabelText(label); await userEvent.clear(field); await userEvent.type(field, value); }
    await userEvent.click(screen.getByRole('button', { name: '保存海洋之心' }));
    const price = screen.getByLabelText('海洋之心的起拍价（元）');
    expect(price).toHaveFocus();
    for (const [label] of values) { const field = screen.getByLabelText(label); expect(field).toHaveAttribute('aria-invalid', 'true'); expect(document.getElementById(field.getAttribute('aria-describedby') || '')).toHaveAttribute('role', 'alert'); }
    const deposit = screen.getByLabelText('海洋之心的保证金（元）');
    await userEvent.clear(deposit); await userEvent.type(deposit, '0.01');
    expect(deposit).toHaveAttribute('aria-invalid', 'false');
  });

  it('loads the exact session selected by browser history navigation', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const second = sessionAggregate({ session: { ...baseSession, id: 'session-2', version: 4 }, messages: [{ sessionId: 'session-2', seq: 1, role: 'USER', content: '第二个会话', runId: 'run-2', createdAtUnixMs: 2 }] });
    const api = sessionApi({ getSession: vi.fn().mockResolvedValueOnce(sessionAggregate()).mockResolvedValueOnce(second) });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    await screen.findByDisplayValue('海洋之心');
    history.pushState(null, '', '/admin/agent?session=session-2');
    act(() => window.dispatchEvent(new PopStateEvent('popstate')));
    expect(await screen.findByText('第二个会话')).toBeInTheDocument();
    expect(api.getSession).toHaveBeenLastCalledWith('session-2', expect.any(AbortSignal));
  });

  it('creates for the current room when popstate has no session and ignores stale rapid navigation', async () => {
    history.replaceState(null, '', '/admin/agent?session=session-1');
    const slow = deferred<AgentSessionAggregate>();
    const fast = deferred<AgentSessionAggregate>();
    const api = sessionApi({ getSession: vi.fn((id: string) => id === 'session-1' ? Promise.resolve(sessionAggregate()) : id === 'session-2' ? slow.promise : fast.promise) });
    render(<AgentConsolePage api={api} roomId="room-1" />);
    await screen.findByDisplayValue('海洋之心');
    history.pushState(null, '', '/admin/agent?session=session-2'); act(() => window.dispatchEvent(new PopStateEvent('popstate')));
    history.pushState(null, '', '/admin/agent?session=session-3'); act(() => window.dispatchEvent(new PopStateEvent('popstate')));
    fast.resolve(sessionAggregate({ session: { ...baseSession, id: 'session-3' }, messages: [{ sessionId: 'session-3', seq: 1, role: 'USER', content: '最新会话', runId: '', createdAtUnixMs: 1 }] }));
    expect(await screen.findByText('最新会话')).toBeInTheDocument();
    slow.resolve(sessionAggregate({ session: { ...baseSession, id: 'session-2' }, messages: [{ sessionId: 'session-2', seq: 1, role: 'USER', content: '过期会话', runId: '', createdAtUnixMs: 1 }] }));
    await act(async () => { await Promise.resolve(); });
    expect(screen.queryByText('过期会话')).not.toBeInTheDocument();
    history.pushState(null, '', '/admin/agent'); act(() => window.dispatchEvent(new PopStateEvent('popstate')));
    await waitFor(() => expect(api.createSession).toHaveBeenCalledWith('room-1', expect.any(AbortSignal)));
    expect(location.search).toBe('?session=session-1');
  });
});

describe('money conversion', () => {
  it.each([['0.01', 1], ['10000.01', 1_000_001], ['1', 100], ['1.2', 120]])('parses %s yuan without drift', (yuan, cents) => { expect(parseYuanToCents(yuan)).toBe(cents); expect(formatYuanFromCents(cents)).toBe(yuan); });
  it.each(['1.001', '-1', 'NaN', ''])('rejects invalid yuan %s', (yuan) => expect(() => parseYuanToCents(yuan)).toThrow());
});

describe('session SSE transport', () => {
  it('authenticates, resumes globally, ignores heartbeat, and reconnects', async () => {
    const requests: Request[] = [];
    const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => { requests.push(new Request(input, init)); return requests.length === 1 ? sse(': heartbeat\n\nid: 3\nevent: agent.run.event\ndata: {"runId":"run-1","seq":3,"type":"tool.completed","message":"ok","payload":{}}\n\n') : sse('id: 4\nevent: agent.run.event\ndata: {"runId":"run-1","seq":4,"type":"run.completed","message":"done","payload":{}}\n\n'); });
    const api = createAgentApi({ fetcher, getAccessToken: async () => 'token', wait: async () => undefined });
    const events: number[] = [];
    await api.streamSessionEvents('session-1', { afterSeq: 2, signal: new AbortController().signal, onEvent: (item) => events.push(item.seq) });
    expect(events).toEqual([3, 4]);
    expect(new URL(requests[0].url).searchParams.get('after_seq')).toBe('2'); expect(new URL(requests[1].url).searchParams.get('after_seq')).toBe('3');
    expect(requests[1].headers.get('Last-Event-ID')).toBe('3'); expect(requests[0].headers.get('Authorization')).toBe('Bearer token');
  });

  it('replays historical runs without closing the active run and resumes the exact global cursor', async () => {
    const requests: Request[] = [];
    const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(new Request(input, init));
      if (requests.length === 1) return sse('id: 1\nevent: agent.run.event\ndata: {"runId":"run-old","seq":1,"type":"run.completed","message":"旧轮完成","payload":{}}\n\nid: 2\nevent: agent.run.event\ndata: {"runId":"run-current","seq":2,"type":"tool.completed","message":"当前进度","payload":{}}\n\n');
      return sse('id: 3\nevent: agent.run.event\ndata: {"runId":"run-current","seq":3,"type":"run.completed","message":"当前完成","payload":{}}\n\n');
    });
    const api = createAgentApi({ fetcher, getAccessToken: async () => 'token', wait: async () => undefined });
    const events: string[] = [];
    await api.streamSessionEvents('session-1', { activeRunId: 'run-current', signal: new AbortController().signal, onEvent: (item) => events.push(`${item.seq}:${item.runId}`) });
    expect(events).toEqual(['1:run-old', '2:run-current', '3:run-current']);
    expect(new URL(requests[1].url).searchParams.get('after_seq')).toBe('2');
    expect(requests[1].headers.get('Last-Event-ID')).toBe('2');
  });

  it('retries a retriable safe error with the same cursor but stops on a permanent error', async () => {
    const seen: string[] = []; let attempt = 0; const waits: number[] = [];
    const retrying = createAgentApi({ fetcher: vi.fn(async (input) => { seen.push(new URL(String(input)).searchParams.get('after_seq') || ''); attempt += 1; return attempt === 1 ? sse('event: agent.run.error\ndata: {"code":"AGENT_EVENT_STREAM_UNAVAILABLE","message":"稍后重试","retriable":true}\n\n') : sse('id: 1\nevent: agent.run.event\ndata: {"runId":"run-1","seq":1,"type":"run.completed","message":"完成","payload":{}}\n\n'); }), getAccessToken: async () => 'token', wait: async (ms) => { waits.push(ms); } });
    await retrying.streamSessionEvents('session-1', { activeRunId: 'run-1', signal: new AbortController().signal, onEvent: () => undefined });
    expect(seen).toEqual(['0', '0']); expect(waits).toHaveLength(1);

    const permanent = createAgentApi({ fetcher: vi.fn(async () => sse('event: agent.run.error\ndata: {"code":"AGENT_EVENT_STREAM_PROTOCOL_ERROR","message":"安全拒绝","retriable":false}\n\n')), getAccessToken: async () => 'token', wait: async () => undefined });
    await expect(permanent.streamSessionEvents('session-1', { activeRunId: 'run-1', signal: new AbortController().signal, onEvent: () => undefined })).rejects.toThrow('安全拒绝');
  });

  it.each([
    ['malformed JSON', 'id: 1\nevent: agent.run.event\ndata: not-json\n\n', '无效 JSON'],
    ['a global sequence gap', 'id: 2\nevent: agent.run.event\ndata: {"runId":"run-1","seq":2,"type":"run.completed","message":"gap","payload":{}}\n\n', '序号'],
  ])('rejects %s as a permanent protocol error', async (_name, body, expected) => {
    const api = createAgentApi({ fetcher: vi.fn(async () => sse(body)), getAccessToken: async () => 'token', wait: async () => undefined });
    await expect(api.streamSessionEvents('session-1', { activeRunId: 'run-1', signal: new AbortController().signal, onEvent: () => undefined })).rejects.toThrow(expected);
  });

  it('ignores duplicate global events and parses chunked heartbeat frames', async () => {
    const encoder = new TextEncoder();
    const body = new ReadableStream<Uint8Array>({ start(controller) { controller.enqueue(encoder.encode(': heart')); controller.enqueue(encoder.encode('beat\n\nid: 1\nevent: agent.run.event\ndata: {"runId":"run-1","seq":1,"type":"tool.completed","message":"一次","payload":{}}\n\nid: 1\nevent: agent.run.event\ndata: {"runId":"run-1","seq":1,"type":"tool.completed","message":"重复","payload":{}}\n\nid: 2\nevent: agent.run.event\ndata: {"runId":"run-1","seq":2,"type":"run.completed","message":"完成","payload":{}}\n\n')); } });
    const events: string[] = [];
    const api = createAgentApi({ fetcher: vi.fn(async () => new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })), getAccessToken: async () => 'token', wait: async () => undefined });
    await api.streamSessionEvents('session-1', { activeRunId: 'run-1', signal: new AbortController().signal, onEvent: (item) => events.push(item.message) });
    expect(events).toEqual(['一次', '完成']);
  });

  it('surfaces a browser-safe server stream error without reconnecting', async () => {
    const fetcher = vi.fn(async () => sse('event: agent.run.error\ndata: {"message":"会话状态暂时不可用"}\n\n'));
    const api = createAgentApi({ fetcher, getAccessToken: async () => 'token', wait: async () => undefined });
    await expect(api.streamSessionEvents('session-1', { signal: new AbortController().signal, onEvent: () => undefined })).rejects.toThrow('会话状态暂时不可用');
    expect(fetcher).toHaveBeenCalledTimes(1);
  });
});

describe('agent console routing regressions', () => {
  it('keeps the admin/host aliases and permission gate', () => {
    expect(isAgentConsolePath('/admin/agent')).toBe(true); expect(isAgentConsolePath('/host/agent')).toBe(true); expect(isAgentConsolePath('/admin/agents')).toBe(false);
    expect(canNavigateToAgentConsole({ permissionCodes: ['lot.view_admin'] })).toBe(true); expect(canNavigateToAgentConsole({ permissionCodes: ['order.manage'] })).toBe(false);
  });
});

const baseSession = { id: 'session-1', roomId: 'room-1', status: 'COLLECTING_INPUT' as const, version: 2, activeSnapshotVersion: 0, conversationSummary: '', createdAtUnixMs: 1, updatedAtUnixMs: 1 };
const approvalFields = ['antiSnipeExtendSeconds', 'antiSnipeWindowSeconds', 'capPriceAmount', 'category', 'depositAmount', 'description', 'durationSeconds', 'mainImageAssetId', 'maxExtendCount', 'minIncrementAmount', 'startPriceAmount', 'stock', 'tags', 'title'];
const completeSources = Object.fromEntries(approvalFields.map((field) => [field, field === 'description' || field === 'title' ? 'AI' : field === 'durationSeconds' ? 'USER' : 'SYSTEM']));
const baseCandidate = { candidate: { id: 'candidate-1', sourceType: 'NEW' as const, startPriceSpecified: true, draft: { title: '海洋之心', startPriceAmount: 1_000_000, durationSeconds: 3000, mainImageAssetId: '', mainImageUrl: '', minIncrementAmount: 10_000, capPriceAmount: 10_000_000, category: '珠宝', description: 'AI 生成介绍', tags: ['宝石'], stock: 1, antiSnipeWindowSeconds: 10, antiSnipeExtendSeconds: 15, maxExtendCount: 5, depositAmount: 0 } }, position: 1, selectedByDefault: true, fieldSourcesJSON: JSON.stringify(completeSources), validationJSON: '{}', provenanceVersion: 1, version: 2, createdAtUnixMs: 1, updatedAtUnixMs: 1 };
function readyCandidateValue() { return { ...baseCandidate, candidate: { ...baseCandidate.candidate, draft: { ...baseCandidate.candidate.draft, mainImageAssetId: 'asset-1', mainImageUrl: 'https://img/asset-1.webp' } }, validationJSON: '{"requiredInputs":[],"errors":[]}' }; }
function reviewSnapshot(item: ReturnType<typeof readyCandidateValue>) { const { mainImageUrl: _url, ...draft } = item.candidate.draft; return { snapshotVersion: 5, contentHash: 'a'.repeat(64), candidates: [{ candidateId: item.candidate.id, candidateVersion: item.version, sourceType: item.candidate.sourceType, draft, fieldSources: approvalFields.map((field) => ({ field: field as keyof typeof draft, source: completeSources[field] as 'USER' | 'AI' | 'SYSTEM' })), assetIds: [draft.mainImageAssetId] }] }; }
function sessionAggregate(overrides: Partial<AgentSessionAggregate> = {}): AgentSessionAggregate { return { session: baseSession, messages: [], candidates: [baseCandidate], activeRun: null, appliedExecution: null, ...overrides }; }
function sessionApi(overrides: Partial<AgentApi> = {}): AgentApi { return { createSession: vi.fn().mockResolvedValue(baseSession), getSession: vi.fn().mockResolvedValue(sessionAggregate()), sendMessage: vi.fn(), cancelSession: vi.fn(), patchCandidate: vi.fn(), bindCandidateImage: vi.fn(), createReviewSnapshot: vi.fn(), getReviewSnapshot: vi.fn(), apply: vi.fn(), streamSessionEvents: vi.fn(async () => undefined), ...overrides }; }
function event(seq: number, type: string, message: string): AgentRunEvent { return { runId: 'run-1', seq, type, message, payload: {} }; }
function sse(body: string) { return new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }); }
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>((next) => { resolve = next; }); return { promise, resolve }; }
