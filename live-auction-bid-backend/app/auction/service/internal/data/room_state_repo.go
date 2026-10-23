package data

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/biz/auction"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

// Global queue-writer lock contract: lock auction_room_states first, then lot
// rows in lexical ID order. QueueLotAsNext, RelistLotAsNext, StartLot, terminal
// Save and Agent Apply all follow this order. A writer that only knows a lot ID
// performs a non-locking discovery read and revalidates room/tenant after the
// ordered locks are held.
func lockAuctionRoomState(ctx context.Context, tx *gorm.DB, roomID string) (AuctionRoomStateModel, error) {
	var state AuctionRoomStateModel
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("room_id = ?", roomID).First(&state).Error
	return state, err
}

// allocateQueuePositions validates in int64 before converting to MySQL INT /
// protobuf int32. The returned next counter must itself remain representable.
func allocateQueuePositions(next int32, count int) ([]int32, int32, error) {
	if next <= 0 || count <= 0 {
		return nil, 0, apperr.ErrQueuePositionConflict
	}
	next64 := int64(next)
	after := next64 + int64(count)
	if after > math.MaxInt32 {
		return nil, 0, apperr.ErrQueuePositionConflict
	}
	positions := make([]int32, count)
	for i := range positions {
		positions[i] = int32(next64 + int64(i))
	}
	return positions, int32(after), nil
}

func (s *Store) QueueLotAsNext(ctx context.Context, lotID, mainAccountID, ownerUserID string, nowMs int64) (*v1.Lot, int32, []v1.AuctionEvent, error) {
	lotID = strings.TrimSpace(lotID)
	mainAccountID = strings.TrimSpace(mainAccountID)
	if lotID == "" {
		return nil, 0, nil, errors.New("lot id is required")
	}
	if mainAccountID == "" {
		return nil, 0, nil, fmt.Errorf("%w: main account id is required", apperr.ErrPermissionDenied)
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}

	var queuedLot *v1.Lot
	var queuePosition int32
	var events []v1.AuctionEvent
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Queue writers always lock room_state before lot rows. A non-locking
		// pre-read discovers the room; the locked read below revalidates it.
		var discovered AuctionLotModel
		if err := tx.WithContext(ctx).Where("id = ?", lotID).First(&discovered).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperr.ErrNotFound
			}
			return err
		}
		if discovered.MainAccountID != mainAccountID {
			return apperr.ErrPermissionDenied
		}
		if discovered.RoomID == "" {
			return errors.New("room id is required")
		}
		if err := ensureRoomStateRecord(ctx, tx, discovered.RoomID, discovered.MainAccountID, nowMs); err != nil {
			return err
		}
		if s.beforeQueueRoomStateLock != nil {
			s.beforeQueueRoomStateLock(tx)
		}
		state, err := lockAuctionRoomState(ctx, tx, discovered.RoomID)
		if err != nil {
			return err
		}
		locked, err := lockAuctionLotsSorted(ctx, tx, []string{lotID})
		if err != nil {
			return err
		}
		if len(locked) != 1 {
			return apperr.ErrNotFound
		}
		current := locked[0]
		if current.MainAccountID != mainAccountID {
			return apperr.ErrPermissionDenied
		}
		if current.RoomID != discovered.RoomID {
			return fmtInvalidRoomState()
		}

		lot, err := modelToLot(&current)
		if err != nil {
			return err
		}
		queuePosition = lot.GetQueuePosition()
		alreadyQueued := lot.GetQueueStatus() == v1.LotQueueStatus_LOT_QUEUE_STATUS_QUEUED && queuePosition > 0
		if alreadyQueued {
			queuedLot = lot
			return nil
		}
		if queuePosition <= 0 {
			positions, _, allocationErr := allocateQueuePositions(state.NextQueuePosition, 1)
			if allocationErr != nil {
				return allocationErr
			}
			queuePosition = positions[0]
		}
		if err := auction.QueueLot(lot, queuePosition); err != nil {
			return err
		}
		model, err := lotToModel(lot)
		if err != nil {
			return err
		}
		result := tx.WithContext(ctx).
			Model(&AuctionLotModel{}).
			Where("id = ? AND version = ?", lot.GetId(), current.Version).
			Select("*").
			Omit("created_at").
			Updates(model)
		if result.Error != nil {
			return mapQueuePositionConflict(result.Error)
		}
		if result.RowsAffected == 0 {
			return apperr.ErrLotVersionConflict
		}
		if state.NextQueuePosition <= queuePosition {
			_, next, allocationErr := allocateQueuePositions(queuePosition, 1)
			if allocationErr != nil {
				return allocationErr
			}
			state.NextQueuePosition = next
		}
		if err := tx.WithContext(ctx).
			Model(&AuctionRoomStateModel{}).
			Where("room_id = ?", lot.GetRoomId()).
			Updates(map[string]any{
				"main_account_id":     lot.GetMainAccountId(),
				"next_queue_position": state.NextQueuePosition,
				"updated_at_unix_ms":  nowMs,
			}).Error; err != nil {
			return err
		}
		if err := attachAssetFilesByURL(tx, ownerUserID, lot.GetId(), lotAssetURLs(lot)); err != nil {
			return err
		}
		event := auction.NewAuctionEvent(v1.AuctionEventType_AUCTION_EVENT_TYPE_LOT_QUEUED, lot)
		events = []v1.AuctionEvent{event}
		if err := createEventModels(ctx, tx, events); err != nil {
			return err
		}
		lot.UpdatedAtUnixMs = time.Now().UnixMilli()
		queuedLot = lot
		return nil
	}); err != nil {
		return nil, 0, nil, err
	}
	if err := s.streamEvents(ctx, events); err != nil {
		return nil, 0, nil, err
	}
	return queuedLot, queuePosition, events, nil
}

