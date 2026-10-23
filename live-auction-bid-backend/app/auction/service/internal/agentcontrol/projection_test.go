package agentcontrol

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	agentv1 "live-auction-bid/backend/api/agent/v1"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

func TestCreateRunIdempotentStartsRuntimeOnceForConcurrentRetries(t *testing.T) {
	repo := newProjectionMemoryRepository()
	runtime := &projectionRuntime{startRelease: make(chan struct{})}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(func() { service.Close() })
	actor := Actor{TenantID: "merchant-a", OperatorID: "operator-a"}

	type result struct {
		run Run
		err error
	}
	results := make(chan result, 2)
	go func() {
		run, createErr := service.CreateRunIdempotent(context.Background(), actor, "retry-key-0001", " 清理库存 ")
		results <- result{run: run, err: createErr}
	}()
	runtime.waitForStarts(t, 1)
	go func() {
		run, createErr := service.CreateRunIdempotent(context.Background(), actor, "retry-key-0001", "清理库存")
		results <- result{run: run, err: createErr}
	}()

	second := <-results
	close(runtime.startRelease)
	first := <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("CreateRunIdempotent() errors = %v, %v", first.err, second.err)
	}
	if first.run.ID == "" || first.run.ID != second.run.ID {
		t.Fatalf("retry run IDs = %q, %q", first.run.ID, second.run.ID)
	}
	if got := runtime.startCount(); got != 1 {
		t.Fatalf("Runtime StartRun calls = %d, want 1", got)
	}
}

func TestCreateRunIdempotentRejectsSameKeyWithDifferentNormalizedGoal(t *testing.T) {
	repo := newProjectionMemoryRepository()
	runtime := &projectionRuntime{}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(func() { service.Close() })
	actor := Actor{TenantID: "merchant-a", OperatorID: "operator-a"}
	if _, err := service.CreateRunIdempotent(context.Background(), actor, "retry-key-0002", "清理库存"); err != nil {
		t.Fatalf("first CreateRunIdempotent() error = %v", err)
	}

	_, err = service.CreateRunIdempotent(context.Background(), actor, "retry-key-0002", "生成新品场")
	if !errors.Is(err, apperr.ErrConflict) {
		t.Fatalf("conflicting CreateRunIdempotent() error = %v, want conflict", err)
	}
	if got := runtime.startCount(); got != 1 {
		t.Fatalf("Runtime StartRun calls = %d, want 1", got)
	}
}

func TestAmbiguousStartResponseKeepsRunProjectableUntilDurableRuntimeCompletes(t *testing.T) {
	repo := newProjectionMemoryRepository()
	runtime := &projectionRuntime{
		startErrors: []error{status.Error(codes.Unavailable, "response lost after durable accept")},
		streams: [][]RuntimeEvent{{
			{RunID: "fixed-ambiguous", Seq: 1, Type: "run.created", Status: StatusCreated, PayloadJSON: `{}`, CreatedAtUnixMs: 101},
			{RunID: "fixed-ambiguous", Seq: 2, Type: "tool.completed", Status: StatusCollectingContext, PayloadJSON: `{"tool":"inventory.query"}`, CreatedAtUnixMs: 102},
			{RunID: "fixed-ambiguous", Seq: 3, Type: "run.completed", Status: StatusCompleted, PayloadJSON: `{}`, CreatedAtUnixMs: 103},
		}},
		getRun: RuntimeRun{RunID: "fixed-ambiguous", Status: StatusCompleted, ProposalJSON: `{"summary":"done"}`, UpdatedAtUnixMs: 103},
	}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(func() { service.Close() })
	service.idGenerator = func(string) (string, error) { return "fixed-ambiguous", nil }
	service.traceGenerator = func() (string, error) { return "11111111111111111111111111111111", nil }
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	actor := Actor{TenantID: "merchant-a", OperatorID: "operator-a"}
	run, err := service.CreateRunIdempotent(context.Background(), actor, "retry-key-ambiguous", "清理库存")
	if err != nil {
		t.Fatalf("ambiguous CreateRunIdempotent() error = %v", err)
	}
	if run.Status != StatusCreated || run.ProjectionComplete {
		t.Fatalf("ambiguous start was made terminal: %+v", run)
	}
	retry, err := service.CreateRunIdempotent(context.Background(), actor, "retry-key-ambiguous", "清理库存")
	if err != nil || retry.ID != run.ID {
		t.Fatalf("idempotent retry = %+v, %v; want run %q", retry, err, run.ID)
	}
	projected := repo.waitForProjectionComplete(t, run.ID)
	if projected.Status != StatusCompleted || projected.LastRuntimeSourceSeq != 3 {
		t.Fatalf("ambiguous projection = %+v", projected)
	}
	if got := runtime.startCount(); got != 1 {
		t.Fatalf("Runtime StartRun calls = %d, want 1", got)
	}
}

