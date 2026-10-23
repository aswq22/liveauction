package agentcontrol

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	agentv1 "live-auction-bid/backend/api/agent/v1"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

func TestAgentServiceCreatesOwnedRunBeforeCallingRuntime(t *testing.T) {
	repo := newMemoryGatewayRepository()
	runtime := &recordingRuntime{}
	runtime.start = func(_ context.Context, request RuntimeStartRequest) (RuntimeRun, error) {
		if _, ok := repo.runs[request.RunID]; !ok {
			t.Fatal("runtime called before local run was persisted")
		}
		return RuntimeRun{RunID: request.RunID, Goal: request.Goal, Status: StatusCreated}, nil
	}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	run, err := service.CreateRun(context.Background(), Actor{TenantID: "merchant-a", OperatorID: "user-a"}, " 清理库存 ")
	if err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if run.TenantID != "merchant-a" || run.OperatorID != "user-a" || run.Goal != "清理库存" {
		t.Fatalf("created run owner/goal = %+v", run)
	}
	if run.Version != 1 || run.Status != StatusCreated {
		t.Fatalf("created run version/status = %d/%s, want 1/CREATED", run.Version, run.Status)
	}
	if !regexp.MustCompile(`^run-[0-9a-f]{32}$`).MatchString(run.ID) {
		t.Fatalf("run ID %q is not a server-generated 128-bit ID", run.ID)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(run.TraceID) || strings.Trim(run.TraceID, "0") == "" {
		t.Fatalf("trace ID %q is not a nonzero 128-bit W3C trace ID", run.TraceID)
	}
	request := runtime.starts[0]
	if request.RunID != run.ID || request.Context.RunID != run.ID {
		t.Fatalf("runtime correlated IDs = %q/%q, want %q", request.RunID, request.Context.RunID, run.ID)
	}
	if request.Context.TenantID != "merchant-a" || request.Context.OperatorID != "user-a" {
		t.Fatalf("runtime context owner = %+v", request.Context)
	}
	if request.Context.TraceID != run.TraceID || !regexp.MustCompile(`^req-[0-9a-f]{32}$`).MatchString(request.Context.RequestID) {
		t.Fatalf("runtime request correlation = %+v", request.Context)
	}
}

func TestAgentServiceCreatesSessionRunBeforeCallingRuntimeAndStartsOnce(t *testing.T) {
	repo := newMemoryGatewayRepository()
	runtime := &recordingRuntime{}
	turnInput := `{"message":"draft two lots","candidate_patches":[]}`
	runtime.start = func(_ context.Context, request RuntimeStartRequest) (RuntimeRun, error) {
		persisted, ok := repo.runs[request.RunID]
		if !ok {
			t.Fatal("runtime called before session run was persisted")
		}
		if persisted.RunType != agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST || persisted.SessionID != "session-1" || persisted.SessionVersion != 7 || persisted.TurnInputJSON != turnInput {
			t.Fatalf("persisted session specification = %+v", persisted)
		}
		return RuntimeRun{
			RunID: request.RunID, Goal: request.Goal, Status: StatusCreated,
			RunType: request.RunType, SessionID: request.SessionID, SessionVersion: request.SessionVersion,
		}, nil
	}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	command := CreateSessionRunCommand{
		SessionID: "session-1", SessionVersion: 7,
		RunType:       agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST,
		TurnInputJSON: turnInput, IdempotencyKey: "turn-1",
	}
	actor := Actor{TenantID: "merchant-a", OperatorID: "user-a"}

	first, err := service.CreateSessionRun(context.Background(), actor, command)
	if err != nil {
		t.Fatalf("CreateSessionRun() error = %v", err)
	}
	replay, err := service.CreateSessionRun(context.Background(), actor, command)
	if err != nil {
		t.Fatalf("idempotent CreateSessionRun() error = %v", err)
	}

	if replay.ID != first.ID || len(runtime.starts) != 1 {
		t.Fatalf("idempotent session run = (%q, starts=%d), want (%q, 1)", replay.ID, len(runtime.starts), first.ID)
	}
	request := runtime.starts[0]
	if request.RunType != command.RunType || request.SessionID != command.SessionID || request.SessionVersion != command.SessionVersion || request.TurnInputJSON != turnInput {
		t.Fatalf("runtime session request = %+v", request)
	}
}

func TestAgentServiceProjectsCompletedSessionTurnResult(t *testing.T) {
	repo := newMemoryGatewayRepository()
	repo.runs["run-session"] = Run{
		ID: "run-session", TenantID: "merchant-a", OperatorID: "creator-a", TraceID: strings.Repeat("1", 32),
		Goal: "Continue agent session", Status: StatusPlanning, Version: 1,
		RunType: agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST, SessionID: "session-1", SessionVersion: 7,
		TurnInputJSON: `{"message":"draft a lot"}`,
	}
	wantResult := `{"assistant_message":"Draft ready","candidate_patches":[]}`
	runtime := &recordingRuntime{getRun: RuntimeRun{
		RunID: "run-session", Status: StatusCompleted,
		RunType: agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST, SessionID: "session-1", SessionVersion: 7,
		TurnResultJSON: wantResult,
	}}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	run, err := service.GetRun(context.Background(), Actor{TenantID: "merchant-a", OperatorID: "operator-b"}, "run-session")
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if run.TurnResultJSON != wantResult {
		t.Fatalf("projected turn result = %q, want %q", run.TurnResultJSON, wantResult)
	}
	if run.SessionID != "session-1" || run.SessionVersion != 7 || run.TurnInputJSON != `{"message":"draft a lot"}` {
		t.Fatalf("runtime replaced durable session input = %+v", run)
	}
}

func TestAgentServiceSessionFailurePreservesDurableTurnInput(t *testing.T) {
	repo := newMemoryGatewayRepository()
	runtime := &recordingRuntime{startErr: status.Error(codes.InvalidArgument, "invalid turn")}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	turnInput := `{"message":"keep this message","candidate_patches":[{"candidate_id":"candidate-1"}]}`

	run, err := service.CreateSessionRun(context.Background(), Actor{TenantID: "merchant-a", OperatorID: "user-a"}, CreateSessionRunCommand{
		SessionID: "session-1", SessionVersion: 4, RunType: agentv1.RunType_RUN_TYPE_SESSION_PLAN,
		TurnInputJSON: turnInput, IdempotencyKey: "turn-failed",
	})
	if err != nil {
		t.Fatalf("CreateSessionRun() error = %v", err)
	}

	if run.Status != StatusFailed || run.SessionID != "session-1" || run.SessionVersion != 4 || run.TurnInputJSON != turnInput {
		t.Fatalf("failed session run lost durable turn input = %+v", run)
	}
	if run.TurnResultJSON != "" {
		t.Fatalf("failed session run persisted partial result = %q", run.TurnResultJSON)
	}
}

func TestMergeRuntimeProjectionPreservesActionablePlanningFailures(t *testing.T) {
	tests := []struct {
		code    string
		message string
	}{
		{code: "NO_ELIGIBLE_INVENTORY", message: "No eligible inventory lots are available."},
		{code: "UNGROUNDED_PROPOSAL", message: "The model proposal was not grounded in current inventory."},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			projected := MergeRuntimeProjection(
				Run{ID: "run-1", Status: StatusPlanning},
				RuntimeRun{RunID: "run-1", Status: StatusFailed, ErrorCode: test.code, ErrorMessage: "provider secret detail"},
			)
			if projected.ErrorCode != test.code || projected.ErrorMessage != test.message {
				t.Fatalf("failure projection = (%q, %q)", projected.ErrorCode, projected.ErrorMessage)
			}
		})
	}
}

