package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/agentapproval"
	"live-auction-bid/backend/app/auction/service/internal/agentconversation"
	"live-auction-bid/backend/app/auction/service/internal/biz/auction"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
	"live-auction-bid/backend/app/auction/service/internal/pkg/idgen"
)

type AgentApprovalRepository struct {
	db                  *gorm.DB
	beforeRoomStateLock func(*gorm.DB)
}

var _ agentapproval.TransactionalRepository = (*AgentApprovalRepository)(nil)
var _ agentapproval.TransactionalRepository = (*Store)(nil)

func NewAgentApprovalRepository(db *gorm.DB) *AgentApprovalRepository {
	return &AgentApprovalRepository{db: db}
}

func (s *Store) approvalRepository() *AgentApprovalRepository {
	if s == nil {
		return &AgentApprovalRepository{}
	}
	return NewAgentApprovalRepository(s.db)
}

func (s *Store) Apply(ctx context.Context, command agentapproval.ApplyCommand) (agentapproval.Execution, error) {
	return s.approvalRepository().Apply(ctx, command)
}

func (s *Store) FindExecutionByKey(ctx context.Context, sessionID string, snapshotVersion int64, key string) (agentapproval.Execution, error) {
	return s.approvalRepository().FindExecutionByKey(ctx, sessionID, snapshotVersion, key)
}
func (s *Store) InvalidateReviewSnapshot(ctx context.Context, command agentapproval.ApplyCommand, cause error) error {
	return s.approvalRepository().InvalidateReviewSnapshot(ctx, command, cause)
}

func (r *AgentApprovalRepository) FindExecutionByKey(ctx context.Context, sessionID string, snapshotVersion int64, key string) (agentapproval.Execution, error) {
	if r == nil || r.db == nil {
		return agentapproval.Execution{}, apperr.ErrAgentBatchApplyFailed
	}
	var model AgentProposalExecutionModel
	err := r.db.WithContext(ctx).Where("session_id = ? AND snapshot_version = ? AND idempotency_key = ?", sessionID, snapshotVersion, key).First(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return agentapproval.Execution{}, apperr.ErrNotFound
	}
	if err != nil {
		return agentapproval.Execution{}, err
	}
	return executionFromModel(model)
}

