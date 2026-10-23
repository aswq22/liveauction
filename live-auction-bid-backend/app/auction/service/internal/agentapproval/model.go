// Package agentapproval defines the trusted Go boundary that applies an
// immutable agent review snapshot. It intentionally contains no database
// implementation; the all-or-nothing transaction is supplied by the data
// layer through TransactionalRepository.
package agentapproval

import "context"

type ApplyTarget string

const (
	ApplyTargetQueue         ApplyTarget = "QUEUE"
	ApplyTargetInventoryOnly ApplyTarget = "INVENTORY_ONLY"
)

type ApplyCommand struct {
	TenantID             string
	OperatorID           string
	SessionID            string
	SnapshotVersion      int64
	ContentHash          string
	SelectedCandidateIDs []string
	OrderedCandidateIDs  []string
	IdempotencyKey       string
	TraceID              string
	Target               ApplyTarget
}

type ExecutionStatus string

const (
	ExecutionApplying ExecutionStatus = "APPLYING"
	ExecutionApplied  ExecutionStatus = "APPLIED"
	ExecutionFailed   ExecutionStatus = "FAILED"
)

// Execution is the durable result of one approval request. RequestFingerprint
// is persisted with the idempotency key so a key can never authorize a
// different payload.
type Execution struct {
	ID                   string
	TenantID             string
	OperatorID           string
	SessionID            string
	SnapshotVersion      int64
	IdempotencyKey       string
	RequestFingerprint   string
	Status               ExecutionStatus
	SelectedCandidateIDs []string
	OrderedCandidateIDs  []string
	CreatedLotIDs        []string
	QueuePositions       []int32
	ErrorCode            string
	CreatedAtUnixMs      int64
	CompletedAtUnixMs    int64
	TraceID              string
	IdempotentReplay     bool
	Target               ApplyTarget
}

// TransactionalRepository owns every mutable authorization and state check.
// Apply must re-lock and revalidate the session, snapshot, lots, assets and
// queue state in the same transaction as all resulting writes. The service's
// immutable-snapshot preflight is not a substitute for those checks.
type TransactionalRepository interface {
	Apply(ctx context.Context, command ApplyCommand) (Execution, error)
	FindExecutionByKey(ctx context.Context, sessionID string, snapshotVersion int64, key string) (Execution, error)
}
