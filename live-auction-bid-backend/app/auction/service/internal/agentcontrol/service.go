package agentcontrol

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	agentv1 "live-auction-bid/backend/api/agent/v1"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

const (
	MaxGoalBytes                 = 4000
	ErrorCodeRuntimeUnavailable  = "AGENT_RUNTIME_UNAVAILABLE"
	ErrorCodeRuntimeError        = "AGENT_RUNTIME_ERROR"
	ErrorCodeRuntimeCapacity     = "AGENT_RUNTIME_CAPACITY_EXCEEDED"
	ErrorCodeModelError          = "MODEL_ERROR"
	ErrorCodeToolError           = "TOOL_ERROR"
	ErrorCodeRunCancelled        = "RUN_CANCELLED"
	ErrorCodeNoEligibleInventory = "NO_ELIGIBLE_INVENTORY"
	ErrorCodeUngroundedProposal  = "UNGROUNDED_PROPOSAL"
	RuntimeUnavailableMessage    = "Agent Runtime is temporarily unavailable. Please try again later."
	RuntimeCapacityMessage       = "Agent Runtime is at capacity. Please try again later."
	NoEligibleInventoryMessage   = "No eligible inventory lots are available."
	UngroundedProposalMessage    = "The model proposal was not grounded in current inventory."
	runtimeFailureEventType      = "run.failed"
	localEventReadLimit          = 1000
	runtimeFailurePersistTimeout = 3 * time.Second
)

type Actor struct {
	TenantID   string
	OperatorID string
}

type CreateSessionRunCommand struct {
	SessionID      string
	SessionVersion int64
	RunType        agentv1.RunType
	TurnInputJSON  string
	IdempotencyKey string
}

type RunAdmissionError struct{ Run Run }

func (e *RunAdmissionError) Error() string { return RuntimeCapacityMessage }

func AdmissionRun(err error) (Run, bool) {
	var admission *RunAdmissionError
	if errors.As(err, &admission) {
		return admission.Run, true
	}
	return Run{}, false
}

type RuntimeRequestContext struct {
	RequestID  string
	TraceID    string
	RunID      string
	TenantID   string
	OperatorID string
}

type RuntimeStartRequest struct {
	RunID          string
	Context        RuntimeRequestContext
	Goal           string
	RunType        agentv1.RunType
	SessionID      string
	SessionVersion int64
	TurnInputJSON  string
}

type RuntimeGetRequest struct {
	Context RuntimeRequestContext
	RunID   string
}

type RuntimeStreamRequest struct {
	Context  RuntimeRequestContext
	RunID    string
	AfterSeq int64
}

type RuntimeRun struct {
	RunID           string
	Goal            string
	Status          RunStatus
	ProposalJSON    string
	ErrorCode       string
	ErrorMessage    string
	CreatedAtUnixMs int64
	UpdatedAtUnixMs int64
	RunType         agentv1.RunType
	SessionID       string
	SessionVersion  int64
	TurnResultJSON  string
}

type RuntimeEvent struct {
	RunID           string
	Seq             int64
	Type            string
	Status          RunStatus
	Message         string
	PayloadJSON     string
	CreatedAtUnixMs int64
}

type EventStream interface {
	Recv() (RuntimeEvent, error)
	Close() error
}

type RuntimeClient interface {
	StartRun(ctx context.Context, request RuntimeStartRequest) (RuntimeRun, error)
	GetRun(ctx context.Context, request RuntimeGetRequest) (RuntimeRun, error)
	StreamRunEvents(ctx context.Context, request RuntimeStreamRequest) (EventStream, error)
	Close() error
}

// GatewayRepository extends the shared run repository with the one atomic
// transition required when an external runtime cannot accept a newly created run.
type GatewayRepository interface {
	RunRepository
	FailRuntimeStart(ctx context.Context, runID string, expectedVersion int64, errorCode, errorMessage string, event Event) error
	CompleteProjection(ctx context.Context, runID string, expectedVersion int64) error
}

