package data

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	agentv1 "live-auction-bid/backend/api/agent/v1"
	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
	"live-auction-bid/backend/app/auction/service/internal/agentconversation"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

func TestAgentConversationRepositoryRestoresTenantScopedAggregate(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	session := agentconversation.Session{
		ID: "session-1", TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1",
		Status: agentconversation.StatusCollectingInput, Version: 1,
		CreatedAtUnixMs: 100, UpdatedAtUnixMs: 100,
	}
	if err := repo.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	if _, err := repo.FindSession(ctx, "tenant-2", session.ID); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("FindSession() cross-tenant error = %v, want %v", err, apperr.ErrNotFound)
	}

	started, err := repo.AppendUserMessageAndStartRun(ctx, agentconversation.StartTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: session.ID, ExpectedVersion: 1,
		Message: agentconversation.Message{Role: agentconversation.MessageRoleUser, Content: "创建海洋之心", CreatedAtUnixMs: 110},
		Run: agentcontrol.Run{
			ID: "run-1", TenantID: "tenant-1", OperatorID: "operator-1", IdempotencyKey: "turn-1",
			Goal: "创建海洋之心", Status: agentcontrol.StatusCreated, Version: 1,
			RunType:         agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST,
			CreatedAtUnixMs: 110, UpdatedAtUnixMs: 110,
		},
	})
	if err != nil {
		t.Fatalf("AppendUserMessageAndStartRun() error = %v", err)
	}
	if started.Session.Version != 2 || len(started.Messages) != 1 || len(started.Runs) != 1 {
		t.Fatalf("AppendUserMessageAndStartRun() aggregate = %#v, want version 2 with one message and run", started)
	}
	if got := started.Runs[0]; got.SessionID != session.ID || got.SessionVersion != 2 || got.RunType != agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST {
		t.Fatalf("started run linkage = %#v, want session %q version 2 and draft-assist type", got, session.ID)
	}

	readVersion := int64(7)
	capPrice := int64(100_000_00)
	wantCandidate := agentconversation.DraftCandidate{
		Candidate: agentconversation.Candidate{
			ID: "candidate-1", SourceType: agentconversation.SourceExisting,
			ExistingLotID: "lot-1", ExistingLotReadVersion: &readVersion, StartPriceSpecified: true,
			Draft: agentconversation.CandidateDraft{
				Title: "海洋之心", StartPriceAmount: 10_000_00, DurationSeconds: 3000,
				MainImageAssetID: "asset-1", MainImageURL: "https://assets.example/1.jpg",
				MinIncrementAmount: 100_00, CapPriceAmount: &capPrice, Category: "珠宝彩宝",
				Description: "蓝色宝石", Tags: []string{"宝石", "蓝色"}, Stock: 1,
				AntiSnipeWindowSeconds: 10, AntiSnipeExtendSeconds: 15, MaxExtendCount: 5,
			},
		},
		Position: 1, SelectedByDefault: false,
		FieldSourcesJSON: `{"title":"USER","category":"AI"}`,
		ValidationJSON:   `{"requiredInputs":[]}`,
		Version:          1,
	}
	merged, err := repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: session.ID, RunID: "run-1", ExpectedVersion: 2,
		AssistantMessage:    agentconversation.Message{Role: agentconversation.MessageRoleAssistant, Content: "草稿已准备好", RunID: "run-1", CreatedAtUnixMs: 120},
		Candidates:          []agentconversation.DraftCandidate{wantCandidate},
		ConversationSummary: "创建海洋之心拍品",
		Status:              agentconversation.StatusReadyForReview,
		UpdatedAtUnixMs:     120,
	})
	if err != nil {
		t.Fatalf("MergeTurnResult() error = %v", err)
	}
	if merged.Session.Version != 3 || len(merged.Messages) != 2 || len(merged.Candidates) != 1 {
		t.Fatalf("MergeTurnResult() aggregate = %#v, want fully merged version 3", merged)
	}

	refreshedRepo := NewAgentConversationRepository(&Store{db: db})
	got, err := refreshedRepo.FindSession(ctx, "tenant-1", session.ID)
	if err != nil {
		t.Fatalf("FindSession() after refresh error = %v", err)
	}
	if got.Session.Status != agentconversation.StatusReadyForReview || got.Session.ConversationSummary != "创建海洋之心拍品" || got.Session.Version != 3 {
		t.Fatalf("FindSession() session = %#v, want restored ready-for-review version 3", got.Session)
	}
	if len(got.Messages) != 2 || got.Messages[0].Seq != 1 || got.Messages[1].Seq != 2 || got.Messages[1].RunID != "run-1" {
		t.Fatalf("FindSession() messages = %#v, want ordered user and assistant messages", got.Messages)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].Candidate.ID != wantCandidate.Candidate.ID || got.Candidates[0].Candidate.Draft.Title != "海洋之心" || got.Candidates[0].FieldSourcesJSON != wantCandidate.FieldSourcesJSON || got.Candidates[0].SelectedByDefault {
		t.Fatalf("FindSession() candidates = %#v, want restored draft candidate", got.Candidates)
	}
	if len(got.Runs) != 1 || got.Runs[0].SessionID != session.ID || got.Runs[0].SessionVersion != 2 {
		t.Fatalf("FindSession() runs = %#v, want restored linked run", got.Runs)
	}
}