func (r *AgentApprovalRepository) Apply(ctx context.Context, command agentapproval.ApplyCommand) (agentapproval.Execution, error) {
	if r == nil || r.db == nil {
		return agentapproval.Execution{}, apperr.ErrAgentBatchApplyFailed
	}
	if command.Target == "" {
		command.Target = agentapproval.ApplyTargetQueue
	}
	if command.Target != agentapproval.ApplyTargetQueue && command.Target != agentapproval.ApplyTargetInventoryOnly {
		return agentapproval.Execution{}, apperr.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return agentapproval.Execution{}, err
	}
	fingerprint := agentapproval.RequestFingerprint(command)
	if fingerprint == "" {
		return agentapproval.Execution{}, apperr.ErrInvalidArgument
	}
	now := time.Now().UnixMilli()
	model, err := newApplyingExecutionModel(command, fingerprint, now)
	if err != nil {
		return agentapproval.Execution{}, apperr.ErrInvalidArgument
	}
	var applied agentapproval.Execution
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The insert is intentionally first. Its unique index is the lock for an
		// absent execution key; concurrent identical requests wait here before
		// either observing the committed result or becoming the sole writer.
		if err := tx.Create(&model).Error; err != nil {
			return err
		}

		var session AgentSessionModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", command.SessionID).First(&session).Error; err != nil {
			return approvalNotFound(err)
		}
		if session.TenantID != command.TenantID || session.OperatorID != command.OperatorID {
			return apperr.ErrNotFound
		}
		if session.Status != string(agentconversation.StatusReadyForReview) || session.ActiveSnapshotVersion != command.SnapshotVersion {
			return apperr.ErrAgentReviewSnapshotStale
		}

		snapshot, err := lockApprovalSnapshot(ctx, tx, session, command)
		if err != nil {
			return err
		}

		// Candidate rows are part of the immutable snapshot authorization. Lock
		// them in lexical order before the room/lot/asset mutation locks.
		candidates, err := lockAndValidateSnapshotCandidates(ctx, tx, session, snapshot)
		if err != nil {
			return err
		}

		if r.beforeRoomStateLock != nil {
			r.beforeRoomStateLock(tx)
		}
		var room AuctionRoomModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", session.RoomID).First(&room).Error; err != nil {
			return approvalNotFound(err)
		}
		if room.MainAccountID != session.TenantID || room.Status != "ACTIVE" {
			return apperr.ErrPermissionDenied
		}
		positions := []int32{}
		var roomState AuctionRoomStateModel
		var nextQueuePosition int32
		existingLots := map[string]*v1.Lot{}
		if command.Target == agentapproval.ApplyTargetInventoryOnly {
			for _, candidate := range snapshot.Candidates {
				if candidate.SourceType != agentconversation.SourceNew {
					return apperr.ErrInvalidArgument
				}
			}
		} else {
			roomState, err = lockAuctionRoomState(ctx, tx, session.RoomID)
			if err != nil {
				return approvalNotFound(err)
			}
			if roomState.MainAccountID != session.TenantID || roomState.NextQueuePosition <= 0 {
				return apperr.ErrAgentBatchApplyFailed
			}
			positions, nextQueuePosition, err = allocateQueuePositions(roomState.NextQueuePosition, len(snapshot.OrderedCandidateIDs))
			if err != nil {
				return err
			}
			existingLots, err = lockAndValidateExistingLots(ctx, tx, session, candidates)
			if err != nil {
				return err
			}
		}
		assets, err := lockAndValidateNewAssets(ctx, tx, session, candidates)
		if err != nil {
			return err
		}

		lots := make(map[string]*v1.Lot, len(candidates))
		createdIDs := make([]string, 0)
		queuedLotIDs := make([]string, 0, len(snapshot.OrderedCandidateIDs))
		for _, candidate := range snapshot.Candidates {
			if candidate.SourceType == agentconversation.SourceExisting {
				lots[candidate.CandidateID] = existingLots[candidate.ExistingLotID]
				continue
			}
			asset := assets[candidate.Draft.MainImageAssetID]
			lot, err := newLotFromSnapshot(session, candidate, asset.PublicURL)
			if err != nil {
				return apperr.ErrAgentCandidateChanged
			}
			lots[candidate.CandidateID] = lot
			createdIDs = append(createdIDs, lot.Id)
		}
		persistNewLot := func(candidateID string, lot *v1.Lot) error {
			lotModel, err := lotToModel(lot)
			if err != nil {
				return err
			}
			if err := tx.Create(lotModel).Error; err != nil {
				return mapQueuePositionConflict(err)
			}
			if err := tx.Create(&AuctionLotStatsModel{LotID: lot.Id, MainAccountID: lot.GetMainAccountId(), RoomID: lot.GetRoomId(), UpdatedAtUnixMs: 0}).Error; err != nil {
				return err
			}
			assetID := candidates[candidateID].Draft.MainImageAssetID
			result := tx.Model(&AssetFileModel{}).Where("id = ? AND status = ? AND attached_lot_id = ''", assetID, AssetStatusTemporary).Updates(map[string]any{"status": AssetStatusAttached, "attached_lot_id": lot.Id, "attached_at_unix_ms": now, "expires_at_unix_ms": 0})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return apperr.ErrAgentImageAssetInvalid
			}
			if result := tx.Where("candidate_id = ? AND session_id = ? AND asset_id = ?", candidateID, session.ID, assetID).Delete(&AgentCandidateAssetModel{}); result.Error != nil || result.RowsAffected != 1 {
				if result.Error != nil {
					return result.Error
				}
				return apperr.ErrAgentImageAssetInvalid
			}
			return nil
		}

		if command.Target == agentapproval.ApplyTargetInventoryOnly {
			for _, candidateID := range snapshot.OrderedCandidateIDs {
				// Inventory is a real, ready-to-plan state. Agent-created lots no longer
				// linger in the otherwise non-actionable DRAFT state.
				lots[candidateID].Status = v1.LotStatus_LOT_STATUS_READY
				if err := persistNewLot(candidateID, lots[candidateID]); err != nil {
					return err
				}
			}
		}

		events := make([]v1.AuctionEvent, 0, len(positions))
		for i, candidateID := range snapshot.OrderedCandidateIDs {
			if command.Target == agentapproval.ApplyTargetInventoryOnly {
				break
			}
			lot := lots[candidateID]
			queuedLotIDs = append(queuedLotIDs, lot.Id)
			position := positions[i]
			if err := auction.QueueLot(lot, position); err != nil {
				return apperr.ErrAgentCandidateChanged
			}
			lotModel, err := lotToModel(lot)
			if err != nil {
				return err
			}
			candidate := candidates[candidateID]
			if candidate.SourceType == agentconversation.SourceNew {
				if err := persistNewLot(candidateID, lot); err != nil {
					return err
				}
			} else {
				result := tx.Model(&AuctionLotModel{}).Where("id = ? AND version = ?", lot.Id, *candidate.ExistingLotReadVersion).Updates(lotModel)
				if result.Error != nil {
					return mapQueuePositionConflict(result.Error)
				}
				if result.RowsAffected != 1 {
					return apperr.ErrAgentCandidateChanged
				}
			}
			events = append(events, auction.NewAuctionEvent(v1.AuctionEventType_AUCTION_EVENT_TYPE_LOT_QUEUED, lot))
		}

		if command.Target == agentapproval.ApplyTargetQueue {
			if result := tx.Model(&AuctionRoomStateModel{}).Where("room_id = ? AND next_queue_position = ?", session.RoomID, roomState.NextQueuePosition).Updates(map[string]any{"next_queue_position": nextQueuePosition, "updated_at_unix_ms": now}); result.Error != nil || result.RowsAffected != 1 {
				if result.Error != nil {
					return result.Error
				}
				return apperr.ErrQueuePositionConflict
			}
		}
		if err := createEventModels(ctx, tx, events); err != nil {
			return err
		}

		createdJSON, _ := json.Marshal(createdIDs)
		positionsJSON, _ := json.Marshal(positions)
		model.Status = string(agentapproval.ExecutionApplied)
		model.CreatedLotIDsJSON = string(createdJSON)
		model.QueuePositionsJSON = string(positionsJSON)
		model.CompletedAtUnixMs = now
		if result := tx.Model(&AgentProposalExecutionModel{}).Where("id = ? AND status = ?", model.ID, string(agentapproval.ExecutionApplying)).Updates(map[string]any{"status": model.Status, "created_lot_ids_json": model.CreatedLotIDsJSON, "queue_positions_json": model.QueuePositionsJSON, "completed_at_unix_ms": now}); result.Error != nil || result.RowsAffected != 1 {
			if result.Error != nil {
				return result.Error
			}
			return apperr.ErrAgentBatchApplyFailed
		}
		auditDetails, _ := json.Marshal(map[string]any{"requestFingerprint": fingerprint, "selectedCandidateIds": command.SelectedCandidateIDs, "orderedCandidateIds": command.OrderedCandidateIDs, "createdLotIds": createdIDs, "queuePositions": positions, "target": command.Target})
		if err := tx.Create(&AgentApprovalAuditModel{ID: idgen.New("audit"), ExecutionID: model.ID, TenantID: session.TenantID, OperatorID: session.OperatorID, SessionID: session.ID, SnapshotVersion: snapshot.SnapshotVersion, DetailsJSON: string(auditDetails), CreatedAtUnixMs: now}).Error; err != nil {
			return err
		}
		outboxPayload, err := json.Marshal(map[string]any{"sessionId": session.ID, "executionId": model.ID, "lotIds": queuedLotIDs, "roomId": session.RoomID, "queuePositions": positions, "traceId": model.TraceID})
		if err != nil {
			return err
		}
		if err := tx.Create(&AgentApprovalOutboxModel{ID: idgen.New("agent-outbox"), ExecutionID: model.ID, SessionID: session.ID, RoomID: session.RoomID, TraceID: model.TraceID, PayloadJSON: string(outboxPayload), VisibleAtUnixMs: now}).Error; err != nil {
			return err
		}
		if result := tx.Model(&AgentSessionModel{}).Where("id = ? AND tenant_id = ? AND operator_id = ? AND version = ? AND status = ? AND active_snapshot_version = ?", session.ID, session.TenantID, session.OperatorID, session.Version, string(agentconversation.StatusReadyForReview), snapshot.SnapshotVersion).Updates(map[string]any{"status": string(agentconversation.StatusApplied), "version": session.Version + 1, "updated_at_unix_ms": now}); result.Error != nil || result.RowsAffected != 1 {
			if result.Error != nil {
				return result.Error
			}
			return apperr.ErrAgentSessionVersionConflict
		}
		var conversionErr error
		applied, conversionErr = executionFromModel(model)
		applied.Target = command.Target
		return conversionErr
	})
	if err == nil {
		return applied, nil
	}
	if isAgentApprovalDuplicateKey(err) {
		existing, findErr := r.FindExecutionByKey(ctx, command.SessionID, command.SnapshotVersion, command.IdempotencyKey)
		if findErr == nil && executionMatchesCommand(existing, command, fingerprint) {
			existing.Target = command.Target
			existing.IdempotentReplay = true
			return existing, nil
		}
		return agentapproval.Execution{}, apperr.ErrConflict
	}
	return agentapproval.Execution{}, mapApprovalError(err)
}

