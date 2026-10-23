package data

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"live-auction-bid/backend/app/auction/service/internal/agentapproval"
	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
	"live-auction-bid/backend/app/auction/service/internal/agentconversation"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

type AgentConversationRepo struct {
	db *gorm.DB
}

var _ agentconversation.Repository = (*AgentConversationRepo)(nil)
var _ agentconversation.ReviewSnapshotRepository = (*AgentConversationRepo)(nil)

func NewAgentConversationRepository(store *Store) *AgentConversationRepo {
	if store == nil {
		return nil
	}
	return &AgentConversationRepo{db: store.db}
}

// FindReviewSnapshot is the read-only approval preflight. The transactional
// approval repository repeats the same integrity and authorization checks
// under row locks before any mutation.
func (r *AgentConversationRepo) FindReviewSnapshot(ctx context.Context, tenantID, operatorID, sessionID string, snapshotVersion int64) (agentconversation.ReviewSnapshot, error) {
	if err := r.ready(); err != nil {
		return agentconversation.ReviewSnapshot{}, err
	}
	var session AgentSessionModel
	if err := r.db.WithContext(ctx).Where("id = ? AND tenant_id = ? AND operator_id = ?", sessionID, tenantID, operatorID).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentconversation.ReviewSnapshot{}, apperr.ErrNotFound
		}
		return agentconversation.ReviewSnapshot{}, err
	}
	var model AgentApprovalSnapshotModel
	if err := r.db.WithContext(ctx).Where("session_id = ? AND snapshot_version = ?", sessionID, snapshotVersion).First(&model).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentconversation.ReviewSnapshot{}, apperr.ErrNotFound
		}
		return agentconversation.ReviewSnapshot{}, err
	}
	var snapshot agentconversation.ReviewSnapshot
	if err := json.Unmarshal([]byte(model.SnapshotJSON), &snapshot); err != nil {
		return agentconversation.ReviewSnapshot{}, apperr.ErrAgentReviewSnapshotStale
	}
	snapshot.SnapshotVersion, snapshot.ContentHash, snapshot.SnapshotJSON = model.SnapshotVersion, model.ContentHash, json.RawMessage(model.SnapshotJSON)
	snapshot.CreatedBy, snapshot.CreatedAtUnixMs = model.CreatedBy, model.CreatedAtUnixMs
	if snapshot.TenantID != tenantID || snapshot.SessionID != sessionID || snapshot.RoomID != session.RoomID || agentconversation.ValidateReviewSnapshotIntegrity(snapshot) != nil {
		return agentconversation.ReviewSnapshot{}, apperr.ErrAgentReviewSnapshotStale
	}
	return snapshot, nil
}

func (r *AgentConversationRepo) CreateSession(ctx context.Context, session agentconversation.Session) error {
	if err := r.ready(); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if session.Status == "" {
		session.Status = agentconversation.StatusCollectingInput
	}
	if session.Version == 0 {
		session.Version = 1
	}
	if session.CreatedAtUnixMs == 0 {
		session.CreatedAtUnixMs = now
	}
	if session.UpdatedAtUnixMs == 0 {
		session.UpdatedAtUnixMs = session.CreatedAtUnixMs
	}
	return r.db.WithContext(ctx).Create(agentSessionModelFromSession(session)).Error
}

func (r *AgentConversationRepo) FindSession(ctx context.Context, tenantID, sessionID string) (agentconversation.SessionAggregate, error) {
	if err := r.ready(); err != nil {
		return agentconversation.SessionAggregate{}, err
	}
	return findAgentSessionAggregate(ctx, r.db, tenantID, sessionID)
}

// AuthorizeReviewSnapshot performs the service-layer preflight against current
// asset/reference state. CreateReviewSnapshot repeats it under transaction
// locks so this check is never the authorization boundary by itself.
func (r *AgentConversationRepo) AuthorizeReviewSnapshot(ctx context.Context, command agentconversation.CreateReviewSnapshotCommand) error {
	if err := r.ready(); err != nil {
		return err
	}
	if err := agentconversation.ValidateReviewSnapshotIntegrity(command.Snapshot); err != nil {
		return err
	}
	var session AgentSessionModel
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND operator_id = ? AND id = ?", command.TenantID, command.OperatorID, command.SessionID).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperr.ErrNotFound
		}
		return err
	}
	if session.RoomID != command.Snapshot.RoomID {
		return apperr.ErrAgentReviewSnapshotStale
	}
	return authorizeReviewSnapshotAssets(ctx, r.db, session, command.Snapshot, false)
}

