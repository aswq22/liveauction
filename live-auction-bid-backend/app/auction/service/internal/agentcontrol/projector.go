package agentcontrol

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	projectableRunLimit      = 10_000
	projectorInitialBackoff  = 100 * time.Millisecond
	projectorMaxBackoff      = 5 * time.Second
	projectorFailureTimeout  = 5 * time.Second
	durableEventPollInterval = 100 * time.Millisecond
)

type projectionManager struct {
	rootCtx    context.Context
	cancel     context.CancelFunc
	repository GatewayRepository
	runtime    RuntimeClient

	mu      sync.Mutex
	runs    map[string]struct{}
	started bool
	closed  bool
	wg      sync.WaitGroup
}

func newProjectionManager(repository GatewayRepository, runtime RuntimeClient) *projectionManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &projectionManager{
		rootCtx: ctx, cancel: cancel, repository: repository, runtime: runtime,
		runs: make(map[string]struct{}),
	}
}

func (m *projectionManager) Start(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return context.Canceled
	}
	m.started = true
	m.mu.Unlock()
	runs, err := m.repository.ListProjectable(ctx, projectableRunLimit)
	if err != nil {
		return err
	}
	for _, run := range runs {
		m.Ensure(run)
	}
	return nil
}

func (m *projectionManager) Ensure(run Run) {
	if m == nil || m.runtime == nil || run.ID == "" || run.ProjectionComplete {
		return
	}
	m.mu.Lock()
	if m.closed || !m.started {
		m.mu.Unlock()
		return
	}
	if _, exists := m.runs[run.ID]; exists {
		m.mu.Unlock()
		return
	}
	m.runs[run.ID] = struct{}{}
	m.wg.Add(1)
	m.mu.Unlock()
	go m.project(run)
}

func (m *projectionManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *projectionManager) project(seed Run) {
	defer func() {
		m.mu.Lock()
		delete(m.runs, seed.ID)
		m.mu.Unlock()
		m.wg.Done()
	}()
	backoff := projectorInitialBackoff
	for m.rootCtx.Err() == nil {
		local, err := m.repository.Find(m.rootCtx, seed.ID)
		if err != nil || local.ProjectionComplete {
			return
		}
		requestID, idErr := newCorrelationID("req-")
		if idErr != nil {
			return
		}
		stream, streamErr := m.runtime.StreamRunEvents(m.rootCtx, RuntimeStreamRequest{
			RunID:    local.ID,
			AfterSeq: local.LastRuntimeSourceSeq,
			Context: RuntimeRequestContext{
				RequestID: requestID, TraceID: local.TraceID, RunID: local.ID,
				TenantID: local.TenantID, OperatorID: local.OperatorID,
			},
		})
		if streamErr != nil {
			if status.Code(streamErr) == codes.NotFound {
				if m.reconcileMissingRuntime(local) {
					return
				}
				if !waitProjectionBackoff(m.rootCtx, backoff) {
					return
				}
				backoff = nextProjectionBackoff(backoff)
				continue
			}
			if permanentRuntimeError(streamErr) {
				m.failProjection(local, streamErr)
				return
			}
			if !waitProjectionBackoff(m.rootCtx, backoff) {
				return
			}
			backoff = nextProjectionBackoff(backoff)
			continue
		}
		if stream == nil {
			if !waitProjectionBackoff(m.rootCtx, backoff) {
				return
			}
			backoff = nextProjectionBackoff(backoff)
			continue
		}
		progressed, terminal, recvErr := m.consume(local, stream)
		_ = stream.Close()
		if terminal || m.rootCtx.Err() != nil {
			return
		}
		if status.Code(recvErr) == codes.NotFound {
			if m.reconcileMissingRuntime(local) {
				return
			}
			if !waitProjectionBackoff(m.rootCtx, backoff) {
				return
			}
			backoff = nextProjectionBackoff(backoff)
			continue
		}
		if permanentRuntimeError(recvErr) {
			m.failProjection(local, recvErr)
			return
		}
		if progressed {
			backoff = projectorInitialBackoff
		} else {
			backoff = nextProjectionBackoff(backoff)
		}
		if !waitProjectionBackoff(m.rootCtx, backoff) {
			return
		}
	}
}

// reconcileMissingRuntime replays the original durable start specification.
// StartRun is idempotent for the same run ID/spec, so this safely covers both a
// genuinely missing runtime mirror and a lost StartRun response.
func (m *projectionManager) reconcileMissingRuntime(run Run) (stop bool) {
	requestID, err := newCorrelationID("req-")
	if err != nil {
		return false
	}
	projected, err := m.runtime.StartRun(m.rootCtx, RuntimeStartRequest{
		RunID: run.ID,
		Context: RuntimeRequestContext{
			RequestID: requestID, TraceID: run.TraceID, RunID: run.ID,
			TenantID: run.TenantID, OperatorID: run.OperatorID,
		},
		Goal: run.Goal, RunType: run.RunType, SessionID: run.SessionID,
		SessionVersion: run.SessionVersion, TurnInputJSON: run.TurnInputJSON,
	})
	if err != nil {
		if runtimeStartOutcomeAmbiguous(err) {
			return false
		}
		m.failProjection(run, err)
		return true
	}
	if projected.RunID != run.ID {
		m.failProjection(run, status.Error(codes.InvalidArgument, "invalid reconciled runtime run"))
		return true
	}
	if _, err := m.repository.ProjectRuntimeSnapshot(m.rootCtx, run.ID, projected); err != nil {
		return false
	}
	return false
}