func TestProjectorReconcilesRuntimeNotFoundWithSameDurableRunSpec(t *testing.T) {
	repo := newProjectionMemoryRepository()
	repo.runs["run-reconcile"] = Run{
		ID: "run-reconcile", TenantID: "merchant-a", OperatorID: "operator-a", TraceID: "11111111111111111111111111111111",
		Goal: "清理库存", IdempotencyKey: "retry-key-reconcile", Status: StatusCreated, Version: 1,
	}
	runtime := &projectionRuntime{
		streamErrors: []error{status.Error(codes.NotFound, "run not found"), nil},
		streams: [][]RuntimeEvent{nil, {
			{RunID: "run-reconcile", Seq: 1, Type: "run.created", Status: StatusCreated, PayloadJSON: `{}`, CreatedAtUnixMs: 201},
			{RunID: "run-reconcile", Seq: 2, Type: "run.completed", Status: StatusCompleted, PayloadJSON: `{}`, CreatedAtUnixMs: 202},
		}},
		getRun: RuntimeRun{RunID: "run-reconcile", Status: StatusCompleted, ProposalJSON: `{"summary":"reconciled"}`, UpdatedAtUnixMs: 202},
	}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(func() { service.Close() })
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	projected := repo.waitForProjectionComplete(t, "run-reconcile")
	if projected.Status != StatusCompleted || projected.ProposalJSON != `{"summary":"reconciled"}` {
		t.Fatalf("reconciled projection = %+v", projected)
	}
	if got := runtime.startCount(); got != 1 {
		t.Fatalf("reconcile StartRun calls = %d, want 1", got)
	}
	request := runtime.lastStart()
	if request.RunID != "run-reconcile" || request.Goal != "清理库存" || request.Context.TenantID != "merchant-a" || request.Context.OperatorID != "operator-a" || request.Context.TraceID != "11111111111111111111111111111111" {
		t.Fatalf("reconcile request lost durable spec: %+v", request)
	}
}

func TestProjectorReconcilesMissingSessionRuntimeWithExactDurableSpec(t *testing.T) {
	repo := newProjectionMemoryRepository()
	run := Run{
		ID: "run-session-reconcile", TenantID: "merchant-a", OperatorID: "operator-a", TraceID: "11111111111111111111111111111111",
		Goal: "Continue agent session", IdempotencyKey: "turn-reconcile", Status: StatusCreated, Version: 1,
		RunType: agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST, SessionID: "session-1", SessionVersion: 7,
		TurnInputJSON: `{"message":"draft two lots","candidate_patches":[]}`,
	}
	repo.runs[run.ID] = run
	runtime := &projectionRuntime{}
	manager := newProjectionManager(repo, runtime)

	if stop := manager.reconcileMissingRuntime(run); stop {
		t.Fatal("missing session runtime reconciliation stopped")
	}
	if stop := manager.reconcileMissingRuntime(run); stop {
		t.Fatal("idempotent existing session runtime reconciliation stopped")
	}
	if got := runtime.startCount(); got != 2 {
		t.Fatalf("StartRun calls = %d, want 2", got)
	}
	for _, request := range runtime.startRequests {
		if request.RunType != run.RunType || request.SessionID != run.SessionID || request.SessionVersion != run.SessionVersion || request.TurnInputJSON != run.TurnInputJSON {
			t.Fatalf("reconcile request did not replay immutable session spec: %+v", request)
		}
	}
}

