package data

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"live-auction-bid/backend/app/auction/service/internal/agentconversation"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

const (
	AssetStatusTemporary = "temporary"
	AssetStatusDeleting  = "deleting"
	AssetStatusAttached  = "attached"
	AssetStatusDeleted   = "deleted"
)

// A process crash after claiming can leave a row in deleting. Reclaiming such
// stale claims needs a timestamped lease and is deliberately deferred; a
// deleting row is fail-closed and cannot be rebound to an active candidate.

// ClaimTemporaryAssetForDelete is the manual, owner-scoped delete claim.
func (s *Store) ClaimTemporaryAssetForDelete(ctx context.Context, assetID, ownerUserID string) (*AssetFile, error) {
	if assetID == "" || ownerUserID == "" {
		return nil, errors.New("asset id and owner user id are required")
	}
	return s.claimAssetForDelete(ctx, assetID, ownerUserID, false)
}

// ClaimExpiredTemporaryAssetForDelete is the cleanup-worker claim. The expiry
// predicate is repeated in the atomic update so a stale list result is harmless.
func (s *Store) ClaimExpiredTemporaryAssetForDelete(ctx context.Context, assetID string) (*AssetFile, error) {
	if assetID == "" {
		return nil, errors.New("asset id is required")
	}
	return s.claimAssetForDelete(ctx, assetID, "", true)
}

func (s *Store) claimAssetForDelete(ctx context.Context, assetID, ownerUserID string, requireExpired bool) (*AssetFile, error) {
	var claimed AssetFileModel
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&AssetFileModel{}).
			Where("id = ? AND status = ? AND deleted_at_unix_ms = 0", assetID, AssetStatusTemporary).
			Where(activeAgentCandidateAssetExclusionSQL(), string(agentconversation.StatusApplied), string(agentconversation.StatusCancelled))
		if ownerUserID != "" {
			query = query.Where("owner_user_id = ?", ownerUserID)
		}
		if requireExpired {
			query = query.Where("expires_at_unix_ms > 0 AND expires_at_unix_ms <= ?", time.Now().UnixMilli())
		}
		result := query.Update("status", AssetStatusDeleting)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		lookup := tx.Where("id = ? AND status = ?", assetID, AssetStatusDeleting)
		if ownerUserID != "" {
			lookup = lookup.Where("owner_user_id = ?", ownerUserID)
		}
		return lookup.First(&claimed).Error
	})
	if err != nil {
		return nil, err
	}
	return assetFileFromModel(&claimed), nil
}

func (s *Store) RestoreDeletingAsset(ctx context.Context, assetID, ownerUserID string) error {
	return s.transitionDeletingAsset(ctx, assetID, ownerUserID, AssetStatusTemporary)
}

func (s *Store) RestoreDeletingAssetByID(ctx context.Context, assetID string) error {
	return s.transitionDeletingAsset(ctx, assetID, "", AssetStatusTemporary)
}

func (s *Store) FinalizeDeletingAsset(ctx context.Context, assetID, ownerUserID string) error {
	return s.transitionDeletingAsset(ctx, assetID, ownerUserID, AssetStatusDeleted)
}

func (s *Store) FinalizeDeletingAssetByID(ctx context.Context, assetID string) error {
	return s.transitionDeletingAsset(ctx, assetID, "", AssetStatusDeleted)
}

