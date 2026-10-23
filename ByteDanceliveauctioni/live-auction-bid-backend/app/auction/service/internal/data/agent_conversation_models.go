package data

import "time"

type AgentSessionModel struct {
	ID                    string `gorm:"column:id;type:varchar(64);primaryKey"`
	TenantID              string `gorm:"column:tenant_id;type:varchar(64);not null;index:idx_agent_session_tenant_updated,priority:1"`
	OperatorID            string `gorm:"column:operator_id;type:varchar(64);not null"`
	RoomID                string `gorm:"column:room_id;type:varchar(64);not null"`
	Status                string `gorm:"column:status;type:varchar(32);not null"`
	Version               int64  `gorm:"column:version;not null;default:1"`
	ConversationSummary   string `gorm:"column:conversation_summary;type:text;not null"`
	ActiveSnapshotVersion int64  `gorm:"column:active_snapshot_version;not null;default:0"`
	CreatedAtUnixMs       int64  `gorm:"column:created_at_unix_ms;not null"`
	UpdatedAtUnixMs       int64  `gorm:"column:updated_at_unix_ms;not null;index:idx_agent_session_tenant_updated,priority:2"`
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

func (AgentSessionModel) TableName() string { return "agent_sessions" }

type AgentSessionMessageModel struct {
	SessionID       string `gorm:"column:session_id;type:varchar(64);not null;uniqueIndex:uidx_agent_session_message_seq,priority:1"`
	Seq             int64  `gorm:"column:seq;not null;uniqueIndex:uidx_agent_session_message_seq,priority:2"`
	Role            string `gorm:"column:role;type:varchar(32);not null"`
	Content         string `gorm:"column:content;type:text;not null"`
	RunID           string `gorm:"column:run_id;type:varchar(64);not null;default:'';index:idx_agent_session_message_run"`
	CreatedAtUnixMs int64  `gorm:"column:created_at_unix_ms;not null"`
	CreatedAt       time.Time
}

func (AgentSessionMessageModel) TableName() string { return "agent_session_messages" }

type AgentDraftCandidateModel struct {
	ID                string `gorm:"column:id;type:varchar(64);primaryKey"`
	SessionID         string `gorm:"column:session_id;type:varchar(64);not null;uniqueIndex:uidx_agent_draft_candidate_position,priority:1"`
	SourceType        string `gorm:"column:source_type;type:varchar(32);not null"`
	ExistingLotID     string `gorm:"column:existing_lot_id;type:varchar(64);not null;default:''"`
	Position          int32  `gorm:"column:position;not null;uniqueIndex:uidx_agent_draft_candidate_position,priority:2"`
	SelectedByDefault *bool  `gorm:"column:selected_by_default;not null;default:true"`
	DraftJSON         string `gorm:"column:draft_json;type:text;not null"`
	FieldSourcesJSON  string `gorm:"column:field_sources_json;type:text;not null"`
	ValidationJSON    string `gorm:"column:validation_json;type:text;not null"`
	ProvenanceVersion int32  `gorm:"column:provenance_version;type:int;not null;default:0"`
	Version           int64  `gorm:"column:version;not null;default:1"`
	CreatedAtUnixMs   int64  `gorm:"column:created_at_unix_ms;not null"`
	UpdatedAtUnixMs   int64  `gorm:"column:updated_at_unix_ms;not null"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (AgentDraftCandidateModel) TableName() string { return "agent_draft_candidates" }

// AgentApprovalSnapshotModel is append-only. Repository code only exposes an
// insert operation; the composite identity prevents replacing an earlier
// approval version.
type AgentApprovalSnapshotModel struct {
	SessionID       string `gorm:"column:session_id;type:varchar(64);not null;uniqueIndex:uidx_agent_approval_snapshot_version,priority:1"`
	SnapshotVersion int64  `gorm:"column:snapshot_version;not null;uniqueIndex:uidx_agent_approval_snapshot_version,priority:2"`
	SnapshotJSON    string `gorm:"column:snapshot_json;type:longtext;not null"`
	ContentHash     string `gorm:"column:content_hash;type:char(64);not null"`
	CreatedBy       string `gorm:"column:created_by;type:varchar(64);not null"`
	CreatedAtUnixMs int64  `gorm:"column:created_at_unix_ms;not null"`
	CreatedAt       time.Time
}

func (AgentApprovalSnapshotModel) TableName() string { return "agent_approval_snapshots" }