type Service struct {
	repository     GatewayRepository
	runtime        RuntimeClient
	now            func() time.Time
	idGenerator    func(string) (string, error)
	traceGenerator func() (string, error)
	projectors     *projectionManager
}

func NewService(repository GatewayRepository, runtime RuntimeClient) (*Service, error) {
	if repository == nil {
		return nil, errors.New("agent gateway repository is required")
	}
	return &Service{
		repository: repository, runtime: runtime, now: time.Now,
		idGenerator: newCorrelationID, traceGenerator: newTraceID,
		projectors: newProjectionManager(repository, runtime),
	}, nil
}

func (s *Service) CreateRun(ctx context.Context, actor Actor, goal string) (Run, error) {
	idempotencyKey, err := s.idGenerator("internal-")
	if err != nil {
		return Run{}, err
	}
	return s.CreateRunIdempotent(ctx, actor, idempotencyKey, goal)
}

func (s *Service) CreateRunIdempotent(ctx context.Context, actor Actor, idempotencyKey, goal string) (Run, error) {
	actor, err := normalizeActor(actor)
	if err != nil {
		return Run{}, err
	}
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return Run{}, fmt.Errorf("%w: goal is required", apperr.ErrInvalidArgument)
	}
	if len([]byte(goal)) > MaxGoalBytes {
		return Run{}, fmt.Errorf("%w: goal is too long", apperr.ErrInvalidArgument)
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return Run{}, fmt.Errorf("%w: idempotency key is required", apperr.ErrInvalidArgument)
	}
	runID, err := s.idGenerator("run-")
	if err != nil {
		return Run{}, err
	}
	requestID, err := s.idGenerator("req-")
	if err != nil {
		return Run{}, err
	}
	traceID, err := s.traceGenerator()
	if err != nil {
		return Run{}, err
	}
	now := s.now().UnixMilli()
	run := Run{
		ID: runID, TenantID: actor.TenantID, OperatorID: actor.OperatorID, TraceID: traceID,
		Goal: goal, IdempotencyKey: idempotencyKey, Status: StatusCreated, Version: 1,
		CreatedAtUnixMs: now, UpdatedAtUnixMs: now,
	}
	persisted, created, err := s.repository.CreateIdempotent(ctx, run)
	if err != nil {
		return Run{}, err
	}
	if !created {
		if persisted.Status == StatusFailed && persisted.ErrorCode == ErrorCodeRuntimeCapacity {
			return persisted, &RunAdmissionError{Run: persisted}
		}
		return persisted, nil
	}
	run = persisted
	request := RuntimeStartRequest{
		RunID: run.ID,
		Context: RuntimeRequestContext{
			RequestID: requestID, TraceID: traceID, RunID: run.ID,
			TenantID: actor.TenantID, OperatorID: actor.OperatorID,
		},
		Goal: goal,
	}
	var runtimeRun RuntimeRun
	if s.runtime == nil {
		return s.persistRuntimeUnavailable(ctx, run)
	} else {
		runtimeRun, err = s.runtime.StartRun(ctx, request)
		if err == nil && runtimeRun.RunID != run.ID {
			err = errors.New("agent runtime returned an invalid run")
		}
	}
	if err != nil {
		if status.Code(err) == codes.ResourceExhausted {
			failed, persistErr := s.persistRuntimeFailure(ctx, run, ErrorCodeRuntimeCapacity, RuntimeCapacityMessage)
			if persistErr != nil {
				return Run{}, persistErr
			}
			return failed, &RunAdmissionError{Run: failed}
		}
		if runtimeStartOutcomeAmbiguous(err) {
			s.projectors.Ensure(run)
			return run, nil
		}
		return s.persistRuntimeFailure(ctx, run, ErrorCodeRuntimeError, "Agent Runtime rejected the run request.")
	}
	projected, projectErr := s.repository.ProjectRuntimeSnapshot(ctx, run.ID, runtimeRun)
	if projectErr != nil {
		return Run{}, projectErr
	}
	s.projectors.Ensure(projected)
	return projected, nil
}