func (s *Store) transitionDeletingAsset(ctx context.Context, assetID, ownerUserID, target string) error {
	if assetID == "" || target != AssetStatusTemporary && target != AssetStatusDeleted {
		return errors.New("valid asset transition is required")
	}
	updates := map[string]any{"status": target}
	if target == AssetStatusDeleted {
		updates["deleted_at_unix_ms"] = time.Now().UnixMilli()
	}
	query := s.db.WithContext(ctx).Model(&AssetFileModel{}).Where("id = ? AND status = ?", assetID, AssetStatusDeleting)
	if ownerUserID != "" {
		query = query.Where("owner_user_id = ?", ownerUserID)
	}
	result := query.Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

type AssetFile struct {
	ID               string
	MainAccountID    string
	OwnerUserID      string
	RoomID           string
	BizType          string
	Status           string
	AttachedLotID    string
	StorageProvider  string
	Bucket           string
	ObjectKey        string
	PublicURL        string
	OriginalName     string
	MimeType         string
	SizeBytes        int64
	SHA256           string
	AttachedAtUnixMs int64
	DeletedAtUnixMs  int64
	ExpiresAtUnixMs  int64
}

func (s *Store) SaveAssetFile(ctx context.Context, asset AssetFile) error {
	if asset.ID == "" {
		return errors.New("asset id is required")
	}
	if asset.OwnerUserID == "" {
		return errors.New("asset owner user id is required")
	}
	if asset.StorageProvider == "" || asset.Bucket == "" || asset.ObjectKey == "" || asset.PublicURL == "" {
		return errors.New("asset storage location is required")
	}
	if asset.MimeType == "" || asset.SizeBytes <= 0 || asset.SHA256 == "" {
		return errors.New("asset file metadata is required")
	}
	model := AssetFileModel{
		ID:               asset.ID,
		MainAccountID:    asset.MainAccountID,
		OwnerUserID:      asset.OwnerUserID,
		RoomID:           asset.RoomID,
		BizType:          asset.BizType,
		Status:           assetStatusOrDefault(asset.Status),
		AttachedLotID:    asset.AttachedLotID,
		StorageProvider:  asset.StorageProvider,
		Bucket:           asset.Bucket,
		ObjectKey:        asset.ObjectKey,
		PublicURL:        asset.PublicURL,
		OriginalName:     asset.OriginalName,
		MimeType:         asset.MimeType,
		SizeBytes:        asset.SizeBytes,
		SHA256:           asset.SHA256,
		AttachedAtUnixMs: asset.AttachedAtUnixMs,
		DeletedAtUnixMs:  asset.DeletedAtUnixMs,
		ExpiresAtUnixMs:  asset.ExpiresAtUnixMs,
	}
	return s.db.WithContext(ctx).Create(&model).Error
}

func assetStatusOrDefault(status string) string {
	if status == "" {
		return AssetStatusTemporary
	}
	return status
}

func (s *Store) AttachAssetFilesByURL(ctx context.Context, ownerUserID, lotID string, publicURLs []string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return attachAssetFilesByURL(tx, ownerUserID, lotID, publicURLs)
	})
}

func attachAssetFilesByURL(tx *gorm.DB, ownerUserID, lotID string, publicURLs []string) error {
	if ownerUserID == "" || lotID == "" || len(publicURLs) == 0 {
		return nil
	}
	urls := compactStrings(publicURLs)
	if len(urls) == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	return tx.Model(&AssetFileModel{}).
		Where("owner_user_id = ? AND public_url IN ? AND status = ? AND deleted_at_unix_ms = 0", ownerUserID, urls, AssetStatusTemporary).
		Updates(map[string]any{
			"status":              AssetStatusAttached,
			"attached_lot_id":     lotID,
			"attached_at_unix_ms": now,
			"expires_at_unix_ms":  int64(0),
		}).Error
}

func compactStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (s *Store) ListExpiredTemporaryAssets(ctx context.Context, limit int) ([]AssetFile, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	var models []AssetFileModel
	now := time.Now().UnixMilli()
	if err := s.db.WithContext(ctx).
		Where("status = ? AND deleted_at_unix_ms = 0 AND expires_at_unix_ms > 0 AND expires_at_unix_ms <= ?", AssetStatusTemporary, now).
		Where(activeAgentCandidateAssetExclusionSQL(), string(agentconversation.StatusApplied), string(agentconversation.StatusCancelled)).
		Order("expires_at_unix_ms ASC").
		Limit(limit).
		Find(&models).Error; err != nil {
		return nil, err
	}
	assets := make([]AssetFile, 0, len(models))
	for i := range models {
		assets = append(assets, *assetFileFromModel(&models[i]))
	}
	return assets, nil
}

