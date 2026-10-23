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
	"live-auction-bid/backend/app/auction/service/internal/storage"
)

func TestAgentCandidateImageBindingIsAtomicAndReplacesOnlyItsReference(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	if err := db.AutoMigrate(&AssetFileModel{}, &AgentCandidateAssetModel{}); err != nil {
		t.Fatal(err)
	}
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	seedImageBindingSession(t, db, "tenant-a", "operator-a", "session-a", "candidate-a", 3, 5)
	seedAgentImageAsset(t, db, "asset-old", "tenant-a", "operator-a", "agent_lot_image", AssetStatusTemporary, time.Now().Add(time.Hour).UnixMilli())
	seedAgentImageAsset(t, db, "asset-new", "tenant-a", "operator-a", "agent_lot_image", AssetStatusTemporary, time.Now().Add(time.Hour).UnixMilli())

	first, err := repo.BindCandidateImage(ctx, agentconversation.BindCandidateImageCommand{TenantID: "tenant-a", OperatorID: "operator-a", SessionID: "session-a", CandidateID: "candidate-a", AssetID: "asset-old", ExpectedSessionVersion: 3, ExpectedCandidateVersion: 5, UpdatedAtUnixMs: 1000})
	if err != nil {
		t.Fatalf("first BindCandidateImage() error = %v", err)
	}
	if first.Session.Version != 4 || first.Session.Status != agentconversation.StatusReadyForReview || first.Session.ActiveSnapshotVersion != 0 || first.Candidates[0].Version != 6 || first.Candidates[0].Candidate.Draft.MainImageAssetID != "asset-old" || first.Candidates[0].Candidate.Draft.MainImageURL != "https://assets.example/asset-old.jpg" {
		t.Fatalf("first binding aggregate = %#v", first)
	}
	assertAgentImageUserProvenance(t, first.Candidates[0].FieldSourcesJSON)

	second, err := repo.BindCandidateImage(ctx, agentconversation.BindCandidateImageCommand{TenantID: "tenant-a", OperatorID: "operator-a", SessionID: "session-a", CandidateID: "candidate-a", AssetID: "asset-new", ExpectedSessionVersion: 4, ExpectedCandidateVersion: 6, UpdatedAtUnixMs: 2000})
	if err != nil {
		t.Fatalf("replacement BindCandidateImage() error = %v", err)
	}
	if second.Candidates[0].Candidate.Draft.MainImageAssetID != "asset-new" || second.Candidates[0].Version != 7 || second.Session.Version != 5 {
		t.Fatalf("replacement aggregate = %#v", second)
	}
	var refs []AgentCandidateAssetModel
	if err := db.Find(&refs).Error; err != nil || len(refs) != 1 || refs[0].AssetID != "asset-new" || refs[0].CandidateID != "candidate-a" {
		t.Fatalf("references = %#v, err=%v", refs, err)
	}
	var assets []AssetFileModel
	if err := db.Order("id").Find(&assets).Error; err != nil || len(assets) != 2 || assets[0].Status != AssetStatusTemporary || assets[1].Status != AssetStatusTemporary {
		t.Fatalf("asset rows changed/deleted: %#v, err=%v", assets, err)
	}
}