func TestAgentConversationModelsEnforceSessionSequenceAndPositionUniqueness(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	ctx := context.Background()
	for _, sessionID := range []string{"session-1", "session-2"} {
		if err := db.WithContext(ctx).Create(&AgentSessionModel{
			ID: sessionID, TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1",
			Status: string(agentconversation.StatusCollectingInput), Version: 1,
		}).Error; err != nil {
			t.Fatalf("create session %q: %v", sessionID, err)
		}
	}

	message := AgentSessionMessageModel{SessionID: "session-1", Seq: 1, Role: "USER", Content: "first"}
	if err := db.Create(&message).Error; err != nil {
		t.Fatalf("create first message: %v", err)
	}
	if err := db.Create(&AgentSessionMessageModel{SessionID: "session-1", Seq: 1, Role: "ASSISTANT", Content: "duplicate"}).Error; err == nil {
		t.Fatal("duplicate session message sequence succeeded, want unique constraint error")
	}
	if err := db.Create(&AgentSessionMessageModel{SessionID: "session-2", Seq: 1, Role: "USER", Content: "independent"}).Error; err != nil {
		t.Fatalf("same message sequence in another session error = %v", err)
	}

	candidate := AgentDraftCandidateModel{ID: "candidate-1", SessionID: "session-1", SourceType: "NEW", Position: 1, DraftJSON: `{}`, FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 1}
	if err := db.Create(&candidate).Error; err != nil {
		t.Fatalf("create first candidate: %v", err)
	}
	if err := db.Create(&AgentDraftCandidateModel{ID: "candidate-2", SessionID: "session-1", SourceType: "NEW", Position: 1, DraftJSON: `{}`, FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 1}).Error; err == nil {
		t.Fatal("duplicate candidate position succeeded, want unique constraint error")
	}
	if err := db.Create(&AgentDraftCandidateModel{ID: "candidate-3", SessionID: "session-2", SourceType: "NEW", Position: 1, DraftJSON: `{}`, FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 1}).Error; err != nil {
		t.Fatalf("same candidate position in another session error = %v", err)
	}
}

func TestAgentConversationRepositoryUsesSessionAndCandidateCAS(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	if err := repo.CreateSession(ctx, agentconversation.Session{
		ID: "session-cas", TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1",
		Status: agentconversation.StatusCollectingInput, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-cas", ExpectedVersion: 1,
		Candidates: []agentconversation.DraftCandidate{{
			Candidate: agentconversation.Candidate{ID: "candidate-cas", SourceType: agentconversation.SourceNew},
			Position:  1, SelectedByDefault: true, FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 1,
		}}, Status: agentconversation.StatusCollectingInput,
	}); err != nil {
		t.Fatalf("seed candidate error = %v", err)
	}

	_, err := repo.PatchCandidate(ctx, agentconversation.PatchCandidateCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-cas", CandidateID: "candidate-cas",
		ExpectedSessionVersion: 1, ExpectedCandidateVersion: 1,
		Candidate: agentconversation.DraftCandidate{Candidate: agentconversation.Candidate{ID: "candidate-cas", SourceType: agentconversation.SourceNew}},
	})
	if !errors.Is(err, apperr.ErrAgentSessionVersionConflict) {
		t.Fatalf("PatchCandidate() stale session error = %v, want %v", err, apperr.ErrAgentSessionVersionConflict)
	}

	_, err = repo.PatchCandidate(ctx, agentconversation.PatchCandidateCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-cas", CandidateID: "candidate-cas",
		ExpectedSessionVersion: 2, ExpectedCandidateVersion: 9,
		Candidate: agentconversation.DraftCandidate{Candidate: agentconversation.Candidate{ID: "candidate-cas", SourceType: agentconversation.SourceNew}},
	})
	if !errors.Is(err, apperr.ErrAgentSessionVersionConflict) {
		t.Fatalf("PatchCandidate() stale candidate error = %v, want %v", err, apperr.ErrAgentSessionVersionConflict)
	}

	if err := repo.CancelSession(ctx, "tenant-1", "operator-1", "session-cas", 1); !errors.Is(err, apperr.ErrAgentSessionVersionConflict) {
		t.Fatalf("CancelSession() stale error = %v, want %v", err, apperr.ErrAgentSessionVersionConflict)
	}
	if err := repo.CancelSession(ctx, "tenant-2", "operator-1", "session-cas", 2); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("CancelSession() cross-tenant error = %v, want %v", err, apperr.ErrNotFound)
	}
	if err := repo.CancelSession(ctx, "tenant-1", "operator-1", "session-cas", 2); err != nil {
		t.Fatalf("CancelSession() error = %v", err)
	}
	cancelled, err := repo.FindSession(ctx, "tenant-1", "session-cas")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Session.Status != agentconversation.StatusCancelled || cancelled.Session.Version != 3 {
		t.Fatalf("cancelled session = %#v, want CANCELLED version 3", cancelled.Session)
	}
}

