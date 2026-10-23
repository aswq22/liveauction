package data

import "time"

type AgentRunModel struct {
	ID                   string `gorm:"column:id;type:varchar(64);primaryKey"`
	TenantID             string `gorm:"column:tenant_id;type:varchar(64);not null;index:idx_agent_run_tenant_updated,priority:1;uniqueIndex:uidx_agent_run_idempotency,priority:1"`
	OperatorID           string `gorm:"column:operator_id;type:varchar(64);not null;uniqueIndex:uidx_agent_run_idempotency,priority:2"`
	IdempotencyKey       string `gorm:"column:idempotency_key;type:varchar(128);not null;uniqueIndex:uidx_agent_run_idempotency,priority:3"`
	TraceID              string `gorm:"column:trace_id;type:varchar(128);not null;default:''"`
	Goal                 string `gorm:"column:goal;type:text;not null"`
	Status               string `gorm:"column:status;type:varchar(32);not null"`
	ProposalJSON         string `gorm:"column:proposal_json;type:text;not null"`
	ErrorCode            string `gorm:"column:error_code;type:varchar(64);not null;default:''"`
	ErrorMessage         string `gorm:"column:error_message;type:text;not null"`
	Version              int64  `gorm:"column:version;not null;default:1"`
	LastRuntimeSourceSeq int64  `gorm:"column:last_runtime_source_seq;not null;default:0"`
	ProjectionComplete   bool   `gorm:"column:projection_complete;not null;default:false;index:idx_agent_run_projection_complete"`
	RunType              int32  `gorm:"column:run_type;not null;default:0"`
	SessionID            string `gorm:"column:session_id;type:varchar(64);not null;default:'';index:idx_agent_run_session"`
	SessionVersion       int64  `gorm:"column:session_version;not null;default:0"`
	TurnInputJSON        string `gorm:"column:turn_input_json;type:text;not null"`
	TurnResultJSON       string `gorm:"column:turn_result_json;type:text;not null"`
	CreatedAtUnixMs      int64  `gorm:"column:created_at_unix_ms;not null"`
	UpdatedAtUnixMs      int64  `gorm:"column:updated_at_unix_ms;not null"`
	CreatedAt            time.Time
	UpdatedAt            time.Time `gorm:"index:idx_agent_run_tenant_updated,priority:2"`
}

func (AgentRunModel) TableName() string { return "agent_runs" }

type AgentRunEventModel struct {
	RunID            string `gorm:"column:run_id;type:varchar(64);not null;uniqueIndex:uidx_agent_run_event_seq,priority:1;uniqueIndex:uidx_agent_run_runtime_source_seq,priority:1;index:idx_agent_run_event_run_seq,priority:1"`
	Seq              int64  `gorm:"column:seq;not null;uniqueIndex:uidx_agent_run_event_seq,priority:2;index:idx_agent_run_event_run_seq,priority:2"`
	RuntimeSourceSeq *int64 `gorm:"column:runtime_source_seq;uniqueIndex:uidx_agent_run_runtime_source_seq,priority:2"`
	EventType        string `gorm:"column:event_type;type:varchar(64);not null"`
	Message          string `gorm:"column:message;type:text;not null"`
	PayloadJSON      string `gorm:"column:payload_json;type:text;not null"`
	CreatedAtUnixMs  int64  `gorm:"column:created_at_unix_ms;not null"`
	CreatedAt        time.Time
}

func (AgentRunEventModel) TableName() string { return "agent_run_events" }