func (s *Store) DequeueLot(ctx context.Context, lotID, mainAccountID string, nowMs int64) (*v1.Lot, []v1.AuctionEvent, error) {
	lotID = strings.TrimSpace(lotID)
	mainAccountID = strings.TrimSpace(mainAccountID)
	if lotID == "" {
		return nil, nil, errors.New("lot id is required")
	}
	if mainAccountID == "" {
		return nil, nil, fmt.Errorf("%w: main account id is required", apperr.ErrPermissionDenied)
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}

	var dequeued *v1.Lot
	var events []v1.AuctionEvent
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var discovered AuctionLotModel
		if err := tx.WithContext(ctx).Where("id = ?", lotID).First(&discovered).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperr.ErrNotFound
			}
			return err
		}
		if discovered.MainAccountID != mainAccountID {
			return apperr.ErrPermissionDenied
		}
		if discovered.RoomID == "" {
			return errors.New("room id is required")
		}
		if err := ensureRoomStateRecord(ctx, tx, discovered.RoomID, discovered.MainAccountID, nowMs); err != nil {
			return err
		}
		if _, err := lockAuctionRoomState(ctx, tx, discovered.RoomID); err != nil {
			return err
		}
		locked, err := lockAuctionLotsSorted(ctx, tx, []string{lotID})
		if err != nil {
			return err
		}
		if len(locked) != 1 {
			return apperr.ErrNotFound
		}
		current := locked[0]
		if current.MainAccountID != mainAccountID {
			return apperr.ErrPermissionDenied
		}
		if current.RoomID != discovered.RoomID {
			return fmtInvalidRoomState()
		}
		lot, err := modelToLot(&current)
		if err != nil {
			return err
		}
		previousVersion := lot.GetVersion()
		if err := auction.DequeueLot(lot); err != nil {
			return err
		}
		if lot.GetVersion() == previousVersion {
			dequeued = lot
			return nil
		}
		lot.UpdatedAtUnixMs = nowMs
		model, err := lotToModel(lot)
		if err != nil {
			return err
		}
		result := tx.WithContext(ctx).
			Model(&AuctionLotModel{}).
			Where("id = ? AND version = ?", lot.GetId(), current.Version).
			Select("*").
			Omit("created_at").
			Updates(model)
		if result.Error != nil {
			return mapQueuePositionConflict(result.Error)
		}
		if result.RowsAffected == 0 {
			return apperr.ErrLotVersionConflict
		}
		event := auction.NewAuctionEvent(v1.AuctionEventType_AUCTION_EVENT_TYPE_LOT_UPDATED, lot)
		events = []v1.AuctionEvent{event}
		if err := createEventModels(ctx, tx, events); err != nil {
			return err
		}
		dequeued = lot
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if err := s.streamEvents(ctx, events); err != nil {
		return nil, nil, err
	}
	return dequeued, events, nil
}

