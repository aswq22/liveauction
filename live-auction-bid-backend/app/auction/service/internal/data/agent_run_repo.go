package data

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	agentv1 "live-auction-bid/backend/api/agent/v1"
	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

type AgentRunRepo struct {
	db *gorm.DB
}

var _ agentcontrol.RunRepository = (*AgentRunRepo)(nil)
var _ agentcontrol.GatewayRepository = (*AgentRunRepo)(nil)

func NewAgentRunRepository(store *Store) *AgentRunRepo {
	if store == nil {
		return nil
	}
	return &AgentRunRepo{db: store.db}
}

func (r *AgentRunRepo) Create(ctx context.Context, run agentcontrol.Run) error {
	if r == nil || r.db == nil {
		return errors.New("agent run repository is not initialized")
	}
	if run.CreatedAtUnixMs == 0 {
		run.CreatedAtUnixMs = time.Now().UnixMilli()
	}
	if run.UpdatedAtUnixMs == 0 {
		run.UpdatedAtUnixMs = run.CreatedAtUnixMs
	}
	if run.Version == 0 {
		run.Version = 1
	}
	return r.db.WithContext(ctx).Create(agentRunModelFromRun(run)).Error
}

func (r *AgentRunRepo) CreateIdempotent(ctx context.Context, run agentcontrol.Run) (agentcontrol.Run, bool, error) {
	if r == nil || r.db == nil {
		return agentcontrol.Run{}, false, errors.New("agent run repository is not initialized")
	}
	if run.CreatedAtUnixMs == 0 {
		run.CreatedAtUnixMs = time.Now().UnixMilli()
	}
	if run.UpdatedAtUnixMs == 0 {
		run.UpdatedAtUnixMs = run.CreatedAtUnixMs
	}
	if run.Version == 0 {
		run.Version = 1
	}
	createErr := r.db.WithContext(ctx).Create(agentRunModelFromRun(run)).Error
	if createErr == nil {
		return run, true, nil
	}
	var model AgentRunModel
	findErr := r.db.WithContext(ctx).
		Where("tenant_id = ? AND operator_id = ? AND idempotency_key = ?", run.TenantID, run.OperatorID, run.IdempotencyKey).
		First(&model).Error
	if findErr != nil {
		return agentcontrol.Run{}, false, createErr
	}
	existing := agentRunFromModel(model)
	if existing.Goal != run.Goal || existing.RunType != run.RunType ||
		existing.SessionID != run.SessionID || existing.SessionVersion != run.SessionVersion ||
		existing.TurnInputJSON != run.TurnInputJSON {
		return agentcontrol.Run{}, false, apperr.ErrConflict
	}
	return existing, false, nil
}

func (r *AgentRunRepo) Find(ctx context.Context, runID string) (agentcontrol.Run, error) {
	if r == nil || r.db == nil {
		return agentcontrol.Run{}, errors.New("agent run repository is not initialized")
	}
	var model AgentRunModel
	if err := r.db.WithContext(ctx).Where("id = ?", runID).First(&model).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentcontrol.Run{}, apperr.ErrNotFound
		}
		return agentcontrol.Run{}, err
	}
	return agentRunFromModel(model), nil
}

func (r *AgentRunRepo) ListProjectable(ctx context.Context, limit int) ([]agentcontrol.Run, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("agent run repository is not initialized")
	}
	query := r.db.WithContext(ctx).Where("projection_complete = ?", false).Order("created_at_unix_ms ASC")
	if limit > 0 {
		query = query.Limit(limit)
	}
	var models []AgentRunModel
	if err := query.Find(&models).Error; err != nil {
		return nil, err
	}
	runs := make([]agentcontrol.Run, 0, len(models))
	for _, model := range models {
		runs = append(runs, agentRunFromModel(model))
	}
	return runs, nil
}

func (r *AgentRunRepo) ProjectRuntimeSnapshot(ctx context.Context, runID string, runtime agentcontrol.RuntimeRun) (agentcontrol.Run, error) {
	if r == nil || r.db == nil {
		return agentcontrol.Run{}, errors.New("agent run repository is not initialized")
	}
	var projected agentcontrol.Run
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model AgentRunModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", runID).First(&model).Error; err != nil {
			return err
		}
		current := agentRunFromModel(model)
		projected = agentcontrol.MergeRuntimeProjection(current, runtime)
		if projected == current {
			return nil
		}
		projected.Version = current.Version + 1
		result := tx.Model(&AgentRunModel{}).Where("id = ? AND version = ?", runID, current.Version).Updates(map[string]any{
			"status": string(projected.Status), "proposal_json": projected.ProposalJSON,
			"turn_result_json": projected.TurnResultJSON,
			"error_code":       projected.ErrorCode, "error_message": projected.ErrorMessage,
			"version": projected.Version, "updated_at_unix_ms": projected.UpdatedAtUnixMs,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return apperr.ErrConflict
		}
		return nil
	})
	return projected, err
}