func TestAgentConversationRepositoryPatchCandidatePersistsCoupledUpdate(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	seedAgentPatchCandidate(t, ctx, repo, "session-patch-success")

	patchedCandidate := agentconversation.DraftCandidate{
		Candidate: agentconversation.Candidate{ID: "candidate-patch", SourceType: agentconversation.SourceNew, StartPriceSpecified: true, Draft: agentconversation.CandidateDraft{Title: "edited", StartPriceAmount: 10000}},
		Position:  1, SelectedByDefault: true, FieldSourcesJSON: `{"title":"USER","startPriceAmount":"USER"}`, ValidationJSON: `{"requiredInputs":["DURATION"]}`, Version: 1,
	}
	got, err := repo.PatchCandidate(ctx, agentconversation.PatchCandidateCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-patch-success", CandidateID: "candidate-patch",
		ExpectedSessionVersion: 2, ExpectedCandidateVersion: 1, Candidate: patchedCandidate, Status: agentconversation.StatusNeedsRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Session.Version != 3 || got.Session.ActiveSnapshotVersion != 0 || got.Session.Status != agentconversation.StatusNeedsRevision {
		t.Fatalf("session=%+v", got.Session)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].Version != 2 || got.Candidates[0].FieldSourcesJSON != patchedCandidate.FieldSourcesJSON || got.Candidates[0].ValidationJSON != patchedCandidate.ValidationJSON || got.Candidates[0].Candidate.Draft.StartPriceAmount != 10000 {
		t.Fatalf("candidate=%+v", got.Candidates)
	}
}

func TestAgentConversationRepositoryPatchCandidateRollsBackWhenSessionUpdateFails(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	seedAgentPatchCandidate(t, ctx, repo, "session-patch-rollback")
	before, err := repo.FindSession(ctx, "tenant-1", "session-patch-rollback")
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected session update failure")
	if err := db.Callback().Update().Before("gorm:update").Register("test:fail_agent_session_update", func(tx *gorm.DB) {
		if tx.Statement.Table == (AgentSessionModel{}).TableName() {
			tx.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}

	replacement := before.Candidates[0]
	replacement.Candidate.Draft.Title = "must roll back"
	replacement.FieldSourcesJSON = `{"title":"USER"}`
	replacement.ValidationJSON = `{"requiredInputs":[]}`
	_, err = repo.PatchCandidate(ctx, agentconversation.PatchCandidateCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-patch-rollback", CandidateID: "candidate-patch",
		ExpectedSessionVersion: 2, ExpectedCandidateVersion: 1, Candidate: replacement, Status: agentconversation.StatusReadyForReview,
	})
	if !errors.Is(err, injected) {
		t.Fatalf("PatchCandidate() error=%v", err)
	}
	after, err := repo.FindSession(ctx, "tenant-1", "session-patch-rollback")
	if err != nil {
		t.Fatal(err)
	}
	if after.Session.Version != before.Session.Version || after.Session.ActiveSnapshotVersion != before.Session.ActiveSnapshotVersion || after.Session.Status != before.Session.Status || after.Candidates[0].Version != before.Candidates[0].Version || after.Candidates[0].Candidate.Draft.Title != before.Candidates[0].Candidate.Draft.Title || after.Candidates[0].FieldSourcesJSON != before.Candidates[0].FieldSourcesJSON || after.Candidates[0].ValidationJSON != before.Candidates[0].ValidationJSON {
		t.Fatalf("transaction leaked partial update: before=%+v after=%+v", before, after)
	}
}

func seedAgentPatchCandidate(t *testing.T, ctx context.Context, repo *AgentConversationRepo, sessionID string) {
	t.Helper()
	if err := repo.CreateSession(ctx, agentconversation.Session{ID: sessionID, TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1", Status: agentconversation.StatusReadyForReview, Version: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: sessionID, ExpectedVersion: 1, Status: agentconversation.StatusReadyForReview, ActiveSnapshotVersion: 6,
		Candidates: []agentconversation.DraftCandidate{{Candidate: agentconversation.Candidate{ID: "candidate-patch", SourceType: agentconversation.SourceNew, Draft: agentconversation.CandidateDraft{Title: "original"}}, Position: 1, SelectedByDefault: true, FieldSourcesJSON: `{"title":"AI"}`, ValidationJSON: `{}`, Version: 1}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentConversationRepositoryRejectsCrossSessionCandidateID(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	for _, session := range []agentconversation.Session{
		{ID: "session-tenant-1", TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1", Status: agentconversation.StatusCollectingInput, Version: 1},
		{ID: "session-tenant-2", TenantID: "tenant-2", OperatorID: "operator-2", RoomID: "room-2", Status: agentconversation.StatusCollectingInput, Version: 1},
	} {
		if err := repo.CreateSession(ctx, session); err != nil {
			t.Fatalf("CreateSession(%q) error = %v", session.ID, err)
		}
	}
	first := agentconversation.DraftCandidate{
		Candidate: agentconversation.Candidate{ID: "candidate-shared", SourceType: agentconversation.SourceNew, Draft: agentconversation.CandidateDraft{Title: "tenant one's draft"}},
		Position:  1, FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 1,
	}
	if _, err := repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-tenant-1", ExpectedVersion: 1,
		Candidates: []agentconversation.DraftCandidate{first}, Status: agentconversation.StatusCollectingInput,
	}); err != nil {
		t.Fatalf("seed first tenant candidate error = %v", err)
	}
	second := first
	second.Candidate.Draft.Title = "tenant two's overwrite"
	if _, err := repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
		TenantID: "tenant-2", OperatorID: "operator-2", SessionID: "session-tenant-2", ExpectedVersion: 1,
		Candidates: []agentconversation.DraftCandidate{second}, Status: agentconversation.StatusCollectingInput,
	}); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("cross-session candidate merge error = %v, want %v", err, apperr.ErrNotFound)
	}

	got, err := repo.FindSession(ctx, "tenant-1", "session-tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].Candidate.Draft.Title != "tenant one's draft" {
		t.Fatalf("first tenant candidate after rejected overwrite = %#v", got.Candidates)
	}
}

func TestAgentConversationRepositoryPersistsRunScopedCandidateIDsAcrossSessions(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	testCases := []struct {
		sessionID   string
		candidateID string
	}{
		{sessionID: "session-scope-a", candidateID: "candidate-db237ed9564e0e09f381e48a"},
		{sessionID: "session-scope-b", candidateID: "candidate-f371b8979ed20df5b08b6e06"},
	}
	for _, testCase := range testCases {
		if err := repo.CreateSession(ctx, agentconversation.Session{
			ID: testCase.sessionID, TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1",
			Status: agentconversation.StatusCollectingInput, Version: 1,
		}); err != nil {
			t.Fatalf("CreateSession(%q) error = %v", testCase.sessionID, err)
		}
		_, err := repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
			TenantID: "tenant-1", OperatorID: "operator-1", SessionID: testCase.sessionID, ExpectedVersion: 1,
			Candidates: []agentconversation.DraftCandidate{{
				Candidate: agentconversation.Candidate{ID: testCase.candidateID, SourceType: agentconversation.SourceNew, Draft: agentconversation.CandidateDraft{Title: "same input"}},
				Position:  1, SelectedByDefault: true, FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 1,
			}},
			Status: agentconversation.StatusCollectingInput,
		})
		if err != nil {
			t.Fatalf("MergeTurnResult(%q) error = %v, want both scoped candidates persisted", testCase.sessionID, err)
		}
	}

	for _, testCase := range testCases {
		got, err := repo.FindSession(ctx, "tenant-1", testCase.sessionID)
		if err != nil {
			t.Fatalf("FindSession(%q) error = %v", testCase.sessionID, err)
		}
		if len(got.Candidates) != 1 || got.Candidates[0].Candidate.ID != testCase.candidateID {
			t.Fatalf("FindSession(%q) candidates = %#v, want %q", testCase.sessionID, got.Candidates, testCase.candidateID)
		}
	}
}

