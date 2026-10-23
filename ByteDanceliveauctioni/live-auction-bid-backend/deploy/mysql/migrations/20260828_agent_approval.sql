CREATE TABLE IF NOT EXISTS agent_proposal_executions (
  id VARCHAR(64) PRIMARY KEY,
  tenant_id VARCHAR(64) NOT NULL,
  operator_id VARCHAR(64) NOT NULL,
  session_id VARCHAR(64) NOT NULL,
  snapshot_version BIGINT NOT NULL,
  idempotency_key VARCHAR(128) NOT NULL,
  request_fingerprint CHAR(64) NOT NULL,
  status VARCHAR(32) NOT NULL,
  selected_candidate_ids_json JSON NOT NULL,
  ordered_candidate_ids_json JSON NOT NULL,
  created_lot_ids_json JSON NOT NULL,
  queue_positions_json JSON NOT NULL,
  error_code VARCHAR(64) NOT NULL DEFAULT '',
  created_at_unix_ms BIGINT NOT NULL,
  completed_at_unix_ms BIGINT NOT NULL DEFAULT 0,
	trace_id VARCHAR(128) NOT NULL DEFAULT '',
  created_at DATETIME(3) NULL,
  updated_at DATETIME(3) NULL,
  UNIQUE INDEX uk_agent_execution_idempotency (session_id, snapshot_version, idempotency_key),
  INDEX idx_agent_execution_tenant (tenant_id),
  INDEX idx_agent_execution_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

DROP PROCEDURE IF EXISTS ensure_agent_approval_trace_id;
DELIMITER $$
CREATE PROCEDURE ensure_agent_approval_trace_id()
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = DATABASE()
      AND table_name = 'agent_proposal_executions'
      AND column_name = 'trace_id'
  ) THEN
    ALTER TABLE agent_proposal_executions
      ADD COLUMN trace_id VARCHAR(128) NOT NULL DEFAULT '' AFTER completed_at_unix_ms;
  END IF;
END$$
DELIMITER ;
CALL ensure_agent_approval_trace_id();
DROP PROCEDURE ensure_agent_approval_trace_id;

CREATE TABLE IF NOT EXISTS agent_approval_outbox (
  id VARCHAR(64) PRIMARY KEY,
  execution_id VARCHAR(64) NOT NULL,
  session_id VARCHAR(64) NOT NULL,
  room_id VARCHAR(64) NOT NULL,
  trace_id VARCHAR(128) NOT NULL DEFAULT '',
  payload_json JSON NOT NULL,
  visible_at_unix_ms BIGINT NOT NULL,
  published_at_unix_ms BIGINT NOT NULL DEFAULT 0,
  claim_token VARCHAR(96) NOT NULL DEFAULT '',
  claim_until_unix_ms BIGINT NOT NULL DEFAULT 0,
  last_publish_error VARCHAR(256) NOT NULL DEFAULT '',
  publish_attempts INT NOT NULL DEFAULT 0,
  next_attempt_at_unix_ms BIGINT NOT NULL DEFAULT 0,
  dead_lettered_at_unix_ms BIGINT NOT NULL DEFAULT 0,
  created_at DATETIME(3) NULL,
  UNIQUE INDEX uk_agent_approval_outbox_execution (execution_id),
  INDEX idx_agent_approval_outbox_session (session_id),
  INDEX idx_agent_approval_outbox_room (room_id),
  INDEX idx_agent_approval_outbox_pending (published_at_unix_ms),
  INDEX idx_agent_approval_outbox_claim (claim_until_unix_ms),
  INDEX idx_agent_approval_outbox_retry (next_attempt_at_unix_ms),
  INDEX idx_agent_approval_outbox_dead (dead_lettered_at_unix_ms)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

DROP PROCEDURE IF EXISTS ensure_agent_approval_outbox_claims;
DELIMITER $$
CREATE PROCEDURE ensure_agent_approval_outbox_claims()
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='agent_approval_outbox' AND column_name='claim_token') THEN
    ALTER TABLE agent_approval_outbox ADD COLUMN claim_token VARCHAR(96) NOT NULL DEFAULT '' AFTER published_at_unix_ms;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='agent_approval_outbox' AND column_name='claim_until_unix_ms') THEN
    ALTER TABLE agent_approval_outbox ADD COLUMN claim_until_unix_ms BIGINT NOT NULL DEFAULT 0 AFTER claim_token;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='agent_approval_outbox' AND column_name='last_publish_error') THEN
    ALTER TABLE agent_approval_outbox ADD COLUMN last_publish_error VARCHAR(256) NOT NULL DEFAULT '' AFTER claim_until_unix_ms;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='agent_approval_outbox' AND column_name='publish_attempts') THEN
    ALTER TABLE agent_approval_outbox ADD COLUMN publish_attempts INT NOT NULL DEFAULT 0 AFTER last_publish_error;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='agent_approval_outbox' AND column_name='next_attempt_at_unix_ms') THEN
    ALTER TABLE agent_approval_outbox ADD COLUMN next_attempt_at_unix_ms BIGINT NOT NULL DEFAULT 0 AFTER publish_attempts;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='agent_approval_outbox' AND column_name='dead_lettered_at_unix_ms') THEN
    ALTER TABLE agent_approval_outbox ADD COLUMN dead_lettered_at_unix_ms BIGINT NOT NULL DEFAULT 0 AFTER next_attempt_at_unix_ms;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='agent_approval_outbox' AND index_name='idx_agent_approval_outbox_claim') THEN
    CREATE INDEX idx_agent_approval_outbox_claim ON agent_approval_outbox(claim_until_unix_ms);
  END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='agent_approval_outbox' AND index_name='idx_agent_approval_outbox_retry') THEN
    CREATE INDEX idx_agent_approval_outbox_retry ON agent_approval_outbox(next_attempt_at_unix_ms);
  END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='agent_approval_outbox' AND index_name='idx_agent_approval_outbox_dead') THEN
    CREATE INDEX idx_agent_approval_outbox_dead ON agent_approval_outbox(dead_lettered_at_unix_ms);
  END IF;
END$$
DELIMITER ;
CALL ensure_agent_approval_outbox_claims();
DROP PROCEDURE ensure_agent_approval_outbox_claims;

CREATE TABLE IF NOT EXISTS agent_approval_audits (
  id VARCHAR(64) PRIMARY KEY,
  execution_id VARCHAR(64) NOT NULL,
  tenant_id VARCHAR(64) NOT NULL,
  operator_id VARCHAR(64) NOT NULL,
  session_id VARCHAR(64) NOT NULL,
  snapshot_version BIGINT NOT NULL,
  details_json JSON NOT NULL,
  created_at_unix_ms BIGINT NOT NULL,
  created_at DATETIME(3) NULL,
  UNIQUE INDEX uk_agent_approval_audit_execution (execution_id),
  INDEX idx_agent_approval_audit_tenant (tenant_id),
  INDEX idx_agent_approval_audit_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