func TestMergeRuntimeProjectionRejectsMismatchedRuntimeLinkage(t *testing.T) {
	local := Run{
		ID: "run-1", Status: StatusPlanning, RunType: agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST,
		SessionID: "session-1", SessionVersion: 7, TurnInputJSON: `{"message":"draft"}`,
	}
	for name, runtime := range map[string]RuntimeRun{
		"run type":        {RunID: "run-1", Status: StatusCompleted, RunType: agentv1.RunType_RUN_TYPE_SESSION_PLAN, SessionID: "session-1", SessionVersion: 7, TurnResultJSON: `{"assistant_message":"done","candidate_patches":[],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`},
		"session ID":      {RunID: "run-1", Status: StatusCompleted, RunType: agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST, SessionID: "other-session", SessionVersion: 7, TurnResultJSON: `{"assistant_message":"done","candidate_patches":[],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`},
		"session version": {RunID: "run-1", Status: StatusCompleted, RunType: agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST, SessionID: "session-1", SessionVersion: 8, TurnResultJSON: `{"assistant_message":"done","candidate_patches":[],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`},
	} {
		t.Run(name, func(t *testing.T) {
			if projected := MergeRuntimeProjection(local, runtime); projected != local {
				t.Fatalf("mismatched runtime was projected: %+v", projected)
			}
		})
	}
	legacy := Run{ID: "legacy-1", Status: StatusPlanning}
	compatible := RuntimeRun{RunID: "legacy-1", Status: StatusCompleted}
	if projected := MergeRuntimeProjection(legacy, compatible); projected.Status != StatusCompleted {
		t.Fatalf("default runtime linkage rejected legacy run: %+v", projected)
	}
}