func (r *AgentApprovalRepository) InvalidateReviewSnapshot(ctx context.Context, command agentapproval.ApplyCommand, cause error) error {
	if r == nil || r.db == nil {
		return apperr.ErrAgentBatchApplyFailed
	}
	if !errors.Is(cause, apperr.ErrAgentReviewSnapshotStale) && !errors.Is(cause, apperr.ErrAgentCandidateChanged) && !errors.Is(cause, apperr.ErrAgentImageAssetInvalid) {
		return nil
	}
	now := time.Now().UnixMilli()
	result := r.db.WithContext(ctx).Model(&AgentSessionModel{}).
		Where("id = ? AND tenant_id = ? AND operator_id = ? AND status = ? AND active_snapshot_version = ?", command.SessionID, command.TenantID, command.OperatorID, string(agentconversation.StatusReadyForReview), command.SnapshotVersion).
		Updates(map[string]any{"status": string(agentconversation.StatusNeedsRevision), "active_snapshot_version": 0, "version": gorm.Expr("version + 1"), "updated_at_unix_ms": now})
	return result.Error
}

func lockApprovalSnapshot(ctx context.Context, tx *gorm.DB, session AgentSessionModel, command agentapproval.ApplyCommand) (agentconversation.ReviewSnapshot, error) {
	var model AgentApprovalSnapshotModel
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("session_id = ? AND snapshot_version = ?", session.ID, command.SnapshotVersion).First(&model).Error; err != nil {
		return agentconversation.ReviewSnapshot{}, approvalNotFound(err)
	}
	var snapshot agentconversation.ReviewSnapshot
	if err := json.Unmarshal([]byte(model.SnapshotJSON), &snapshot); err != nil {
		return agentconversation.ReviewSnapshot{}, apperr.ErrAgentReviewSnapshotStale
	}
	snapshot.SnapshotVersion = model.SnapshotVersion
	snapshot.ContentHash = model.ContentHash
	snapshot.SnapshotJSON = json.RawMessage(model.SnapshotJSON)
	snapshot.CreatedBy = model.CreatedBy
	snapshot.CreatedAtUnixMs = model.CreatedAtUnixMs
	if err := agentconversation.ValidateReviewSnapshotIntegrity(snapshot); err != nil || snapshot.TenantID != command.TenantID || snapshot.OperatorID != command.OperatorID || snapshot.RoomID != session.RoomID || snapshot.ContentHash != command.ContentHash || !reflect.DeepEqual(snapshot.SelectedCandidateIDs, command.SelectedCandidateIDs) || !reflect.DeepEqual(snapshot.OrderedCandidateIDs, command.OrderedCandidateIDs) {
		return agentconversation.ReviewSnapshot{}, apperr.ErrAgentReviewSnapshotStale
	}
	return snapshot, nil
}

