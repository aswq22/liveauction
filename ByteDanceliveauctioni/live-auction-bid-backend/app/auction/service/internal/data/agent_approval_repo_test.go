package data

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/agentapproval"
	"live-auction-bid/backend/app/auction/service/internal/agentconversation"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

const agentTestDatabase = "live_auction_agent_test"

func TestFieldSourcesLegacyBackfillIsExplicitlyVersionGated(t *testing.T) {
	draft := agentconversation.CandidateDraft{}
	snapshot := []agentconversation.SnapshotFieldSource{{Field: "capPriceAmount", Source: "AI"}, {Field: "category", Source: "AI"}, {Field: "description", Source: "AI"}, {Field: "tags", Source: "AI"}, {Field: "depositAmount", Source: "AI"}}
	if !fieldSourcesMatch(`{}`, 0, draft, snapshot) {
		t.Fatal("historical provenance version 0 did not receive narrow optional-zero backfill")
	}
	if fieldSourcesMatch(`{}`, agentconversation.CurrentProvenanceVersion, draft, snapshot) {
		t.Fatal("current provenance received legacy optional-zero backfill")
	}
	if fieldSourcesMatch(`{}`, 99, draft, snapshot) {
		t.Fatal("unknown provenance version was accepted")
	}
}

func TestAgentApprovalMySQLAtomicAppendAndReplay(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	fixture := seedAgentApprovalFixture(t, db, "atomic", 5, true)
	repo := NewAgentApprovalRepository(db)

	got, err := repo.Apply(context.Background(), fixture.command)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got.Status != agentapproval.ExecutionApplied || len(got.CreatedLotIDs) != 1 {
		t.Fatalf("execution = %+v", got)
	}
	assertApprovalState(t, db, fixture, []int32{5, 6}, 7, 1, 2)
	var appliedSession AgentSessionModel
	if err := db.First(&appliedSession, "id = ?", fixture.sessionID).Error; err != nil {
		t.Fatal(err)
	}
	recovered, err := findRecoveredAgentExecution(context.Background(), db, appliedSession)
	if err != nil {
		t.Fatalf("applied recovery error = %v", err)
	}
	if recovered.ExecutionID != got.ID || recovered.SnapshotVersion != fixture.command.SnapshotVersion || len(recovered.QueuedLots) != 2 {
		t.Fatalf("recovered execution = %+v, want durable applied result", recovered)
	}
	for i, queued := range recovered.QueuedLots {
		if queued.CandidateID != fixture.command.OrderedCandidateIDs[i] || queued.QueuePosition != int32(5+i) {
			t.Fatalf("recovered queue mapping %d = %+v", i, queued)
		}
	}

	replay, err := repo.Apply(context.Background(), fixture.command)
	if err != nil || replay.ID != got.ID {
		t.Fatalf("replay = (%+v, %v), want execution %s", replay, err, got.ID)
	}
	assertApprovalState(t, db, fixture, []int32{5, 6}, 7, 1, 2)

	different := fixture.command
	different.ContentHash = strings.Repeat("f", 64)
	beforeConflict := captureApprovalCounts(t, db, fixture)
	if _, err := repo.Apply(context.Background(), different); !errors.Is(err, apperr.ErrConflict) {
		t.Fatalf("same key different payload error = %v, want conflict", err)
	}
	if afterConflict := captureApprovalCounts(t, db, fixture); beforeConflict != afterConflict {
		t.Fatalf("different replay mutated state: before=%+v after=%+v", beforeConflict, afterConflict)
	}
}

func TestAgentApprovalMySQLStoresNewLotsWithoutQueueMutation(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	fixture := seedSingleNewApprovalFixture(t, db, "inventory-only", 5)
	fixture.command.Target = agentapproval.ApplyTargetInventoryOnly
	repo := NewAgentApprovalRepository(db)

	got, err := repo.Apply(context.Background(), fixture.command)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got.Status != agentapproval.ExecutionApplied || got.Target != agentapproval.ApplyTargetInventoryOnly || len(got.CreatedLotIDs) != 1 || len(got.QueuePositions) != 0 {
		t.Fatalf("execution = %+v", got)
	}
	var lot AuctionLotModel
	if err := db.First(&lot, "id = ?", got.CreatedLotIDs[0]).Error; err != nil {
		t.Fatal(err)
	}
	if v1.LotStatus(lot.Status) != v1.LotStatus_LOT_STATUS_READY || v1.LotQueueStatus(lot.QueueStatus) != v1.LotQueueStatus_LOT_QUEUE_STATUS_NONE || lot.QueuePosition != 0 {
		t.Fatalf("inventory lot status=%d queue_status=%d queue_position=%d", lot.Status, lot.QueueStatus, lot.QueuePosition)
	}
	var state AuctionRoomStateModel
	if err := db.First(&state, "room_id = ?", fixture.roomID).Error; err != nil {
		t.Fatal(err)
	}
	if state.NextQueuePosition != 5 {
		t.Fatalf("next queue position = %d, want unchanged 5", state.NextQueuePosition)
	}
	var eventCount int64
	if err := db.Model(&AuctionEventModel{}).Count(&eventCount).Error; err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("auction event count = %d, want 0", eventCount)
	}

	var appliedSession AgentSessionModel
	if err := db.First(&appliedSession, "id = ?", fixture.sessionID).Error; err != nil {
		t.Fatal(err)
	}
	recovered, err := findRecoveredAgentExecution(context.Background(), db, appliedSession)
	if err != nil {
		t.Fatalf("recovery error = %v", err)
	}
	if recovered.Target != string(agentapproval.ApplyTargetInventoryOnly) || len(recovered.CreatedLotIDs) != 1 || len(recovered.QueuedLots) != 0 {
		t.Fatalf("recovered execution = %+v", recovered)
	}

	replay, err := repo.Apply(context.Background(), fixture.command)
	if err != nil || replay.ID != got.ID || replay.Target != agentapproval.ApplyTargetInventoryOnly || !replay.IdempotentReplay {
		t.Fatalf("replay = (%+v, %v), want inventory-only execution %s", replay, err, got.ID)
	}
	var createdCount int64
	if err := db.Model(&AuctionLotModel{}).Where("id = ?", got.CreatedLotIDs[0]).Count(&createdCount).Error; err != nil {
		t.Fatal(err)
	}
	if createdCount != 1 {
		t.Fatalf("created lot count after replay = %d, want 1", createdCount)
	}
}