func TestProjectorPersistsRuntimeEventsAndTerminalRunBeforeAdvancingCheckpoint(t *testing.T) {
	repo := newProjectionMemoryRepository()
	runtime := &projectionRuntime{
		streams: [][]RuntimeEvent{{
			{RunID: "fixed-by-start", Seq: 1, Type: "run.created", Status: StatusCreated, PayloadJSON: `{}`, CreatedAtUnixMs: 101},
			{RunID: "fixed-by-start", Seq: 2, Type: "tool.completed", Status: StatusCollectingContext, PayloadJSON: `{"tool":"inventory.query"}`, CreatedAtUnixMs: 102},
			{RunID: "fixed-by-start", Seq: 3, Type: "run.completed", Status: StatusCompleted, PayloadJSON: `{}`, CreatedAtUnixMs: 103},
		}},
		getRun: RuntimeRun{RunID: "fixed-by-start", Status: StatusCompleted, ProposalJSON: `{"summary":"done"}`, UpdatedAtUnixMs: 103},
	}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(func() { service.Close() })
	service.idGenerator = func(string) (string, error) { return "fixed-by-start", nil }
	service.traceGenerator = func() (string, error) { return "11111111111111111111111111111111", nil }
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	run, err := service.CreateRunIdempotent(context.Background(), Actor{TenantID: "merchant-a", OperatorID: "operator-a"}, "retry-key-0003", "清理库存")
	if err != nil {
		t.Fatalf("CreateRunIdempotent() error = %v", err)
	}
	if run.ID != "fixed-by-start" {
		t.Fatalf("run ID = %q", run.ID)
	}

	projected := repo.waitForProjectionComplete(t, run.ID)
	if projected.Status != StatusCompleted || projected.ProposalJSON != `{"summary":"done"}` || projected.LastRuntimeSourceSeq != 3 {
		t.Fatalf("projected run = %+v", projected)
	}
	events, err := repo.ListEvents(context.Background(), run.ID, 0, 10)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("durable events = %+v", events)
	}
	for index, event := range events {
		wantSource := int64(index + 1)
		if event.Seq != wantSource || event.RuntimeSourceSeq != wantSource {
			t.Fatalf("event[%d] seq/source = %d/%d, want %d/%d", index, event.Seq, event.RuntimeSourceSeq, wantSource, wantSource)
		}
	}
	if repo.checkpointAdvancedBeforeEvent {
		t.Fatal("runtime checkpoint advanced before durable event append")
	}
}

func TestServiceStartResumesExactlyOneProjectorPerUnfinishedRun(t *testing.T) {
	repo := newProjectionMemoryRepository()
	repo.runs["run-restart"] = Run{
		ID: "run-restart", TenantID: "merchant-a", OperatorID: "operator-a", TraceID: "11111111111111111111111111111111",
		Goal: "清理库存", Status: StatusPlanning, Version: 2, LastRuntimeSourceSeq: 1,
	}
	repo.events["run-restart"] = []Event{{RunID: "run-restart", Seq: 1, RuntimeSourceSeq: 1, EventType: "run.created"}}
	runtime := &projectionRuntime{
		streams: [][]RuntimeEvent{{
			{RunID: "run-restart", Seq: 2, Type: "run.failed", Status: StatusFailed, PayloadJSON: `{"reason":"runtime_restarted"}`, CreatedAtUnixMs: 202},
		}},
		getRun: RuntimeRun{RunID: "run-restart", Status: StatusFailed, ErrorCode: "AGENT_RUNTIME_RESTARTED", UpdatedAtUnixMs: 202},
	}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(func() { service.Close() })
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}

	projected := repo.waitForProjectionComplete(t, "run-restart")
	if projected.Status != StatusFailed || projected.LastRuntimeSourceSeq != 2 {
		t.Fatalf("resumed projection = %+v", projected)
	}
	if got := runtime.streamCount(); got != 1 {
		t.Fatalf("runtime stream calls = %d, want 1", got)
	}
}