func (s *Store) RelistLotAsNext(ctx context.Context, sourceLotID, newLotID, mainAccountID, ownerUserID string, nowMs int64) (*v1.Lot, int32, []v1.AuctionEvent, error) {
	sourceLotID = strings.TrimSpace(sourceLotID)
	newLotID = strings.TrimSpace(newLotID)
	mainAccountID = strings.TrimSpace(mainAccountID)
	if sourceLotID == "" || newLotID == "" {
		return nil, 0, nil, errors.New("source lot id and new lot id are required")
	}
	if mainAccountID == "" {
		return nil, 0, nil, fmt.Errorf("%w: main account id is required", apperr.ErrPermissionDenied)
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}

	var queuedLot *v1.Lot
	var queuePosition int32
	var events []v1.AuctionEvent
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var discovered AuctionLotModel
		if err := tx.WithContext(ctx).Where("id = ?", sourceLotID).First(&discovered).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperr.ErrNotFound
			}
			return err
		}
		if discovered.MainAccountID != mainAccountID {
			return apperr.ErrPermissionDenied
		}
		if discovered.RoomID == "" {
			return errors.New("room id is required")
		}
		if err := ensureRoomStateRecord(ctx, tx, discovered.RoomID, discovered.MainAccountID, nowMs); err != nil {
			return err
		}
		if s.beforeQueueRoomStateLock != nil {
			s.beforeQueueRoomStateLock(tx)
		}
		state, err := lockAuctionRoomState(ctx, tx, discovered.RoomID)
		if err != nil {
			return err
		}
		locked, err := lockAuctionLotsSorted(ctx, tx, []string{sourceLotID})
		if err != nil {
			return err
		}
		if len(locked) != 1 {
			return apperr.ErrNotFound
		}
		sourceModel := locked[0]
		if sourceModel.MainAccountID != mainAccountID {
			return apperr.ErrPermissionDenied
		}
		if sourceModel.RoomID != discovered.RoomID {
			return fmtInvalidRoomState()
		}
		sourceLot, err := modelToLot(&sourceModel)
		if err != nil {
			return err
		}
		relistReason := relistEventReason(sourceLotID)
		var existingRelistCount int64
		if err := tx.WithContext(ctx).
			Model(&AuctionEventModel{}).
			Where("main_account_id = ? AND room_id = ? AND type = ? AND reason = ?", sourceLot.GetMainAccountId(), sourceLot.GetRoomId(), int32(v1.AuctionEventType_AUCTION_EVENT_TYPE_LOT_QUEUED), relistReason).
			Count(&existingRelistCount).Error; err != nil {
			return err
		}
		if existingRelistCount > 0 || lotRelistMarker(sourceModel.CancelReason) != "" {
			return fmt.Errorf("%w: lot has already been relisted", apperr.ErrInvalidArgument)
		}
		positions, nextQueuePosition, err := allocateQueuePositions(state.NextQueuePosition, 1)
		if err != nil {
			return err
		}
		queuePosition = positions[0]
		lot, err := auction.NewRelistLotFromFailed(newLotID, sourceLot)
		if err != nil {
			return err
		}
		if err := auction.QueueLot(lot, queuePosition); err != nil {
			return err
		}
		model, err := lotToModel(lot)
		if err != nil {
			return err
		}
		if err := tx.WithContext(ctx).Create(model).Error; err != nil {
			return err
		}
		lot.CreatedAtUnixMs = modelTimeUnixMsOr(model.CreatedAt, nowMs)
		lot.UpdatedAtUnixMs = modelTimeUnixMsOr(model.UpdatedAt, lot.CreatedAtUnixMs)
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&AuctionLotStatsModel{
			LotID:           lot.Id,
			MainAccountID:   lot.GetMainAccountId(),
			RoomID:          lot.RoomId,
			UpdatedAtUnixMs: 0,
		}).Error; err != nil {
			return err
		}
		state.NextQueuePosition = nextQueuePosition
		if err := tx.WithContext(ctx).
			Model(&AuctionRoomStateModel{}).
			Where("room_id = ?", lot.GetRoomId()).
			Updates(map[string]any{
				"main_account_id":     lot.GetMainAccountId(),
				"next_queue_position": state.NextQueuePosition,
				"updated_at_unix_ms":  nowMs,
			}).Error; err != nil {
			return err
		}
		if err := attachAssetFilesByURL(tx, ownerUserID, lot.GetId(), lotAssetURLs(lot)); err != nil {
			return err
		}
		event := auction.NewAuctionEvent(v1.AuctionEventType_AUCTION_EVENT_TYPE_LOT_QUEUED, lot)
		event.Reason = relistReason
		events = []v1.AuctionEvent{event}
		if err := createEventModels(ctx, tx, events); err != nil {
			return err
		}
		sourceReason := appendRelistMarker(sourceModel.CancelReason, newLotID)
		if err := tx.WithContext(ctx).
			Model(&AuctionLotModel{}).
			Where("id = ?", sourceLotID).
			Updates(map[string]any{
				"cancel_reason":      sourceReason,
				"updated_at_unix_ms": nowMs,
			}).Error; err != nil {
			return err
		}
		queuedLot = lot
		return nil
	}); err != nil {
		return nil, 0, nil, err
	}
	if err := s.streamEvents(ctx, events); err != nil {
		return nil, 0, nil, err
	}
	return queuedLot, queuePosition, events, nil
}