// CreateSessionRun durably links a runtime execution to the operator-owned
// session turn before the Python runtime is contacted.
func (s *Service) CreateSessionRun(ctx context.Context, actor Actor, command CreateSessionRunCommand) (Run, error) {
	actor, err := normalizeActor(actor)
	if err != nil {
		return Run{}, err
	}
	command.SessionID = strings.TrimSpace(command.SessionID)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	if command.SessionID == "" || command.SessionVersion <= 0 || command.IdempotencyKey == "" {
		return Run{}, fmt.Errorf("%w: session turn fields are required", apperr.ErrInvalidArgument)
	}
	if command.RunType != agentv1.RunType_RUN_TYPE_SESSION_PLAN && command.RunType != agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST {
		return Run{}, fmt.Errorf("%w: session run type is required", apperr.ErrInvalidArgument)
	}
	if !validJSONDocument(command.TurnInputJSON, 1024*1024) {
		return Run{}, fmt.Errorf("%w: turn input must be JSON", apperr.ErrInvalidArgument)
	}
	runID, err := s.idGenerator("run-")
	if err != nil {
		return Run{}, err
	}
	requestID, err := s.idGenerator("req-")
	if err != nil {
		return Run{}, err
	}
	traceID, err := s.traceGenerator()
	if err != nil {
		return Run{}, err
	}
	now := s.now().UnixMilli()
	run := Run{
		ID: runID, TenantID: actor.TenantID, OperatorID: actor.OperatorID, TraceID: traceID,
		Goal: "Continue agent session", IdempotencyKey: command.IdempotencyKey,
		Status: StatusCreated, Version: 1, RunType: command.RunType,
		SessionID: command.SessionID, SessionVersion: command.SessionVersion,
		TurnInputJSON:   command.TurnInputJSON,
		CreatedAtUnixMs: now, UpdatedAtUnixMs: now,
	}
	persisted, created, err := s.repository.CreateIdempotent(ctx, run)
	if err != nil {
		return Run{}, err
	}
	if !created {
		if persisted.Status == StatusFailed && persisted.ErrorCode == ErrorCodeRuntimeCapacity {
			return persisted, &RunAdmissionError{Run: persisted}
		}
		return persisted, nil
	}
	run = persisted
	request := RuntimeStartRequest{
		RunID: run.ID,
		Context: RuntimeRequestContext{
			RequestID: requestID, TraceID: traceID, RunID: run.ID,
			TenantID: actor.TenantID, OperatorID: actor.OperatorID,
		},
		Goal: run.Goal, RunType: run.RunType, SessionID: run.SessionID,
		SessionVersion: run.SessionVersion, TurnInputJSON: run.TurnInputJSON,
	}
	var runtimeRun RuntimeRun
	if s.runtime == nil {
		return s.persistRuntimeUnavailable(ctx, run)
	}
	runtimeRun, err = s.runtime.StartRun(ctx, request)
	if err == nil && runtimeRun.RunID != run.ID {
		err = errors.New("agent runtime returned an invalid run")
	}
	if err != nil {
		if status.Code(err) == codes.ResourceExhausted {
			failed, persistErr := s.persistRuntimeFailure(ctx, run, ErrorCodeRuntimeCapacity, RuntimeCapacityMessage)
			if persistErr != nil {
				return Run{}, persistErr
			}
			return failed, &RunAdmissionError{Run: failed}
		}
		if runtimeStartOutcomeAmbiguous(err) {
			s.projectors.Ensure(run)
			return run, nil
		}
		return s.persistRuntimeFailure(ctx, run, ErrorCodeRuntimeError, "Agent Runtime rejected the run request.")
	}
	projected, projectErr := s.repository.ProjectRuntimeSnapshot(ctx, run.ID, runtimeRun)
	if projectErr != nil {
		return Run{}, projectErr
	}
	s.projectors.Ensure(projected)
	return projected, nil
}