func TestAgentConversationRepositoryRejectsCandidatePositionCollisionWithoutMutation(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	if err := repo.CreateSession(ctx, agentconversation.Session{
		ID: "session-position", TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1",
		Status: agentconversation.StatusCollectingInput, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&AgentDraftCandidateModel{
		ID: "candidate-original", SessionID: "session-position", SourceType: "NEW", Position: 1,
		SelectedByDefault: testBoolPointer(true), DraftJSON: `{"id":"candidate-original","sourceType":"NEW","draft":{"title":"original"}}`,
		FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 1,
	}).Error; err != nil {
		t.Fatalf("seed original candidate: %v", err)
	}

	_, err := repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-position", ExpectedVersion: 1,
		AssistantMessage: agentconversation.Message{Content: "must roll back"},
		Candidates: []agentconversation.DraftCandidate{{
			Candidate: agentconversation.Candidate{ID: "candidate-collision", SourceType: agentconversation.SourceNew, Draft: agentconversation.CandidateDraft{Title: "collision"}},
			Position:  1, SelectedByDefault: true, FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 1,
		}},
		Status: agentconversation.StatusReadyForReview,
	})
	if err == nil {
		t.Fatal("MergeTurnResult() position collision error = nil, want conflict/error")
	}
	got, findErr := repo.FindSession(ctx, "tenant-1", "session-position")
	if findErr != nil {
		t.Fatal(findErr)
	}
	if got.Session.Version != 1 || got.Session.Status != agentconversation.StatusCollectingInput || len(got.Messages) != 0 || len(got.Candidates) != 1 || got.Candidates[0].Candidate.ID != "candidate-original" || got.Candidates[0].Candidate.Draft.Title != "original" {
		t.Fatalf("aggregate after position collision = %#v, want original version 1 aggregate", got)
	}
}