func lockAndValidateSnapshotCandidates(ctx context.Context, tx *gorm.DB, session AgentSessionModel, snapshot agentconversation.ReviewSnapshot) (map[string]agentconversation.SnapshotCandidate, error) {
	ids := append([]string(nil), snapshot.SelectedCandidateIDs...)
	sort.Strings(ids)
	var models []AgentDraftCandidateModel
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("session_id = ? AND id IN ?", session.ID, ids).Order("id ASC").Find(&models).Error; err != nil {
		return nil, err
	}
	if len(models) != len(ids) {
		return nil, apperr.ErrAgentCandidateChanged
	}
	modelsByID := make(map[string]AgentDraftCandidateModel, len(models))
	for _, model := range models {
		modelsByID[model.ID] = model
	}
	result := make(map[string]agentconversation.SnapshotCandidate, len(ids))
	for _, candidate := range snapshot.Candidates {
		model, ok := modelsByID[candidate.CandidateID]
		if !ok || model.Version != candidate.CandidateVersion || model.SourceType != string(candidate.SourceType) || model.ExistingLotID != candidate.ExistingLotID {
			return nil, apperr.ErrAgentCandidateChanged
		}
		var current agentconversation.Candidate
		if err := json.Unmarshal([]byte(model.DraftJSON), &current); err != nil || !candidateDraftMatches(current, candidate) || !fieldSourcesMatch(model.FieldSourcesJSON, model.ProvenanceVersion, current.Draft, candidate.FieldSources) {
			return nil, apperr.ErrAgentCandidateChanged
		}
		result[candidate.CandidateID] = candidate
	}
	return result, nil
}