func TestAgentApprovalMySQLAppliesRecoveredLegacyOptionalDefaultSources(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	fixture := seedSingleNewApprovalFixture(t, db, "legacy-sources", 5)

	var model AgentDraftCandidateModel
	if err := db.First(&model, "session_id = ? AND id = ?", fixture.sessionID, fixture.command.SelectedCandidateIDs[0]).Error; err != nil {
		t.Fatal(err)
	}
	var candidate agentconversation.Candidate
	if err := json.Unmarshal([]byte(model.DraftJSON), &candidate); err != nil {
		t.Fatal(err)
	}
	candidate.Draft.CapPriceAmount = nil
	candidate.Draft.Category = ""
	candidate.Draft.Description = ""
	candidate.Draft.Tags = nil
	candidate.Draft.DepositAmount = 0
	var sources map[string]string
	if err := json.Unmarshal([]byte(model.FieldSourcesJSON), &sources); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"capPriceAmount", "category", "description", "tags", "depositAmount"} {
		delete(sources, field)
	}
	draftJSON, _ := json.Marshal(candidate)
	sourcesJSON, _ := json.Marshal(sources)
	if err := db.Model(&AgentDraftCandidateModel{}).Where("id = ? AND session_id = ?", model.ID, fixture.sessionID).Updates(map[string]any{"draft_json": string(draftJSON), "field_sources_json": string(sourcesJSON)}).Error; err != nil {
		t.Fatal(err)
	}

	aggregate := agentconversation.SessionAggregate{Session: agentconversation.Session{ID: fixture.sessionID, TenantID: "tenant-1", OperatorID: "operator-1", RoomID: fixture.roomID}, Candidates: []agentconversation.DraftCandidate{{Candidate: candidate, FieldSourcesJSON: string(sourcesJSON), Version: model.Version}}}
	snapshot, err := agentconversation.BuildReviewSnapshot(aggregate, "operator-1", []string{model.ID}, []string{model.ID})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.SnapshotVersion = 1
	snapshot.CreatedBy = "operator-1"
	if err := agentconversation.ValidateReviewSnapshotIntegrity(snapshot); err != nil {
		t.Fatalf("rebuilt snapshot integrity = %v", err)
	}
	if err := db.Model(&AgentApprovalSnapshotModel{}).Where("session_id = ? AND snapshot_version = 1", fixture.sessionID).Updates(map[string]any{"snapshot_json": string(snapshot.SnapshotJSON), "content_hash": snapshot.ContentHash}).Error; err != nil {
		t.Fatal(err)
	}
	fixture.command.ContentHash = snapshot.ContentHash

	execution, err := NewAgentApprovalRepository(db).Apply(context.Background(), fixture.command)
	if err != nil {
		t.Fatalf("Apply() recovered legacy candidate error = %v", err)
	}
	if execution.Status != agentapproval.ExecutionApplied || len(execution.CreatedLotIDs) != 1 || len(execution.QueuePositions) != 1 || execution.QueuePositions[0] != 5 {
		t.Fatalf("execution = %+v", execution)
	}
	var lot AuctionLotModel
	if err := db.First(&lot, "id = ?", execution.CreatedLotIDs[0]).Error; err != nil {
		t.Fatal(err)
	}
	if v1.LotStatus(lot.Status) == v1.LotStatus_LOT_STATUS_LIVE || lot.QueuePosition != 5 {
		t.Fatalf("approved lot status=%d queue_position=%d", lot.Status, lot.QueuePosition)
	}
}

func TestAgentApprovalMySQLNthInvalidRollsBackEverything(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	fixture := seedAgentApprovalFixture(t, db, "rollback", 9, true)
	if err := db.Model(&AuctionLotModel{}).Where("id = ?", fixture.existingLotID).Update("version", gorm.Expr("version + 1")).Error; err != nil {
		t.Fatal(err)
	}

	before := captureApprovalCounts(t, db, fixture)
	if _, err := NewAgentApprovalRepository(db).Apply(context.Background(), fixture.command); !errors.Is(err, apperr.ErrAgentCandidateChanged) {
		t.Fatalf("Apply() error = %v, want candidate changed", err)
	}
	after := captureApprovalCounts(t, db, fixture)
	if before != after {
		t.Fatalf("rollback state changed: before=%+v after=%+v", before, after)
	}
}

func TestAgentApprovalMySQLLateOutboxFailureRollsBackEverything(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	fixture := seedAgentApprovalFixture(t, db, "late-failure", 12, true)
	before := captureApprovalCounts(t, db, fixture)
	if err := db.Exec("CREATE TRIGGER agent_test_fail_event BEFORE INSERT ON auction_events FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'agent test event failure'").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Exec("DROP TRIGGER IF EXISTS agent_test_fail_event").Error })

	if _, err := NewAgentApprovalRepository(db).Apply(context.Background(), fixture.command); !errors.Is(err, apperr.ErrAgentBatchApplyFailed) {
		t.Fatalf("Apply() error = %v, want safe batch failure", err)
	}
	after := captureApprovalCounts(t, db, fixture)
	if before != after {
		t.Fatalf("late rollback state changed: before=%+v after=%+v", before, after)
	}
}

func TestAgentApprovalMySQLConcurrentBatchesHaveUniqueContiguousPositions(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	first := seedAgentApprovalFixture(t, db, "concurrent-a", 20, false)
	second := seedAgentApprovalFixtureInRoom(t, db, "concurrent-b", first.roomID, 20, false)
	repo := NewAgentApprovalRepository(db)
	reached := make(chan uint64, 2)
	release := make(chan struct{})
	repo.beforeRoomStateLock = func(tx *gorm.DB) {
		var connectionID uint64
		if err := tx.Raw("SELECT CONNECTION_ID()").Scan(&connectionID).Error; err != nil {
			panic(err)
		}
		reached <- connectionID
		<-release
	}

	start := make(chan struct{})
	results := make(chan agentapproval.Execution, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, command := range []agentapproval.ApplyCommand{first.command, second.command} {
		command := command
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := repo.Apply(context.Background(), command)
			results <- result
			errs <- err
		}()
	}
	close(start)
	firstConnection := waitForApprovalConnection(t, reached)
	secondConnection := waitForApprovalConnection(t, reached)
	close(release)
	if firstConnection == secondConnection {
		t.Fatalf("concurrent transactions reused connection %d", firstConnection)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Apply() error = %v", err)
		}
	}
	var batches [][]int32
	for result := range results {
		positions := append([]int32(nil), result.QueuePositions...)
		if len(positions) != 2 || positions[1] != positions[0]+1 {
			t.Fatalf("batch positions = %v, want contiguous pair", positions)
		}
		batches = append(batches, positions)
	}
	all := append(append([]int32{}, batches[0]...), batches[1]...)
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	if want := []int32{20, 21, 22, 23}; !equalInt32s(all, want) {
		t.Fatalf("positions = %v, want %v", all, want)
	}
}

