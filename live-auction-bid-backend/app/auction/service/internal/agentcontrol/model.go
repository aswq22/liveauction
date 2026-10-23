package agentcontrol

import (
	"context"

	agentv1 "live-auction-bid/backend/api/agent/v1"
)

type RunStatus string

const (
	StatusCreated            RunStatus = "CREATED"
	StatusPlanning           RunStatus = "PLANNING"
	StatusCollectingContext  RunStatus = "COLLECTING_CONTEXT"
	StatusGeneratingProposal RunStatus = "GENERATING_PROPOSAL"
	StatusVerifying          RunStatus = "VERIFYING"
	StatusCompleted          RunStatus = "COMPLETED"
	StatusFailed             RunStatus = "FAILED"
	StatusCancelled          RunStatus = "CANCELLED"
)

type Run struct {
	ID                   string
	TenantID             string
	OperatorID           string
	TraceID              string
	Goal                 string
	IdempotencyKey       string
	Status               RunStatus
	ProposalJSON         string
	ErrorCode            string
	ErrorMessage         string
	Version              int64
	LastRuntimeSourceSeq int64
	ProjectionComplete   bool
	RunType              agentv1.RunType
	SessionID            string
	SessionVersion       int64
	TurnInputJSON        string
	TurnResultJSON       string
	CreatedAtUnixMs      int64
	UpdatedAtUnixMs      int64
}

type Event struct {
	RunID            string
	Seq              int64
	RuntimeSourceSeq int64
	EventType        string
	Message          string
	PayloadJSON      string
	CreatedAtUnixMs  int64
}

type RunRepository interface {
	Create(ctx context.Context, run Run) error
	CreateIdempotent(ctx context.Context, run Run) (persisted Run, created bool, err error)
	Find(ctx context.Context, runID string) (Run, error)
	ListProjectable(ctx context.Context, limit int) ([]Run, error)
	ProjectRuntimeEvent(ctx context.Context, event RuntimeEvent, terminal *RuntimeRun) (Run, bool, error)
	ProjectRuntimeSnapshot(ctx context.Context, runID string, runtime RuntimeRun) (Run, error)
	UpdateStatus(ctx context.Context, runID string, expectedVersion int64, status RunStatus, proposalJSON, errorCode, errorMessage string) error
	AppendEvent(ctx context.Context, event Event) error
	ListEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]Event, error)
}