func TestAgentConversationRepositoryRollsBackSimulatedConcurrentCrossSessionCandidateID(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	for _, session := range []agentconversation.Session{
		{ID: "session-request", TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1", Status: agentconversation.StatusCollectingInput, Version: 1},
		{ID: "session-racer", TenantID: "tenant-2", OperatorID: "operator-2", RoomID: "room-2", Status: agentconversation.StatusCollectingInput, Version: 1},
	} {
		if err := repo.CreateSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	injected := false
	if err := db.Callback().Create().Before("gorm:create").Register("test:inject_candidate_id_race", func(tx *gorm.DB) {
		if injected || tx.Statement.Table != "agent_draft_candidates" {
			return
		}
		injected = true
		collision := &AgentDraftCandidateModel{
			ID: "candidate-race", SessionID: "session-racer", SourceType: "NEW", Position: 1,
			SelectedByDefault: testBoolPointer(true), DraftJSON: `{"id":"candidate-race","sourceType":"NEW","draft":{"title":"racer"}}`,
			FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 1,
		}
		if err := tx.Session(&gorm.Session{SkipHooks: true}).Create(collision).Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}

	_, err := repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-request", ExpectedVersion: 1,
		AssistantMessage: agentconversation.Message{Content: "must roll back"},
		Candidates: []agentconversation.DraftCandidate{{
			Candidate: agentconversation.Candidate{ID: "candidate-race", SourceType: agentconversation.SourceNew, Draft: agentconversation.CandidateDraft{Title: "request"}},
			Position:  1, SelectedByDefault: true, FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 1,
		}},
		Status: agentconversation.StatusReadyForReview,
	})
	if err == nil {
		t.Fatal("MergeTurnResult() simulated concurrent ID collision error = nil, want conflict/error")
	}
	for _, check := range []struct{ tenantID, sessionID string }{{"tenant-1", "session-request"}, {"tenant-2", "session-racer"}} {
		got, findErr := repo.FindSession(ctx, check.tenantID, check.sessionID)
		if findErr != nil {
			t.Fatal(findErr)
		}
		if got.Session.Version != 1 || len(got.Messages) != 0 || len(got.Candidates) != 0 {
			t.Fatalf("aggregate %q after simulated ID collision = %#v, want untouched version 1", check.sessionID, got)
		}
	}
}

func TestAgentConversationRepositoryRejectsRunBindingMismatchAtomically(t *testing.T) {
	testCases := []struct {
		name    string
		updates map[string]any
	}{
		{name: "persisted operator", updates: map[string]any{"operator_id": "operator-other"}},
		{name: "persisted session version", updates: map[string]any{"session_version": int64(99)}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			db := newAgentConversationSQLiteDB(t)
			repo := NewAgentConversationRepository(&Store{db: db})
			ctx := context.Background()
			if err := repo.CreateSession(ctx, agentconversation.Session{
				ID: "session-run-binding", TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1",
				Status: agentconversation.StatusCollectingInput, Version: 1,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.AppendUserMessageAndStartRun(ctx, agentconversation.StartTurnCommand{
				TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-run-binding", ExpectedVersion: 1,
				Message: agentconversation.Message{Content: "start", CreatedAtUnixMs: 10},
				Run: agentcontrol.Run{
					ID: "run-binding", IdempotencyKey: "turn-binding", Goal: "start", Status: agentcontrol.StatusCreated,
					Version: 1, RunType: agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST, CreatedAtUnixMs: 10, UpdatedAtUnixMs: 10,
				},
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&AgentRunModel{}).Where("id = ?", "run-binding").Updates(testCase.updates).Error; err != nil {
				t.Fatal(err)
			}

			_, err := repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
				TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-run-binding", RunID: "run-binding", ExpectedVersion: 2,
				AssistantMessage: agentconversation.Message{Content: "must roll back", RunID: "run-binding", CreatedAtUnixMs: 20},
				Candidates: []agentconversation.DraftCandidate{{
					Candidate: agentconversation.Candidate{ID: "candidate-binding", SourceType: agentconversation.SourceNew},
					Position:  1, SelectedByDefault: true, FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 1,
				}},
				Status: agentconversation.StatusReadyForReview, UpdatedAtUnixMs: 20,
			})
			if !errors.Is(err, apperr.ErrNotFound) {
				t.Fatalf("MergeTurnResult() binding mismatch error = %v, want %v", err, apperr.ErrNotFound)
			}
			got, findErr := repo.FindSession(ctx, "tenant-1", "session-run-binding")
			if findErr != nil {
				t.Fatal(findErr)
			}
			if got.Session.Version != 2 || got.Session.Status != agentconversation.StatusCollectingInput || len(got.Messages) != 1 || len(got.Runs) != 1 || len(got.Candidates) != 0 {
				t.Fatalf("aggregate after Run binding mismatch = %#v, want unchanged started turn", got)
			}
		})
	}
}

func TestAgentConversationRepositoryMergesEligibleHistoricalRunExactlyOnce(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	const sessionID = "session-historical-run"
	if err := repo.CreateSession(ctx, agentconversation.Session{
		ID: sessionID, TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1",
		Status: agentconversation.StatusCollectingInput, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendUserMessageAndStartRun(ctx, agentconversation.StartTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: sessionID, ExpectedVersion: 1,
		Message: agentconversation.Message{Content: "start", CreatedAtUnixMs: 10},
		Run: agentcontrol.Run{
			ID: "run-historical", IdempotencyKey: "turn-historical", Goal: "start", Status: agentcontrol.StatusCompleted,
			Version: 1, RunType: agentv1.RunType_RUN_TYPE_SESSION_PLAN, CreatedAtUnixMs: 10, UpdatedAtUnixMs: 10,
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate later durable session activity: the completed run remains bound
	// to version 2 and is still eligible when recovery sees version 4.
	if err := db.Model(&AgentSessionModel{}).Where("id = ?", sessionID).Updates(map[string]any{"version": int64(4)}).Error; err != nil {
		t.Fatal(err)
	}
	command := agentconversation.MergeTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: sessionID, RunID: "run-historical", ExpectedVersion: 4,
		AssistantMessage:    agentconversation.Message{Content: "recovered", RunID: "run-historical", CreatedAtUnixMs: 20},
		ConversationSummary: "recovered", Status: agentconversation.StatusCollectingInput, UpdatedAtUnixMs: 20,
	}
	first, err := repo.MergeTurnResult(ctx, command)
	if err != nil {
		t.Fatalf("first MergeTurnResult() error = %v", err)
	}
	if first.Session.Version != 5 || len(first.Messages) != 2 || first.Messages[1].RunID != "run-historical" {
		t.Fatalf("first merge = %#v, want version 5 and one durable run marker", first)
	}
	command.ExpectedVersion = first.Session.Version
	second, err := repo.MergeTurnResult(ctx, command)
	if err != nil {
		t.Fatalf("replayed MergeTurnResult() error = %v", err)
	}
	if second.Session.Version != 5 || len(second.Messages) != 2 {
		t.Fatalf("replayed merge = %#v, want unchanged exactly-once aggregate", second)
	}
}

func TestAgentConversationRepositoryRejectsSpoofedAssistantRunMarkerAndDerivesBlankMarker(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	const sessionID = "session-run-marker"
	if err := repo.CreateSession(ctx, agentconversation.Session{
		ID: sessionID, TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1",
		Status: agentconversation.StatusCollectingInput, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendUserMessageAndStartRun(ctx, agentconversation.StartTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: sessionID, ExpectedVersion: 1,
		Message: agentconversation.Message{Content: "start", CreatedAtUnixMs: 10},
		Run: agentcontrol.Run{
			ID: "run-a", IdempotencyKey: "turn-a", Goal: "start", Status: agentcontrol.StatusCompleted,
			Version: 1, RunType: agentv1.RunType_RUN_TYPE_SESSION_PLAN, CreatedAtUnixMs: 10, UpdatedAtUnixMs: 10,
		},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: sessionID, ExpectedVersion: 2,
		AssistantMessage:    agentconversation.Message{Content: "asymmetric spoof", RunID: "run-a", CreatedAtUnixMs: 15},
		ConversationSummary: "asymmetric spoof", Status: agentconversation.StatusCollectingInput, UpdatedAtUnixMs: 15,
	})
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("MergeTurnResult() blank-command spoof error = %v, want %v", err, apperr.ErrInvalidArgument)
	}
	unchanged, findErr := repo.FindSession(ctx, "tenant-1", sessionID)
	if findErr != nil {
		t.Fatal(findErr)
	}
	if unchanged.Session.Version != 2 || unchanged.Session.ConversationSummary != "" || len(unchanged.Messages) != 1 {
		t.Fatalf("aggregate after blank-command spoof = %#v, want unchanged", unchanged)
	}

	_, err = repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: sessionID, RunID: "run-a", ExpectedVersion: 2,
		AssistantMessage:    agentconversation.Message{Content: "spoofed", RunID: "run-b", CreatedAtUnixMs: 20},
		ConversationSummary: "spoofed", Status: agentconversation.StatusCollectingInput, UpdatedAtUnixMs: 20,
	})
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("MergeTurnResult() spoofed marker error = %v, want %v", err, apperr.ErrInvalidArgument)
	}
	unchanged, findErr = repo.FindSession(ctx, "tenant-1", sessionID)
	if findErr != nil {
		t.Fatal(findErr)
	}
	if unchanged.Session.Version != 2 || unchanged.Session.ConversationSummary != "" || len(unchanged.Messages) != 1 {
		t.Fatalf("aggregate after spoofed marker = %#v, want unchanged", unchanged)
	}

	command := agentconversation.MergeTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: sessionID, RunID: "run-a", ExpectedVersion: 2,
		AssistantMessage:    agentconversation.Message{Content: "derived marker", CreatedAtUnixMs: 30},
		ConversationSummary: "derived marker", Status: agentconversation.StatusCollectingInput, UpdatedAtUnixMs: 30,
	}
	merged, err := repo.MergeTurnResult(ctx, command)
	if err != nil {
		t.Fatalf("MergeTurnResult() blank marker error = %v", err)
	}
	if merged.Session.Version != 3 || len(merged.Messages) != 2 || merged.Messages[1].RunID != "run-a" {
		t.Fatalf("blank marker merge = %#v, want marker derived from command RunID", merged)
	}
	command.ExpectedVersion = merged.Session.Version
	replayed, err := repo.MergeTurnResult(ctx, command)
	if err != nil || replayed.Session.Version != 3 || len(replayed.Messages) != 2 {
		t.Fatalf("derived marker replay = %#v err=%v, want exactly once", replayed, err)
	}
}