func TestAgentApprovalMySQLConcurrentSameKeyCreatesOneBatch(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	fixture := seedAgentApprovalFixture(t, db, "same-key", 30, false)
	repo := NewAgentApprovalRepository(db)
	start := make(chan struct{})
	ids := make(chan string, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := repo.Apply(context.Background(), fixture.command)
			ids <- result.ID
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent replay error = %v", err)
		}
	}
	var executionIDs []string
	for id := range ids {
		executionIDs = append(executionIDs, id)
	}
	if len(executionIDs) != 2 || executionIDs[0] == "" || executionIDs[0] != executionIDs[1] {
		t.Fatalf("execution IDs = %v", executionIDs)
	}
	assertApprovalState(t, db, fixture, []int32{30, 31}, 32, 1, 2)
}

func TestAgentApprovalMySQLCreatesStatsAndLateFailureRollsThemBack(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	fixture := seedAgentApprovalFixture(t, db, "stats", 40, false)
	result, err := NewAgentApprovalRepository(db).Apply(context.Background(), fixture.command)
	if err != nil || len(result.CreatedLotIDs) != 1 {
		t.Fatalf("Apply() = (%+v, %v)", result, err)
	}
	var stats AuctionLotStatsModel
	if err := db.First(&stats, "lot_id = ?", result.CreatedLotIDs[0]).Error; err != nil {
		t.Fatalf("new lot stats missing: %v", err)
	}
	if stats.MainAccountID != "tenant-1" || stats.RoomID != fixture.roomID || stats.BidCount != 0 || stats.ParticipantCount != 0 || stats.ProjectedVersion != 0 {
		t.Fatalf("new lot stats = %+v", stats)
	}

	resetAgentApprovalSchema(t, db)
	failed := seedAgentApprovalFixture(t, db, "stats-rollback", 50, false)
	before := captureApprovalCounts(t, db, failed)
	if err := db.Exec("CREATE TRIGGER agent_test_fail_event BEFORE INSERT ON auction_events FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'agent test event failure'").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := NewAgentApprovalRepository(db).Apply(context.Background(), failed.command); !errors.Is(err, apperr.ErrAgentBatchApplyFailed) {
		t.Fatalf("Apply() error = %v", err)
	}
	_ = db.Exec("DROP TRIGGER IF EXISTS agent_test_fail_event").Error
	if after := captureApprovalCounts(t, db, failed); before != after {
		t.Fatalf("stats rollback changed state: before=%+v after=%+v", before, after)
	}
}

func TestAgentApprovalMySQLQueueCounterBoundaryAndOverflow(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	boundary := seedSingleNewApprovalFixture(t, db, "boundary", math.MaxInt32-1)
	result, err := NewAgentApprovalRepository(db).Apply(context.Background(), boundary.command)
	if err != nil || !equalInt32s(result.QueuePositions, []int32{math.MaxInt32 - 1}) {
		t.Fatalf("boundary Apply() = (%+v, %v)", result, err)
	}
	var state AuctionRoomStateModel
	if err := db.First(&state, "room_id = ?", boundary.roomID).Error; err != nil || state.NextQueuePosition != math.MaxInt32 {
		t.Fatalf("boundary state = (%+v, %v)", state, err)
	}

	resetAgentApprovalSchema(t, db)
	overflow := seedAgentApprovalFixture(t, db, "overflow", math.MaxInt32-1, false)
	before := captureApprovalCounts(t, db, overflow)
	if _, err := NewAgentApprovalRepository(db).Apply(context.Background(), overflow.command); !errors.Is(err, apperr.ErrQueuePositionConflict) {
		t.Fatalf("overflow error = %v", err)
	}
	if after := captureApprovalCounts(t, db, overflow); before != after {
		t.Fatalf("overflow changed state: before=%+v after=%+v", before, after)
	}

	resetAgentApprovalSchema(t, db)
	manual := seedAgentApprovalFixture(t, db, "manual-overflow", math.MaxInt32, false)
	before = captureApprovalCounts(t, db, manual)
	store := &Store{db: db, eventStreamer: noopApprovalPublisher{}}
	if _, _, _, err := store.QueueLotAsNext(context.Background(), manual.existingLotID, "tenant-1", "operator-1", time.Now().UnixMilli()); !errors.Is(err, apperr.ErrQueuePositionConflict) {
		t.Fatalf("manual overflow error = %v", err)
	}
	if after := captureApprovalCounts(t, db, manual); before != after {
		t.Fatalf("manual overflow changed state: before=%+v after=%+v", before, after)
	}
}

func TestAgentApprovalMySQLSchemaParityAndIdempotentMigrate(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	if err := db.AutoMigrate(&AgentProposalExecutionModel{}, &AgentApprovalAuditModel{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&AgentProposalExecutionModel{}, &AgentApprovalAuditModel{}); err != nil {
		t.Fatalf("second migration failed: %v", err)
	}
	var columns string
	if err := db.Raw("SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'agent_proposal_executions' AND index_name = 'uk_agent_execution_idempotency'").Scan(&columns).Error; err != nil {
		t.Fatal(err)
	}
	if columns != "session_id,snapshot_version,idempotency_key" {
		t.Fatalf("execution unique index columns = %q", columns)
	}
	var jsonColumns int64
	if err := db.Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'agent_proposal_executions' AND column_name IN ('selected_candidate_ids_json','ordered_candidate_ids_json','created_lot_ids_json','queue_positions_json') AND data_type = 'json'").Scan(&jsonColumns).Error; err != nil {
		t.Fatal(err)
	}
	if jsonColumns != 4 {
		t.Fatalf("execution JSON column parity = %d/4", jsonColumns)
	}
}

func TestAgentApprovalModelsDeclareExecutionIdentity(t *testing.T) {
	assertSchemaIndex(t, &AgentProposalExecutionModel{}, "uk_agent_execution_idempotency", "UNIQUE", "SessionID", "SnapshotVersion", "IdempotencyKey")
	assertSchemaIndex(t, &AgentApprovalAuditModel{}, "uk_agent_approval_audit_execution", "UNIQUE", "ExecutionID")
}

