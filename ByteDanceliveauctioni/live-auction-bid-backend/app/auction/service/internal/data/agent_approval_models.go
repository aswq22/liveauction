package data

import "time"

// AgentProposalExecutionModel is both the durable idempotency record and the
// transaction result. The unique key is deliberately scoped to one immutable
// snapshot; an idempotency key cannot authorize a later snapshot.
type AgentProposalExecutionModel struct {
	ID                       string `gorm:"column:id;type:varchar(64);primaryKey"`
	TenantID                 string `gorm:"column:tenant_id;type:varchar(64);not null;index:idx_agent_execution_tenant"`
	OperatorID               string `gorm:"column:operator_id;type:varchar(64);not null"`
	SessionID                string `gorm:"column:session_id;type:varchar(64);not null;uniqueIndex:uk_agent_execution_idempotency,priority:1"`
	SnapshotVersion          int64  `gorm:"column:snapshot_version;not null;uniqueIndex:uk_agent_execution_idempotency,priority:2"`
	IdempotencyKey           string `gorm:"column:idempotency_key;type:varchar(128);not null;uniqueIndex:uk_agent_execution_idempotency,priority:3"`
	RequestFingerprint       string `gorm:"column:request_fingerprint;type:char(64);not null"`
	Status                   string `gorm:"column:status;type:varchar(32);not null;index:idx_agent_execution_status"`
	SelectedCandidateIDsJSON string `gorm:"column:selected_candidate_ids_json;type:json;not null"`
	OrderedCandidateIDsJSON  string `gorm:"column:ordered_candidate_ids_json;type:json;not null"`
	CreatedLotIDsJSON        string `gorm:"column:created_lot_ids_json;type:json;not null"`
	QueuePositionsJSON       string `gorm:"column:queue_positions_json;type:json;not null"`
	ErrorCode                string `gorm:"column:error_code;type:varchar(64);not null;default:''"`
	CreatedAtUnixMs          int64  `gorm:"column:created_at_unix_ms;not null"`
	CompletedAtUnixMs        int64  `gorm:"column:completed_at_unix_ms;not null;default:0"`
	TraceID                  string `gorm:"column:trace_id;type:varchar(128);not null;default:''"`
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

func (AgentProposalExecutionModel) TableName() string { return "agent_proposal_executions" }

// AgentApprovalAuditModel is immutable and one-to-one with a successful
// execution. Details is canonical request/result metadata, not conversation
// text or model-provider material.
type AgentApprovalAuditModel struct {
	ID              string `gorm:"column:id;type:varchar(64);primaryKey"`
	ExecutionID     string `gorm:"column:execution_id;type:varchar(64);not null;uniqueIndex:uk_agent_approval_audit_execution"`
	TenantID        string `gorm:"column:tenant_id;type:varchar(64);not null;index:idx_agent_approval_audit_tenant"`
	OperatorID      string `gorm:"column:operator_id;type:varchar(64);not null"`
	SessionID       string `gorm:"column:session_id;type:varchar(64);not null;index:idx_agent_approval_audit_session"`
	SnapshotVersion int64  `gorm:"column:snapshot_version;not null"`
	DetailsJSON     string `gorm:"column:details_json;type:json;not null"`
	CreatedAtUnixMs int64  `gorm:"column:created_at_unix_ms;not null"`
	CreatedAt       time.Time
}

func (AgentApprovalAuditModel) TableName() string { return "agent_approval_audits" }

// AgentApprovalOutboxModel is the safe, transactionally-visible integration
// event for proposal approval. PayloadJSON intentionally contains identifiers
// only; auction display events remain separate records.
type AgentApprovalOutboxModel struct {
	ID                   string `gorm:"column:id;type:varchar(64);primaryKey"`
	ExecutionID          string `gorm:"column:execution_id;type:varchar(64);not null;uniqueIndex:uk_agent_approval_outbox_execution"`
	SessionID            string `gorm:"column:session_id;type:varchar(64);not null;index:idx_agent_approval_outbox_session"`
	RoomID               string `gorm:"column:room_id;type:varchar(64);not null;index:idx_agent_approval_outbox_room"`
	TraceID              string `gorm:"column:trace_id;type:varchar(128);not null;default:''"`
	PayloadJSON          string `gorm:"column:payload_json;type:json;not null"`
	VisibleAtUnixMs      int64  `gorm:"column:visible_at_unix_ms;not null"`
	PublishedAtUnixMs    int64  `gorm:"column:published_at_unix_ms;not null;default:0;index:idx_agent_approval_outbox_pending"`
	ClaimToken           string `gorm:"column:claim_token;type:varchar(96);not null;default:''"`
	ClaimUntilUnixMs     int64  `gorm:"column:claim_until_unix_ms;not null;default:0;index:idx_agent_approval_outbox_claim"`
	LastPublishError     string `gorm:"column:last_publish_error;type:varchar(256);not null;default:''"`
	PublishAttempts      int32  `gorm:"column:publish_attempts;not null;default:0"`
	NextAttemptAtUnixMs  int64  `gorm:"column:next_attempt_at_unix_ms;not null;default:0;index:idx_agent_approval_outbox_retry"`
	DeadLetteredAtUnixMs int64  `gorm:"column:dead_lettered_at_unix_ms;not null;default:0;index:idx_agent_approval_outbox_dead"`
	CreatedAt            time.Time
}

func (AgentApprovalOutboxModel) TableName() string { return "agent_approval_outbox" }