func TestAgentServiceUnknownStartOutcomeRemainsProjectableWithoutLeakingDetails(t *testing.T) {
	repo := newMemoryGatewayRepository()
	runtime := &recordingRuntime{startErr: errors.New("dial 10.0.0.8:19081: Bearer super-secret")}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	run, err := service.CreateRun(context.Background(), Actor{TenantID: "merchant-a", OperatorID: "user-a"}, "清理库存")
	if err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if run.Status != StatusCreated || run.ProjectionComplete || run.Version != 1 {
		t.Fatalf("ambiguous run = %+v", run)
	}
	if strings.Contains(run.ErrorMessage, "10.0.0.8") || strings.Contains(run.ErrorMessage, "super-secret") {
		t.Fatalf("failed run leaked provider details: %q", run.ErrorMessage)
	}
	events, err := repo.ListEvents(context.Background(), run.ID, 0, 10)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("ambiguous start emitted terminal events = %+v", events)
	}
}

func TestAgentServiceRequestCancellationKeepsAmbiguousStartProjectable(t *testing.T) {
	repo := newMemoryGatewayRepository()
	repo.honorFailContext = true
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	runtime := &recordingRuntime{start: func(_ context.Context, _ RuntimeStartRequest) (RuntimeRun, error) {
		cancelRequest()
		return RuntimeRun{}, context.Canceled
	}}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	run, err := service.CreateRun(requestCtx, Actor{TenantID: "merchant-a", OperatorID: "user-a"}, "清理库存")
	if err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if run.Status != StatusCreated || run.ProjectionComplete || run.Version != 1 {
		t.Fatalf("cancelled request made ambiguous run terminal = %+v", run)
	}
	if events := repo.events[run.ID]; len(events) != 0 {
		t.Fatalf("cancelled ambiguous request emitted terminal events = %+v", events)
	}
}

func TestAgentServiceExplicitRuntimeRejectionPersistsTerminalFailure(t *testing.T) {
	for _, code := range []codes.Code{codes.InvalidArgument, codes.PermissionDenied, codes.FailedPrecondition} {
		t.Run(code.String(), func(t *testing.T) {
			repo := newMemoryGatewayRepository()
			runtime := &recordingRuntime{startErr: status.Error(code, "unsafe provider detail")}
			service, err := NewService(repo, runtime)
			if err != nil {
				t.Fatalf("NewService() error = %v", err)
			}
			run, err := service.CreateRun(context.Background(), Actor{TenantID: "merchant-a", OperatorID: "user-a"}, "清理库存")
			if err != nil {
				t.Fatalf("CreateRun() error = %v", err)
			}
			if run.Status != StatusFailed || !run.ProjectionComplete || run.ErrorCode != ErrorCodeRuntimeError || strings.Contains(run.ErrorMessage, "provider") {
				t.Fatalf("explicit rejection projection = %+v", run)
			}
		})
	}
}