func TestAgentApprovalMySQLCrossWriterUsesRoomThenLotLockOrder(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	fixture := seedAgentApprovalFixture(t, db, "cross-writer", 60, false)
	release := make(chan struct{})
	reached := make(chan approvalWriterArrival, 2)
	repo := NewAgentApprovalRepository(db)
	repo.beforeRoomStateLock = func(tx *gorm.DB) { reached <- approvalWriterArrival{"agent", approvalConnectionID(tx)}; <-release }
	store := &Store{db: db, eventStreamer: noopApprovalPublisher{}}
	store.beforeQueueRoomStateLock = func(tx *gorm.DB) { reached <- approvalWriterArrival{"manual", approvalConnectionID(tx)}; <-release }

	agentErr := make(chan error, 1)
	manualErr := make(chan error, 1)
	go func() { _, err := repo.Apply(context.Background(), fixture.command); agentErr <- err }()
	go func() {
		_, _, _, err := store.QueueLotAsNext(context.Background(), fixture.existingLotID, "tenant-1", "operator-1", time.Now().UnixMilli())
		manualErr <- err
	}()
	firstWriter := waitForApprovalWriter(t, reached)
	secondWriter := waitForApprovalWriter(t, reached)
	seen := map[string]bool{firstWriter.writer: true, secondWriter.writer: true}
	close(release)
	if !seen["agent"] || !seen["manual"] {
		t.Fatalf("writers did not both reach room-first barrier: %v", seen)
	}
	if firstWriter.connectionID == secondWriter.connectionID {
		t.Fatalf("cross-writers reused MySQL connection %d", firstWriter.connectionID)
	}
	aErr := waitForApprovalError(t, agentErr)
	mErr := waitForApprovalError(t, manualErr)
	if mErr != nil {
		t.Fatalf("manual queue error = %v", mErr)
	}
	if aErr != nil && !errors.Is(aErr, apperr.ErrAgentCandidateChanged) {
		t.Fatalf("agent error = %v", aErr)
	}
	var executions, events, stats int64
	db.Model(&AgentProposalExecutionModel{}).Count(&executions)
	db.Model(&AuctionEventModel{}).Count(&events)
	db.Model(&AuctionLotStatsModel{}).Count(&stats)
	if aErr == nil {
		if executions != 1 || events != 2 || stats != 1 {
			t.Fatalf("agent win materialization executions=%d events=%d stats=%d", executions, events, stats)
		}
	} else if executions != 0 || events != 1 || stats != 0 {
		t.Fatalf("manual win materialization executions=%d events=%d stats=%d", executions, events, stats)
	}
	assertRoomQueueConsistency(t, db, fixture.roomID)
}

func TestAgentApprovalMySQLRejectionMatrixHasZeroWrites(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *gorm.DB, *approvalFixture)
		want   error
	}{
		{"session status", func(t *testing.T, db *gorm.DB, f *approvalFixture) {
			mustUpdate(t, db.Model(&AgentSessionModel{}).Where("id = ?", f.sessionID).Update("status", string(agentconversation.StatusCancelled)))
		}, apperr.ErrAgentReviewSnapshotStale},
		{"active snapshot", func(t *testing.T, db *gorm.DB, f *approvalFixture) {
			mustUpdate(t, db.Model(&AgentSessionModel{}).Where("id = ?", f.sessionID).Update("active_snapshot_version", 2))
		}, apperr.ErrAgentReviewSnapshotStale},
		{"snapshot hash", func(t *testing.T, db *gorm.DB, f *approvalFixture) {
			mustUpdate(t, db.Model(&AgentApprovalSnapshotModel{}).Where("session_id = ?", f.sessionID).Update("content_hash", strings.Repeat("b", 64)))
		}, apperr.ErrAgentReviewSnapshotStale},
		{"operator", func(t *testing.T, db *gorm.DB, f *approvalFixture) { f.command.OperatorID = "operator-2" }, apperr.ErrNotFound},
		{"session room", func(t *testing.T, db *gorm.DB, f *approvalFixture) {
			mustUpdate(t, db.Model(&AgentSessionModel{}).Where("id = ?", f.sessionID).Update("room_id", "room-other"))
		}, apperr.ErrAgentReviewSnapshotStale},
		{"room tenant", func(t *testing.T, db *gorm.DB, f *approvalFixture) {
			mustUpdate(t, db.Model(&AuctionRoomModel{}).Where("id = ?", f.roomID).Update("main_account_id", "tenant-2"))
		}, apperr.ErrPermissionDenied},
		{"candidate version", func(t *testing.T, db *gorm.DB, f *approvalFixture) {
			mustUpdate(t, db.Model(&AgentDraftCandidateModel{}).Where("id = ?", "candidate-new-reject-candidate-version").Update("version", 2))
		}, apperr.ErrAgentCandidateChanged},
		{"candidate content", func(t *testing.T, db *gorm.DB, f *approvalFixture) {
			mustUpdate(t, db.Model(&AgentDraftCandidateModel{}).Where("id = ?", "candidate-new-reject-candidate-content").Update("draft_json", `{}`))
		}, apperr.ErrAgentCandidateChanged},
		{"existing version", func(t *testing.T, db *gorm.DB, f *approvalFixture) {
			mustUpdate(t, db.Model(&AuctionLotModel{}).Where("id = ?", f.existingLotID).Update("version", 2))
		}, apperr.ErrAgentCandidateChanged},
		{"existing status", func(t *testing.T, db *gorm.DB, f *approvalFixture) {
			mustUpdate(t, db.Model(&AuctionLotModel{}).Where("id = ?", f.existingLotID).Update("status", int32(v1.LotStatus_LOT_STATUS_LIVE)))
		}, apperr.ErrAgentCandidateChanged},
		{"existing room", func(t *testing.T, db *gorm.DB, f *approvalFixture) {
			mustUpdate(t, db.Model(&AuctionLotModel{}).Where("id = ?", f.existingLotID).Update("room_id", "room-other"))
		}, apperr.ErrAgentCandidateChanged},
		{"asset status", func(t *testing.T, db *gorm.DB, f *approvalFixture) {
			mustUpdate(t, db.Model(&AssetFileModel{}).Where("id = ?", f.assetID).Update("status", AssetStatusDeleting))
		}, apperr.ErrAgentImageAssetInvalid},
		{"asset expiry", func(t *testing.T, db *gorm.DB, f *approvalFixture) {
			mustUpdate(t, db.Model(&AssetFileModel{}).Where("id = ?", f.assetID).Update("expires_at_unix_ms", time.Now().Add(-time.Minute).UnixMilli()))
		}, apperr.ErrAgentImageAssetInvalid},
		{"asset owner", func(t *testing.T, db *gorm.DB, f *approvalFixture) {
			mustUpdate(t, db.Model(&AssetFileModel{}).Where("id = ?", f.assetID).Update("owner_user_id", "operator-2"))
		}, apperr.ErrAgentImageAssetInvalid},
		{"asset binding", func(t *testing.T, db *gorm.DB, f *approvalFixture) {
			mustUpdate(t, db.Where("session_id = ?", f.sessionID).Delete(&AgentCandidateAssetModel{}))
		}, apperr.ErrAgentImageAssetInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openAgentApprovalMySQL(t)
			resetAgentApprovalSchema(t, db)
			suffix := "reject-" + strings.ReplaceAll(tt.name, " ", "-")
			fixture := seedAgentApprovalFixture(t, db, suffix, 70, false)
			tt.mutate(t, db, &fixture)
			before := captureApprovalCounts(t, db, fixture)
			_, err := NewAgentApprovalRepository(db).Apply(context.Background(), fixture.command)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Apply() error = %v, want %v", err, tt.want)
			}
			if after := captureApprovalCounts(t, db, fixture); before != after {
				t.Fatalf("rejection mutated state: before=%+v after=%+v", before, after)
			}
		})
	}
}