// StartPersistedSessionRun starts a session run which was inserted by the
// conversation repository in the same transaction as its user message.  It
// deliberately does not call CreateIdempotent: doing so would split the
// durable turn boundary and can create a second agent_runs record.
func (s *Service) StartPersistedSessionRun(ctx context.Context, actor Actor, runID string) (Run, error) {
	actor, err := normalizeActor(actor)
	if err != nil {
		return Run{}, err
	}
	run, err := s.authorizeLocalRun(ctx, actor, runID)
	if err != nil {
		return Run{}, err
	}
	if run.OperatorID != actor.OperatorID || run.SessionID == "" || run.SessionVersion <= 0 ||
		(run.RunType != agentv1.RunType_RUN_TYPE_SESSION_PLAN && run.RunType != agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST) ||
		!validJSONDocument(run.TurnInputJSON, 1024*1024) {
		return Run{}, fmt.Errorf("%w: invalid persisted session run", apperr.ErrInvalidArgument)
	}
	if isTerminalStatus(run.Status) {
		return run, nil
	}
	requestID, err := s.idGenerator("req-")
	if err != nil {
		return Run{}, err
	}
	if s.runtime == nil {
		return s.persistRuntimeUnavailable(ctx, run)
	}
	runtimeRun, err := s.runtime.StartRun(ctx, RuntimeStartRequest{
		RunID:   run.ID,
		Context: RuntimeRequestContext{RequestID: requestID, TraceID: run.TraceID, RunID: run.ID, TenantID: actor.TenantID, OperatorID: actor.OperatorID},
		Goal:    run.Goal, RunType: run.RunType, SessionID: run.SessionID, SessionVersion: run.SessionVersion, TurnInputJSON: run.TurnInputJSON,
	})
	if err == nil && runtimeRun.RunID != run.ID {
		err = errors.New("agent runtime returned an invalid run")
	}
	if err != nil {
		if status.Code(err) == codes.ResourceExhausted {
			failed, persistErr := s.persistRuntimeFailure(ctx, run, ErrorCodeRuntimeCapacity, RuntimeCapacityMessage)
			if persistErr != nil {
				return Run{}, persistErr
			}
			return failed, &RunAdmissionError{Run: failed}
		}
		if runtimeStartOutcomeAmbiguous(err) {
			s.projectors.Ensure(run)
			return run, nil
		}
		return s.persistRuntimeFailure(ctx, run, ErrorCodeRuntimeError, "Agent Runtime rejected the run request.")
	}
	projected, err := s.repository.ProjectRuntimeSnapshot(ctx, run.ID, runtimeRun)
	if err != nil {
		return Run{}, err
	}
	s.projectors.Ensure(projected)
	return projected, nil
}

func (s *Service) Start(ctx context.Context) error {
	return s.projectors.Start(ctx)
}

func (s *Service) Close() {
	s.projectors.Close()
}

func (s *Service) GetRun(ctx context.Context, actor Actor, runID string) (Run, error) {
	actor, err := normalizeActor(actor)
	if err != nil {
		return Run{}, err
	}
	local, err := s.authorizeLocalRun(ctx, actor, runID)
	if err != nil {
		return Run{}, err
	}
	if s.runtime == nil || isLocalRuntimeUnavailable(local) {
		return local, nil
	}
	requestID, err := newCorrelationID("req-")
	if err != nil {
		return local, nil
	}
	runtimeRun, err := s.runtime.GetRun(ctx, RuntimeGetRequest{
		RunID: local.ID,
		Context: RuntimeRequestContext{
			RequestID: requestID, TraceID: local.TraceID, RunID: local.ID,
			TenantID: actor.TenantID, OperatorID: actor.OperatorID,
		},
	})
	if err != nil || runtimeRun.RunID != local.ID {
		return local, nil
	}
	projected, err := s.repository.ProjectRuntimeSnapshot(ctx, local.ID, runtimeRun)
	if err != nil {
		return local, nil
	}
	s.projectors.Ensure(projected)
	return projected, nil
}

