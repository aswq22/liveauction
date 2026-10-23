package data

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"live-auction-bid/backend/app/auction/service/internal/agentconversation"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

func TestAgentReviewSnapshotRepositoryAppendsVersionsAndNeverMutatesEarlierSnapshot(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	prepareReviewSnapshotTables(t, db)
	repo := NewAgentConversationRepository(&Store{db: db})
	seedReviewSession(t, db, 5)

	if err := repo.AuthorizeReviewSnapshot(context.Background(), snapshotCommand(5, "first")); err != nil {
		t.Fatalf("legitimate current candidate preflight: %v", err)
	}
	first, err := repo.CreateReviewSnapshot(context.Background(), snapshotCommand(5, "first"))
	if err != nil {
		t.Fatal(err)
	}
	secondCommand := snapshotCommand(6, "first")
	second, err := repo.CreateReviewSnapshot(context.Background(), secondCommand)
	if err != nil {
		t.Fatal(err)
	}
	if first.SnapshotVersion != 1 || second.SnapshotVersion != 2 {
		t.Fatalf("versions = %d, %d", first.SnapshotVersion, second.SnapshotVersion)
	}
	var stored []AgentApprovalSnapshotModel
	if err := db.Order("snapshot_version").Find(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].ContentHash != first.ContentHash || stored[0].SnapshotJSON != string(first.SnapshotJSON) || stored[1].ContentHash != second.ContentHash {
		t.Fatalf("append-only rows = %#v", stored)
	}
	var session AgentSessionModel
	if err := db.First(&session, "id = ?", "session-review").Error; err != nil {
		t.Fatal(err)
	}
	if session.Version != 7 || session.ActiveSnapshotVersion != 2 || session.Status != string(agentconversation.StatusReadyForReview) {
		t.Fatalf("session after snapshots = %#v", session)
	}
}