// CreateReviewSnapshot atomically appends a new immutable version and moves
// the session's active pointer. There is intentionally no Save/update/upsert
// counterpart for approval snapshots.
func (r *AgentConversationRepo) CreateReviewSnapshot(ctx context.Context, command agentconversation.CreateReviewSnapshotCommand) (agentconversation.ReviewSnapshot, error) {
	if err := r.ready(); err != nil {
		return agentconversation.ReviewSnapshot{}, err
	}
	if command.SessionID == "" || command.TenantID == "" || command.OperatorID == "" ||
		command.Snapshot.SessionID != command.SessionID || command.Snapshot.TenantID != command.TenantID ||
		command.Snapshot.OperatorID != command.OperatorID {
		return agentconversation.ReviewSnapshot{}, apperr.ErrInvalidArgument
	}
	if err := agentconversation.ValidateReviewSnapshotIntegrity(command.Snapshot); err != nil {
		return agentconversation.ReviewSnapshot{}, err
	}
	resultSnapshot := command.Snapshot
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session AgentSessionModel
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND operator_id = ? AND id = ?", command.TenantID, command.OperatorID, command.SessionID).
			First(&session).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if session.Version != command.ExpectedSessionVersion {
			return apperr.ErrAgentSessionVersionConflict
		}
		if session.Status != string(agentconversation.StatusReadyForReview) {
			return apperr.ErrAgentReviewSnapshotStale
		}
		if session.RoomID != command.Snapshot.RoomID {
			return apperr.ErrAgentReviewSnapshotStale
		}
		if err := validateCurrentReviewSnapshotCandidates(ctx, tx, session, command); err != nil {
			return err
		}
		if err := authorizeReviewSnapshotAssets(ctx, tx, session, command.Snapshot, true); err != nil {
			return err
		}
		var nextVersion int64
		if err := tx.Model(&AgentApprovalSnapshotModel{}).Where("session_id = ?", command.SessionID).
			Select("COALESCE(MAX(snapshot_version), 0) + 1").Scan(&nextVersion).Error; err != nil {
			return err
		}
		resultSnapshot.SnapshotVersion = nextVersion
		model := AgentApprovalSnapshotModel{
			SessionID: command.SessionID, SnapshotVersion: nextVersion,
			SnapshotJSON: string(resultSnapshot.SnapshotJSON), ContentHash: resultSnapshot.ContentHash,
			CreatedBy: resultSnapshot.CreatedBy, CreatedAtUnixMs: resultSnapshot.CreatedAtUnixMs,
		}
		if err := tx.Create(&model).Error; err != nil {
			return err
		}
		updated := tx.Model(&AgentSessionModel{}).
			Where("id = ? AND tenant_id = ? AND operator_id = ? AND version = ?", command.SessionID, command.TenantID, command.OperatorID, command.ExpectedSessionVersion).
			Updates(map[string]any{"active_snapshot_version": nextVersion, "version": command.ExpectedSessionVersion + 1, "updated_at_unix_ms": resultSnapshot.CreatedAtUnixMs})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return apperr.ErrAgentSessionVersionConflict
		}
		return nil
	})
	if err != nil {
		return agentconversation.ReviewSnapshot{}, err
	}
	return resultSnapshot, nil
}

func validateCurrentReviewSnapshotCandidates(ctx context.Context, tx *gorm.DB, session AgentSessionModel, command agentconversation.CreateReviewSnapshotCommand) error {
	ids := command.Snapshot.SelectedCandidateIDs
	if len(ids) == 0 {
		return apperr.ErrInvalidArgument
	}
	var models []AgentDraftCandidateModel
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("session_id = ? AND id IN ?", session.ID, ids).Find(&models).Error; err != nil {
		return err
	}
	if len(models) != len(ids) {
		return apperr.ErrAgentCandidateChanged
	}
	candidates := make([]agentconversation.DraftCandidate, 0, len(models))
	for _, model := range models {
		candidate, err := agentDraftCandidateFromModel(model)
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate)
	}
	rebuilt, err := agentconversation.BuildReviewSnapshot(agentconversation.SessionAggregate{Session: agentSessionFromModel(session), Candidates: candidates}, command.OperatorID, command.Snapshot.SelectedCandidateIDs, command.Snapshot.OrderedCandidateIDs)
	if err != nil {
		return err
	}
	if rebuilt.ContentHash != command.Snapshot.ContentHash || !bytes.Equal(rebuilt.SnapshotJSON, command.Snapshot.SnapshotJSON) {
		return apperr.ErrAgentCandidateChanged
	}
	return nil
}