func TestTerminalSnapshotBeforeRestartStillReplaysEveryDurableEvent(t *testing.T) {
	repo := newProjectionMemoryRepository()
	repo.runs["run-terminal-restart"] = Run{
		ID: "run-terminal-restart", TenantID: "merchant-a", OperatorID: "operator-a", TraceID: "11111111111111111111111111111111",
		Goal: "清理库存", Status: StatusCreated, Version: 1,
	}
	runtime := &projectionRuntime{
		streams: [][]RuntimeEvent{{
			{RunID: "run-terminal-restart", Seq: 1, Type: "run.created", Status: StatusCreated, PayloadJSON: `{}`, CreatedAtUnixMs: 301},
			{RunID: "run-terminal-restart", Seq: 2, Type: "tool.completed", Status: StatusCollectingContext, PayloadJSON: `{"tool":"inventory.query"}`, CreatedAtUnixMs: 302},
			{RunID: "run-terminal-restart", Seq: 3, Type: "run.completed", Status: StatusCompleted, PayloadJSON: `{}`, CreatedAtUnixMs: 303},
		}},
		getRun: RuntimeRun{RunID: "run-terminal-restart", Status: StatusCompleted, ProposalJSON: `{"summary":"keep after restart"}`, UpdatedAtUnixMs: 303},
	}
	first, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("first NewService() error = %v", err)
	}
	actor := Actor{TenantID: "merchant-a", OperatorID: "operator-a"}
	snapshot, err := first.GetRun(context.Background(), actor, "run-terminal-restart")
	if err != nil {
		t.Fatalf("GetRun() terminal snapshot error = %v", err)
	}
	if snapshot.Status != StatusCompleted || snapshot.ProjectionComplete || snapshot.ProposalJSON != `{"summary":"keep after restart"}` {
		t.Fatalf("terminal snapshot = %+v", snapshot)
	}
	if events := repo.events[snapshot.ID]; len(events) != 0 {
		t.Fatalf("events persisted before projector = %+v", events)
	}
	first.Close()

	restarted, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("restarted NewService() error = %v", err)
	}
	t.Cleanup(func() { restarted.Close() })
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatalf("restarted Start() error = %v", err)
	}
	projected := repo.waitForProjectionComplete(t, snapshot.ID)
	if projected.Status != StatusCompleted || projected.LastRuntimeSourceSeq != 3 || projected.ProposalJSON != `{"summary":"keep after restart"}` {
		t.Fatalf("restarted projection = %+v", projected)
	}

	stream, err := restarted.StreamRunEvents(context.Background(), actor, snapshot.ID, 0)
	if err != nil {
		t.Fatalf("StreamRunEvents() error = %v", err)
	}
	defer stream.Close()
	var eventTypes []string
	for {
		event, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatalf("durable stream Recv() error = %v", recvErr)
		}
		eventTypes = append(eventTypes, event.Type)
	}
	wantTypes := []string{"run.created", "tool.completed", "run.completed"}
	if len(eventTypes) != len(wantTypes) {
		t.Fatalf("durable event types = %v, want %v", eventTypes, wantTypes)
	}
	for index := range wantTypes {
		if eventTypes[index] != wantTypes[index] {
			t.Fatalf("durable event types = %v, want %v", eventTypes, wantTypes)
		}
	}
}

func TestServiceCloseCancelsAndWaitsForProjectors(t *testing.T) {
	repo := newProjectionMemoryRepository()
	runtime := &projectionRuntime{blockStream: true}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := service.CreateRunIdempotent(context.Background(), Actor{TenantID: "merchant-a", OperatorID: "operator-a"}, "retry-key-0004", "清理库存"); err != nil {
		t.Fatalf("CreateRunIdempotent() error = %v", err)
	}
	runtime.waitForStreams(t, 1)

	done := make(chan struct{})
	go func() { service.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close() did not wait for projector cancellation")
	}
	if runtime.activeStreams() != 0 {
		t.Fatalf("active runtime streams = %d, want 0", runtime.activeStreams())
	}
}