func TestAgentReviewSnapshotRepositoryCASAndLateFailureRollbackAtomically(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	prepareReviewSnapshotTables(t, db)
	repo := NewAgentConversationRepository(&Store{db: db})
	seedReviewSession(t, db, 5)

	for _, bad := range []agentconversation.CreateReviewSnapshotCommand{
		func() agentconversation.CreateReviewSnapshotCommand {
			c := snapshotCommand(5, "hash")
			c.Snapshot.ContentHash = "forged"
			return c
		}(),
		func() agentconversation.CreateReviewSnapshotCommand {
			c := snapshotCommand(5, "identity")
			c.Snapshot.OperatorID = "operator-b"
			return c
		}(),
	} {
		if _, err := repo.CreateReviewSnapshot(context.Background(), bad); !errors.Is(err, apperr.ErrInvalidArgument) {
			t.Fatalf("invalid snapshot boundary error = %v", err)
		}
	}
	if _, err := repo.CreateReviewSnapshot(context.Background(), snapshotCommand(4, "stale")); !errors.Is(err, apperr.ErrAgentSessionVersionConflict) {
		t.Fatalf("stale error = %v", err)
	}
	var count int64
	if err := db.Model(&AgentApprovalSnapshotModel{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("snapshot count after stale = %d err=%v", count, err)
	}
	if err := db.Exec(`CREATE TRIGGER fail_review_session_update BEFORE UPDATE ON agent_sessions BEGIN SELECT RAISE(ABORT, 'late failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateReviewSnapshot(context.Background(), snapshotCommand(5, "rollback")); err == nil {
		t.Fatal("expected injected late failure")
	}
	if err := db.Model(&AgentApprovalSnapshotModel{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("snapshot count after rollback = %d err=%v", count, err)
	}
}

func TestAgentReviewSnapshotRepositoryReauthorizesSelectedNewCandidateAsset(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*gorm.DB)
	}{
		{"missing asset", func(db *gorm.DB) { db.Delete(&AssetFileModel{}, "id = ?", "asset-1") }},
		{"wrong tenant", func(db *gorm.DB) {
			db.Model(&AssetFileModel{}).Where("id = ?", "asset-1").Update("main_account_id", "tenant-b")
		}},
		{"wrong owner", func(db *gorm.DB) {
			db.Model(&AssetFileModel{}).Where("id = ?", "asset-1").Update("owner_user_id", "operator-b")
		}},
		{"wrong biz type", func(db *gorm.DB) {
			db.Model(&AssetFileModel{}).Where("id = ?", "asset-1").Update("biz_type", "lot_image")
		}},
		{"wrong status", func(db *gorm.DB) {
			db.Model(&AssetFileModel{}).Where("id = ?", "asset-1").Update("status", AssetStatusAttached)
		}},
		{"expired", func(db *gorm.DB) {
			db.Model(&AssetFileModel{}).Where("id = ?", "asset-1").Update("expires_at_unix_ms", time.Now().Add(-time.Minute).UnixMilli())
		}},
		{"blank room", func(db *gorm.DB) { db.Model(&AssetFileModel{}).Where("id = ?", "asset-1").Update("room_id", "") }},
		{"wrong room", func(db *gorm.DB) { db.Model(&AssetFileModel{}).Where("id = ?", "asset-1").Update("room_id", "room-b") }},
		{"wrong reference session", func(db *gorm.DB) {
			db.Model(&AgentCandidateAssetModel{}).Where("candidate_id = ?", "candidate-1").Update("session_id", "session-other")
		}},
		{"wrong reference candidate", func(db *gorm.DB) {
			db.Model(&AgentCandidateAssetModel{}).Where("candidate_id = ?", "candidate-1").Update("candidate_id", "candidate-other")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := newAgentConversationSQLiteDB(t)
			prepareReviewSnapshotTables(t, db)
			seedReviewSession(t, db, 5)
			tt.mutate(db)
			repo := NewAgentConversationRepository(&Store{db: db})
			if err := repo.AuthorizeReviewSnapshot(context.Background(), snapshotCommand(5, "first")); !errors.Is(err, apperr.ErrAgentImageAssetInvalid) {
				t.Fatalf("service-layer asset preflight error=%v", err)
			}
			if _, err := repo.CreateReviewSnapshot(context.Background(), snapshotCommand(5, "first")); !errors.Is(err, apperr.ErrAgentImageAssetInvalid) {
				t.Fatalf("transaction asset authorization error=%v", err)
			}
			assertReviewSnapshotZeroWrite(t, db, 5)
		})
	}
}

func TestAgentReviewSnapshotRepositoryRejectsForeignRoomFabricatedAndStaleCandidates(t *testing.T) {
	for _, tt := range []struct {
		name    string
		command func() agentconversation.CreateReviewSnapshotCommand
	}{
		{"foreign room", func() agentconversation.CreateReviewSnapshotCommand {
			return snapshotCommandFor(5, "candidate-1", "first", 1, "room-b")
		}},
		{"fabricated candidate", func() agentconversation.CreateReviewSnapshotCommand {
			return snapshotCommandFor(5, "candidate-fake", "first", 1, "room-1")
		}},
		{"stale version", func() agentconversation.CreateReviewSnapshotCommand {
			return snapshotCommandFor(5, "candidate-1", "first", 2, "room-1")
		}},
		{"stale content", func() agentconversation.CreateReviewSnapshotCommand {
			return snapshotCommandFor(5, "candidate-1", "changed", 1, "room-1")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := newAgentConversationSQLiteDB(t)
			prepareReviewSnapshotTables(t, db)
			seedReviewSession(t, db, 5)
			repo := NewAgentConversationRepository(&Store{db: db})
			if _, err := repo.CreateReviewSnapshot(context.Background(), tt.command()); err == nil {
				t.Fatal("foreign/fabricated/stale snapshot accepted")
			}
			assertReviewSnapshotZeroWrite(t, db, 5)
		})
	}
}

func TestAgentReviewSnapshotRepositorySequentialCASVersionAllocation(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	prepareReviewSnapshotTables(t, db)
	seedReviewSession(t, db, 5)
	repo := NewAgentConversationRepository(&Store{db: db})
	if _, err := repo.CreateReviewSnapshot(context.Background(), snapshotCommand(5, "first")); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateReviewSnapshot(context.Background(), snapshotCommand(5, "first")); !errors.Is(err, apperr.ErrAgentSessionVersionConflict) {
		t.Fatalf("stale sequential CAS error=%v", err)
	}
	var rows []AgentApprovalSnapshotModel
	if err := db.Find(&rows).Error; err != nil || len(rows) != 1 || rows[0].SnapshotVersion != 1 {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
}

func TestAgentReviewSnapshotRepositoryLoadsLegacyDisplayProvenanceButRejectsOtherUnknownKeys(t *testing.T) {
	for _, tt := range []struct {
		name       string
		legacyKey  string
		wantAccept bool
	}{
		{"legacy main image URL", "mainImageUrl", true},
		{"unknown", "providerSecret", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := newAgentConversationSQLiteDB(t)
			prepareReviewSnapshotTables(t, db)
			seedReviewSession(t, db, 5)
			legacy := strings.TrimSuffix(reviewSnapshotSources("USER"), "}") + `,"` + tt.legacyKey + `":"USER"}`
			if err := db.Model(&AgentDraftCandidateModel{}).Where("id = ?", "candidate-1").Update("field_sources_json", legacy).Error; err != nil {
				t.Fatal(err)
			}
			repo := NewAgentConversationRepository(&Store{db: db})
			_, err := repo.CreateReviewSnapshot(context.Background(), snapshotCommand(5, "first"))
			if tt.wantAccept && err != nil {
				t.Fatalf("legacy persisted candidate rejected: %v", err)
			}
			if !tt.wantAccept && !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Fatalf("unknown persisted provenance error=%v", err)
			}
		})
	}
}

func TestAgentReviewSnapshotRepositoryExistingCandidateUsesLotVersionWithoutDraftAsset(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	prepareReviewSnapshotTables(t, db)
	command, candidate := existingSnapshotCommand(5)
	if err := db.Create(&AgentSessionModel{ID: "session-review", TenantID: "tenant-a", OperatorID: "operator-a", RoomID: "room-1", Status: string(agentconversation.StatusReadyForReview), Version: 5}).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&AgentDraftCandidateModel{ID: candidate.ID, SessionID: "session-review", SourceType: string(agentconversation.SourceExisting), ExistingLotID: candidate.ExistingLotID, Position: 1, SelectedByDefault: testBoolPointer(true), DraftJSON: string(raw), FieldSourcesJSON: reviewSnapshotSources("EXISTING"), ValidationJSON: `{}`, Version: 1}).Error; err != nil {
		t.Fatal(err)
	}
	repo := NewAgentConversationRepository(&Store{db: db})
	if err := repo.AuthorizeReviewSnapshot(context.Background(), command); err != nil {
		t.Fatalf("existing preflight: %v", err)
	}
	snapshot, err := repo.CreateReviewSnapshot(context.Background(), command)
	if err != nil || len(snapshot.Candidates) != 1 || snapshot.Candidates[0].ExistingLotID != "lot-1" || snapshot.Candidates[0].ExistingLotReadVersion == nil || *snapshot.Candidates[0].ExistingLotReadVersion != 9 {
		t.Fatalf("existing snapshot=%#v err=%v", snapshot, err)
	}
}

func TestAgentApprovalSnapshotModelDeclaresAppendOnlyUniqueKey(t *testing.T) {
	assertSchemaIndex(t, &AgentApprovalSnapshotModel{}, "uidx_agent_approval_snapshot_version", "UNIQUE", "SessionID", "SnapshotVersion")
}

func seedReviewSession(t *testing.T, db *gorm.DB, version int64) {
	t.Helper()
	if err := db.Create(&AgentSessionModel{ID: "session-review", TenantID: "tenant-a", OperatorID: "operator-a", RoomID: "room-1", Status: string(agentconversation.StatusReadyForReview), Version: version}).Error; err != nil {
		t.Fatal(err)
	}
	candidate := reviewSnapshotCandidate("candidate-1", "first")
	raw, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&AgentDraftCandidateModel{ID: "candidate-1", SessionID: "session-review", SourceType: string(agentconversation.SourceNew), Position: 1, SelectedByDefault: testBoolPointer(true), DraftJSON: string(raw), FieldSourcesJSON: reviewSnapshotSources("USER"), ValidationJSON: `{}`, Version: 1}).Error; err != nil {
		t.Fatal(err)
	}
	seedAgentImageAsset(t, db, "asset-1", "tenant-a", "operator-a", "agent_lot_image", AssetStatusTemporary, time.Now().Add(time.Hour).UnixMilli())
	if err := db.Model(&AssetFileModel{}).Where("id = ?", "asset-1").Update("room_id", "room-1").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&AgentCandidateAssetModel{CandidateID: "candidate-1", SessionID: "session-review", AssetID: "asset-1", TenantID: "tenant-a", OperatorID: "operator-a"}).Error; err != nil {
		t.Fatal(err)
	}
}

func snapshotCommand(expected int64, title string) agentconversation.CreateReviewSnapshotCommand {
	return snapshotCommandFor(expected, "candidate-1", title, 1, "room-1")
}

func snapshotCommandFor(expected int64, candidateID, title string, candidateVersion int64, roomID string) agentconversation.CreateReviewSnapshotCommand {
	candidate := reviewSnapshotCandidate(candidateID, title)
	aggregate := agentconversation.SessionAggregate{Session: agentconversation.Session{ID: "session-review", TenantID: "tenant-a", OperatorID: "operator-a", RoomID: roomID, Status: agentconversation.StatusReadyForReview, Version: expected}, Candidates: []agentconversation.DraftCandidate{{Candidate: candidate, Position: 1, Version: candidateVersion, FieldSourcesJSON: reviewSnapshotSources("USER")}}}
	snapshot, err := agentconversation.BuildReviewSnapshot(aggregate, "operator-a", []string{candidateID}, []string{candidateID})
	if err != nil {
		panic(err)
	}
	snapshot.CreatedAtUnixMs = 10
	return agentconversation.CreateReviewSnapshotCommand{
		TenantID: "tenant-a", OperatorID: "operator-a", SessionID: "session-review", ExpectedSessionVersion: expected,
		Snapshot: snapshot,
	}
}

func reviewSnapshotCandidate(id, title string) agentconversation.Candidate {
	return agentconversation.Candidate{ID: id, SourceType: agentconversation.SourceNew, StartPriceSpecified: true, Draft: agentconversation.CandidateDraft{Title: title, StartPriceAmount: 100, DurationSeconds: 120, MainImageAssetID: "asset-1", MinIncrementAmount: 10, AntiSnipeWindowSeconds: 10, AntiSnipeExtendSeconds: 15, MaxExtendCount: 5}}
}

func existingSnapshotCommand(expected int64) (agentconversation.CreateReviewSnapshotCommand, agentconversation.Candidate) {
	lotVersion := int64(9)
	candidate := agentconversation.Candidate{ID: "candidate-existing", SourceType: agentconversation.SourceExisting, ExistingLotID: "lot-1", ExistingLotReadVersion: &lotVersion, StartPriceSpecified: true, Draft: agentconversation.CandidateDraft{Title: "existing", StartPriceAmount: 100, DurationSeconds: 120, MainImageURL: "https://lot.example/image.jpg", MinIncrementAmount: 10, AntiSnipeWindowSeconds: 10, AntiSnipeExtendSeconds: 15, MaxExtendCount: 5}}
	aggregate := agentconversation.SessionAggregate{Session: agentconversation.Session{ID: "session-review", TenantID: "tenant-a", OperatorID: "operator-a", RoomID: "room-1", Status: agentconversation.StatusReadyForReview, Version: expected}, Candidates: []agentconversation.DraftCandidate{{Candidate: candidate, Position: 1, Version: 1, FieldSourcesJSON: reviewSnapshotSources("EXISTING")}}}
	snapshot, err := agentconversation.BuildReviewSnapshot(aggregate, "operator-a", []string{candidate.ID}, []string{candidate.ID})
	if err != nil {
		panic(err)
	}
	snapshot.CreatedAtUnixMs = 10
	return agentconversation.CreateReviewSnapshotCommand{TenantID: "tenant-a", OperatorID: "operator-a", SessionID: "session-review", ExpectedSessionVersion: expected, Snapshot: snapshot}, candidate
}

func reviewSnapshotSources(source string) string {
	fields := []string{"antiSnipeExtendSeconds", "antiSnipeWindowSeconds", "capPriceAmount", "category", "depositAmount", "description", "durationSeconds", "mainImageAssetId", "maxExtendCount", "minIncrementAmount", "startPriceAmount", "stock", "tags", "title"}
	values := make([]string, 0, len(fields))
	for _, field := range fields {
		values = append(values, `"`+field+`":"`+source+`"`)
	}
	return "{" + strings.Join(values, ",") + "}"
}

func prepareReviewSnapshotTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.AutoMigrate(&AgentApprovalSnapshotModel{}, &AssetFileModel{}, &AgentCandidateAssetModel{}); err != nil {
		t.Fatal(err)
	}
}

func assertReviewSnapshotZeroWrite(t *testing.T, db *gorm.DB, wantVersion int64) {
	t.Helper()
	var count int64
	if err := db.Model(&AgentApprovalSnapshotModel{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("snapshot count=%d err=%v", count, err)
	}
	var session AgentSessionModel
	if err := db.First(&session, "id = ?", "session-review").Error; err != nil || session.Version != wantVersion || session.ActiveSnapshotVersion != 0 {
		t.Fatalf("session=%#v err=%v", session, err)
	}
}