type noopApprovalPublisher struct{}

type approvalWriterArrival struct {
	writer       string
	connectionID uint64
}

func (noopApprovalPublisher) Publish(context.Context, v1.AuctionEvent) error { return nil }

func mustUpdate(t *testing.T, result *gorm.DB) {
	t.Helper()
	if result.Error != nil || result.RowsAffected != 1 {
		t.Fatalf("fixture mutation rows=%d error=%v", result.RowsAffected, result.Error)
	}
}

func approvalConnectionID(tx *gorm.DB) uint64 {
	var id uint64
	if err := tx.Raw("SELECT CONNECTION_ID()").Scan(&id).Error; err != nil {
		panic(err)
	}
	return id
}

type approvalFixture struct {
	command       agentapproval.ApplyCommand
	roomID        string
	sessionID     string
	existingLotID string
	priorLotID    string
	assetID       string
}

func openAgentApprovalMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("AUCTION_AGENT_TEST_MYSQL_DSN"))
	if dsn == "" {
		dsn = "root:auction_root@tcp(127.0.0.1:13306)/" + agentTestDatabase + "?parseTime=true&charset=utf8mb4&loc=Local"
	}
	if !strings.Contains(dsn, "/"+agentTestDatabase) {
		t.Fatalf("refusing destructive test setup outside %s", agentTestDatabase)
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open dedicated MySQL: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(12)
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("ping dedicated MySQL: %v", err)
	}
	return db
}

func resetAgentApprovalSchema(t *testing.T, db *gorm.DB) {
	t.Helper()
	var name string
	if err := db.Raw("SELECT DATABASE()").Scan(&name).Error; err != nil || name != agentTestDatabase {
		t.Fatalf("refusing schema reset for database %q: %v", name, err)
	}
	tables := []any{&AgentApprovalOutboxModel{}, &AgentApprovalAuditModel{}, &AgentProposalExecutionModel{}, &AuctionEventModel{}, &AuctionLotStatsModel{}, &AgentCandidateAssetModel{}, &AssetFileModel{}, &AuctionLotModel{}, &AuctionRoomStateModel{}, &AuctionRoomModel{}, &AgentApprovalSnapshotModel{}, &AgentDraftCandidateModel{}, &AgentSessionModel{}}
	for _, table := range tables {
		if err := db.Migrator().DropTable(table); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AutoMigrate(tables...); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DROP TRIGGER IF EXISTS agent_test_fail_event").Error; err != nil {
		t.Fatal(err)
	}
}