func lockAndValidateExistingLots(ctx context.Context, tx *gorm.DB, session AgentSessionModel, candidates map[string]agentconversation.SnapshotCandidate) (map[string]*v1.Lot, error) {
	byLotID := make(map[string]agentconversation.SnapshotCandidate)
	ids := make([]string, 0)
	for _, candidate := range candidates {
		if candidate.SourceType == agentconversation.SourceExisting {
			if candidate.ExistingLotID == "" || candidate.ExistingLotReadVersion == nil {
				return nil, apperr.ErrAgentCandidateChanged
			}
			ids = append(ids, candidate.ExistingLotID)
			byLotID[candidate.ExistingLotID] = candidate
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return map[string]*v1.Lot{}, nil
	}
	models, err := lockAuctionLotsSorted(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	if len(models) != len(ids) {
		return nil, apperr.ErrAgentCandidateChanged
	}
	result := make(map[string]*v1.Lot, len(ids))
	for i := range models {
		candidate := byLotID[models[i].ID]
		if models[i].MainAccountID != session.TenantID || models[i].RoomID != session.RoomID || models[i].Version != *candidate.ExistingLotReadVersion || models[i].StartedAtUnixMs != 0 || (v1.LotStatus(models[i].Status) != v1.LotStatus_LOT_STATUS_DRAFT && v1.LotStatus(models[i].Status) != v1.LotStatus_LOT_STATUS_READY) || v1.LotQueueStatus(models[i].QueueStatus) != v1.LotQueueStatus_LOT_QUEUE_STATUS_NONE || models[i].QueuePosition != 0 {
			return nil, apperr.ErrAgentCandidateChanged
		}
		lot, err := modelToLot(&models[i])
		if err != nil {
			return nil, err
		}
		result[models[i].ID] = lot
	}
	return result, nil
}

func lockAndValidateNewAssets(ctx context.Context, tx *gorm.DB, session AgentSessionModel, candidates map[string]agentconversation.SnapshotCandidate) (map[string]AssetFileModel, error) {
	ids := make([]string, 0)
	assetCandidate := make(map[string]string)
	for candidateID, candidate := range candidates {
		if candidate.SourceType != agentconversation.SourceNew {
			continue
		}
		if candidate.ExistingLotID != "" || candidate.ExistingLotReadVersion != nil || candidate.Draft.MainImageAssetID == "" || len(candidate.AssetIDs) != 1 || candidate.AssetIDs[0] != candidate.Draft.MainImageAssetID {
			return nil, apperr.ErrAgentImageAssetInvalid
		}
		ids = append(ids, candidate.Draft.MainImageAssetID)
		assetCandidate[candidate.Draft.MainImageAssetID] = candidateID
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return map[string]AssetFileModel{}, nil
	}
	var assets []AssetFileModel
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", ids).Order("id ASC").Find(&assets).Error; err != nil {
		return nil, err
	}
	if len(assets) != len(ids) {
		return nil, apperr.ErrAgentImageAssetInvalid
	}
	now := time.Now().UnixMilli()
	result := make(map[string]AssetFileModel, len(ids))
	for _, asset := range assets {
		candidateID := assetCandidate[asset.ID]
		var reference AgentCandidateAssetModel
		if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("candidate_id = ? AND session_id = ? AND asset_id = ? AND tenant_id = ? AND operator_id = ?", candidateID, session.ID, asset.ID, session.TenantID, session.OperatorID).First(&reference).Error; err != nil {
			return nil, apperr.ErrAgentImageAssetInvalid
		}
		if asset.MainAccountID != session.TenantID || asset.OwnerUserID != session.OperatorID || asset.RoomID != session.RoomID || asset.BizType != "agent_lot_image" || asset.Status != AssetStatusTemporary || asset.AttachedLotID != "" || asset.DeletedAtUnixMs != 0 || asset.ExpiresAtUnixMs <= now {
			return nil, apperr.ErrAgentImageAssetInvalid
		}
		result[asset.ID] = asset
	}
	return result, nil
}

func newLotFromSnapshot(session AgentSessionModel, candidate agentconversation.SnapshotCandidate, imageURL string) (*v1.Lot, error) {
	draft := candidate.Draft
	rule := &v1.BidRule{StartPrice: &v1.Money{Amount: draft.StartPriceAmount, Currency: "CNY"}, MinIncrement: &v1.Money{Amount: draft.MinIncrementAmount, Currency: "CNY"}, DurationSeconds: draft.DurationSeconds, AntiSnipeWindowSeconds: draft.AntiSnipeWindowSeconds, AntiSnipeExtendSeconds: draft.AntiSnipeExtendSeconds, MaxExtendCount: draft.MaxExtendCount}
	if draft.CapPriceAmount != nil {
		rule.CapPrice = &v1.Money{Amount: *draft.CapPriceAmount, Currency: "CNY"}
	}
	lot, err := auction.NewLotFromRequest(idgen.New("lot"), &v1.CreateLotRequest{RoomId: session.RoomID, Title: draft.Title, Description: draft.Description, ImageUrl: imageURL, Rule: rule, Category: draft.Category, Tags: append([]string(nil), draft.Tags...), Stock: draft.Stock, DepositAmount: &v1.Money{Amount: draft.DepositAmount, Currency: "CNY"}})
	if err != nil {
		return nil, err
	}
	lot.MainAccountId = session.TenantID
	return lot, nil
}

func candidateDraftMatches(current agentconversation.Candidate, snapshot agentconversation.SnapshotCandidate) bool {
	if current.ID != snapshot.CandidateID || current.SourceType != snapshot.SourceType || current.ExistingLotID != snapshot.ExistingLotID || !reflect.DeepEqual(current.ExistingLotReadVersion, snapshot.ExistingLotReadVersion) {
		return false
	}
	d := current.Draft
	s := snapshot.Draft
	return d.Title == s.Title && d.StartPriceAmount == s.StartPriceAmount && d.DurationSeconds == s.DurationSeconds && d.MainImageAssetID == s.MainImageAssetID && d.MinIncrementAmount == s.MinIncrementAmount && reflect.DeepEqual(d.CapPriceAmount, s.CapPriceAmount) && d.Category == s.Category && d.Description == s.Description && reflect.DeepEqual(nonNilStrings(d.Tags), nonNilStrings(s.Tags)) && d.Stock == s.Stock && d.AntiSnipeWindowSeconds == s.AntiSnipeWindowSeconds && d.AntiSnipeExtendSeconds == s.AntiSnipeExtendSeconds && d.MaxExtendCount == s.MaxExtendCount && d.DepositAmount == s.DepositAmount
}

func fieldSourcesMatch(encoded string, provenanceVersion int32, draft agentconversation.CandidateDraft, snapshot []agentconversation.SnapshotFieldSource) bool {
	var sources map[string]string
	if json.Unmarshal([]byte(encoded), &sources) != nil {
		return false
	}
	if provenanceVersion == 0 {
		delete(sources, "mainImageUrl")
		agentconversation.BackfillLegacyOptionalSources(draft, sources)
	} else if provenanceVersion != agentconversation.CurrentProvenanceVersion {
		return false
	}
	if len(sources) != len(snapshot) {
		return false
	}
	for _, field := range snapshot {
		if sources[field.Field] != field.Source {
			return false
		}
	}
	return true
}

func newApplyingExecutionModel(command agentapproval.ApplyCommand, fingerprint string, now int64) (AgentProposalExecutionModel, error) {
	selected, err := json.Marshal(command.SelectedCandidateIDs)
	if err != nil {
		return AgentProposalExecutionModel{}, err
	}
	ordered, err := json.Marshal(command.OrderedCandidateIDs)
	if err != nil {
		return AgentProposalExecutionModel{}, err
	}
	traceID := strings.TrimSpace(command.TraceID)
	if !validApprovalTraceID(traceID) {
		traceID = ""
	}
	return AgentProposalExecutionModel{ID: idgen.New("agent-exec"), TenantID: command.TenantID, OperatorID: command.OperatorID, SessionID: command.SessionID, SnapshotVersion: command.SnapshotVersion, IdempotencyKey: command.IdempotencyKey, RequestFingerprint: fingerprint, Status: string(agentapproval.ExecutionApplying), SelectedCandidateIDsJSON: string(selected), OrderedCandidateIDsJSON: string(ordered), CreatedLotIDsJSON: "[]", QueuePositionsJSON: "[]", CreatedAtUnixMs: now, TraceID: traceID}, nil
}

func validApprovalTraceID(value string) bool {
	if len(value) != 32 || strings.Trim(value, "0") == "" {
		return false
	}
	for _, char := range value {
		if (char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') {
			continue
		}
		return false
	}
	return true
}

func executionFromModel(model AgentProposalExecutionModel) (agentapproval.Execution, error) {
	result := agentapproval.Execution{ID: model.ID, TenantID: model.TenantID, OperatorID: model.OperatorID, SessionID: model.SessionID, SnapshotVersion: model.SnapshotVersion, IdempotencyKey: model.IdempotencyKey, RequestFingerprint: model.RequestFingerprint, Status: agentapproval.ExecutionStatus(model.Status), ErrorCode: model.ErrorCode, CreatedAtUnixMs: model.CreatedAtUnixMs, CompletedAtUnixMs: model.CompletedAtUnixMs, TraceID: model.TraceID}
	if err := json.Unmarshal([]byte(model.SelectedCandidateIDsJSON), &result.SelectedCandidateIDs); err != nil {
		return agentapproval.Execution{}, err
	}
	if err := json.Unmarshal([]byte(model.OrderedCandidateIDsJSON), &result.OrderedCandidateIDs); err != nil {
		return agentapproval.Execution{}, err
	}
	if err := json.Unmarshal([]byte(model.CreatedLotIDsJSON), &result.CreatedLotIDs); err != nil {
		return agentapproval.Execution{}, err
	}
	if err := json.Unmarshal([]byte(model.QueuePositionsJSON), &result.QueuePositions); err != nil {
		return agentapproval.Execution{}, err
	}
	return result, nil
}

func executionMatchesCommand(execution agentapproval.Execution, command agentapproval.ApplyCommand, fingerprint string) bool {
	return execution.Status == agentapproval.ExecutionApplied && execution.TenantID == command.TenantID && execution.OperatorID == command.OperatorID && execution.SessionID == command.SessionID && execution.SnapshotVersion == command.SnapshotVersion && execution.IdempotencyKey == command.IdempotencyKey && execution.RequestFingerprint == fingerprint && reflect.DeepEqual(execution.SelectedCandidateIDs, command.SelectedCandidateIDs) && reflect.DeepEqual(execution.OrderedCandidateIDs, command.OrderedCandidateIDs)
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
func approvalNotFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return apperr.ErrAgentReviewSnapshotStale
	}
	return err
}
func isAgentApprovalDuplicateKey(err error) bool {
	text := strings.ToLower(fmt.Sprint(err))
	return strings.Contains(text, "duplicate entry") || strings.Contains(text, "unique constraint")
}
func mapApprovalError(err error) error {
	for _, safe := range []error{context.Canceled, context.DeadlineExceeded, apperr.ErrNotFound, apperr.ErrConflict, apperr.ErrInvalidArgument, apperr.ErrPermissionDenied, apperr.ErrAgentSessionVersionConflict, apperr.ErrAgentReviewSnapshotStale, apperr.ErrAgentCandidateChanged, apperr.ErrAgentImageAssetInvalid, apperr.ErrQueuePositionConflict} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	if isAgentApprovalDuplicateKey(err) {
		return apperr.ErrConflict
	}
	return apperr.ErrAgentBatchApplyFailed
}