func authorizeReviewSnapshotAssets(ctx context.Context, db *gorm.DB, session AgentSessionModel, snapshot agentconversation.ReviewSnapshot, lock bool) error {
	now := time.Now().UnixMilli()
	for _, candidate := range snapshot.Candidates {
		if candidate.SourceType == agentconversation.SourceExisting {
			if candidate.Draft.MainImageAssetID != "" || len(candidate.AssetIDs) != 0 {
				return apperr.ErrAgentImageAssetInvalid
			}
			continue
		}
		assetID := candidate.Draft.MainImageAssetID
		if candidate.SourceType != agentconversation.SourceNew || assetID == "" || len(candidate.AssetIDs) != 1 || candidate.AssetIDs[0] != assetID {
			return apperr.ErrAgentImageAssetInvalid
		}
		var reference AgentCandidateAssetModel
		referenceQuery := db.WithContext(ctx)
		if lock {
			referenceQuery = referenceQuery.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := referenceQuery.Where("candidate_id = ? AND session_id = ? AND asset_id = ? AND tenant_id = ? AND operator_id = ?", candidate.CandidateID, session.ID, assetID, session.TenantID, session.OperatorID).First(&reference).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperr.ErrAgentImageAssetInvalid
			}
			return err
		}
		var asset AssetFileModel
		assetQuery := db.WithContext(ctx)
		if lock {
			assetQuery = assetQuery.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := assetQuery.Where("id = ?", assetID).First(&asset).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperr.ErrAgentImageAssetInvalid
			}
			return err
		}
		if asset.MainAccountID != session.TenantID || asset.OwnerUserID != session.OperatorID || asset.BizType != "agent_lot_image" || asset.Status != AssetStatusTemporary || asset.DeletedAtUnixMs != 0 || asset.AttachedLotID != "" || asset.ExpiresAtUnixMs <= now || asset.RoomID != session.RoomID {
			return apperr.ErrAgentImageAssetInvalid
		}
	}
	return nil
}

func (r *AgentConversationRepo) AppendUserMessageAndStartRun(ctx context.Context, command agentconversation.StartTurnCommand) (agentconversation.SessionAggregate, error) {
	if err := r.ready(); err != nil {
		return agentconversation.SessionAggregate{}, err
	}
	now := time.Now().UnixMilli()
	if command.Message.CreatedAtUnixMs == 0 {
		command.Message.CreatedAtUnixMs = now
	}
	if command.Run.CreatedAtUnixMs == 0 {
		command.Run.CreatedAtUnixMs = command.Message.CreatedAtUnixMs
	}
	if command.Run.UpdatedAtUnixMs == 0 {
		command.Run.UpdatedAtUnixMs = command.Run.CreatedAtUnixMs
	}
	if command.Run.Version == 0 {
		command.Run.Version = 1
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The idempotency key belongs to the durable turn, not to a best-effort
		// runtime call.  Check it before the session CAS so retries continue to
		// return the original turn after its version has advanced.
		var existingRun AgentRunModel
		findRunErr := tx.Where("tenant_id = ? AND operator_id = ? AND idempotency_key = ?", command.TenantID, command.OperatorID, command.Run.IdempotencyKey).First(&existingRun).Error
		if findRunErr == nil {
			existing := agentRunFromModel(existingRun)
			if existing.Goal != command.Run.Goal || existing.RunType != command.Run.RunType || existing.SessionID != command.SessionID || existing.TurnInputJSON != command.Run.TurnInputJSON {
				return apperr.ErrConflict
			}
			return nil
		}
		if !errors.Is(findRunErr, gorm.ErrRecordNotFound) {
			return findRunErr
		}
		session, err := findScopedAgentSession(ctx, tx, command.TenantID, command.OperatorID, command.SessionID)
		if err != nil {
			return err
		}
		if session.Version != command.ExpectedVersion {
			return apperr.ErrAgentSessionVersionConflict
		}
		nextVersion := session.Version + 1
		updatedAt := command.Message.CreatedAtUnixMs
		nextStatus := session.Status
		if session.Status == agentconversation.StatusCollectingInput || session.Status == agentconversation.StatusReadyForReview || session.Status == agentconversation.StatusNeedsRevision {
			nextStatus = agentconversation.StatusCollectingInput
		}
		result := tx.Model(&AgentSessionModel{}).
			Where("id = ? AND tenant_id = ? AND operator_id = ? AND version = ?", command.SessionID, command.TenantID, command.OperatorID, command.ExpectedVersion).
			Updates(map[string]any{"status": nextStatus, "version": nextVersion, "active_snapshot_version": 0, "updated_at_unix_ms": updatedAt})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return apperr.ErrAgentSessionVersionConflict
		}
		seq, err := nextAgentSessionMessageSeq(tx, command.SessionID)
		if err != nil {
			return err
		}
		command.Message.SessionID = command.SessionID
		command.Message.Seq = seq
		command.Message.Role = agentconversation.MessageRoleUser
		if err := tx.Create(agentSessionMessageModelFromMessage(command.Message)).Error; err != nil {
			return err
		}
		command.Run.TenantID = session.TenantID
		command.Run.OperatorID = session.OperatorID
		command.Run.SessionID = session.ID
		command.Run.SessionVersion = nextVersion
		return tx.Create(agentRunModelFromRun(command.Run)).Error
	})
	if err != nil {
		return agentconversation.SessionAggregate{}, err
	}
	return r.FindSession(ctx, command.TenantID, command.SessionID)
}