func TestProjectorRefetchesVersionBeforePersistingPermanentProtocolFailure(t *testing.T) {
	repo := newProjectionMemoryRepository()
	runtime := &projectionRuntime{streams: [][]RuntimeEvent{{
		{RunID: "fixed-protocol", Seq: 1, Type: "run.planning", Status: StatusPlanning, PayloadJSON: `{}`, CreatedAtUnixMs: 101},
		{RunID: "wrong-run", Seq: 2, Type: "run.planning", Status: StatusPlanning, PayloadJSON: `{}`, CreatedAtUnixMs: 102},
	}}}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(func() { service.Close() })
	service.idGenerator = func(string) (string, error) { return "fixed-protocol", nil }
	service.traceGenerator = func() (string, error) { return "11111111111111111111111111111111", nil }
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := service.CreateRunIdempotent(context.Background(), Actor{TenantID: "merchant-a", OperatorID: "operator-a"}, "retry-key-protocol", "清理库存"); err != nil {
		t.Fatalf("CreateRunIdempotent() error = %v", err)
	}

	projected := repo.waitForProjectionComplete(t, "fixed-protocol")
	if projected.Status != StatusFailed || projected.LastRuntimeSourceSeq != 1 {
		t.Fatalf("permanent failure projection = %+v", projected)
	}
}

func TestProjectorPermanentFailureOnlyMarksExistingTerminalProjectionComplete(t *testing.T) {
	repo := newProjectionMemoryRepository()
	repo.runs["run-terminal"] = Run{
		ID: "run-terminal", TenantID: "merchant-a", OperatorID: "operator-a", TraceID: "11111111111111111111111111111111",
		Goal: "清理库存", Status: StatusCompleted, ProposalJSON: `{"summary":"keep"}`, Version: 4,
	}
	runtime := &projectionRuntime{streams: [][]RuntimeEvent{{
		{RunID: "wrong-run", Seq: 1, Type: "run.completed", Status: StatusCompleted, PayloadJSON: `{}`, CreatedAtUnixMs: 301},
	}}}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(func() { service.Close() })
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	projected := repo.waitForProjectionComplete(t, "run-terminal")
	if projected.Status != StatusCompleted || projected.ProposalJSON != `{"summary":"keep"}` || projected.ErrorCode != "" {
		t.Fatalf("terminal projection was downgraded: %+v", projected)
	}
	if events := repo.events["run-terminal"]; len(events) != 0 {
		t.Fatalf("terminal completion appended a failure event: %+v", events)
	}
}

type projectionMemoryRepository struct {
	mu                            sync.Mutex
	runs                          map[string]Run
	events                        map[string][]Event
	checkpointAdvancedBeforeEvent bool
}

func newProjectionMemoryRepository() *projectionMemoryRepository {
	return &projectionMemoryRepository{runs: map[string]Run{}, events: map[string][]Event{}}
}

func (r *projectionMemoryRepository) Create(_ context.Context, run Run) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[run.ID] = run
	return nil
}

func (r *projectionMemoryRepository) CreateIdempotent(_ context.Context, run Run) (Run, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.runs {
		if existing.TenantID == run.TenantID && existing.OperatorID == run.OperatorID && existing.IdempotencyKey == run.IdempotencyKey {
			if existing.Goal != run.Goal {
				return Run{}, false, apperr.ErrConflict
			}
			return existing, false, nil
		}
	}
	r.runs[run.ID] = run
	return run, true, nil
}

func (r *projectionMemoryRepository) Find(_ context.Context, runID string) (Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[runID]
	if !ok {
		return Run{}, apperr.ErrNotFound
	}
	return run, nil
}