func TestAgentServiceGetUsesMerchantScopeAndRuntimeProjection(t *testing.T) {
	repo := newMemoryGatewayRepository()
	repo.runs["run-1"] = Run{
		ID: "run-1", TenantID: "merchant-a", OperatorID: "creator-a", TraceID: strings.Repeat("1", 32),
		Goal: "清理库存", Status: StatusCreated, Version: 1, CreatedAtUnixMs: 100, UpdatedAtUnixMs: 100,
	}
	runtime := &recordingRuntime{getRun: RuntimeRun{
		RunID: "run-1", Goal: "runtime must not replace local goal", Status: StatusCompleted,
		ProposalJSON: `{"summary":"均衡清仓方案"}`, CreatedAtUnixMs: 200, UpdatedAtUnixMs: 300,
	}}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	run, err := service.GetRun(context.Background(), Actor{TenantID: "merchant-a", OperatorID: "another-operator"}, "run-1")
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if run.OperatorID != "creator-a" || run.Goal != "清理库存" || run.CreatedAtUnixMs != 100 {
		t.Fatalf("runtime replaced authoritative local fields: %+v", run)
	}
	if run.Status != StatusCompleted || run.ProposalJSON != `{"summary":"均衡清仓方案"}` || run.UpdatedAtUnixMs != 300 {
		t.Fatalf("runtime projection was not merged safely: %+v", run)
	}
	if len(runtime.gets) != 1 || runtime.gets[0].Context.TenantID != "merchant-a" || runtime.gets[0].Context.OperatorID != "another-operator" {
		t.Fatalf("runtime get context = %+v", runtime.gets)
	}
}