func TestAgentConversationRepositoryRejectsFutureAndTerminalRunMergesWithoutMutation(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		status     agentconversation.SessionStatus
		runVersion int64
		wantErr    error
	}{
		{name: "forged future binding", status: agentconversation.StatusCollectingInput, runVersion: 3, wantErr: apperr.ErrNotFound},
		{name: "cancelled", status: agentconversation.StatusCancelled, runVersion: 1, wantErr: apperr.ErrConflict},
		{name: "applying", status: agentconversation.StatusApplying, runVersion: 1, wantErr: apperr.ErrConflict},
		{name: "applied", status: agentconversation.StatusApplied, runVersion: 1, wantErr: apperr.ErrConflict},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			db := newAgentConversationSQLiteDB(t)
			repo := NewAgentConversationRepository(&Store{db: db})
			ctx := context.Background()
			if err := repo.CreateSession(ctx, agentconversation.Session{
				ID: "session-guard", TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1",
				Status: testCase.status, Version: 2,
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&AgentRunModel{
				ID: "run-guard", TenantID: "tenant-1", OperatorID: "operator-1", IdempotencyKey: "turn-guard",
				Goal: "guard", Status: string(agentcontrol.StatusCompleted), Version: 1,
				RunType: int32(agentv1.RunType_RUN_TYPE_SESSION_PLAN), SessionID: "session-guard", SessionVersion: testCase.runVersion,
			}).Error; err != nil {
				t.Fatal(err)
			}
			_, err := repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
				TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-guard", RunID: "run-guard", ExpectedVersion: 2,
				AssistantMessage:    agentconversation.Message{Content: "must not appear", RunID: "run-guard"},
				ConversationSummary: "must not appear", Status: agentconversation.StatusCollectingInput,
			})
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("MergeTurnResult() error = %v, want %v", err, testCase.wantErr)
			}
			// Inspect the mutation boundary directly. An APPLIED aggregate now
			// intentionally requires its durable execution and snapshot, which this
			// narrow terminal-merge fixture does not seed.
			var stored AgentSessionModel
			if findErr := db.Where("id = ? AND tenant_id = ?", "session-guard", "tenant-1").First(&stored).Error; findErr != nil {
				t.Fatal(findErr)
			}
			var messageCount, candidateCount int64
			if err := db.Model(&AgentSessionMessageModel{}).Where("session_id = ?", "session-guard").Count(&messageCount).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&AgentDraftCandidateModel{}).Where("session_id = ?", "session-guard").Count(&candidateCount).Error; err != nil {
				t.Fatal(err)
			}
			if stored.Version != 2 || agentconversation.SessionStatus(stored.Status) != testCase.status || stored.ConversationSummary != "" || messageCount != 0 || candidateCount != 0 {
				t.Fatalf("stored state after rejected merge = %#v messages=%d candidates=%d, want unchanged", stored, messageCount, candidateCount)
			}
		})
	}
}