func (r *AgentConversationRepo) MergeTurnResult(ctx context.Context, command agentconversation.MergeTurnCommand) (agentconversation.SessionAggregate, error) {
	if err := r.ready(); err != nil {
		return agentconversation.SessionAggregate{}, err
	}
	if command.AssistantMessage.RunID != "" && command.AssistantMessage.RunID != command.RunID {
		return agentconversation.SessionAggregate{}, apperr.ErrInvalidArgument
	}
	updatedAt := command.UpdatedAtUnixMs
	if updatedAt == 0 {
		updatedAt = time.Now().UnixMilli()
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		session, err := findScopedAgentSession(ctx, tx, command.TenantID, command.OperatorID, command.SessionID)
		if err != nil {
			return err
		}
		if session.Version != command.ExpectedVersion {
			return apperr.ErrAgentSessionVersionConflict
		}
		if session.Status != agentconversation.StatusCollectingInput && session.Status != agentconversation.StatusReadyForReview && session.Status != agentconversation.StatusNeedsRevision {
			return apperr.ErrConflict
		}
		if command.RunID != "" {
			var count int64
			if err := tx.Model(&AgentRunModel{}).
				Where(
					"id = ? AND tenant_id = ? AND operator_id = ? AND session_id = ? AND session_version <= ?",
					command.RunID, session.TenantID, session.OperatorID, session.ID, session.Version,
				).
				Count(&count).Error; err != nil {
				return err
			}
			if count != 1 {
				return apperr.ErrNotFound
			}
			// The assistant message is the durable materialization marker for a
			// completed run. A recovery replay must not advance the session or
			// apply its candidate patches a second time.
			if err := tx.Model(&AgentSessionMessageModel{}).
				Where("session_id = ? AND run_id = ? AND role = ?", session.ID, command.RunID, string(agentconversation.MessageRoleAssistant)).
				Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return nil
			}
		}
		status := command.Status
		if status == "" {
			status = session.Status
		}
		result := tx.Model(&AgentSessionModel{}).
			Where("id = ? AND tenant_id = ? AND operator_id = ? AND version = ?", command.SessionID, command.TenantID, command.OperatorID, command.ExpectedVersion).
			Updates(map[string]any{
				"status": status, "conversation_summary": command.ConversationSummary,
				"active_snapshot_version": command.ActiveSnapshotVersion,
				"version":                 session.Version + 1, "updated_at_unix_ms": updatedAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return apperr.ErrAgentSessionVersionConflict
		}
		if command.AssistantMessage.Content != "" {
			seq, err := nextAgentSessionMessageSeq(tx, command.SessionID)
			if err != nil {
				return err
			}
			message := command.AssistantMessage
			message.SessionID = command.SessionID
			message.Seq = seq
			message.Role = agentconversation.MessageRoleAssistant
			if message.RunID == "" {
				message.RunID = command.RunID
			}
			if message.CreatedAtUnixMs == 0 {
				message.CreatedAtUnixMs = updatedAt
			}
			if err := tx.Create(agentSessionMessageModelFromMessage(message)).Error; err != nil {
				return err
			}
		}
		for _, candidate := range command.Candidates {
			if err := persistMergedAgentCandidate(tx, command.SessionID, candidate, updatedAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return agentconversation.SessionAggregate{}, err
	}
	return r.FindSession(ctx, command.TenantID, command.SessionID)
}

func (r *AgentConversationRepo) PatchCandidate(ctx context.Context, command agentconversation.PatchCandidateCommand) (agentconversation.SessionAggregate, error) {
	if err := r.ready(); err != nil {
		return agentconversation.SessionAggregate{}, err
	}
	updatedAt := command.UpdatedAtUnixMs
	if updatedAt == 0 {
		updatedAt = time.Now().UnixMilli()
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		session, err := findScopedAgentSession(ctx, tx, command.TenantID, command.OperatorID, command.SessionID)
		if err != nil {
			return err
		}
		if session.Version != command.ExpectedSessionVersion {
			return apperr.ErrAgentSessionVersionConflict
		}
		var current AgentDraftCandidateModel
		if err := tx.Where("id = ? AND session_id = ?", command.CandidateID, command.SessionID).First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperr.ErrNotFound
			}
			return err
		}
		if current.Version != command.ExpectedCandidateVersion {
			return apperr.ErrAgentSessionVersionConflict
		}
		candidate := command.Candidate
		candidate.Candidate.ID = command.CandidateID
		if candidate.Position == 0 {
			candidate.Position = current.Position
		}
		candidate.Version = current.Version + 1
		candidate.CreatedAtUnixMs = current.CreatedAtUnixMs
		candidate.UpdatedAtUnixMs = updatedAt
		model, err := agentDraftCandidateModelFromCandidate(command.SessionID, candidate, updatedAt)
		if err != nil {
			return err
		}
		candidateResult := tx.Model(&AgentDraftCandidateModel{}).
			Where("id = ? AND session_id = ? AND version = ?", command.CandidateID, command.SessionID, command.ExpectedCandidateVersion).
			Updates(map[string]any{
				"source_type": model.SourceType, "existing_lot_id": model.ExistingLotID,
				"position": model.Position, "selected_by_default": model.SelectedByDefault,
				"draft_json": model.DraftJSON, "field_sources_json": model.FieldSourcesJSON, "provenance_version": model.ProvenanceVersion,
				"validation_json": model.ValidationJSON, "version": model.Version,
				"updated_at_unix_ms": updatedAt,
			})
		if candidateResult.Error != nil {
			return candidateResult.Error
		}
		if candidateResult.RowsAffected != 1 {
			return apperr.ErrAgentSessionVersionConflict
		}
		status := command.Status
		if status == "" {
			status = session.Status
		}
		sessionResult := tx.Model(&AgentSessionModel{}).
			Where("id = ? AND tenant_id = ? AND operator_id = ? AND version = ?", command.SessionID, command.TenantID, command.OperatorID, command.ExpectedSessionVersion).
			Updates(map[string]any{
				"status": status, "active_snapshot_version": 0,
				"version": session.Version + 1, "updated_at_unix_ms": updatedAt,
			})
		if sessionResult.Error != nil {
			return sessionResult.Error
		}
		if sessionResult.RowsAffected != 1 {
			return apperr.ErrAgentSessionVersionConflict
		}
		return nil
	})
	if err != nil {
		return agentconversation.SessionAggregate{}, err
	}
	return r.FindSession(ctx, command.TenantID, command.SessionID)
}