const relistMarkerPrefix = "已重新上架为 "

func relistEventReason(sourceLotID string) string {
	return "relisted from " + strings.TrimSpace(sourceLotID)
}

func lotRelistMarker(reason string) string {
	reason = strings.TrimSpace(reason)
	for _, part := range strings.Split(reason, "；") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, relistMarkerPrefix) {
			return strings.TrimSpace(strings.TrimPrefix(part, relistMarkerPrefix))
		}
	}
	return ""
}

func appendRelistMarker(reason, newLotID string) string {
	reason = strings.TrimSpace(reason)
	if marker := lotRelistMarker(reason); marker != "" {
		return reason
	}
	marker := relistMarkerPrefix + strings.TrimSpace(newLotID)
	if reason == "" {
		return marker
	}
	return reason + "；" + marker
}

func (s *Store) StartLotAsOnlyActive(ctx context.Context, lot *v1.Lot, expectedVersion int64, events []v1.AuctionEvent) error {
	if lot == nil {
		return errors.New("lot is required")
	}
	if expectedVersion <= 0 {
		return errors.New("lot expected version is required")
	}
	model, err := lotToModel(lot)
	if err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureRoomStateRecord(ctx, tx, lot.GetRoomId(), lot.GetMainAccountId(), lot.GetStartedAtUnixMs()); err != nil {
			return err
		}
		var state AuctionRoomStateModel
		if err := tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("room_id = ?", lot.GetRoomId()).
			First(&state).Error; err != nil {
			return err
		}
		if err := clearStaleOrRejectActiveLot(ctx, tx, &state, lot.GetId()); err != nil {
			return err
		}

		var current AuctionLotModel
		if err := tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", lot.GetId()).
			First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperr.ErrNotFound
			}
			return err
		}
		if current.MainAccountID != lot.GetMainAccountId() {
			return apperr.ErrPermissionDenied
		}
		if current.RoomID != lot.GetRoomId() {
			return fmtInvalidRoomState()
		}
		if current.Version != expectedVersion {
			return apperr.ErrLotVersionConflict
		}
		if current.Status != int32(v1.LotStatus_LOT_STATUS_DRAFT) && current.Status != int32(v1.LotStatus_LOT_STATUS_READY) && current.Status != int32(v1.LotStatus_LOT_STATUS_QUEUED) {
			return fmtInvalidLotStartStatus(v1.LotStatus(current.Status))
		}

		result := tx.WithContext(ctx).
			Model(&AuctionLotModel{}).
			Where("id = ? AND version = ?", lot.GetId(), expectedVersion).
			Select("*").
			Omit("created_at").
			Updates(model)
		if result.Error != nil {
			return mapActiveLotConflict(result.Error)
		}
		if result.RowsAffected == 0 {
			return apperr.ErrLotVersionConflict
		}
		if err := tx.WithContext(ctx).
			Model(&AuctionRoomStateModel{}).
			Where("room_id = ?", lot.GetRoomId()).
			Updates(map[string]any{
				"main_account_id":    lot.GetMainAccountId(),
				"active_lot_id":      lot.GetId(),
				"active_lot_version": lot.GetVersion(),
				"updated_at_unix_ms": lot.GetStartedAtUnixMs(),
			}).Error; err != nil {
			return err
		}
		lot.UpdatedAtUnixMs = time.Now().UnixMilli()
		return createEventModels(ctx, tx, events)
	}); err != nil {
		return err
	}
	return s.streamEvents(ctx, events)
}