func (r *projectionMemoryRepository) ListProjectable(_ context.Context, limit int) ([]Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Run, 0)
	for _, run := range r.runs {
		if run.ProjectionComplete {
			continue
		}
		result = append(result, run)
		if limit > 0 && len(result) == limit {
			break
		}
	}
	return result, nil
}

func (r *projectionMemoryRepository) ProjectRuntimeEvent(_ context.Context, event RuntimeEvent, terminal *RuntimeRun) (Run, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[event.RunID]
	if !ok {
		return Run{}, false, apperr.ErrNotFound
	}
	if event.Seq <= run.LastRuntimeSourceSeq {
		return run, false, nil
	}
	if event.Seq != run.LastRuntimeSourceSeq+1 {
		return Run{}, false, apperr.ErrConflict
	}
	for _, existing := range r.events[event.RunID] {
		if existing.RuntimeSourceSeq == event.Seq {
			return run, false, nil
		}
	}
	localSeq := int64(len(r.events[event.RunID]) + 1)
	r.events[event.RunID] = append(r.events[event.RunID], Event{
		RunID: event.RunID, Seq: localSeq, RuntimeSourceSeq: event.Seq, EventType: event.Type,
		PayloadJSON: event.PayloadJSON, CreatedAtUnixMs: event.CreatedAtUnixMs,
	})
	run.LastRuntimeSourceSeq = event.Seq
	run.Status = event.Status
	run.Version++
	if terminal != nil {
		run.Status = terminal.Status
		run.ProposalJSON = terminal.ProposalJSON
		run.ErrorCode = safeRuntimeErrorCode(terminal.ErrorCode)
		run.ProjectionComplete = true
	}
	r.runs[event.RunID] = run
	return run, true, nil
}

func (r *projectionMemoryRepository) ProjectRuntimeSnapshot(_ context.Context, runID string, runtime RuntimeRun) (Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.runs[runID]
	if !isTerminalStatus(run.Status) || run.Status == runtime.Status {
		run.Status = runtime.Status
		if runtime.ProposalJSON != "" {
			run.ProposalJSON = runtime.ProposalJSON
		}
	}
	r.runs[runID] = run
	return run, nil
}

func (r *projectionMemoryRepository) UpdateStatus(context.Context, string, int64, RunStatus, string, string, string) error {
	return errors.New("unused")
}

func (r *projectionMemoryRepository) AppendEvent(_ context.Context, event Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events[event.RunID] = append(r.events[event.RunID], event)
	return nil
}

func (r *projectionMemoryRepository) ListEvents(_ context.Context, runID string, afterSeq int64, limit int) ([]Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Event, 0)
	for _, event := range r.events[runID] {
		if event.Seq <= afterSeq {
			continue
		}
		result = append(result, event)
		if limit > 0 && len(result) == limit {
			break
		}
	}
	return result, nil
}

func (r *projectionMemoryRepository) FailRuntimeStart(_ context.Context, runID string, expectedVersion int64, errorCode, errorMessage string, event Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.runs[runID]
	if run.Version != expectedVersion {
		return apperr.ErrConflict
	}
	run.Status = StatusFailed
	run.ErrorCode = errorCode
	run.ErrorMessage = errorMessage
	run.ProjectionComplete = true
	run.Version++
	r.runs[runID] = run
	r.events[runID] = append(r.events[runID], event)
	return nil
}

func (r *projectionMemoryRepository) CompleteProjection(_ context.Context, runID string, expectedVersion int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.runs[runID]
	if run.Version != expectedVersion {
		return apperr.ErrConflict
	}
	run.ProjectionComplete = true
	run.Version++
	r.runs[runID] = run
	return nil
}