func (r *AgentConversationRepo) CancelSession(ctx context.Context, tenantID, operatorID, sessionID string, expectedVersion int64) error {
	if err := r.ready(); err != nil {
		return err
	}
	session, err := findScopedAgentSession(ctx, r.db, tenantID, operatorID, sessionID)
	if err != nil {
		return err
	}
	if session.Version != expectedVersion {
		return apperr.ErrAgentSessionVersionConflict
	}
	result := r.db.WithContext(ctx).Model(&AgentSessionModel{}).
		Where("id = ? AND tenant_id = ? AND operator_id = ? AND version = ?", sessionID, tenantID, operatorID, expectedVersion).
		Updates(map[string]any{
			"status": agentconversation.StatusCancelled, "active_snapshot_version": 0,
			"version": expectedVersion + 1, "updated_at_unix_ms": time.Now().UnixMilli(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apperr.ErrAgentSessionVersionConflict
	}
	return nil
}

func (r *AgentConversationRepo) ready() error {
	if r == nil || r.db == nil {
		return errors.New("agent conversation repository is not initialized")
	}
	return nil
}

func findAgentSessionAggregate(ctx context.Context, db *gorm.DB, tenantID, sessionID string) (agentconversation.SessionAggregate, error) {
	var sessionModel AgentSessionModel
	if err := db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, sessionID).First(&sessionModel).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentconversation.SessionAggregate{}, apperr.ErrNotFound
		}
		return agentconversation.SessionAggregate{}, err
	}
	aggregate := agentconversation.SessionAggregate{Session: agentSessionFromModel(sessionModel)}
	var messageModels []AgentSessionMessageModel
	if err := db.WithContext(ctx).Where("session_id = ?", sessionID).Order("seq ASC").Find(&messageModels).Error; err != nil {
		return agentconversation.SessionAggregate{}, err
	}
	aggregate.Messages = make([]agentconversation.Message, 0, len(messageModels))
	for _, model := range messageModels {
		aggregate.Messages = append(aggregate.Messages, agentSessionMessageFromModel(model))
	}
	var candidateModels []AgentDraftCandidateModel
	if err := db.WithContext(ctx).Where("session_id = ?", sessionID).Order("position ASC").Find(&candidateModels).Error; err != nil {
		return agentconversation.SessionAggregate{}, err
	}
	aggregate.Candidates = make([]agentconversation.DraftCandidate, 0, len(candidateModels))
	for _, model := range candidateModels {
		candidate, err := agentDraftCandidateFromModel(model)
		if err != nil {
			return agentconversation.SessionAggregate{}, err
		}
		aggregate.Candidates = append(aggregate.Candidates, candidate)
	}
	var runModels []AgentRunModel
	if err := db.WithContext(ctx).Where("tenant_id = ? AND session_id = ?", tenantID, sessionID).Order("created_at_unix_ms ASC").Find(&runModels).Error; err != nil {
		return agentconversation.SessionAggregate{}, err
	}
	aggregate.Runs = make([]agentcontrol.Run, 0, len(runModels))
	for _, model := range runModels {
		aggregate.Runs = append(aggregate.Runs, agentRunFromModel(model))
	}
	if aggregate.Session.Status == agentconversation.StatusApplied {
		recovered, err := findRecoveredAgentExecution(ctx, db, sessionModel)
		if err != nil {
			return agentconversation.SessionAggregate{}, err
		}
		aggregate.AppliedExecution = &recovered
	}
	return aggregate, nil
}