func (r *AgentRunRepo) ProjectRuntimeEvent(ctx context.Context, event agentcontrol.RuntimeEvent, terminal *agentcontrol.RuntimeRun) (agentcontrol.Run, bool, error) {
	if r == nil || r.db == nil {
		return agentcontrol.Run{}, false, errors.New("agent run repository is not initialized")
	}
	var projected agentcontrol.Run
	applied := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model AgentRunModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", event.RunID).First(&model).Error; err != nil {
			return err
		}
		current := agentRunFromModel(model)
		if terminal != nil && !agentcontrol.RuntimeLinkageMatches(current, *terminal) {
			return apperr.ErrConflict
		}
		projected = current
		if event.Seq <= current.LastRuntimeSourceSeq {
			return nil
		}
		if event.Seq != current.LastRuntimeSourceSeq+1 {
			return apperr.ErrConflict
		}
		var nextSeq int64
		if err := tx.Model(&AgentRunEventModel{}).Where("run_id = ?", event.RunID).
			Select("COALESCE(MAX(seq), 0) + 1").Scan(&nextSeq).Error; err != nil {
			return err
		}
		persisted := agentcontrol.Event{RunID: event.RunID, Seq: nextSeq, RuntimeSourceSeq: event.Seq,
			EventType: event.Type, Message: event.Message, PayloadJSON: event.PayloadJSON, CreatedAtUnixMs: event.CreatedAtUnixMs}
		if err := tx.Create(agentRunEventModelFromEvent(persisted)).Error; err != nil {
			return err
		}
		projected = agentcontrol.MergeRuntimeProjection(projected, agentcontrol.RuntimeRun{
			RunID: event.RunID, Status: event.Status, UpdatedAtUnixMs: event.CreatedAtUnixMs,
			RunType: projected.RunType, SessionID: projected.SessionID, SessionVersion: projected.SessionVersion,
		})
		projected.LastRuntimeSourceSeq = event.Seq
		projected.Version = current.Version + 1
		if event.CreatedAtUnixMs > projected.UpdatedAtUnixMs {
			projected.UpdatedAtUnixMs = event.CreatedAtUnixMs
		}
		if terminal != nil {
			projected = agentcontrol.MergeRuntimeProjection(projected, *terminal)
			projected.ProjectionComplete = true
		}
		result := tx.Model(&AgentRunModel{}).Where("id = ? AND version = ?", event.RunID, current.Version).Updates(map[string]any{
			"status": string(projected.Status), "proposal_json": projected.ProposalJSON,
			"turn_result_json": projected.TurnResultJSON,
			"error_code":       projected.ErrorCode, "error_message": projected.ErrorMessage,
			"last_runtime_source_seq": projected.LastRuntimeSourceSeq, "projection_complete": projected.ProjectionComplete,
			"version": projected.Version, "updated_at_unix_ms": projected.UpdatedAtUnixMs,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return apperr.ErrConflict
		}
		applied = true
		return nil
	})
	return projected, applied, err
}

func (r *AgentRunRepo) UpdateStatus(ctx context.Context, runID string, expectedVersion int64, status agentcontrol.RunStatus, proposalJSON, errorCode, errorMessage string) error {
	if r == nil || r.db == nil {
		return errors.New("agent run repository is not initialized")
	}
	now := time.Now().UnixMilli()
	result := r.db.WithContext(ctx).Model(&AgentRunModel{}).
		Where("id = ? AND version = ?", runID, expectedVersion).
		Updates(map[string]any{
			"status":             string(status),
			"proposal_json":      proposalJSON,
			"error_code":         errorCode,
			"error_message":      errorMessage,
			"version":            gorm.Expr("version + 1"),
			"updated_at_unix_ms": now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return apperr.ErrConflict
	}
	return nil
}

func (r *AgentRunRepo) FailRuntimeStart(ctx context.Context, runID string, expectedVersion int64, errorCode, errorMessage string, event agentcontrol.Event) error {
	if r == nil || r.db == nil {
		return errors.New("agent run repository is not initialized")
	}
	now := time.Now().UnixMilli()
	if event.CreatedAtUnixMs == 0 {
		event.CreatedAtUnixMs = now
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&AgentRunModel{}).
			Where("id = ? AND version = ?", runID, expectedVersion).
			Updates(map[string]any{
				"status":              string(agentcontrol.StatusFailed),
				"proposal_json":       "",
				"error_code":          errorCode,
				"error_message":       errorMessage,
				"version":             gorm.Expr("version + 1"),
				"projection_complete": true,
				"updated_at_unix_ms":  event.CreatedAtUnixMs,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return apperr.ErrConflict
		}
		if event.Seq <= 0 {
			if err := tx.Model(&AgentRunEventModel{}).Where("run_id = ?", runID).
				Select("COALESCE(MAX(seq), 0) + 1").Scan(&event.Seq).Error; err != nil {
				return err
			}
		}
		return tx.Create(agentRunEventModelFromEvent(event)).Error
	})
}

