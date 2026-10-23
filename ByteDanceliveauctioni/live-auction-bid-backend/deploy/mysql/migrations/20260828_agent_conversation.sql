-- Persist recoverable agent conversations and link asynchronous runs to turns.
-- Safe to execute repeatedly against the live_auction schema.

CREATE TABLE IF NOT EXISTS agent_runs (
  id VARCHAR(64) PRIMARY KEY,
  tenant_id VARCHAR(64) NOT NULL,
  operator_id VARCHAR(64) NOT NULL,
  idempotency_key VARCHAR(128) NOT NULL,
  trace_id VARCHAR(128) NOT NULL DEFAULT '',
  goal TEXT NOT NULL,
  status VARCHAR(32) NOT NULL,
  proposal_json TEXT NOT NULL,
  error_code VARCHAR(64) NOT NULL DEFAULT '',
  error_message TEXT NOT NULL,
  version BIGINT NOT NULL DEFAULT 1,
  last_runtime_source_seq BIGINT NOT NULL DEFAULT 0,
  projection_complete BOOLEAN NOT NULL DEFAULT FALSE,
  run_type INT NOT NULL DEFAULT 0,
  session_id VARCHAR(64) NOT NULL DEFAULT '',
  session_version BIGINT NOT NULL DEFAULT 0,
  turn_input_json TEXT NOT NULL,
  turn_result_json TEXT NOT NULL,
  created_at_unix_ms BIGINT NOT NULL,
  updated_at_unix_ms BIGINT NOT NULL,
  created_at DATETIME(3) NULL,
  updated_at DATETIME(3) NULL,
  UNIQUE INDEX uidx_agent_run_idempotency (tenant_id, operator_id, idempotency_key),
  INDEX idx_agent_run_tenant_updated (tenant_id, updated_at),
  INDEX idx_agent_run_projection_complete (projection_complete),
  INDEX idx_agent_run_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

DROP PROCEDURE IF EXISTS migrate_20260828_agent_run_linkage;

DELIMITER $$
CREATE PROCEDURE migrate_20260828_agent_run_linkage()
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'agent_runs' AND column_name = 'session_id'
  ) THEN
    ALTER TABLE agent_runs ADD COLUMN session_id VARCHAR(64) NOT NULL DEFAULT '';
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'agent_runs' AND column_name = 'run_type'
  ) THEN
    ALTER TABLE agent_runs ADD COLUMN run_type INT NOT NULL DEFAULT 0;
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'agent_runs' AND column_name = 'session_version'
  ) THEN
    ALTER TABLE agent_runs ADD COLUMN session_version BIGINT NOT NULL DEFAULT 0;
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'agent_runs' AND column_name = 'turn_input_json'
  ) THEN
    ALTER TABLE agent_runs ADD COLUMN turn_input_json TEXT NOT NULL;
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'agent_runs' AND column_name = 'turn_result_json'
  ) THEN
    ALTER TABLE agent_runs ADD COLUMN turn_result_json TEXT NOT NULL;
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM information_schema.statistics
    WHERE table_schema = DATABASE() AND table_name = 'agent_runs' AND index_name = 'idx_agent_run_session'
  ) THEN
    ALTER TABLE agent_runs ADD INDEX idx_agent_run_session (session_id);
  END IF;
END$$
DELIMITER ;

CALL migrate_20260828_agent_run_linkage();
DROP PROCEDURE IF EXISTS migrate_20260828_agent_run_linkage;