func findRecoveredAgentExecution(ctx context.Context, db *gorm.DB, session AgentSessionModel) (agentconversation.RecoveredExecution, error) {
	var execution AgentProposalExecutionModel
	if err := db.WithContext(ctx).Where("tenant_id = ? AND operator_id = ? AND session_id = ? AND status = ?", session.TenantID, session.OperatorID, session.ID, "APPLIED").Order("completed_at_unix_ms DESC, id DESC").First(&execution).Error; err != nil {
		return agentconversation.RecoveredExecution{}, err
	}
	var snapshotModel AgentApprovalSnapshotModel
	if err := db.WithContext(ctx).Where("session_id = ? AND snapshot_version = ?", session.ID, execution.SnapshotVersion).First(&snapshotModel).Error; err != nil {
		return agentconversation.RecoveredExecution{}, err
	}
	var snapshot agentconversation.ReviewSnapshot
	if err := json.Unmarshal([]byte(snapshotModel.SnapshotJSON), &snapshot); err != nil {
		return agentconversation.RecoveredExecution{}, apperr.ErrAgentBatchApplyFailed
	}
	snapshot.SnapshotVersion, snapshot.ContentHash, snapshot.SnapshotJSON = snapshotModel.SnapshotVersion, snapshotModel.ContentHash, json.RawMessage(snapshotModel.SnapshotJSON)
	snapshot.CreatedBy, snapshot.CreatedAtUnixMs = snapshotModel.CreatedBy, snapshotModel.CreatedAtUnixMs
	if snapshot.SessionID != session.ID || snapshot.TenantID != session.TenantID || snapshot.OperatorID != session.OperatorID || snapshot.RoomID != session.RoomID || agentconversation.ValidateReviewSnapshotIntegrity(snapshot) != nil {
		return agentconversation.RecoveredExecution{}, apperr.ErrAgentBatchApplyFailed
	}
	var selected, ordered, created []string
	var positions []int32
	if json.Unmarshal([]byte(execution.SelectedCandidateIDsJSON), &selected) != nil || json.Unmarshal([]byte(execution.OrderedCandidateIDsJSON), &ordered) != nil || json.Unmarshal([]byte(execution.CreatedLotIDsJSON), &created) != nil || json.Unmarshal([]byte(execution.QueuePositionsJSON), &positions) != nil || !reflect.DeepEqual(selected, snapshot.SelectedCandidateIDs) || !reflect.DeepEqual(ordered, snapshot.OrderedCandidateIDs) || strings.TrimSpace(execution.ID) == "" || execution.CompletedAtUnixMs <= 0 {
		return agentconversation.RecoveredExecution{}, apperr.ErrAgentBatchApplyFailed
	}
	inventoryOnly := len(positions) == 0
	if inventoryOnly {
		if len(created) != len(snapshot.Candidates) {
			return agentconversation.RecoveredExecution{}, apperr.ErrAgentBatchApplyFailed
		}
		for _, candidate := range snapshot.Candidates {
			if candidate.SourceType != agentconversation.SourceNew {
				return agentconversation.RecoveredExecution{}, apperr.ErrAgentBatchApplyFailed
			}
		}
		return agentconversation.RecoveredExecution{ExecutionID: execution.ID, SnapshotVersion: execution.SnapshotVersion, CreatedLotIDs: append([]string(nil), created...), QueuedLots: []agentconversation.RecoveredQueuedLot{}, Target: string(agentapproval.ApplyTargetInventoryOnly)}, nil
	}
	if len(positions) != len(snapshot.Candidates) {
		return agentconversation.RecoveredExecution{}, apperr.ErrAgentBatchApplyFailed
	}
	result := agentconversation.RecoveredExecution{ExecutionID: execution.ID, SnapshotVersion: execution.SnapshotVersion, CreatedLotIDs: append([]string(nil), created...), QueuedLots: make([]agentconversation.RecoveredQueuedLot, 0, len(snapshot.Candidates)), Target: string(agentapproval.ApplyTargetQueue)}
	createdIndex := 0
	seenLots := make(map[string]struct{}, len(snapshot.Candidates))
	for i, candidate := range snapshot.Candidates {
		lotID := candidate.ExistingLotID
		if candidate.SourceType == agentconversation.SourceNew {
			if createdIndex >= len(created) {
				return agentconversation.RecoveredExecution{}, apperr.ErrAgentBatchApplyFailed
			}
			lotID = created[createdIndex]
			createdIndex++
		}
		if strings.TrimSpace(lotID) == "" || positions[i] <= 0 || i > 0 && positions[i] != positions[i-1]+1 {
			return agentconversation.RecoveredExecution{}, apperr.ErrAgentBatchApplyFailed
		}
		if _, duplicate := seenLots[lotID]; duplicate {
			return agentconversation.RecoveredExecution{}, apperr.ErrAgentBatchApplyFailed
		}
		seenLots[lotID] = struct{}{}
		result.QueuedLots = append(result.QueuedLots, agentconversation.RecoveredQueuedLot{CandidateID: candidate.CandidateID, SourceType: candidate.SourceType, LotID: lotID, QueuePosition: positions[i]})
	}
	if createdIndex != len(created) {
		return agentconversation.RecoveredExecution{}, apperr.ErrAgentBatchApplyFailed
	}
	return result, nil
}