func (s *Store) FindOrCreateRoomState(ctx context.Context, roomID, mainAccountID string, nowMs int64) (*auction.RoomState, error) {
	if err := ensureRoomStateRecord(ctx, s.db, roomID, mainAccountID, nowMs); err != nil {
		return nil, err
	}
	var model AuctionRoomStateModel
	if err := s.db.WithContext(ctx).Where("room_id = ?", strings.TrimSpace(roomID)).First(&model).Error; err != nil {
		return nil, err
	}
	return roomStateFromModel(&model), nil
}

func (s *Store) RepairRoomActiveLot(ctx context.Context, roomID, activeLotID string, nowMs int64) error {
	roomID = strings.TrimSpace(roomID)
	activeLotID = strings.TrimSpace(activeLotID)
	if roomID == "" || activeLotID == "" {
		return nil
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	return s.db.WithContext(ctx).
		Model(&AuctionRoomStateModel{}).
		Where("room_id = ? AND active_lot_id = ?", roomID, activeLotID).
		Updates(map[string]any{
			"active_lot_id":      "",
			"active_lot_version": int64(0),
			"updated_at_unix_ms": nowMs,
		}).Error
}

func ensureRoomStateRecord(ctx context.Context, db *gorm.DB, roomID, mainAccountID string, nowMs int64) error {
	roomID = strings.TrimSpace(roomID)
	mainAccountID = strings.TrimSpace(mainAccountID)
	if roomID == "" {
		return errors.New("room id is required")
	}
	if mainAccountID == "" {
		return errors.New("main account id is required")
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	return db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&AuctionRoomStateModel{
		RoomID:            roomID,
		MainAccountID:     mainAccountID,
		ActiveLotID:       "",
		ActiveLotVersion:  0,
		NextQueuePosition: 1,
		UpdatedAtUnixMs:   nowMs,
	}).Error
}

func clearStaleOrRejectActiveLot(ctx context.Context, tx *gorm.DB, state *AuctionRoomStateModel, startingLotID string) error {
	if state == nil || strings.TrimSpace(state.ActiveLotID) == "" {
		return nil
	}
	if state.ActiveLotID == startingLotID {
		return apperr.ErrRoomActiveLotExists
	}
	var active AuctionLotModel
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", state.ActiveLotID).
		First(&active).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	} else if auction.IsAuctionOpenStatus(v1.LotStatus(active.Status)) {
		return apperr.ErrRoomActiveLotExists
	}
	return tx.WithContext(ctx).
		Model(&AuctionRoomStateModel{}).
		Where("room_id = ? AND active_lot_id = ?", state.RoomID, state.ActiveLotID).
		Updates(map[string]any{
			"active_lot_id":      "",
			"active_lot_version": int64(0),
			"updated_at_unix_ms": time.Now().UnixMilli(),
		}).Error
}

