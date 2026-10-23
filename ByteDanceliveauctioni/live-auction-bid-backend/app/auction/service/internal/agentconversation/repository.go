package agentconversation

import (
	"context"
	"encoding/json"

	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
)

type MessageRole string

const (
	MessageRoleUser      MessageRole = "USER"
	MessageRoleAssistant MessageRole = "ASSISTANT"
)

type Session struct {
	ID                    string
	TenantID              string
	OperatorID            string
	RoomID                string
	Status                SessionStatus
	Version               int64
	ConversationSummary   string
	ActiveSnapshotVersion int64
	CreatedAtUnixMs       int64
	UpdatedAtUnixMs       int64
}

type Message struct {
	SessionID       string
	Seq             int64
	Role            MessageRole
	Content         string
	RunID           string
	CreatedAtUnixMs int64
}

type DraftCandidate struct {
	Candidate         Candidate
	Position          int32
	SelectedByDefault bool
	FieldSourcesJSON  string
	ValidationJSON    string
	// ProvenanceVersion is 0 only for rows created before provenance became
	// schema-bound. Current application writes use version 1 and never receive
	// legacy missing-source backfill.
	ProvenanceVersion int32
	Version           int64
	CreatedAtUnixMs   int64
	UpdatedAtUnixMs   int64
}

const CurrentProvenanceVersion int32 = 1

type SessionAggregate struct {
	Session          Session
	Messages         []Message
	Candidates       []DraftCandidate
	Runs             []agentcontrol.Run
	AppliedExecution *RecoveredExecution
}

// RecoveredExecution is a browser-safe projection of the durable APPLIED
// execution and its immutable snapshot mapping.
type RecoveredExecution struct {
	ExecutionID     string               `json:"executionId"`
	SnapshotVersion int64                `json:"snapshotVersion"`
	CreatedLotIDs   []string             `json:"createdLotIds"`
	QueuedLots      []RecoveredQueuedLot `json:"queuedLots"`
	Target          string               `json:"target"`
}

type RecoveredQueuedLot struct {
	CandidateID   string     `json:"candidateId"`
	SourceType    SourceType `json:"sourceType"`
	LotID         string     `json:"lotId"`
	QueuePosition int32      `json:"queuePosition"`
}

type StartTurnCommand struct {
	TenantID        string
	OperatorID      string
	SessionID       string
	ExpectedVersion int64
	Message         Message
	Run             agentcontrol.Run
}

type MergeTurnCommand struct {
	TenantID              string
	OperatorID            string
	SessionID             string
	RunID                 string
	ExpectedVersion       int64
	AssistantMessage      Message
	Candidates            []DraftCandidate
	ConversationSummary   string
	Status                SessionStatus
	ActiveSnapshotVersion int64
	UpdatedAtUnixMs       int64
}

type PatchCandidateCommand struct {
	TenantID                 string
	OperatorID               string
	SessionID                string
	CandidateID              string
	ExpectedSessionVersion   int64
	ExpectedCandidateVersion int64
	// Fields is the browser-facing JSON Merge Patch. The service consumes it
	// and supplies Candidate to the repository as the validated replacement.
	Fields          map[string]json.RawMessage
	Candidate       DraftCandidate
	Status          SessionStatus
	UpdatedAtUnixMs int64
}

type BindCandidateImageCommand struct {
	TenantID                 string
	OperatorID               string
	SessionID                string
	CandidateID              string
	AssetID                  string
	ExpectedSessionVersion   int64
	ExpectedCandidateVersion int64
	UpdatedAtUnixMs          int64
}

type CandidateImageBinder interface {
	BindCandidateImage(ctx context.Context, command BindCandidateImageCommand) (SessionAggregate, error)
}

type Repository interface {
	CreateSession(ctx context.Context, session Session) error
	FindSession(ctx context.Context, tenantID, sessionID string) (SessionAggregate, error)
	AppendUserMessageAndStartRun(ctx context.Context, command StartTurnCommand) (SessionAggregate, error)
	MergeTurnResult(ctx context.Context, command MergeTurnCommand) (SessionAggregate, error)
	PatchCandidate(ctx context.Context, command PatchCandidateCommand) (SessionAggregate, error)
	CancelSession(ctx context.Context, tenantID, operatorID, sessionID string, expectedVersion int64) error
}