func TestAgentConversationRepositoryRollsBackStartTurnWhenRunInsertFails(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	if err := repo.CreateSession(ctx, agentconversation.Session{
		ID: "session-start-atomic", TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1",
		Status: agentconversation.StatusCollectingInput, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&AgentRunModel{
		ID: "run-conflict", TenantID: "tenant-other", OperatorID: "operator-other", IdempotencyKey: "existing",
		Goal: "existing", Status: string(agentcontrol.StatusCreated), Version: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}

	_, err := repo.AppendUserMessageAndStartRun(ctx, agentconversation.StartTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-start-atomic", ExpectedVersion: 1,
		Message: agentconversation.Message{Content: "must roll back", CreatedAtUnixMs: 10},
		Run: agentcontrol.Run{
			ID: "run-conflict", IdempotencyKey: "new", Goal: "new", Status: agentcontrol.StatusCreated,
			Version: 1, RunType: agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST, CreatedAtUnixMs: 10, UpdatedAtUnixMs: 10,
		},
	})
	if err == nil {
		t.Fatal("AppendUserMessageAndStartRun() Run conflict error = nil, want error")
	}
	got, findErr := repo.FindSession(ctx, "tenant-1", "session-start-atomic")
	if findErr != nil {
		t.Fatal(findErr)
	}
	if got.Session.Version != 1 || got.Session.Status != agentconversation.StatusCollectingInput || len(got.Messages) != 0 || len(got.Runs) != 0 || len(got.Candidates) != 0 {
		t.Fatalf("aggregate after failed StartTurn = %#v, want untouched version 1", got)
	}
}

func TestAgentConversationRepositoryRollsBackMergeAfterMessageWhenCandidateInsertFails(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	if err := repo.CreateSession(ctx, agentconversation.Session{
		ID: "session-merge-atomic", TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1",
		Status: agentconversation.StatusCollectingInput, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendUserMessageAndStartRun(ctx, agentconversation.StartTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-merge-atomic", ExpectedVersion: 1,
		Message: agentconversation.Message{Content: "start", CreatedAtUnixMs: 10},
		Run: agentcontrol.Run{
			ID: "run-merge-atomic", IdempotencyKey: "turn-merge-atomic", Goal: "start", Status: agentcontrol.StatusCreated,
			Version: 1, RunType: agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST, CreatedAtUnixMs: 10, UpdatedAtUnixMs: 10,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&AgentDraftCandidateModel{
		ID: "candidate-existing", SessionID: "session-merge-atomic", SourceType: "NEW", Position: 1,
		SelectedByDefault: testBoolPointer(true), DraftJSON: `{"id":"candidate-existing","sourceType":"NEW","draft":{"title":"existing"}}`,
		FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}

	_, err := repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-merge-atomic", RunID: "run-merge-atomic", ExpectedVersion: 2,
		AssistantMessage: agentconversation.Message{Content: "must roll back", RunID: "run-merge-atomic", CreatedAtUnixMs: 20},
		Candidates: []agentconversation.DraftCandidate{{
			Candidate: agentconversation.Candidate{ID: "candidate-conflict", SourceType: agentconversation.SourceNew, Draft: agentconversation.CandidateDraft{Title: "conflict"}},
			Position:  1, SelectedByDefault: true, FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 1,
		}},
		Status: agentconversation.StatusReadyForReview, UpdatedAtUnixMs: 20,
	})
	if err == nil {
		t.Fatal("MergeTurnResult() candidate position conflict error = nil, want error")
	}
	got, findErr := repo.FindSession(ctx, "tenant-1", "session-merge-atomic")
	if findErr != nil {
		t.Fatal(findErr)
	}
	if got.Session.Version != 2 || got.Session.Status != agentconversation.StatusCollectingInput || len(got.Messages) != 1 || len(got.Runs) != 1 || len(got.Candidates) != 1 || got.Candidates[0].Candidate.ID != "candidate-existing" {
		t.Fatalf("aggregate after failed MergeTurnResult = %#v, want unchanged started turn", got)
	}
}

func TestAgentConversationRepositoryUpdatesExistingCandidateWithVersionCAS(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	if err := repo.CreateSession(ctx, agentconversation.Session{
		ID: "session-candidate-update", TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1",
		Status: agentconversation.StatusCollectingInput, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&AgentDraftCandidateModel{
		ID: "candidate-update", SessionID: "session-candidate-update", SourceType: "NEW", Position: 1,
		SelectedByDefault: testBoolPointer(true), DraftJSON: `{"id":"candidate-update","sourceType":"NEW","draft":{"title":"before"}}`,
		FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 3,
	}).Error; err != nil {
		t.Fatal(err)
	}

	got, err := repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-candidate-update", ExpectedVersion: 1,
		Candidates: []agentconversation.DraftCandidate{{
			Candidate: agentconversation.Candidate{ID: "candidate-update", SourceType: agentconversation.SourceNew, Draft: agentconversation.CandidateDraft{Title: "after"}},
			Position:  1, SelectedByDefault: false, FieldSourcesJSON: `{"title":"AI"}`, ValidationJSON: `{}`, Version: 3,
		}},
		Status: agentconversation.StatusCollectingInput, UpdatedAtUnixMs: 20,
	})
	if err != nil {
		t.Fatalf("MergeTurnResult() existing candidate error = %v", err)
	}
	if got.Session.Version != 2 || len(got.Candidates) != 1 || got.Candidates[0].Version != 4 || got.Candidates[0].Candidate.Draft.Title != "after" || got.Candidates[0].SelectedByDefault {
		t.Fatalf("updated candidate aggregate = %#v, want candidate version 4 with explicit false", got)
	}
}

func TestAgentConversationRepositoryRejectsStaleMergedCandidateVersionAtomically(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	repo := NewAgentConversationRepository(&Store{db: db})
	ctx := context.Background()
	if err := repo.CreateSession(ctx, agentconversation.Session{
		ID: "session-candidate-stale", TenantID: "tenant-1", OperatorID: "operator-1", RoomID: "room-1",
		Status: agentconversation.StatusCollectingInput, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&AgentDraftCandidateModel{
		ID: "candidate-stale", SessionID: "session-candidate-stale", SourceType: "NEW", Position: 1,
		SelectedByDefault: testBoolPointer(true), DraftJSON: `{"id":"candidate-stale","sourceType":"NEW","draft":{"title":"current"}}`,
		FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 3,
	}).Error; err != nil {
		t.Fatal(err)
	}

	_, err := repo.MergeTurnResult(ctx, agentconversation.MergeTurnCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-candidate-stale", ExpectedVersion: 1,
		AssistantMessage: agentconversation.Message{Content: "must roll back"},
		Candidates: []agentconversation.DraftCandidate{{
			Candidate: agentconversation.Candidate{ID: "candidate-stale", SourceType: agentconversation.SourceNew, Draft: agentconversation.CandidateDraft{Title: "stale overwrite"}},
			Position:  1, SelectedByDefault: false, FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 2,
		}},
		Status: agentconversation.StatusReadyForReview,
	})
	if !errors.Is(err, apperr.ErrAgentSessionVersionConflict) {
		t.Fatalf("MergeTurnResult() stale candidate error = %v, want %v", err, apperr.ErrAgentSessionVersionConflict)
	}
	got, findErr := repo.FindSession(ctx, "tenant-1", "session-candidate-stale")
	if findErr != nil {
		t.Fatal(findErr)
	}
	if got.Session.Version != 1 || got.Session.Status != agentconversation.StatusCollectingInput || len(got.Messages) != 0 || len(got.Candidates) != 1 || got.Candidates[0].Version != 3 || got.Candidates[0].Candidate.Draft.Title != "current" || !got.Candidates[0].SelectedByDefault {
		t.Fatalf("aggregate after stale candidate merge = %#v, want unchanged current candidate", got)
	}
}

func TestAgentConversationModelsDeclareRequiredIndexes(t *testing.T) {
	assertSchemaIndex(t, &AgentSessionModel{}, "idx_agent_session_tenant_updated", "", "TenantID", "UpdatedAtUnixMs")
	assertSchemaIndex(t, &AgentSessionMessageModel{}, "uidx_agent_session_message_seq", "UNIQUE", "SessionID", "Seq")
	assertSchemaIndex(t, &AgentDraftCandidateModel{}, "uidx_agent_draft_candidate_position", "UNIQUE", "SessionID", "Position")
}

func TestAgentConversationAutoMigrateDefaultsSelectionAndLegacyProvenance(t *testing.T) {
	db := newAgentConversationSQLiteDB(t)
	model := AgentDraftCandidateModel{
		ID: "candidate-default", SessionID: "session-default", SourceType: "NEW", Position: 1,
		DraftJSON: `{}`, FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 1,
	}
	if err := db.Create(&model).Error; err != nil {
		t.Fatalf("create candidate without explicit selection error = %v", err)
	}
	var selected bool
	if err := db.Model(&AgentDraftCandidateModel{}).Select("selected_by_default").Where("id = ?", model.ID).Scan(&selected).Error; err != nil {
		t.Fatal(err)
	}
	if !selected {
		t.Fatal("AutoMigrate selected_by_default default = false, want true")
	}
	var provenanceVersion int32
	if err := db.Model(&AgentDraftCandidateModel{}).Select("provenance_version").Where("id = ?", model.ID).Scan(&provenanceVersion).Error; err != nil {
		t.Fatal(err)
	}
	if provenanceVersion != 0 {
		t.Fatalf("migrated historical provenance version = %d, want 0", provenanceVersion)
	}
}

func newAgentConversationSQLiteDB(t *testing.T) *gorm.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-conversation.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("gorm.Open(sqlite) error = %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB() error = %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close SQLite database: %v", err)
		}
	})
	if err := db.AutoMigrate(&AgentRunModel{}, &AgentSessionModel{}, &AgentSessionMessageModel{}, &AgentDraftCandidateModel{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	return db
}

func testBoolPointer(value bool) *bool { return &value }