func (m *projectionManager) consume(local Run, stream EventStream) (progressed, terminal bool, err error) {
	lastSource := local.LastRuntimeSourceSeq
	for m.rootCtx.Err() == nil {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			return progressed, false, recvErr
		}
		if event.RunID != local.ID || event.Seq <= 0 || !validRunStatus(event.Status) {
			return progressed, false, status.Error(codes.InvalidArgument, "invalid runtime event protocol")
		}
		if event.Seq <= lastSource {
			continue
		}
		if event.Seq != lastSource+1 {
			return progressed, false, status.Error(codes.Unavailable, "runtime event sequence handoff")
		}
		var terminalRun *RuntimeRun
		if isTerminalStatus(event.Status) {
			projected, getErr := m.runtime.GetRun(m.rootCtx, RuntimeGetRequest{
				RunID: local.ID,
				Context: RuntimeRequestContext{
					RequestID: mustCorrelationID("req-"), TraceID: local.TraceID, RunID: local.ID,
					TenantID: local.TenantID, OperatorID: local.OperatorID,
				},
			})
			if getErr != nil {
				return progressed, false, getErr
			}
			if projected.RunID != local.ID || !isTerminalStatus(projected.Status) || projected.Status != event.Status {
				return progressed, false, status.Error(codes.InvalidArgument, "invalid terminal runtime projection")
			}
			terminalRun = &projected
		}
		_, applied, projectErr := m.repository.ProjectRuntimeEvent(m.rootCtx, event, terminalRun)
		if projectErr != nil {
			return progressed, false, projectErr
		}
		lastSource = event.Seq
		progressed = progressed || applied
		if terminalRun != nil {
			return progressed, true, nil
		}
	}
	return progressed, false, m.rootCtx.Err()
}

func (m *projectionManager) failProjection(run Run, cause error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(m.rootCtx), projectorFailureTimeout)
	defer cancel()
	now := time.Now().UnixMilli()
	message := "Agent Runtime rejected the durable event projection."
	event := Event{RunID: run.ID, Seq: 0, EventType: runtimeFailureEventType, Message: message,
		PayloadJSON: `{"errorCode":"AGENT_RUNTIME_ERROR"}`, CreatedAtUnixMs: now}
	for attempt := 0; attempt < 3; attempt++ {
		current, err := m.repository.Find(ctx, run.ID)
		if err != nil || current.ProjectionComplete {
			return
		}
		if isTerminalStatus(current.Status) {
			err = m.repository.CompleteProjection(ctx, current.ID, current.Version)
			if err == nil || !errors.Is(err, apperr.ErrConflict) {
				return
			}
			continue
		}
		err = m.repository.FailRuntimeStart(ctx, current.ID, current.Version, ErrorCodeRuntimeError, message, event)
		if err == nil || !errors.Is(err, apperr.ErrConflict) {
			return
		}
	}
	_ = cause
}

func permanentRuntimeError(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch status.Code(err) {
	case codes.PermissionDenied, codes.Unauthenticated, codes.NotFound, codes.InvalidArgument, codes.FailedPrecondition, codes.Unimplemented:
		return true
	default:
		return false
	}
}

func waitProjectionBackoff(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextProjectionBackoff(current time.Duration) time.Duration {
	current *= 2
	if current > projectorMaxBackoff {
		return projectorMaxBackoff
	}
	return current
}

func mustCorrelationID(prefix string) string {
	value, _ := newCorrelationID(prefix)
	return value
}

type repositoryEventStream struct {
	ctx        context.Context
	repository GatewayRepository
	runID      string
	afterSeq   int64
	buffer     []Event
	closed     bool
}

func (s *repositoryEventStream) Recv() (RuntimeEvent, error) {
	for !s.closed {
		if len(s.buffer) > 0 {
			event := s.buffer[0]
			s.buffer = s.buffer[1:]
			s.afterSeq = event.Seq
			return RuntimeEvent{RunID: event.RunID, Seq: event.Seq, Type: event.EventType,
				Message: event.Message, PayloadJSON: event.PayloadJSON, CreatedAtUnixMs: event.CreatedAtUnixMs}, nil
		}
		events, err := s.repository.ListEvents(s.ctx, s.runID, s.afterSeq, localEventReadLimit)
		if err != nil {
			return RuntimeEvent{}, err
		}
		if len(events) > 0 {
			s.buffer = events
			continue
		}
		run, err := s.repository.Find(s.ctx, s.runID)
		if err != nil {
			return RuntimeEvent{}, err
		}
		if run.ProjectionComplete || isLocalRuntimeUnavailable(run) {
			return RuntimeEvent{}, io.EOF
		}
		if !waitProjectionBackoff(s.ctx, durableEventPollInterval) {
			return RuntimeEvent{}, s.ctx.Err()
		}
	}
	return RuntimeEvent{}, io.EOF
}

func (s *repositoryEventStream) Close() error {
	s.closed = true
	return nil
}