CREATE TABLE IF NOT EXISTS agent_run_events (
  run_id VARCHAR(64) NOT NULL,
  seq BIGINT NOT NULL,
  runtime_source_seq BIGINT NULL,
  event_type VARCHAR(64) NOT NULL,
  message TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  created_at_unix_ms BIGINT NOT NULL,
  created_at DATETIME(3) NULL,
  UNIQUE INDEX uidx_agent_run_event_seq (run_id, seq),
  UNIQUE INDEX uidx_agent_run_runtime_source_seq (run_id, runtime_source_seq),
  INDEX idx_agent_run_event_run_seq (run_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS agent_sessions (
  id VARCHAR(64) PRIMARY KEY,
  tenant_id VARCHAR(64) NOT NULL,
  operator_id VARCHAR(64) NOT NULL,
  room_id VARCHAR(64) NOT NULL,
  status VARCHAR(32) NOT NULL,
  version BIGINT NOT NULL DEFAULT 1,
  conversation_summary TEXT NOT NULL,
  active_snapshot_version BIGINT NOT NULL DEFAULT 0,
  created_at_unix_ms BIGINT NOT NULL,
  updated_at_unix_ms BIGINT NOT NULL,
  created_at DATETIME(3) NULL,
  updated_at DATETIME(3) NULL,
  INDEX idx_agent_session_tenant_updated (tenant_id, updated_at_unix_ms)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS agent_session_messages (
  session_id VARCHAR(64) NOT NULL,
  seq BIGINT NOT NULL,
  role VARCHAR(32) NOT NULL,
  content TEXT NOT NULL,
  run_id VARCHAR(64) NOT NULL DEFAULT '',
  created_at_unix_ms BIGINT NOT NULL,
  created_at DATETIME(3) NULL,
  UNIQUE INDEX uidx_agent_session_message_seq (session_id, seq),
  INDEX idx_agent_session_message_run (run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS agent_draft_candidates (
  id VARCHAR(64) PRIMARY KEY,
  session_id VARCHAR(64) NOT NULL,
  source_type VARCHAR(32) NOT NULL,
  existing_lot_id VARCHAR(64) NOT NULL DEFAULT '',
  position INT NOT NULL,
  selected_by_default BOOLEAN NOT NULL DEFAULT TRUE,
  draft_json TEXT NOT NULL,
  field_sources_json TEXT NOT NULL,
  validation_json TEXT NOT NULL,
  provenance_version INT NOT NULL DEFAULT 0,
  version BIGINT NOT NULL DEFAULT 1,
  created_at_unix_ms BIGINT NOT NULL,
  updated_at_unix_ms BIGINT NOT NULL,
  created_at DATETIME(3) NULL,
  updated_at DATETIME(3) NULL,
  UNIQUE INDEX uidx_agent_draft_candidate_position (session_id, position)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Rows predating schema-bound provenance remain version 0. Application-created
-- candidates explicitly write version 1, so compatibility backfill can never
-- weaken validation for new data.
ALTER TABLE agent_draft_candidates
  ADD COLUMN IF NOT EXISTS provenance_version INT NOT NULL DEFAULT 0 AFTER validation_json;

CREATE TABLE IF NOT EXISTS agent_candidate_assets (
  candidate_id VARCHAR(64) PRIMARY KEY,
  session_id VARCHAR(64) NOT NULL,
  asset_id VARCHAR(64) NOT NULL,
  tenant_id VARCHAR(64) NOT NULL,
  operator_id VARCHAR(64) NOT NULL,
  created_at_unix_ms BIGINT NOT NULL,
  updated_at_unix_ms BIGINT NOT NULL,
  created_at DATETIME(3) NULL,
  updated_at DATETIME(3) NULL,
  UNIQUE INDEX uidx_agent_candidate_asset_asset (asset_id),
  INDEX idx_agent_candidate_asset_session (session_id),
  INDEX idx_agent_candidate_asset_tenant (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS agent_approval_snapshots (
  session_id VARCHAR(64) NOT NULL,
  snapshot_version BIGINT NOT NULL,
  snapshot_json LONGTEXT NOT NULL,
  content_hash CHAR(64) NOT NULL,
  created_by VARCHAR(64) NOT NULL,
  created_at_unix_ms BIGINT NOT NULL,
  created_at DATETIME(3) NULL,
  UNIQUE INDEX uidx_agent_approval_snapshot_version (session_id, snapshot_version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