// AuthorizeRun performs the local authoritative ownership check without
// contacting the external runtime. Transports use it before parsing or
// emitting stream-specific responses whose shape could otherwise become an
// existence oracle.
func (s *Service) AuthorizeRun(ctx context.Context, actor Actor, runID string) error {
	actor, err := normalizeActor(actor)
	if err != nil {
		return err
	}
	_, err = s.authorizeLocalRun(ctx, actor, runID)
	return err
}

func (s *Service) StreamRunEvents(ctx context.Context, actor Actor, runID string, afterSeq int64) (EventStream, error) {
	actor, err := normalizeActor(actor)
	if err != nil {
		return nil, err
	}
	local, err := s.authorizeLocalRun(ctx, actor, runID)
	if err != nil {
		return nil, err
	}
	if afterSeq < 0 {
		return nil, fmt.Errorf("%w: after_seq must be nonnegative", apperr.ErrInvalidArgument)
	}
	return &repositoryEventStream{ctx: ctx, repository: s.repository, runID: local.ID, afterSeq: afterSeq}, nil
}

func (s *Service) authorizeLocalRun(ctx context.Context, actor Actor, runID string) (Run, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return Run{}, apperr.ErrPermissionDenied
	}
	run, err := s.repository.Find(ctx, runID)
	if err != nil {
		if apperr.IsNotFound(err) {
			return Run{}, apperr.ErrPermissionDenied
		}
		return Run{}, err
	}
	if strings.TrimSpace(run.TenantID) == "" || run.TenantID != actor.TenantID {
		return Run{}, apperr.ErrPermissionDenied
	}
	return run, nil
}

func (s *Service) persistRuntimeUnavailable(ctx context.Context, created Run) (Run, error) {
	return s.persistRuntimeFailure(ctx, created, ErrorCodeRuntimeUnavailable, RuntimeUnavailableMessage)
}

func (s *Service) persistRuntimeFailure(ctx context.Context, created Run, errorCode, errorMessage string) (Run, error) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeFailurePersistTimeout)
	defer cancel()
	now := s.now().UnixMilli()
	event := Event{
		RunID: created.ID, Seq: 1, EventType: runtimeFailureEventType,
		Message:         errorMessage,
		PayloadJSON:     fmt.Sprintf(`{"errorCode":%q}`, errorCode),
		CreatedAtUnixMs: now,
	}
	if err := s.repository.FailRuntimeStart(persistCtx, created.ID, created.Version, errorCode, errorMessage, event); err != nil {
		return Run{}, err
	}
	failed, err := s.repository.Find(persistCtx, created.ID)
	if err != nil {
		return Run{}, err
	}
	return failed, nil
}

func normalizeActor(actor Actor) (Actor, error) {
	actor.TenantID = strings.TrimSpace(actor.TenantID)
	actor.OperatorID = strings.TrimSpace(actor.OperatorID)
	if actor.TenantID == "" || actor.OperatorID == "" {
		return Actor{}, apperr.ErrPermissionDenied
	}
	return actor, nil
}

func IsPermissionDenied(err error) bool {
	return apperr.IsPermissionDenied(err)
}

func isLocalRuntimeUnavailable(run Run) bool {
	return run.Status == StatusFailed && run.ErrorCode == ErrorCodeRuntimeUnavailable
}

func runtimeStartOutcomeAmbiguous(err error) bool {
	if err == nil {
		return false
	}
	switch status.Code(err) {
	case codes.InvalidArgument, codes.PermissionDenied, codes.Unauthenticated, codes.FailedPrecondition,
		codes.Unimplemented, codes.NotFound, codes.AlreadyExists, codes.ResourceExhausted:
		return false
	default:
		return true
	}
}