func releaseActiveLotIfTerminal(ctx context.Context, tx *gorm.DB, lot *v1.Lot) error {
	if lot == nil || !isTerminalLotStatus(lot.GetStatus()) || lot.GetRoomId() == "" || lot.GetId() == "" {
		return nil
	}
	return tx.WithContext(ctx).
		Model(&AuctionRoomStateModel{}).
		Where("room_id = ? AND active_lot_id = ?", lot.GetRoomId(), lot.GetId()).
		Updates(map[string]any{
			"active_lot_id":      "",
			"active_lot_version": int64(0),
			"updated_at_unix_ms": time.Now().UnixMilli(),
		}).Error
}

func lockRoomStateForTerminalLot(ctx context.Context, tx *gorm.DB, lot *v1.Lot) error {
	if lot == nil || !isTerminalLotStatus(lot.GetStatus()) || lot.GetRoomId() == "" || lot.GetMainAccountId() == "" {
		return nil
	}
	if err := ensureRoomStateRecord(ctx, tx, lot.GetRoomId(), lot.GetMainAccountId(), time.Now().UnixMilli()); err != nil {
		return err
	}
	var state AuctionRoomStateModel
	return tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("room_id = ?", lot.GetRoomId()).
		First(&state).Error
}

func isTerminalLotStatus(status v1.LotStatus) bool {
	switch status {
	case v1.LotStatus_LOT_STATUS_SETTLED, v1.LotStatus_LOT_STATUS_CANCELLED, v1.LotStatus_LOT_STATUS_FAILED:
		return true
	default:
		return false
	}
}

func roomStateFromModel(model *AuctionRoomStateModel) *auction.RoomState {
	if model == nil {
		return nil
	}
	return &auction.RoomState{
		RoomID:            model.RoomID,
		MainAccountID:     model.MainAccountID,
		ActiveLotID:       model.ActiveLotID,
		ActiveLotVersion:  model.ActiveLotVersion,
		NextQueuePosition: model.NextQueuePosition,
		UpdatedAtUnixMs:   model.UpdatedAtUnixMs,
	}
}

func mapActiveLotConflict(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if strings.Contains(message, "uidx_one_active_lot_per_room") || strings.Contains(message, "active_room_key") {
		return apperr.ErrRoomActiveLotExists
	}
	return err
}

func mapQueuePositionConflict(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if strings.Contains(message, "uidx_one_queued_position_per_room") || strings.Contains(message, "queued_room_position_key") {
		return apperr.ErrQueuePositionConflict
	}
	return err
}

func fmtInvalidLotStartStatus(status v1.LotStatus) error {
	return fmt.Errorf("%w: only draft or queued lot can be started, current status: %s", apperr.ErrInvalidArgument, status)
}

func fmtInvalidRoomState() error {
	return fmt.Errorf("%w: lot room changed while starting", apperr.ErrInvalidArgument)
}
