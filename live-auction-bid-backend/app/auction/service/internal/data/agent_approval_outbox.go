package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"live-auction-bid/backend/app/auction/service/internal/pkg/idgen"
)

const agentApprovalOutboxStream = "auction:agent-approval-events"

var ErrAgentApprovalOutboxRetryable = errors.New("AGENT_APPROVAL_OUTBOX_RETRYABLE")
var ErrAgentApprovalOutboxInternal = errors.New("AGENT_APPROVAL_OUTBOX_INTERNAL")

type AgentApprovalOutboxEvent struct {
	ID, SessionID, ExecutionID, RoomID, TraceID, Payload string
	VisibleAtUnixMs                                      int64
}

// Delivery is at-least-once: a process can stop after Redis accepts XADD but
// before MySQL records PublishedAtUnixMs. ID is stable across every retry so
// consumers can deduplicate without relying on a transport-specific stream ID.
type AgentApprovalOutboxPublisher interface {
	PublishAgentApproval(context.Context, AgentApprovalOutboxEvent) error
}
type AgentApprovalOutboxObserver interface {
	BeginOutboxPublish(context.Context, string) (context.Context, func(string))
	RecordOutboxVisibleLatency(time.Duration)
}
type AgentApprovalOutboxRepository struct {
	db        *gorm.DB
	publisher AgentApprovalOutboxPublisher
	observer  AgentApprovalOutboxObserver
	now       func() time.Time
}

func NewAgentApprovalOutboxRepository(db *gorm.DB, publisher AgentApprovalOutboxPublisher) *AgentApprovalOutboxRepository {
	return &AgentApprovalOutboxRepository{db: db, publisher: publisher, now: time.Now}
}
func (r *AgentApprovalOutboxRepository) WithObserver(o AgentApprovalOutboxObserver) *AgentApprovalOutboxRepository {
	if r != nil {
		r.observer = o
	}
	return r
}