func TestAgentServiceGetRuntimeFailureFallsBackToLocalOwner(t *testing.T) {
	repo := newMemoryGatewayRepository()
	want := Run{ID: "run-1", TenantID: "merchant-a", OperatorID: "creator-a", Goal: "清理库存", Status: StatusPlanning, Version: 1}
	repo.runs[want.ID] = want
	runtime := &recordingRuntime{getErr: errors.New("rpc error: token super-secret")}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	got, err := service.GetRun(context.Background(), Actor{TenantID: "merchant-a", OperatorID: "operator-b"}, want.ID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if got != want {
		t.Fatalf("GetRun() = %+v, want local fallback %+v", got, want)
	}
}

func TestAgentServiceUnknownAndCrossTenantRunsSharePermissionDeniedBehavior(t *testing.T) {
	repo := newMemoryGatewayRepository()
	repo.runs["run-other"] = Run{ID: "run-other", TenantID: "merchant-b", Status: StatusCreated, Version: 1}
	runtime := &recordingRuntime{}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	actor := Actor{TenantID: "merchant-a", OperatorID: "user-a"}

	for _, runID := range []string{"run-missing", "run-other"} {
		_, getErr := service.GetRun(context.Background(), actor, runID)
		if !IsPermissionDenied(getErr) {
			t.Fatalf("GetRun(%q) error = %v, want permission denied", runID, getErr)
		}
		_, streamErr := service.StreamRunEvents(context.Background(), actor, runID, 0)
		if !IsPermissionDenied(streamErr) {
			t.Fatalf("StreamRunEvents(%q) error = %v, want permission denied", runID, streamErr)
		}
	}
	if len(runtime.gets) != 0 || len(runtime.streams) != 0 {
		t.Fatalf("runtime was called for unauthorized runs: gets=%d streams=%d", len(runtime.gets), len(runtime.streams))
	}
}

func TestAgentServiceStreamsStableLocalFailureWithoutRuntime(t *testing.T) {
	repo := newMemoryGatewayRepository()
	repo.runs["run-1"] = Run{
		ID: "run-1", TenantID: "merchant-a", OperatorID: "creator-a", Status: StatusFailed,
		ErrorCode: ErrorCodeRuntimeUnavailable, ErrorMessage: RuntimeUnavailableMessage, Version: 2,
	}
	repo.events["run-1"] = []Event{{
		RunID: "run-1", Seq: 1, EventType: "run.failed", Message: RuntimeUnavailableMessage,
		PayloadJSON: `{"errorCode":"AGENT_RUNTIME_UNAVAILABLE"}`,
	}}
	runtime := &recordingRuntime{streamErr: errors.New("runtime must not be called")}
	service, err := NewService(repo, runtime)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	stream, err := service.StreamRunEvents(context.Background(), Actor{TenantID: "merchant-a", OperatorID: "operator-b"}, "run-1", 0)
	if err != nil {
		t.Fatalf("StreamRunEvents() error = %v", err)
	}
	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if event.Seq != 1 || event.Type != "run.failed" || event.Message != RuntimeUnavailableMessage {
		t.Fatalf("local event = %+v", event)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("second Recv() error = %v, want EOF", err)
	}
	if len(runtime.streams) != 0 {
		t.Fatal("runtime stream called for locally failed unavailable run")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestAgentServiceRejectsMissingActorIdentity(t *testing.T) {
	service, err := NewService(newMemoryGatewayRepository(), &recordingRuntime{})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	for _, actor := range []Actor{{OperatorID: "user-a"}, {TenantID: "merchant-a"}} {
		_, err := service.CreateRun(context.Background(), actor, "清理库存")
		if !IsPermissionDenied(err) {
			t.Fatalf("CreateRun(%+v) error = %v, want permission denied", actor, err)
		}
	}
}

type memoryGatewayRepository struct {
	runs                   map[string]Run
	events                 map[string][]Event
	honorFailContext       bool
	failContextCanceled    bool
	failContextHasDeadline bool
	failContextRemaining   time.Duration
}

func newMemoryGatewayRepository() *memoryGatewayRepository {
	return &memoryGatewayRepository{runs: map[string]Run{}, events: map[string][]Event{}}
}

func (r *memoryGatewayRepository) Create(_ context.Context, run Run) error {
	r.runs[run.ID] = run
	return nil
}

func (r *memoryGatewayRepository) CreateIdempotent(ctx context.Context, run Run) (Run, bool, error) {
	for _, existing := range r.runs {
		if existing.TenantID == run.TenantID && existing.OperatorID == run.OperatorID && existing.IdempotencyKey == run.IdempotencyKey {
			if existing.Goal != run.Goal {
				return Run{}, false, apperr.ErrConflict
			}
			return existing, false, nil
		}
	}
	if err := r.Create(ctx, run); err != nil {
		return Run{}, false, err
	}
	return run, true, nil
}

func (r *memoryGatewayRepository) Find(_ context.Context, runID string) (Run, error) {
	run, ok := r.runs[runID]
	if !ok {
		return Run{}, errTestNotFound
	}
	return run, nil
}

func (r *memoryGatewayRepository) ListProjectable(_ context.Context, _ int) ([]Run, error) {
	return nil, nil
}

func (r *memoryGatewayRepository) ProjectRuntimeSnapshot(_ context.Context, runID string, runtime RuntimeRun) (Run, error) {
	current, err := r.Find(context.Background(), runID)
	if err != nil {
		return Run{}, err
	}
	projected := MergeRuntimeProjection(current, runtime)
	r.runs[runID] = projected
	return projected, nil
}

func (r *memoryGatewayRepository) ProjectRuntimeEvent(_ context.Context, event RuntimeEvent, terminal *RuntimeRun) (Run, bool, error) {
	run, err := r.Find(context.Background(), event.RunID)
	if err != nil {
		return Run{}, false, err
	}
	if event.Seq <= run.LastRuntimeSourceSeq {
		return run, false, nil
	}
	run.Status = event.Status
	run.LastRuntimeSourceSeq = event.Seq
	if terminal != nil {
		run = MergeRuntimeProjection(run, *terminal)
		run.ProjectionComplete = true
	}
	r.runs[event.RunID] = run
	r.events[event.RunID] = append(r.events[event.RunID], Event{RunID: event.RunID, Seq: int64(len(r.events[event.RunID]) + 1), RuntimeSourceSeq: event.Seq, EventType: event.Type, PayloadJSON: event.PayloadJSON})
	return run, true, nil
}

func (r *memoryGatewayRepository) UpdateStatus(_ context.Context, runID string, expectedVersion int64, status RunStatus, proposalJSON, errorCode, errorMessage string) error {
	run, ok := r.runs[runID]
	if !ok {
		return errTestNotFound
	}
	if run.Version != expectedVersion {
		return errors.New("version conflict")
	}
	run.Status = status
	run.ProposalJSON = proposalJSON
	run.ErrorCode = errorCode
	run.ErrorMessage = errorMessage
	run.Version++
	r.runs[runID] = run
	return nil
}

func (r *memoryGatewayRepository) AppendEvent(_ context.Context, event Event) error {
	r.events[event.RunID] = append(r.events[event.RunID], event)
	return nil
}

func (r *memoryGatewayRepository) ListEvents(_ context.Context, runID string, afterSeq int64, limit int) ([]Event, error) {
	var out []Event
	for _, event := range r.events[runID] {
		if event.Seq <= afterSeq {
			continue
		}
		out = append(out, event)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (r *memoryGatewayRepository) FailRuntimeStart(ctx context.Context, runID string, expectedVersion int64, errorCode, errorMessage string, event Event) error {
	deadline, hasDeadline := ctx.Deadline()
	r.failContextHasDeadline = hasDeadline
	if hasDeadline {
		r.failContextRemaining = time.Until(deadline)
	}
	if ctx.Err() != nil {
		r.failContextCanceled = true
		if r.honorFailContext {
			return ctx.Err()
		}
	}
	run, ok := r.runs[runID]
	if !ok {
		return errTestNotFound
	}
	if run.Version != expectedVersion {
		return errors.New("version conflict")
	}
	run.Status = StatusFailed
	run.ErrorCode = errorCode
	run.ErrorMessage = errorMessage
	run.ProjectionComplete = true
	run.Version++
	run.UpdatedAtUnixMs = event.CreatedAtUnixMs
	r.runs[runID] = run
	r.events[runID] = append(r.events[runID], event)
	return nil
}

func (r *memoryGatewayRepository) CompleteProjection(_ context.Context, runID string, expectedVersion int64) error {
	run := r.runs[runID]
	if run.Version != expectedVersion {
		return apperr.ErrConflict
	}
	run.ProjectionComplete = true
	run.Version++
	r.runs[runID] = run
	return nil
}

var errTestNotFound = apperr.ErrNotFound

type recordingRuntime struct {
	start     func(context.Context, RuntimeStartRequest) (RuntimeRun, error)
	startErr  error
	getRun    RuntimeRun
	getErr    error
	stream    EventStream
	streamErr error
	starts    []RuntimeStartRequest
	gets      []RuntimeGetRequest
	streams   []RuntimeStreamRequest
}

func (r *recordingRuntime) StartRun(ctx context.Context, request RuntimeStartRequest) (RuntimeRun, error) {
	r.starts = append(r.starts, request)
	if r.start != nil {
		return r.start(ctx, request)
	}
	return RuntimeRun{}, r.startErr
}

func (r *recordingRuntime) GetRun(_ context.Context, request RuntimeGetRequest) (RuntimeRun, error) {
	r.gets = append(r.gets, request)
	return r.getRun, r.getErr
}

func (r *recordingRuntime) StreamRunEvents(_ context.Context, request RuntimeStreamRequest) (EventStream, error) {
	r.streams = append(r.streams, request)
	return r.stream, r.streamErr
}

func (r *recordingRuntime) Close() error { return nil }