func (r *AgentRunRepo) CompleteProjection(ctx context.Context, runID string, expectedVersion int64) error {
	if r == nil || r.db == nil {
		return errors.New("agent run repository is not initialized")
	}
	result := r.db.WithContext(ctx).Model(&AgentRunModel{}).
		Where("id = ? AND version = ?", runID, expectedVersion).
		Updates(map[string]any{
			"projection_complete": true,
			"version":             gorm.Expr("version + 1"),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apperr.ErrConflict
	}
	return nil
}

func (r *AgentRunRepo) AppendEvent(ctx context.Context, event agentcontrol.Event) error {
	if r == nil || r.db == nil {
		return errors.New("agent run repository is not initialized")
	}
	if event.CreatedAtUnixMs == 0 {
		event.CreatedAtUnixMs = time.Now().UnixMilli()
	}
	return r.db.WithContext(ctx).Create(agentRunEventModelFromEvent(event)).Error
}

func (r *AgentRunRepo) ListEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]agentcontrol.Event, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("agent run repository is not initialized")
	}
	query := r.db.WithContext(ctx).
		Where("run_id = ? AND seq > ?", runID, afterSeq).
		Order("seq ASC")
	if limit > 0 {
		query = query.Limit(limit)
	}
	var models []AgentRunEventModel
	if err := query.Find(&models).Error; err != nil {
		return nil, err
	}
	events := make([]agentcontrol.Event, 0, len(models))
	for _, model := range models {
		events = append(events, agentRunEventFromModel(model))
	}
	return events, nil
}

func agentRunModelFromRun(run agentcontrol.Run) *AgentRunModel {
	return &AgentRunModel{
		ID:                   run.ID,
		TenantID:             run.TenantID,
		OperatorID:           run.OperatorID,
		IdempotencyKey:       run.IdempotencyKey,
		TraceID:              run.TraceID,
		Goal:                 run.Goal,
		Status:               string(run.Status),
		ProposalJSON:         run.ProposalJSON,
		ErrorCode:            run.ErrorCode,
		ErrorMessage:         run.ErrorMessage,
		Version:              run.Version,
		LastRuntimeSourceSeq: run.LastRuntimeSourceSeq,
		ProjectionComplete:   run.ProjectionComplete,
		RunType:              int32(run.RunType),
		SessionID:            run.SessionID,
		SessionVersion:       run.SessionVersion,
		TurnInputJSON:        run.TurnInputJSON,
		TurnResultJSON:       run.TurnResultJSON,
		CreatedAtUnixMs:      run.CreatedAtUnixMs,
		UpdatedAtUnixMs:      run.UpdatedAtUnixMs,
	}
}

func agentRunFromModel(model AgentRunModel) agentcontrol.Run {
	return agentcontrol.Run{
		ID:                   model.ID,
		TenantID:             model.TenantID,
		OperatorID:           model.OperatorID,
		IdempotencyKey:       model.IdempotencyKey,
		TraceID:              model.TraceID,
		Goal:                 model.Goal,
		Status:               agentcontrol.RunStatus(model.Status),
		ProposalJSON:         model.ProposalJSON,
		ErrorCode:            model.ErrorCode,
		ErrorMessage:         model.ErrorMessage,
		Version:              model.Version,
		LastRuntimeSourceSeq: model.LastRuntimeSourceSeq,
		ProjectionComplete:   model.ProjectionComplete,
		RunType:              agentv1.RunType(model.RunType),
		SessionID:            model.SessionID,
		SessionVersion:       model.SessionVersion,
		TurnInputJSON:        model.TurnInputJSON,
		TurnResultJSON:       model.TurnResultJSON,
		CreatedAtUnixMs:      model.CreatedAtUnixMs,
		UpdatedAtUnixMs:      model.UpdatedAtUnixMs,
	}
}

func agentRunEventModelFromEvent(event agentcontrol.Event) *AgentRunEventModel {
	var runtimeSourceSeq *int64
	if event.RuntimeSourceSeq > 0 {
		value := event.RuntimeSourceSeq
		runtimeSourceSeq = &value
	}
	return &AgentRunEventModel{
		RunID:            event.RunID,
		Seq:              event.Seq,
		RuntimeSourceSeq: runtimeSourceSeq,
		EventType:        event.EventType,
		Message:          event.Message,
		PayloadJSON:      event.PayloadJSON,
		CreatedAtUnixMs:  event.CreatedAtUnixMs,
	}
}

func agentRunEventFromModel(model AgentRunEventModel) agentcontrol.Event {
	var runtimeSourceSeq int64
	if model.RuntimeSourceSeq != nil {
		runtimeSourceSeq = *model.RuntimeSourceSeq
	}
	return agentcontrol.Event{
		RunID:            model.RunID,
		Seq:              model.Seq,
		RuntimeSourceSeq: runtimeSourceSeq,
		EventType:        model.EventType,
		Message:          model.Message,
		PayloadJSON:      model.PayloadJSON,
		CreatedAtUnixMs:  model.CreatedAtUnixMs,
	}
}