// BindCandidateImage atomically validates a temporary upload, replaces the
// candidate's draft-only reference, and advances both optimistic-lock versions.
// The asset remains temporary until a later approval transaction attaches it.
func (r *AgentConversationRepo) BindCandidateImage(ctx context.Context, command agentconversation.BindCandidateImageCommand) (agentconversation.SessionAggregate, error) {
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
		status := agentconversation.SessionStatus(session.Status)
		if status != agentconversation.StatusCollectingInput && status != agentconversation.StatusReadyForReview && status != agentconversation.StatusNeedsRevision {
			return apperr.ErrConflict
		}

		var candidateModel AgentDraftCandidateModel
		if err := tx.Where("id = ? AND session_id = ?", command.CandidateID, session.ID).First(&candidateModel).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperr.ErrNotFound
			}
			return err
		}
		if candidateModel.Version != command.ExpectedCandidateVersion {
			return apperr.ErrAgentSessionVersionConflict
		}

		var asset AssetFileModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", command.AssetID).First(&asset).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperr.ErrPermissionDenied
			}
			return err
		}
		if asset.MainAccountID != command.TenantID || asset.OwnerUserID != command.OperatorID {
			return apperr.ErrPermissionDenied
		}
		if asset.Status != AssetStatusTemporary || asset.BizType != "agent_lot_image" || asset.DeletedAtUnixMs != 0 || asset.ExpiresAtUnixMs <= time.Now().UnixMilli() || asset.AttachedLotID != "" {
			return apperr.ErrAgentImageAssetInvalid
		}
		if asset.RoomID != "" && asset.RoomID != session.RoomID {
			return apperr.ErrPermissionDenied
		}
		var occupied int64
		if err := tx.Model(&AgentCandidateAssetModel{}).Where("asset_id = ? AND candidate_id <> ?", asset.ID, candidateModel.ID).Count(&occupied).Error; err != nil {
			return err
		}
		if occupied != 0 {
			return apperr.ErrAgentImageAssetInvalid
		}

		candidate, err := agentDraftCandidateFromModel(candidateModel)
		if err != nil {
			return err
		}
		candidate.Candidate.Draft.MainImageAssetID = asset.ID
		candidate.Candidate.Draft.MainImageURL = asset.PublicURL
		sources := map[string]string{}
		if candidate.FieldSourcesJSON != "" && json.Unmarshal([]byte(candidate.FieldSourcesJSON), &sources) != nil {
			return apperr.ErrInvalidArgument
		}
		sources["mainImageAssetId"] = "USER"
		delete(sources, "mainImageUrl")
		sourceJSON, err := json.Marshal(sources)
		if err != nil {
			return err
		}
		candidate.FieldSourcesJSON = string(sourceJSON)
		candidate.ValidationJSON, err = candidateImageValidationJSON(candidate.Candidate)
		if err != nil {
			return err
		}
		candidate.Version = candidateModel.Version + 1
		candidate.UpdatedAtUnixMs = updatedAt
		nextModel, err := agentDraftCandidateModelFromCandidate(session.ID, candidate, updatedAt)
		if err != nil {
			return err
		}
		candidateUpdate := tx.Model(&AgentDraftCandidateModel{}).
			Where("id = ? AND session_id = ? AND version = ?", candidateModel.ID, session.ID, command.ExpectedCandidateVersion).
			Updates(map[string]any{"draft_json": nextModel.DraftJSON, "field_sources_json": nextModel.FieldSourcesJSON, "validation_json": nextModel.ValidationJSON, "version": nextModel.Version, "updated_at_unix_ms": updatedAt})
		if candidateUpdate.Error != nil {
			return candidateUpdate.Error
		}
		if candidateUpdate.RowsAffected != 1 {
			return apperr.ErrAgentSessionVersionConflict
		}

		var reference AgentCandidateAssetModel
		findRef := tx.Where("candidate_id = ?", candidateModel.ID).First(&reference).Error
		switch {
		case findRef == nil:
			result := tx.Model(&AgentCandidateAssetModel{}).Where("candidate_id = ?", candidateModel.ID).
				Updates(map[string]any{"session_id": session.ID, "asset_id": asset.ID, "tenant_id": command.TenantID, "operator_id": command.OperatorID, "updated_at_unix_ms": updatedAt})
			if result.Error != nil {
				if duplicateConstraint(result.Error) {
					return apperr.ErrAgentImageAssetInvalid
				}
				return result.Error
			}
		case errors.Is(findRef, gorm.ErrRecordNotFound):
			reference = AgentCandidateAssetModel{CandidateID: candidateModel.ID, SessionID: session.ID, AssetID: asset.ID, TenantID: command.TenantID, OperatorID: command.OperatorID, CreatedAtUnixMs: updatedAt, UpdatedAtUnixMs: updatedAt}
			if err := tx.Create(&reference).Error; err != nil {
				if duplicateConstraint(err) {
					return apperr.ErrAgentImageAssetInvalid
				}
				return err
			}
		default:
			return findRef
		}

		nextStatus, err := agentCandidateAggregateStatus(tx, session.ID, status)
		if err != nil {
			return err
		}
		sessionUpdate := tx.Model(&AgentSessionModel{}).
			Where("id = ? AND tenant_id = ? AND operator_id = ? AND version = ?", session.ID, command.TenantID, command.OperatorID, command.ExpectedSessionVersion).
			Updates(map[string]any{"status": nextStatus, "active_snapshot_version": 0, "version": session.Version + 1, "updated_at_unix_ms": updatedAt})
		if sessionUpdate.Error != nil {
			return sessionUpdate.Error
		}
		if sessionUpdate.RowsAffected != 1 {
			return apperr.ErrAgentSessionVersionConflict
		}
		return nil
	})
	if err != nil {
		return agentconversation.SessionAggregate{}, err
	}
	return r.FindSession(ctx, command.TenantID, command.SessionID)
}