func seedAgentApprovalFixture(t *testing.T, db *gorm.DB, suffix string, next int32, includePreexisting bool) approvalFixture {
	roomID := "room-" + suffix
	if err := db.Create(&AuctionRoomModel{ID: roomID, MainAccountID: "tenant-1", Name: "Room", Status: "ACTIVE", CreatedByUserID: "operator-1", CreatedAtUnixMs: 1, UpdatedAtUnixMs: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&AuctionRoomStateModel{RoomID: roomID, MainAccountID: "tenant-1", NextQueuePosition: next, UpdatedAtUnixMs: 1}).Error; err != nil {
		t.Fatal(err)
	}
	priorLotID := ""
	if includePreexisting {
		priorLotID = "prior-" + suffix
		lot := approvalTestLot(priorLotID, roomID, next-1)
		if err := db.Create(mustLotModel(t, lot)).Error; err != nil {
			t.Fatal(err)
		}
	}
	fixture := seedAgentApprovalFixtureInRoom(t, db, suffix, roomID, next, true)
	fixture.priorLotID = priorLotID
	return fixture
}

func seedAgentApprovalFixtureInRoom(t *testing.T, db *gorm.DB, suffix, roomID string, next int32, ensureState bool) approvalFixture {
	t.Helper()
	if ensureState {
		var count int64
		if err := db.Model(&AuctionRoomStateModel{}).Where("room_id = ?", roomID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			if err := db.Create(&AuctionRoomStateModel{RoomID: roomID, MainAccountID: "tenant-1", NextQueuePosition: next, UpdatedAtUnixMs: 1}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	sessionID := "session-" + suffix
	existingID := "existing-" + suffix
	assetID := "asset-" + suffix
	newCandidate := snapshotCandidateForTest("candidate-new-"+suffix, agentconversation.SourceNew, "", nil, assetID)
	readVersion := int64(1)
	existingCandidate := snapshotCandidateForTest("candidate-existing-"+suffix, agentconversation.SourceExisting, existingID, &readVersion, "")
	ordered := []string{newCandidate.CandidateID, existingCandidate.CandidateID}
	selected := append([]string(nil), ordered...)
	sort.Strings(selected)
	snapshot := approvalSnapshotForTest(sessionID, roomID, selected, ordered, []agentconversation.SnapshotCandidate{newCandidate, existingCandidate})
	if err := db.Create(&AgentSessionModel{ID: sessionID, TenantID: "tenant-1", OperatorID: "operator-1", RoomID: roomID, Status: string(agentconversation.StatusReadyForReview), Version: 3, ActiveSnapshotVersion: 1, CreatedAtUnixMs: 1, UpdatedAtUnixMs: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&AgentApprovalSnapshotModel{SessionID: sessionID, SnapshotVersion: 1, SnapshotJSON: string(snapshot.SnapshotJSON), ContentHash: snapshot.ContentHash, CreatedBy: "operator-1", CreatedAtUnixMs: 2}).Error; err != nil {
		t.Fatal(err)
	}
	for i, candidate := range []agentconversation.SnapshotCandidate{newCandidate, existingCandidate} {
		mainImageURL := ""
		if candidate.SourceType == agentconversation.SourceExisting {
			mainImageURL = "https://example.com/existing.jpg"
		}
		draft := agentconversation.Candidate{ID: candidate.CandidateID, SourceType: candidate.SourceType, ExistingLotID: candidate.ExistingLotID, ExistingLotReadVersion: candidate.ExistingLotReadVersion, StartPriceSpecified: true, Draft: agentconversation.CandidateDraft{Title: candidate.Draft.Title, StartPriceAmount: candidate.Draft.StartPriceAmount, DurationSeconds: candidate.Draft.DurationSeconds, MainImageAssetID: candidate.Draft.MainImageAssetID, MinIncrementAmount: candidate.Draft.MinIncrementAmount, CapPriceAmount: candidate.Draft.CapPriceAmount, Category: candidate.Draft.Category, Description: candidate.Draft.Description, Tags: candidate.Draft.Tags, Stock: candidate.Draft.Stock, AntiSnipeWindowSeconds: candidate.Draft.AntiSnipeWindowSeconds, AntiSnipeExtendSeconds: candidate.Draft.AntiSnipeExtendSeconds, MaxExtendCount: candidate.Draft.MaxExtendCount, DepositAmount: candidate.Draft.DepositAmount}}
		draft.Draft.MainImageURL = mainImageURL
		encoded, _ := jsonMarshal(draft)
		if err := db.Create(&AgentDraftCandidateModel{ID: candidate.CandidateID, SessionID: sessionID, SourceType: string(candidate.SourceType), ExistingLotID: candidate.ExistingLotID, Position: int32(i + 1), SelectedByDefault: boolPointer(true), DraftJSON: encoded, FieldSourcesJSON: fieldSourcesJSONForTest(), ValidationJSON: `{}`, Version: 1, CreatedAtUnixMs: 1, UpdatedAtUnixMs: 1}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(mustLotModel(t, approvalTestLot(existingID, roomID, 0))).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if err := db.Create(&AssetFileModel{ID: assetID, MainAccountID: "tenant-1", OwnerUserID: "operator-1", RoomID: roomID, BizType: "agent_lot_image", Status: AssetStatusTemporary, StorageProvider: "local", Bucket: "uploads", ObjectKey: suffix + ".jpg", PublicURL: "https://example.com/" + suffix + ".jpg", MimeType: "image/jpeg", SizeBytes: 20, SHA256: strings.Repeat("a", 64), ExpiresAtUnixMs: now + 60000}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&AgentCandidateAssetModel{CandidateID: newCandidate.CandidateID, SessionID: sessionID, AssetID: assetID, TenantID: "tenant-1", OperatorID: "operator-1", CreatedAtUnixMs: 1, UpdatedAtUnixMs: 1}).Error; err != nil {
		t.Fatal(err)
	}
	return approvalFixture{command: agentapproval.ApplyCommand{TenantID: "tenant-1", OperatorID: "operator-1", SessionID: sessionID, SnapshotVersion: 1, ContentHash: snapshot.ContentHash, SelectedCandidateIDs: selected, OrderedCandidateIDs: ordered, IdempotencyKey: "idem-" + suffix}, roomID: roomID, sessionID: sessionID, existingLotID: existingID, assetID: assetID}
}

func seedSingleNewApprovalFixture(t *testing.T, db *gorm.DB, suffix string, next int32) approvalFixture {
	t.Helper()
	fixture := seedAgentApprovalFixture(t, db, suffix, next, false)
	var model AgentApprovalSnapshotModel
	if err := db.First(&model, "session_id = ? AND snapshot_version = 1", fixture.sessionID).Error; err != nil {
		t.Fatal(err)
	}
	var decoded agentconversation.ReviewSnapshot
	if err := json.Unmarshal([]byte(model.SnapshotJSON), &decoded); err != nil {
		t.Fatal(err)
	}
	newID := "candidate-new-" + suffix
	selected := []string{newID}
	var candidates []agentconversation.SnapshotCandidate
	for _, candidate := range decoded.Candidates {
		if candidate.CandidateID == newID {
			candidates = append(candidates, candidate)
		}
	}
	rebuilt := approvalSnapshotForTest(fixture.sessionID, fixture.roomID, selected, selected, candidates)
	if err := db.Model(&AgentApprovalSnapshotModel{}).Where("session_id = ? AND snapshot_version = 1", fixture.sessionID).Updates(map[string]any{"snapshot_json": string(rebuilt.SnapshotJSON), "content_hash": rebuilt.ContentHash}).Error; err != nil {
		t.Fatal(err)
	}
	fixture.command.SelectedCandidateIDs = selected
	fixture.command.OrderedCandidateIDs = selected
	fixture.command.ContentHash = rebuilt.ContentHash
	return fixture
}

func snapshotCandidateForTest(id string, source agentconversation.SourceType, lotID string, version *int64, assetID string) agentconversation.SnapshotCandidate {
	fields := []string{"antiSnipeExtendSeconds", "antiSnipeWindowSeconds", "capPriceAmount", "category", "depositAmount", "description", "durationSeconds", "mainImageAssetId", "maxExtendCount", "minIncrementAmount", "startPriceAmount", "stock", "tags", "title"}
	sources := make([]agentconversation.SnapshotFieldSource, 0, len(fields))
	for _, field := range fields {
		sources = append(sources, agentconversation.SnapshotFieldSource{Field: field, Source: "USER"})
	}
	return agentconversation.SnapshotCandidate{CandidateID: id, CandidateVersion: 1, SourceType: source, ExistingLotID: lotID, ExistingLotReadVersion: version, Draft: agentconversation.SnapshotCandidateDraft{Title: "Lot " + id, StartPriceAmount: 10000, DurationSeconds: 300, MainImageAssetID: assetID, MinIncrementAmount: 100, Category: "collectible", Description: "description", Tags: []string{"tag"}, Stock: 1, AntiSnipeWindowSeconds: 10, AntiSnipeExtendSeconds: 15, MaxExtendCount: 5}, FieldSources: sources, AssetIDs: func() []string {
		if assetID == "" {
			return []string{}
		}
		return []string{assetID}
	}()}
}

func approvalSnapshotForTest(sessionID, roomID string, selected, ordered []string, candidates []agentconversation.SnapshotCandidate) agentconversation.ReviewSnapshot {
	aggregate := agentconversation.SessionAggregate{Session: agentconversation.Session{ID: sessionID, TenantID: "tenant-1", OperatorID: "operator-1", RoomID: roomID}, Candidates: make([]agentconversation.DraftCandidate, 0, len(candidates))}
	for _, candidate := range candidates {
		mainImageURL := ""
		if candidate.SourceType == agentconversation.SourceExisting {
			mainImageURL = "https://example.com/existing.jpg"
		}
		sources := map[string]string{}
		for _, source := range candidate.FieldSources {
			sources[source.Field] = source.Source
		}
		sourceJSON, _ := jsonMarshal(sources)
		aggregate.Candidates = append(aggregate.Candidates, agentconversation.DraftCandidate{Candidate: agentconversation.Candidate{ID: candidate.CandidateID, SourceType: candidate.SourceType, ExistingLotID: candidate.ExistingLotID, ExistingLotReadVersion: candidate.ExistingLotReadVersion, StartPriceSpecified: true, Draft: agentconversation.CandidateDraft{Title: candidate.Draft.Title, StartPriceAmount: candidate.Draft.StartPriceAmount, DurationSeconds: candidate.Draft.DurationSeconds, MainImageAssetID: candidate.Draft.MainImageAssetID, MainImageURL: mainImageURL, MinIncrementAmount: candidate.Draft.MinIncrementAmount, CapPriceAmount: candidate.Draft.CapPriceAmount, Category: candidate.Draft.Category, Description: candidate.Draft.Description, Tags: candidate.Draft.Tags, Stock: candidate.Draft.Stock, AntiSnipeWindowSeconds: candidate.Draft.AntiSnipeWindowSeconds, AntiSnipeExtendSeconds: candidate.Draft.AntiSnipeExtendSeconds, MaxExtendCount: candidate.Draft.MaxExtendCount, DepositAmount: candidate.Draft.DepositAmount}}, FieldSourcesJSON: sourceJSON, Version: candidate.CandidateVersion})
	}
	snapshot, err := agentconversation.BuildReviewSnapshot(aggregate, "operator-1", selected, ordered)
	if err != nil {
		panic(err)
	}
	snapshot.SnapshotVersion = 1
	snapshot.CreatedBy = "operator-1"
	return snapshot
}

func approvalTestLot(id, roomID string, position int32) *v1.Lot {
	status := v1.LotStatus_LOT_STATUS_DRAFT
	queueStatus := v1.LotQueueStatus_LOT_QUEUE_STATUS_NONE
	if position > 0 {
		status, queueStatus = v1.LotStatus_LOT_STATUS_QUEUED, v1.LotQueueStatus_LOT_QUEUE_STATUS_QUEUED
	}
	return &v1.Lot{Id: id, MainAccountId: "tenant-1", RoomId: roomID, Title: "Existing " + id, Description: "description", ImageUrl: "https://example.com/existing.jpg", Status: status, QueueStatus: queueStatus, QueuePosition: position, Rule: &v1.BidRule{StartPrice: &v1.Money{Amount: 10000, Currency: "CNY"}, MinIncrement: &v1.Money{Amount: 100, Currency: "CNY"}, DurationSeconds: 300, AntiSnipeWindowSeconds: 10, AntiSnipeExtendSeconds: 15, MaxExtendCount: 5}, CurrentPrice: &v1.Money{Amount: 10000, Currency: "CNY"}, FinalPrice: &v1.Money{Currency: "CNY"}, Version: 1, Stock: 1}
}

func mustLotModel(t *testing.T, lot *v1.Lot) *AuctionLotModel {
	t.Helper()
	model, err := lotToModel(lot)
	if err != nil {
		t.Fatal(err)
	}
	return model
}
func jsonMarshal(value any) (string, error) {
	encoded, err := json.Marshal(value)
	return string(encoded), err
}
func fieldSourcesJSONForTest() string {
	sources := map[string]string{}
	for _, field := range []string{"antiSnipeExtendSeconds", "antiSnipeWindowSeconds", "capPriceAmount", "category", "depositAmount", "description", "durationSeconds", "mainImageAssetId", "maxExtendCount", "minIncrementAmount", "startPriceAmount", "stock", "tags", "title"} {
		sources[field] = "USER"
	}
	encoded, _ := jsonMarshal(sources)
	return encoded
}

type approvalCounts struct {
	Lots, Stats, Executions, Audits, Events, References int64
	Next, ExistingStatus, ExistingQueuePosition         int32
	AssetStatus, AttachedLot, AssetOwner                string
	SessionStatus, SessionRoom, RoomTenant              string
	CandidateDraft, SnapshotHash, ExistingRoom          string
	SessionVersion, SessionActiveSnapshot               int64
	CandidateVersion, ExistingVersion, AssetExpiry      int64
}

func captureApprovalCounts(t *testing.T, db *gorm.DB, fixture approvalFixture) approvalCounts {
	t.Helper()
	var c approvalCounts
	for table, target := range map[any]*int64{&AuctionLotModel{}: &c.Lots, &AuctionLotStatsModel{}: &c.Stats, &AgentProposalExecutionModel{}: &c.Executions, &AgentApprovalAuditModel{}: &c.Audits, &AuctionEventModel{}: &c.Events, &AgentCandidateAssetModel{}: &c.References} {
		if err := db.Model(table).Count(target).Error; err != nil {
			t.Fatal(err)
		}
	}
	var state AuctionRoomStateModel
	if err := db.First(&state, "room_id = ?", fixture.roomID).Error; err != nil {
		t.Fatal(err)
	}
	c.Next = state.NextQueuePosition
	var asset AssetFileModel
	if err := db.First(&asset, "id = ?", fixture.assetID).Error; err != nil {
		t.Fatal(err)
	}
	c.AssetStatus, c.AttachedLot, c.AssetOwner, c.AssetExpiry = asset.Status, asset.AttachedLotID, asset.OwnerUserID, asset.ExpiresAtUnixMs
	var session AgentSessionModel
	if err := db.First(&session, "id = ?", fixture.sessionID).Error; err != nil {
		t.Fatal(err)
	}
	c.SessionStatus, c.SessionRoom, c.SessionVersion, c.SessionActiveSnapshot = session.Status, session.RoomID, session.Version, session.ActiveSnapshotVersion
	var room AuctionRoomModel
	if err := db.First(&room, "id = ?", fixture.roomID).Error; err != nil {
		t.Fatal(err)
	}
	c.RoomTenant = room.MainAccountID
	var candidate AgentDraftCandidateModel
	if err := db.Where("session_id = ? AND source_type = ?", fixture.sessionID, string(agentconversation.SourceNew)).First(&candidate).Error; err != nil {
		t.Fatal(err)
	}
	c.CandidateVersion, c.CandidateDraft = candidate.Version, candidate.DraftJSON
	var snapshot AgentApprovalSnapshotModel
	if err := db.First(&snapshot, "session_id = ? AND snapshot_version = 1", fixture.sessionID).Error; err != nil {
		t.Fatal(err)
	}
	c.SnapshotHash = snapshot.ContentHash
	var existing AuctionLotModel
	if err := db.First(&existing, "id = ?", fixture.existingLotID).Error; err != nil {
		t.Fatal(err)
	}
	c.ExistingVersion, c.ExistingStatus, c.ExistingRoom, c.ExistingQueuePosition = existing.Version, existing.Status, existing.RoomID, existing.QueuePosition
	return c
}
func assertApprovalState(t *testing.T, db *gorm.DB, fixture approvalFixture, positions []int32, next int32, executions, events int64) {
	t.Helper()
	var lots []AuctionLotModel
	if err := db.Where("room_id = ? AND queue_position IN ?", fixture.roomID, positions).Order("queue_position").Find(&lots).Error; err != nil {
		t.Fatal(err)
	}
	if len(lots) != len(positions) {
		t.Fatalf("queued lots=%d want=%d", len(lots), len(positions))
	}
	for i := range lots {
		if lots[i].QueuePosition != positions[i] {
			t.Fatalf("positions got %v", lots)
		}
		if v1.LotStatus(lots[i].Status) != v1.LotStatus_LOT_STATUS_QUEUED || v1.LotQueueStatus(lots[i].QueueStatus) != v1.LotQueueStatus_LOT_QUEUE_STATUS_QUEUED || lots[i].StartedAtUnixMs != 0 || lots[i].EndsAtUnixMs != 0 {
			t.Fatalf("approval started instead of queued: %+v", lots[i])
		}
	}
	if fixture.priorLotID != "" {
		var prior AuctionLotModel
		if err := db.First(&prior, "id = ?", fixture.priorLotID).Error; err != nil {
			t.Fatal(err)
		}
		if prior.QueuePosition != positions[0]-1 || v1.LotQueueStatus(prior.QueueStatus) != v1.LotQueueStatus_LOT_QUEUE_STATUS_QUEUED {
			t.Fatalf("existing queue row reordered: %+v", prior)
		}
	}
	var state AuctionRoomStateModel
	db.First(&state, "room_id = ?", fixture.roomID)
	if state.NextQueuePosition != next {
		t.Fatalf("next=%d want=%d", state.NextQueuePosition, next)
	}
	var count int64
	db.Model(&AgentProposalExecutionModel{}).Count(&count)
	if count != executions {
		t.Fatalf("executions=%d want=%d", count, executions)
	}
	db.Model(&AuctionEventModel{}).Count(&count)
	if count != events {
		t.Fatalf("events=%d want=%d", count, events)
	}
	var startedEvents int64
	db.Model(&AuctionEventModel{}).Where("type = ?", int32(v1.AuctionEventType_AUCTION_EVENT_TYPE_LOT_STARTED)).Count(&startedEvents)
	if startedEvents != 0 {
		t.Fatalf("start events=%d", startedEvents)
	}
	db.Model(&AgentApprovalAuditModel{}).Count(&count)
	if count != executions {
		t.Fatalf("audits=%d want=%d", count, executions)
	}
	db.Model(&AuctionLotStatsModel{}).Count(&count)
	if count != executions {
		t.Fatalf("new-lot stats=%d want=%d", count, executions)
	}
	var asset AssetFileModel
	db.First(&asset, "id = ?", fixture.assetID)
	if asset.Status != "attached" || asset.AttachedLotID == "" {
		t.Fatalf("asset=%+v", asset)
	}
	var references int64
	db.Model(&AgentCandidateAssetModel{}).Count(&references)
	if references != 0 {
		t.Fatalf("candidate references=%d want=0", references)
	}
}
func equalInt32s(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func waitForApprovalConnection(t *testing.T, reached <-chan uint64) uint64 {
	t.Helper()
	select {
	case connectionID := <-reached:
		return connectionID
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for two transactions at room-state barrier")
		return 0
	}
}

func waitForApprovalWriter(t *testing.T, reached <-chan approvalWriterArrival) approvalWriterArrival {
	t.Helper()
	select {
	case writer := <-reached:
		return writer
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cross-writer room barrier")
		return approvalWriterArrival{}
	}
}

func waitForApprovalError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(8 * time.Second):
		t.Fatal("cross-writer operation deadlocked")
		return nil
	}
}

func assertRoomQueueConsistency(t *testing.T, db *gorm.DB, roomID string) {
	t.Helper()
	var queued []AuctionLotModel
	if err := db.Where("room_id = ? AND queue_status = ?", roomID, int32(v1.LotQueueStatus_LOT_QUEUE_STATUS_QUEUED)).Order("queue_position").Find(&queued).Error; err != nil {
		t.Fatal(err)
	}
	seen := map[int32]bool{}
	for _, lot := range queued {
		if lot.QueuePosition <= 0 || seen[lot.QueuePosition] {
			t.Fatalf("invalid/duplicate queued position: %+v", queued)
		}
		seen[lot.QueuePosition] = true
	}
	var state AuctionRoomStateModel
	if err := db.First(&state, "room_id = ?", roomID).Error; err != nil {
		t.Fatal(err)
	}
	for position := int32(60); position < state.NextQueuePosition; position++ {
		if !seen[position] {
			t.Fatalf("queue gap at %d: state=%+v lots=%+v", position, state, queued)
		}
	}
}