func (r *AgentApprovalOutboxRepository) PublishPending(ctx context.Context, limit int) error {
	if r == nil || r.db == nil || r.publisher == nil {
		return errors.New("agent approval outbox publisher is not initialized")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, token, err := r.claim(ctx, limit)
	if err != nil {
		return err
	}
	retryableFailure := false
	internalFailure := false
	for _, row := range rows {
		event, err := approvalOutboxEvent(row)
		if err != nil {
			if updateErr := r.deadLetter(ctx, row.ID, token, "INVALID_PAYLOAD"); updateErr != nil {
				slog.Error("agent approval outbox dead-letter update failed", "event_id", row.ID, "error", updateErr)
				internalFailure = true
			}
			continue
		}
		publishCtx := ctx
		end := func(string) {}
		if r.observer != nil {
			publishCtx, end = r.observer.BeginOutboxPublish(ctx, row.TraceID)
		}
		err = r.publisher.PublishAgentApproval(publishCtx, event)
		if err != nil {
			end("retryable_failure")
			slog.Error("agent approval outbox transport failed", "event_id", row.ID, "error", err)
			if updateErr := r.retryLater(ctx, row, token); updateErr != nil {
				slog.Error("agent approval outbox retry update failed", "event_id", row.ID, "error", updateErr)
				internalFailure = true
			}
			retryableFailure = true
			continue
		}
		now := r.now()
		result := r.db.WithContext(ctx).Model(&AgentApprovalOutboxModel{}).Where("id = ? AND claim_token = ? AND published_at_unix_ms = 0", row.ID, token).Updates(map[string]any{"published_at_unix_ms": now.UnixMilli(), "claim_token": "", "claim_until_unix_ms": 0, "last_publish_error": "", "next_attempt_at_unix_ms": 0})
		if result.Error != nil || result.RowsAffected != 1 {
			end("retryable_failure")
			if result.Error != nil {
				slog.Error("agent approval outbox publish mark failed", "event_id", row.ID, "error", result.Error)
			}
			internalFailure = true
			continue
		}
		end("success")
		if r.observer != nil {
			r.observer.RecordOutboxVisibleLatency(time.Duration(now.UnixMilli()-row.VisibleAtUnixMs) * time.Millisecond)
		}
	}
	if internalFailure {
		return ErrAgentApprovalOutboxInternal
	}
	if retryableFailure {
		return ErrAgentApprovalOutboxRetryable
	}
	return nil
}

func (r *AgentApprovalOutboxRepository) claim(ctx context.Context, limit int) ([]AgentApprovalOutboxModel, string, error) {
	token := idgen.New("approval-claim")
	var rows []AgentApprovalOutboxModel
	now := r.now().UnixMilli()
	until := now + 30000
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("published_at_unix_ms = 0 AND dead_lettered_at_unix_ms = 0 AND next_attempt_at_unix_ms <= ? AND (claim_until_unix_ms = 0 OR claim_until_unix_ms < ?)", now, now).Order("visible_at_unix_ms ASC,id ASC").Limit(limit).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		ids := make([]string, len(rows))
		for i := range rows {
			ids[i] = rows[i].ID
		}
		res := tx.Model(&AgentApprovalOutboxModel{}).Where("id IN ? AND published_at_unix_ms = 0", ids).Updates(map[string]any{"claim_token": token, "claim_until_unix_ms": until})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != int64(len(ids)) {
			return errors.New("agent approval outbox claim conflict")
		}
		return nil
	})
	return rows, token, err
}
func (r *AgentApprovalOutboxRepository) retryLater(ctx context.Context, row AgentApprovalOutboxModel, token string) error {
	attempts := row.PublishAttempts + 1
	delay := time.Second * time.Duration(1<<minInt(int(attempts-1), 8))
	now := r.now().UnixMilli()
	return r.db.WithContext(ctx).Model(&AgentApprovalOutboxModel{}).Where("id = ? AND claim_token = ? AND published_at_unix_ms = 0", row.ID, token).Updates(map[string]any{"claim_token": "", "claim_until_unix_ms": 0, "last_publish_error": "PUBLISH_RETRYABLE", "publish_attempts": attempts, "next_attempt_at_unix_ms": now + delay.Milliseconds()}).Error
}
func (r *AgentApprovalOutboxRepository) deadLetter(ctx context.Context, id, token, code string) error {
	now := r.now().UnixMilli()
	return r.db.WithContext(ctx).Model(&AgentApprovalOutboxModel{}).Where("id = ? AND claim_token = ? AND published_at_unix_ms = 0", id, token).Updates(map[string]any{"claim_token": "", "claim_until_unix_ms": 0, "last_publish_error": code, "publish_attempts": gorm.Expr("publish_attempts + 1"), "dead_lettered_at_unix_ms": now}).Error
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func approvalOutboxEvent(row AgentApprovalOutboxModel) (AgentApprovalOutboxEvent, error) {
	var payload struct {
		SessionID      string   `json:"sessionId"`
		ExecutionID    string   `json:"executionId"`
		LotIDs         []string `json:"lotIds"`
		RoomID         string   `json:"roomId"`
		QueuePositions []int32  `json:"queuePositions"`
		TraceID        string   `json:"traceId"`
	}
	d := json.NewDecoder(strings.NewReader(row.PayloadJSON))
	d.DisallowUnknownFields()
	if err := d.Decode(&payload); err != nil {
		return AgentApprovalOutboxEvent{}, fmt.Errorf("invalid approval outbox payload")
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		return AgentApprovalOutboxEvent{}, errors.New("invalid approval outbox trailing data")
	}
	if payload.SessionID != row.SessionID || payload.ExecutionID != row.ExecutionID || payload.RoomID != row.RoomID || payload.TraceID != row.TraceID || len(payload.LotIDs) == 0 || len(payload.LotIDs) != len(payload.QueuePositions) {
		return AgentApprovalOutboxEvent{}, errors.New("invalid approval outbox identity")
	}
	return AgentApprovalOutboxEvent{ID: row.ID, SessionID: row.SessionID, ExecutionID: row.ExecutionID, RoomID: row.RoomID, TraceID: row.TraceID, Payload: row.PayloadJSON, VisibleAtUnixMs: row.VisibleAtUnixMs}, nil
}

func (s *Store) PublishAgentApproval(ctx context.Context, event AgentApprovalOutboxEvent) error {
	if s == nil || s.redis == nil {
		return errors.New("redis is not initialized")
	}
	return s.redis.XAdd(ctx, &redis.XAddArgs{Stream: agentApprovalOutboxStream, ID: "*", Values: map[string]any{"event_id": event.ID, "session_id": event.SessionID, "execution_id": event.ExecutionID, "room_id": event.RoomID, "trace_id": event.TraceID, "payload": event.Payload}}).Err()
}