func candidateImageValidationJSON(candidate agentconversation.Candidate) (string, error) {
	missing := agentconversation.MissingRequiredInputs(candidate)
	validationErrors := []string{}
	if len(missing) == 0 {
		if err := agentconversation.ValidateCandidate(candidate); err != nil {
			validationErrors = append(validationErrors, err.Error())
		}
	}
	raw, err := json.Marshal(map[string]any{"requiredInputs": missing, "errors": validationErrors})
	return string(raw), err
}

func agentCandidateAggregateStatus(tx *gorm.DB, sessionID string, current agentconversation.SessionStatus) (agentconversation.SessionStatus, error) {
	var models []AgentDraftCandidateModel
	if err := tx.Where("session_id = ?", sessionID).Find(&models).Error; err != nil {
		return "", err
	}
	allValid := len(models) > 0
	for _, model := range models {
		candidate, err := agentDraftCandidateFromModel(model)
		if err != nil {
			return "", err
		}
		if agentconversation.ValidateCandidate(candidate.Candidate) != nil {
			allValid = false
		}
	}
	if allValid {
		return agentconversation.StatusReadyForReview, nil
	}
	if current == agentconversation.StatusCollectingInput {
		return agentconversation.StatusCollectingInput, nil
	}
	return agentconversation.StatusNeedsRevision, nil
}

func duplicateConstraint(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") || strings.Contains(message, "duplicate entry")
}

func activeAgentCandidateAssetExclusionSQL() string {
	return `NOT EXISTS (
		SELECT 1 FROM agent_candidate_assets aca
		JOIN agent_sessions ags ON ags.id = aca.session_id
		WHERE aca.asset_id = asset_files.id AND ags.status NOT IN (?, ?)
	)`
}

func assetFileFromModel(model *AssetFileModel) *AssetFile {
	return &AssetFile{
		ID:               model.ID,
		MainAccountID:    model.MainAccountID,
		OwnerUserID:      model.OwnerUserID,
		RoomID:           model.RoomID,
		BizType:          model.BizType,
		Status:           model.Status,
		AttachedLotID:    model.AttachedLotID,
		StorageProvider:  model.StorageProvider,
		Bucket:           model.Bucket,
		ObjectKey:        model.ObjectKey,
		PublicURL:        model.PublicURL,
		OriginalName:     model.OriginalName,
		MimeType:         model.MimeType,
		SizeBytes:        model.SizeBytes,
		SHA256:           model.SHA256,
		AttachedAtUnixMs: model.AttachedAtUnixMs,
		DeletedAtUnixMs:  model.DeletedAtUnixMs,
		ExpiresAtUnixMs:  model.ExpiresAtUnixMs,
	}
}