func findScopedAgentSession(ctx context.Context, db *gorm.DB, tenantID, operatorID, sessionID string) (agentconversation.Session, error) {
	var model AgentSessionModel
	err := db.WithContext(ctx).Where("tenant_id = ? AND operator_id = ? AND id = ?", tenantID, operatorID, sessionID).First(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return agentconversation.Session{}, apperr.ErrNotFound
	}
	if err != nil {
		return agentconversation.Session{}, err
	}
	return agentSessionFromModel(model), nil
}

func nextAgentSessionMessageSeq(db *gorm.DB, sessionID string) (int64, error) {
	var next int64
	err := db.Model(&AgentSessionMessageModel{}).Where("session_id = ?", sessionID).
		Select("COALESCE(MAX(seq), 0) + 1").Scan(&next).Error
	return next, err
}

func persistMergedAgentCandidate(tx *gorm.DB, sessionID string, candidate agentconversation.DraftCandidate, updatedAt int64) error {
	var current AgentDraftCandidateModel
	findErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", candidate.Candidate.ID).Take(&current).Error
	if findErr == nil {
		if current.SessionID != sessionID {
			return apperr.ErrNotFound
		}
		if candidate.Version != current.Version {
			return apperr.ErrAgentSessionVersionConflict
		}
		candidate.Version = current.Version + 1
		candidate.CreatedAtUnixMs = current.CreatedAtUnixMs
		candidate.UpdatedAtUnixMs = updatedAt
		model, err := agentDraftCandidateModelFromCandidate(sessionID, candidate, updatedAt)
		if err != nil {
			return err
		}
		result := tx.Model(&AgentDraftCandidateModel{}).
			Where("id = ? AND session_id = ? AND version = ?", current.ID, sessionID, current.Version).
			Updates(map[string]any{
				"source_type": model.SourceType, "existing_lot_id": model.ExistingLotID,
				"position": model.Position, "selected_by_default": model.SelectedByDefault,
				"draft_json": model.DraftJSON, "field_sources_json": model.FieldSourcesJSON, "provenance_version": model.ProvenanceVersion,
				"validation_json": model.ValidationJSON, "version": model.Version,
				"updated_at_unix_ms": model.UpdatedAtUnixMs,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return apperr.ErrAgentSessionVersionConflict
		}
		return nil
	}
	if !errors.Is(findErr, gorm.ErrRecordNotFound) {
		return findErr
	}
	model, err := agentDraftCandidateModelFromCandidate(sessionID, candidate, updatedAt)
	if err != nil {
		return err
	}
	return tx.Create(model).Error
}

func agentSessionModelFromSession(session agentconversation.Session) *AgentSessionModel {
	return &AgentSessionModel{
		ID: session.ID, TenantID: session.TenantID, OperatorID: session.OperatorID, RoomID: session.RoomID,
		Status: string(session.Status), Version: session.Version, ConversationSummary: session.ConversationSummary,
		ActiveSnapshotVersion: session.ActiveSnapshotVersion,
		CreatedAtUnixMs:       session.CreatedAtUnixMs, UpdatedAtUnixMs: session.UpdatedAtUnixMs,
	}
}

func agentSessionFromModel(model AgentSessionModel) agentconversation.Session {
	return agentconversation.Session{
		ID: model.ID, TenantID: model.TenantID, OperatorID: model.OperatorID, RoomID: model.RoomID,
		Status: agentconversation.SessionStatus(model.Status), Version: model.Version,
		ConversationSummary: model.ConversationSummary, ActiveSnapshotVersion: model.ActiveSnapshotVersion,
		CreatedAtUnixMs: model.CreatedAtUnixMs, UpdatedAtUnixMs: model.UpdatedAtUnixMs,
	}
}

func agentSessionMessageModelFromMessage(message agentconversation.Message) *AgentSessionMessageModel {
	return &AgentSessionMessageModel{
		SessionID: message.SessionID, Seq: message.Seq, Role: string(message.Role), Content: message.Content,
		RunID: message.RunID, CreatedAtUnixMs: message.CreatedAtUnixMs,
	}
}

func agentSessionMessageFromModel(model AgentSessionMessageModel) agentconversation.Message {
	return agentconversation.Message{
		SessionID: model.SessionID, Seq: model.Seq, Role: agentconversation.MessageRole(model.Role),
		Content: model.Content, RunID: model.RunID, CreatedAtUnixMs: model.CreatedAtUnixMs,
	}
}

func agentDraftCandidateModelFromCandidate(sessionID string, candidate agentconversation.DraftCandidate, now int64) (*AgentDraftCandidateModel, error) {
	draftJSON, err := json.Marshal(candidate.Candidate)
	if err != nil {
		return nil, err
	}
	if candidate.Version == 0 {
		candidate.Version = 1
	}
	if candidate.CreatedAtUnixMs == 0 {
		candidate.CreatedAtUnixMs = now
	}
	if candidate.UpdatedAtUnixMs == 0 {
		candidate.UpdatedAtUnixMs = now
	}
	return &AgentDraftCandidateModel{
		ID: candidate.Candidate.ID, SessionID: sessionID, SourceType: string(candidate.Candidate.SourceType),
		ExistingLotID: candidate.Candidate.ExistingLotID, Position: candidate.Position,
		SelectedByDefault: boolPointer(candidate.SelectedByDefault), DraftJSON: string(draftJSON),
		FieldSourcesJSON: candidate.FieldSourcesJSON, ValidationJSON: candidate.ValidationJSON,
		ProvenanceVersion: candidate.ProvenanceVersion,
		Version:           candidate.Version, CreatedAtUnixMs: candidate.CreatedAtUnixMs, UpdatedAtUnixMs: candidate.UpdatedAtUnixMs,
	}, nil
}

func agentDraftCandidateFromModel(model AgentDraftCandidateModel) (agentconversation.DraftCandidate, error) {
	var candidate agentconversation.Candidate
	if err := json.Unmarshal([]byte(model.DraftJSON), &candidate); err != nil {
		return agentconversation.DraftCandidate{}, err
	}
	selectedByDefault := true
	if model.SelectedByDefault != nil {
		selectedByDefault = *model.SelectedByDefault
	}
	return agentconversation.DraftCandidate{
		Candidate: candidate, Position: model.Position, SelectedByDefault: selectedByDefault,
		FieldSourcesJSON: model.FieldSourcesJSON, ValidationJSON: model.ValidationJSON,
		ProvenanceVersion: model.ProvenanceVersion,
		Version:           model.Version, CreatedAtUnixMs: model.CreatedAtUnixMs, UpdatedAtUnixMs: model.UpdatedAtUnixMs,
	}, nil
}

func boolPointer(value bool) *bool { return &value }