func (r *projectionMemoryRepository) waitForProjectionComplete(t *testing.T, runID string) Run {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		run, err := r.Find(context.Background(), runID)
		if err == nil && run.ProjectionComplete {
			return run
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("run %s projection did not complete", runID)
	return Run{}
}

type projectionRuntime struct {
	mu            sync.Mutex
	starts        int
	startRequests []RuntimeStartRequest
	startErrors   []error
	streamCalls   int
	streamErrors  []error
	active        int
	startRelease  chan struct{}
	streams       [][]RuntimeEvent
	getRun        RuntimeRun
	blockStream   bool
}

func (r *projectionRuntime) StartRun(ctx context.Context, request RuntimeStartRequest) (RuntimeRun, error) {
	r.mu.Lock()
	r.starts++
	r.startRequests = append(r.startRequests, request)
	index := r.starts - 1
	var startErr error
	if index < len(r.startErrors) {
		startErr = r.startErrors[index]
	}
	r.mu.Unlock()
	if r.startRelease != nil {
		select {
		case <-r.startRelease:
		case <-ctx.Done():
			return RuntimeRun{}, ctx.Err()
		}
	}
	if startErr != nil {
		return RuntimeRun{}, startErr
	}
	return RuntimeRun{RunID: request.RunID, Goal: request.Goal, Status: StatusCreated}, nil
}

func (r *projectionRuntime) GetRun(_ context.Context, request RuntimeGetRequest) (RuntimeRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.getRun
	if run.RunID == "" {
		run = RuntimeRun{RunID: request.RunID, Status: StatusCreated}
	}
	return run, nil
}

func (r *projectionRuntime) StreamRunEvents(ctx context.Context, request RuntimeStreamRequest) (EventStream, error) {
	r.mu.Lock()
	r.streamCalls++
	r.active++
	index := r.streamCalls - 1
	var streamErr error
	if index < len(r.streamErrors) {
		streamErr = r.streamErrors[index]
	}
	var events []RuntimeEvent
	if index < len(r.streams) {
		events = append(events, r.streams[index]...)
	}
	block := r.blockStream
	r.mu.Unlock()
	if streamErr != nil {
		r.mu.Lock()
		r.active--
		r.mu.Unlock()
		return nil, streamErr
	}
	for i := range events {
		if events[i].RunID == "fixed-by-start" && request.RunID != "fixed-by-start" {
			events[i].RunID = request.RunID
		}
	}
	return &projectionEventStream{ctx: ctx, runtime: r, events: events, block: block}, nil
}

func (*projectionRuntime) Close() error { return nil }

func (r *projectionRuntime) startCount() int { r.mu.Lock(); defer r.mu.Unlock(); return r.starts }
func (r *projectionRuntime) lastStart() RuntimeStartRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startRequests[len(r.startRequests)-1]
}
func (r *projectionRuntime) streamCount() int   { r.mu.Lock(); defer r.mu.Unlock(); return r.streamCalls }
func (r *projectionRuntime) activeStreams() int { r.mu.Lock(); defer r.mu.Unlock(); return r.active }

func (r *projectionRuntime) waitForStarts(t *testing.T, want int) {
	t.Helper()
	waitForProjectionCount(t, func() int { return r.startCount() }, want, "runtime starts")
}

func (r *projectionRuntime) waitForStreams(t *testing.T, want int) {
	t.Helper()
	waitForProjectionCount(t, func() int { return r.streamCount() }, want, "runtime streams")
}

func waitForProjectionCount(t *testing.T, current func() int, want int, label string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if current() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s = %d, want at least %d", label, current(), want)
}

type projectionEventStream struct {
	ctx     context.Context
	runtime *projectionRuntime
	events  []RuntimeEvent
	index   int
	block   bool
	once    sync.Once
}

func (s *projectionEventStream) Recv() (RuntimeEvent, error) {
	if s.index < len(s.events) {
		event := s.events[s.index]
		s.index++
		return event, nil
	}
	if s.block {
		<-s.ctx.Done()
		return RuntimeEvent{}, s.ctx.Err()
	}
	return RuntimeEvent{}, io.EOF
}

func (s *projectionEventStream) Close() error {
	s.once.Do(func() {
		s.runtime.mu.Lock()
		s.runtime.active--
		s.runtime.mu.Unlock()
	})
	return nil
}