func TestAgentCandidateImageRejectsInvalidAssetAndRollsBackCAS(t *testing.T) {
	tests := []struct {
		name, tenant, operator, bizType, status string
		expires                                 int64
		want                                    error
	}{
		{name: "not temporary", tenant: "tenant-a", operator: "operator-a", bizType: "agent_lot_image", status: AssetStatusAttached, expires: time.Now().Add(time.Hour).UnixMilli(), want: apperr.ErrAgentImageAssetInvalid},
		{name: "wrong biz type", tenant: "tenant-a", operator: "operator-a", bizType: "lot_image", status: AssetStatusTemporary, expires: time.Now().Add(time.Hour).UnixMilli(), want: apperr.ErrAgentImageAssetInvalid},
		{name: "expired", tenant: "tenant-a", operator: "operator-a", bizType: "agent_lot_image", status: AssetStatusTemporary, expires: time.Now().Add(-time.Hour).UnixMilli(), want: apperr.ErrAgentImageAssetInvalid},
		{name: "cross tenant", tenant: "tenant-b", operator: "operator-a", bizType: "agent_lot_image", status: AssetStatusTemporary, expires: time.Now().Add(time.Hour).UnixMilli(), want: apperr.ErrPermissionDenied},
		{name: "wrong owner", tenant: "tenant-a", operator: "operator-b", bizType: "agent_lot_image", status: AssetStatusTemporary, expires: time.Now().Add(time.Hour).UnixMilli(), want: apperr.ErrPermissionDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newAgentConversationSQLiteDB(t)
			if err := db.AutoMigrate(&AssetFileModel{}, &AgentCandidateAssetModel{}); err != nil {
				t.Fatal(err)
			}
			repo := NewAgentConversationRepository(&Store{db: db})
			seedImageBindingSession(t, db, "tenant-a", "operator-a", "session-a", "candidate-a", 3, 5)
			seedAgentImageAsset(t, db, "asset-a", tt.tenant, tt.operator, tt.bizType, tt.status, tt.expires)
			_, err := repo.BindCandidateImage(context.Background(), agentconversation.BindCandidateImageCommand{TenantID: "tenant-a", OperatorID: "operator-a", SessionID: "session-a", CandidateID: "candidate-a", AssetID: "asset-a", ExpectedSessionVersion: 3, ExpectedCandidateVersion: 5})
			if !errors.Is(err, tt.want) {
				t.Fatalf("error=%v, want %v", err, tt.want)
			}
			assertUnchangedCandidateBinding(t, db)
		})
	}

	db := newAgentConversationSQLiteDB(t)
	if err := db.AutoMigrate(&AssetFileModel{}, &AgentCandidateAssetModel{}); err != nil {
		t.Fatal(err)
	}
	repo := NewAgentConversationRepository(&Store{db: db})
	seedImageBindingSession(t, db, "tenant-a", "operator-a", "session-a", "candidate-a", 3, 5)
	seedImageBindingSession(t, db, "tenant-a", "operator-a", "session-b", "candidate-b", 1, 1)
	seedAgentImageAsset(t, db, "asset-shared", "tenant-a", "operator-a", "agent_lot_image", AssetStatusTemporary, time.Now().Add(time.Hour).UnixMilli())
	if _, err := repo.BindCandidateImage(context.Background(), agentconversation.BindCandidateImageCommand{TenantID: "tenant-a", OperatorID: "operator-a", SessionID: "session-b", CandidateID: "candidate-b", AssetID: "asset-shared", ExpectedSessionVersion: 1, ExpectedCandidateVersion: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := repo.BindCandidateImage(context.Background(), agentconversation.BindCandidateImageCommand{TenantID: "tenant-a", OperatorID: "operator-a", SessionID: "session-a", CandidateID: "candidate-a", AssetID: "asset-shared", ExpectedSessionVersion: 3, ExpectedCandidateVersion: 5})
	if !errors.Is(err, apperr.ErrAgentImageAssetInvalid) {
		t.Fatalf("already-bound error=%v", err)
	}
	assertUnchangedCandidateBinding(t, db)

	seedAgentImageAsset(t, db, "asset-cas", "tenant-a", "operator-a", "agent_lot_image", AssetStatusTemporary, time.Now().Add(time.Hour).UnixMilli())
	_, err = repo.BindCandidateImage(context.Background(), agentconversation.BindCandidateImageCommand{TenantID: "tenant-a", OperatorID: "operator-a", SessionID: "session-a", CandidateID: "candidate-a", AssetID: "asset-cas", ExpectedSessionVersion: 2, ExpectedCandidateVersion: 5})
	if !errors.Is(err, apperr.ErrAgentSessionVersionConflict) {
		t.Fatalf("stale session error=%v", err)
	}
	assertUnchangedCandidateBinding(t, db)
	_, err = repo.BindCandidateImage(context.Background(), agentconversation.BindCandidateImageCommand{TenantID: "tenant-a", OperatorID: "operator-a", SessionID: "session-a", CandidateID: "candidate-a", AssetID: "asset-cas", ExpectedSessionVersion: 3, ExpectedCandidateVersion: 4})
	if !errors.Is(err, apperr.ErrAgentSessionVersionConflict) {
		t.Fatalf("stale candidate error=%v", err)
	}
	assertUnchangedCandidateBinding(t, db)
}

func TestExpiredTemporaryAssetCleanupSkipsActiveAgentCandidateReferences(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	if err := db.AutoMigrate(&AssetFileModel{}, &AgentCandidateAssetModel{}); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	now := time.Now().Add(-time.Hour).UnixMilli()
	seedImageBindingSession(t, db, "tenant-a", "operator-a", "session-active", "candidate-active", 1, 1)
	seedImageBindingSession(t, db, "tenant-a", "operator-a", "session-terminal", "candidate-terminal", 1, 1)
	if err := db.Model(&AgentSessionModel{}).Where("id = ?", "session-terminal").Update("status", agentconversation.StatusCancelled).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"asset-active", "asset-terminal", "asset-free"} {
		seedAgentImageAsset(t, db, id, "tenant-a", "operator-a", "agent_lot_image", AssetStatusTemporary, now)
	}
	if err := db.Create([]AgentCandidateAssetModel{{CandidateID: "candidate-active", SessionID: "session-active", AssetID: "asset-active", TenantID: "tenant-a", OperatorID: "operator-a"}, {CandidateID: "candidate-terminal", SessionID: "session-terminal", AssetID: "asset-terminal", TenantID: "tenant-a", OperatorID: "operator-a"}}).Error; err != nil {
		t.Fatal(err)
	}
	assets, err := store.ListExpiredTemporaryAssets(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, asset := range assets {
		got[asset.ID] = true
	}
	if got["asset-active"] || !got["asset-terminal"] || !got["asset-free"] {
		t.Fatalf("cleanup candidates=%v", got)
	}
	if _, err := store.ClaimTemporaryAssetForDelete(context.Background(), "asset-active", "operator-a"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("active referenced asset deletable: %v", err)
	}
	if asset, err := store.ClaimTemporaryAssetForDelete(context.Background(), "asset-terminal", "operator-a"); err != nil || asset.ID != "asset-terminal" {
		t.Fatalf("terminal orphan not deletable: asset=%#v err=%v", asset, err)
	} else if err := store.RestoreDeletingAsset(context.Background(), asset.ID, "operator-a"); err != nil {
		t.Fatal(err)
	}
}

func TestAgentCandidateImageDeleteClaimAndBindingCannotBothWin(t *testing.T) {
	t.Run("binding wins before manual delete claim", func(t *testing.T) {
		db := newAgentConversationSQLiteDB(t)
		if err := db.AutoMigrate(&AssetFileModel{}, &AgentCandidateAssetModel{}); err != nil {
			t.Fatal(err)
		}
		store := &Store{db: db}
		repo := NewAgentConversationRepository(store)
		seedImageBindingSession(t, db, "tenant-a", "operator-a", "session-a", "candidate-a", 1, 1)
		seedAgentImageAsset(t, db, "asset-race", "tenant-a", "operator-a", "agent_lot_image", AssetStatusTemporary, time.Now().Add(time.Hour).UnixMilli())
		if _, err := repo.BindCandidateImage(context.Background(), agentconversation.BindCandidateImageCommand{TenantID: "tenant-a", OperatorID: "operator-a", SessionID: "session-a", CandidateID: "candidate-a", AssetID: "asset-race", ExpectedSessionVersion: 1, ExpectedCandidateVersion: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ClaimTemporaryAssetForDelete(context.Background(), "asset-race", "operator-a"); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("delete claim after bind error=%v", err)
		}
		var asset AssetFileModel
		if err := db.First(&asset, "id = ?", "asset-race").Error; err != nil {
			t.Fatal(err)
		}
		if asset.Status != AssetStatusTemporary {
			t.Fatalf("asset status=%q", asset.Status)
		}
	})

	t.Run("manual delete claim wins before binding", func(t *testing.T) {
		db := newAgentConversationSQLiteDB(t)
		if err := db.AutoMigrate(&AssetFileModel{}, &AgentCandidateAssetModel{}); err != nil {
			t.Fatal(err)
		}
		store := &Store{db: db}
		repo := NewAgentConversationRepository(store)
		seedImageBindingSession(t, db, "tenant-a", "operator-a", "session-a", "candidate-a", 1, 1)
		seedAgentImageAsset(t, db, "asset-race", "tenant-a", "operator-a", "agent_lot_image", AssetStatusTemporary, time.Now().Add(time.Hour).UnixMilli())
		claimed, err := store.ClaimTemporaryAssetForDelete(context.Background(), "asset-race", "operator-a")
		if err != nil || claimed.Status != AssetStatusDeleting {
			t.Fatalf("claim=%#v err=%v", claimed, err)
		}
		if _, err := repo.BindCandidateImage(context.Background(), agentconversation.BindCandidateImageCommand{TenantID: "tenant-a", OperatorID: "operator-a", SessionID: "session-a", CandidateID: "candidate-a", AssetID: "asset-race", ExpectedSessionVersion: 1, ExpectedCandidateVersion: 1}); !errors.Is(err, apperr.ErrAgentImageAssetInvalid) {
			t.Fatalf("bind after claim error=%v", err)
		}
		if err := store.FinalizeDeletingAsset(context.Background(), "asset-race", "operator-a"); err != nil {
			t.Fatal(err)
		}
		var refs int64
		if err := db.Model(&AgentCandidateAssetModel{}).Where("asset_id = ?", "asset-race").Count(&refs).Error; err != nil {
			t.Fatal(err)
		}
		var asset AssetFileModel
		if err := db.First(&asset, "id = ?", "asset-race").Error; err != nil {
			t.Fatal(err)
		}
		if refs != 0 || asset.Status != AssetStatusDeleted || asset.DeletedAtUnixMs == 0 {
			t.Fatalf("refs=%d asset=%#v", refs, asset)
		}
	})
}

func TestAgentCandidateImageBindingRollsBackLateSessionFailureAndRetainsOldReference(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	if err := db.AutoMigrate(&AssetFileModel{}, &AgentCandidateAssetModel{}); err != nil {
		t.Fatal(err)
	}
	repo := NewAgentConversationRepository(&Store{db: db})
	seedImageBindingSession(t, db, "tenant-a", "operator-a", "session-a", "candidate-a", 3, 5)
	seedAgentImageAsset(t, db, "asset-old", "tenant-a", "operator-a", "agent_lot_image", AssetStatusTemporary, time.Now().Add(time.Hour).UnixMilli())
	seedAgentImageAsset(t, db, "asset-new", "tenant-a", "operator-a", "agent_lot_image", AssetStatusTemporary, time.Now().Add(time.Hour).UnixMilli())
	var candidate AgentDraftCandidateModel
	if err := db.First(&candidate, "id = ?", "candidate-a").Error; err != nil {
		t.Fatal(err)
	}
	draft, err := agentDraftCandidateFromModel(candidate)
	if err != nil {
		t.Fatal(err)
	}
	draft.Candidate.Draft.MainImageAssetID, draft.Candidate.Draft.MainImageURL = "asset-old", "https://assets.example/asset-old.jpg"
	draft.FieldSourcesJSON = `{"mainImageAssetId":"USER","mainImageUrl":"USER"}`
	oldModel, err := agentDraftCandidateModelFromCandidate("session-a", draft, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&candidate).Updates(map[string]any{"draft_json": oldModel.DraftJSON, "field_sources_json": oldModel.FieldSourcesJSON, "validation_json": `{"old":true}`}).Error; err != nil {
		t.Fatal(err)
	}
	oldRef := AgentCandidateAssetModel{CandidateID: "candidate-a", SessionID: "session-a", AssetID: "asset-old", TenantID: "tenant-a", OperatorID: "operator-a", CreatedAtUnixMs: 1, UpdatedAtUnixMs: 1}
	if err := db.Create(&oldRef).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER fail_agent_image_session_update BEFORE UPDATE ON agent_sessions WHEN OLD.id = 'session-a' BEGIN SELECT RAISE(ABORT, 'forced late session failure'); END`).Error; err != nil {
		t.Fatal(err)
	}

	_, err = repo.BindCandidateImage(context.Background(), agentconversation.BindCandidateImageCommand{TenantID: "tenant-a", OperatorID: "operator-a", SessionID: "session-a", CandidateID: "candidate-a", AssetID: "asset-new", ExpectedSessionVersion: 3, ExpectedCandidateVersion: 5})
	if err == nil || strings.Contains(err.Error(), "asset-new") {
		t.Fatalf("late failure error=%v", err)
	}
	var session AgentSessionModel
	if err := db.First(&session, "id = ?", "session-a").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&candidate, "id = ?", "candidate-a").Error; err != nil {
		t.Fatal(err)
	}
	var ref AgentCandidateAssetModel
	if err := db.First(&ref, "candidate_id = ?", "candidate-a").Error; err != nil {
		t.Fatal(err)
	}
	if session.Version != 3 || session.Status != string(agentconversation.StatusCollectingInput) || session.ActiveSnapshotVersion != 9 || candidate.Version != 5 || candidate.DraftJSON != oldModel.DraftJSON || candidate.FieldSourcesJSON != oldModel.FieldSourcesJSON || candidate.ValidationJSON != `{"old":true}` || ref.AssetID != "asset-old" {
		t.Fatalf("rollback session=%#v candidate=%#v ref=%#v", session, candidate, ref)
	}
}

func TestAgentCandidateImageRejectsWrongRoomWithoutMutation(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	if err := db.AutoMigrate(&AssetFileModel{}, &AgentCandidateAssetModel{}); err != nil {
		t.Fatal(err)
	}
	repo := NewAgentConversationRepository(&Store{db: db})
	seedImageBindingSession(t, db, "tenant-a", "operator-a", "session-a", "candidate-a", 3, 5)
	seedAgentImageAsset(t, db, "asset-room-b", "tenant-a", "operator-a", "agent_lot_image", AssetStatusTemporary, time.Now().Add(time.Hour).UnixMilli())
	if err := db.Model(&AssetFileModel{}).Where("id = ?", "asset-room-b").Update("room_id", "room-b").Error; err != nil {
		t.Fatal(err)
	}
	_, err := repo.BindCandidateImage(context.Background(), agentconversation.BindCandidateImageCommand{TenantID: "tenant-a", OperatorID: "operator-a", SessionID: "session-a", CandidateID: "candidate-a", AssetID: "asset-room-b", ExpectedSessionVersion: 3, ExpectedCandidateVersion: 5})
	if !errors.Is(err, apperr.ErrPermissionDenied) {
		t.Fatalf("wrong-room error=%v", err)
	}
	assertUnchangedCandidateBinding(t, db)
}

func TestAssetCleanupWorkerClaimsBeforeObjectDeleteAndRestoresOnFailure(t *testing.T) {
	for _, failDelete := range []bool{false, true} {
		t.Run(map[bool]string{false: "success finalizes", true: "failure restores"}[failDelete], func(t *testing.T) {
			db := newAgentConversationSQLiteDB(t)
			if err := db.AutoMigrate(&AssetFileModel{}, &AgentCandidateAssetModel{}); err != nil {
				t.Fatal(err)
			}
			store := &Store{db: db}
			seedAgentImageAsset(t, db, "asset-worker", "tenant-a", "operator-a", "agent_lot_image", AssetStatusTemporary, time.Now().Add(-time.Hour).UnixMilli())
			provider := &claimObservingStorage{db: db, assetID: "asset-worker", fail: failDelete}
			NewAssetCleanupWorker(store, provider, time.Hour, 10).runOnce(context.Background())
			if provider.statusAtDelete != AssetStatusDeleting || provider.deleteCalls != 1 {
				t.Fatalf("statusAtDelete=%q calls=%d", provider.statusAtDelete, provider.deleteCalls)
			}
			var asset AssetFileModel
			if err := db.First(&asset, "id = ?", "asset-worker").Error; err != nil {
				t.Fatal(err)
			}
			want := AssetStatusDeleted
			if failDelete {
				want = AssetStatusTemporary
			}
			if asset.Status != want {
				t.Fatalf("final status=%q want=%q", asset.Status, want)
			}
		})
	}
}

type claimObservingStorage struct {
	db             *gorm.DB
	assetID        string
	fail           bool
	deleteCalls    int
	statusAtDelete string
}

func (s *claimObservingStorage) PutObject(context.Context, storage.PutObjectInput) (*storage.StoredObject, error) {
	return nil, errors.New("unused")
}
func (s *claimObservingStorage) DeleteObject(_ context.Context, _ string) error {
	s.deleteCalls++
	var asset AssetFileModel
	if err := s.db.First(&asset, "id = ?", s.assetID).Error; err != nil {
		return err
	}
	s.statusAtDelete = asset.Status
	if s.fail {
		return errors.New("object delete failed")
	}
	return nil
}

func seedImageBindingSession(t *testing.T, db *gorm.DB, tenant, operator, sessionID, candidateID string, sessionVersion, candidateVersion int64) {
	t.Helper()
	session := AgentSessionModel{ID: sessionID, TenantID: tenant, OperatorID: operator, RoomID: "room-a", Status: string(agentconversation.StatusCollectingInput), Version: sessionVersion, ActiveSnapshotVersion: 9, CreatedAtUnixMs: 1, UpdatedAtUnixMs: 1}
	draft := agentconversation.Candidate{ID: candidateID, SourceType: agentconversation.SourceNew, StartPriceSpecified: true, Draft: agentconversation.CandidateDraft{Title: "Ocean", StartPriceAmount: 10000, DurationSeconds: 300, MinIncrementAmount: 100, Stock: 1, AntiSnipeWindowSeconds: 10, AntiSnipeExtendSeconds: 15, MaxExtendCount: 3}}
	draftJSON, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	candidate := AgentDraftCandidateModel{ID: candidateID, SessionID: sessionID, SourceType: string(agentconversation.SourceNew), Position: 1, SelectedByDefault: boolPointer(true), DraftJSON: string(draftJSON), FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: candidateVersion, CreatedAtUnixMs: 1, UpdatedAtUnixMs: 1}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&candidate).Error; err != nil {
		t.Fatal(err)
	}
}

func seedAgentImageAsset(t *testing.T, db *gorm.DB, id, tenant, operator, bizType, status string, expires int64) {
	t.Helper()
	asset := AssetFileModel{ID: id, MainAccountID: tenant, OwnerUserID: operator, BizType: bizType, Status: status, StorageProvider: "test", Bucket: "test", ObjectKey: id, PublicURL: "https://assets.example/" + id + ".jpg", MimeType: "image/jpeg", SizeBytes: 10, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ExpiresAtUnixMs: expires}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
}

func assertAgentImageUserProvenance(t *testing.T, raw string) {
	t.Helper()
	var sources map[string]string
	if err := json.Unmarshal([]byte(raw), &sources); err != nil {
		t.Fatal(err)
	}
	if sources["mainImageAssetId"] != "USER" {
		t.Fatalf("sources=%v", sources)
	}
	if _, stale := sources["mainImageUrl"]; stale {
		t.Fatalf("non-executable image URL provenance persisted: %v", sources)
	}
}

func assertUnchangedCandidateBinding(t *testing.T, db *gorm.DB) {
	t.Helper()
	var session AgentSessionModel
	var candidate AgentDraftCandidateModel
	var refs int64
	if err := db.Where("id = ?", "session-a").First(&session).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("id = ?", "candidate-a").First(&candidate).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&AgentCandidateAssetModel{}).Where("candidate_id = ?", "candidate-a").Count(&refs).Error; err != nil {
		t.Fatal(err)
	}
	if session.Version != 3 || candidate.Version != 5 || refs != 0 {
		t.Fatalf("partial mutation session=%d candidate=%d refs=%d", session.Version, candidate.Version, refs)
	}
}