func MergeRuntimeProjection(local Run, runtime RuntimeRun) Run {
	if runtime.RunID != local.ID || !validRunStatus(runtime.Status) || !RuntimeLinkageMatches(local, runtime) {
		return local
	}
	if isTerminalStatus(local.Status) && runtime.Status != local.Status {
		return local
	}
	if !isTerminalStatus(runtime.Status) && runStatusRank(runtime.Status) < runStatusRank(local.Status) {
		return local
	}
	local.Status = runtime.Status
	if validJSONDocument(runtime.ProposalJSON, 1024*1024) {
		local.ProposalJSON = runtime.ProposalJSON
	}
	if runtime.Status == StatusCompleted && validJSONDocument(runtime.TurnResultJSON, 1024*1024) {
		local.TurnResultJSON = runtime.TurnResultJSON
	}
	if runtime.UpdatedAtUnixMs > local.UpdatedAtUnixMs {
		local.UpdatedAtUnixMs = runtime.UpdatedAtUnixMs
	}
	if runtime.Status == StatusFailed {
		local.ErrorCode = safeRuntimeErrorCode(runtime.ErrorCode)
		local.ErrorMessage = safeRuntimeErrorMessage(local.ErrorCode)
	} else if runtime.Status == StatusCancelled {
		local.ErrorCode = ErrorCodeRunCancelled
		local.ErrorMessage = "Agent run cancelled."
	} else {
		local.ErrorCode = ""
		local.ErrorMessage = ""
	}
	return local
}

// RuntimeLinkageMatches verifies that a runtime projection belongs to the
// immutable session turn persisted by Go.
func RuntimeLinkageMatches(local Run, runtime RuntimeRun) bool {
	return local.RunType == runtime.RunType &&
		local.SessionID == runtime.SessionID &&
		local.SessionVersion == runtime.SessionVersion
}

func runStatusRank(status RunStatus) int {
	switch status {
	case StatusCreated:
		return 1
	case StatusPlanning:
		return 2
	case StatusCollectingContext:
		return 3
	case StatusGeneratingProposal:
		return 4
	case StatusVerifying:
		return 5
	case StatusCompleted, StatusFailed, StatusCancelled:
		return 6
	default:
		return 0
	}
}

func safeRuntimeErrorCode(value string) string {
	switch strings.TrimSpace(value) {
	case ErrorCodeRuntimeUnavailable:
		return ErrorCodeRuntimeUnavailable
	case ErrorCodeRuntimeCapacity:
		return ErrorCodeRuntimeCapacity
	case ErrorCodeModelError:
		return ErrorCodeModelError
	case ErrorCodeToolError:
		return ErrorCodeToolError
	case ErrorCodeRunCancelled:
		return ErrorCodeRunCancelled
	case ErrorCodeNoEligibleInventory:
		return ErrorCodeNoEligibleInventory
	case ErrorCodeUngroundedProposal:
		return ErrorCodeUngroundedProposal
	case ErrorCodeRuntimeError:
		return ErrorCodeRuntimeError
	default:
		return ErrorCodeRuntimeError
	}
}

func safeRuntimeErrorMessage(code string) string {
	switch code {
	case ErrorCodeNoEligibleInventory:
		return NoEligibleInventoryMessage
	case ErrorCodeUngroundedProposal:
		return UngroundedProposalMessage
	default:
		return "Agent run failed. Please try again."
	}
}

func validRunStatus(status RunStatus) bool {
	switch status {
	case StatusCreated, StatusPlanning, StatusCollectingContext, StatusGeneratingProposal,
		StatusVerifying, StatusCompleted, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

func isTerminalStatus(status RunStatus) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCancelled
}

func validJSONDocument(raw string, limit int) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	return len(raw) <= limit && json.Valid([]byte(raw))
}

func newCorrelationID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate agent correlation ID: %w", err)
	}
	return prefix + hex.EncodeToString(value[:]), nil
}

func newTraceID() (string, error) {
	for {
		var value [16]byte
		if _, err := rand.Read(value[:]); err != nil {
			return "", fmt.Errorf("generate agent trace ID: %w", err)
		}
		if value != [16]byte{} {
			return hex.EncodeToString(value[:]), nil
		}
	}
}
