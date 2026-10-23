package agentconversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	agentv1 "live-auction-bid/backend/api/agent/v1"
	auctionv1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
	"live-auction-bid/backend/app/auction/service/internal/biz/auction"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

func TestAgentSessionPlanIntroducesExistingCandidateOnlyFromTrustedTenantInventory(t *testing.T) {
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	owned := &auctionv1.Lot{
		Id: "lot-owned", RoomId: "room-1", MainAccountId: actor.TenantID,
		Title: "Trusted title", ImageUrl: "https://example.test/trusted.jpg",
		Category: "trusted-category", Description: "trusted-description", Tags: []string{"trusted-tag"},
		Status: auctionv1.LotStatus_LOT_STATUS_READY, QueueStatus: auctionv1.LotQueueStatus_LOT_QUEUE_STATUS_NONE, Version: 9, Stock: 2, DepositAmount: &auctionv1.Money{Amount: 700, Currency: "CNY"},
		Rule: &auctionv1.BidRule{
			StartPrice:      &auctionv1.Money{Amount: 1200, Currency: "CNY"},
			MinIncrement:    &auctionv1.Money{Amount: 100, Currency: "CNY"},
			CapPrice:        &auctionv1.Money{Amount: 9000, Currency: "CNY"},
			DurationSeconds: 300, AntiSnipeWindowSeconds: 30,
			AntiSnipeExtendSeconds: 10, MaxExtendCount: 2,
		},
	}
	otherRoom := &auctionv1.Lot{Id: "lot-other-room", RoomId: "room-2", MainAccountId: actor.TenantID, Status: auctionv1.LotStatus_LOT_STATUS_READY, Version: 3}
	otherTenant := &auctionv1.Lot{Id: "lot-other-tenant", RoomId: "room-1", MainAccountId: "tenant-b", Status: auctionv1.LotStatus_LOT_STATUS_READY, Version: 3}
	illegalStatus := &auctionv1.Lot{Id: "lot-live", RoomId: "room-1", MainAccountId: actor.TenantID, Status: auctionv1.LotStatus_LOT_STATUS_LIVE, Version: 3}
	alreadyQueued := *owned
	alreadyQueued.Id, alreadyQueued.QueueStatus, alreadyQueued.QueuePosition = "lot-already-queued", auctionv1.LotQueueStatus_LOT_QUEUE_STATUS_QUEUED, 8
	inventory := trustedInventoryRoomFinder{room: &auction.Room{ID: "room-1", MainAccountID: actor.TenantID}, lots: []*auctionv1.Lot{owned, otherRoom, otherTenant, illegalStatus, &alreadyQueued}}
	runtime := &conversationRuntime{runType: agentv1.RunType_RUN_TYPE_SESSION_PLAN, complete: `{"assistant_message":"Use the owned lot","candidate_patches":[{"candidate_id":"lot-owned","source_type":"EXISTING","fields":{"existingLotId":"lot-owned","title":"forged","mainImageUrl":"https://forged.invalid/x","startPriceAmount":1,"minIncrementAmount":1,"capPriceAmount":2,"durationSeconds":60,"category":"forged","description":"forged","tags":["forged"],"stock":99,"antiSnipeWindowSeconds":1,"antiSnipeExtendSeconds":30,"maxExtendCount":99,"depositAmount":999},"field_sources":{"existingLotId":"EXISTING","title":"AI"}}],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`}
	repo := newConversationMemoryRepo()
	service, err := NewService(repo, inventory, runtime)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateSession(context.Background(), actor, "room-1")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := service.SubmitMessage(context.Background(), actor, SubmitCommand{SessionID: created.Session.ID, ExpectedVersion: 1, Content: "plan", RunType: agentv1.RunType_RUN_TYPE_SESSION_PLAN, IdempotencyKey: "trusted-existing"})
	if err != nil {
		t.Fatalf("trusted SESSION_PLAN merge error = %v", err)
	}
	if len(got.Candidates) != 1 {
		t.Fatalf("candidates = %+v", got.Candidates)
	}
	candidate := got.Candidates[0].Candidate
	if got.Candidates[0].ProvenanceVersion != CurrentProvenanceVersion {
		t.Fatalf("trusted candidate provenance version = %d", got.Candidates[0].ProvenanceVersion)
	}
	if candidate.SourceType != SourceExisting || candidate.ExistingLotID != owned.GetId() || candidate.ExistingLotReadVersion == nil || *candidate.ExistingLotReadVersion != owned.GetVersion() {
		t.Fatalf("trusted identity/version not materialized: %+v", candidate)
	}
	want := candidateFromTrustedLot(owned)
	want.ID = existingSessionCandidateID(created.Session.ID, owned.GetId())
	if !reflect.DeepEqual(candidate, want) {
		t.Fatalf("model replaced trusted lot facts:\n got=%+v\nwant=%+v", candidate, want)
	}
	if _, err := BuildReviewSnapshot(got, actor.OperatorID, []string{candidate.ID}, []string{candidate.ID}); err != nil {
		t.Fatalf("trusted inventory candidate cannot reach review snapshot: %v", err)
	}

	for name, result := range map[string]struct {
		runType agentv1.RunType
		result  string
	}{
		"fabricated lot": {agentv1.RunType_RUN_TYPE_SESSION_PLAN, `{"assistant_message":"bad","candidate_patches":[{"candidate_id":"lot-foreign","source_type":"EXISTING","fields":{"existingLotId":"lot-foreign"},"field_sources":{"existingLotId":"EXISTING"}}],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`},
		"other room":     {agentv1.RunType_RUN_TYPE_SESSION_PLAN, `{"assistant_message":"bad","candidate_patches":[{"candidate_id":"lot-other-room","source_type":"EXISTING","fields":{"existingLotId":"lot-other-room"},"field_sources":{"existingLotId":"EXISTING"}}],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`},
		"other tenant":   {agentv1.RunType_RUN_TYPE_SESSION_PLAN, `{"assistant_message":"bad","candidate_patches":[{"candidate_id":"lot-other-tenant","source_type":"EXISTING","fields":{"existingLotId":"lot-other-tenant"},"field_sources":{"existingLotId":"EXISTING"}}],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`},
		"illegal status": {agentv1.RunType_RUN_TYPE_SESSION_PLAN, `{"assistant_message":"bad","candidate_patches":[{"candidate_id":"lot-live","source_type":"EXISTING","fields":{"existingLotId":"lot-live"},"field_sources":{"existingLotId":"EXISTING"}}],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`},
		"already queued": {agentv1.RunType_RUN_TYPE_SESSION_PLAN, `{"assistant_message":"bad","candidate_patches":[{"candidate_id":"lot-already-queued","source_type":"EXISTING","fields":{"existingLotId":"lot-already-queued"},"field_sources":{"existingLotId":"EXISTING"}}],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`},
		"wrong run type": {agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST, `{"assistant_message":"bad","candidate_patches":[{"candidate_id":"lot-owned","source_type":"EXISTING","fields":{"existingLotId":"lot-owned"},"field_sources":{"existingLotId":"EXISTING"}}],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newConversationMemoryRepo()
			service, _ := NewService(repo, inventory, &conversationRuntime{runType: result.runType, complete: result.result})
			created, _ := service.CreateSession(context.Background(), actor, "room-1")
			aggregate, _, err := service.SubmitMessage(context.Background(), actor, SubmitCommand{SessionID: created.Session.ID, ExpectedVersion: 1, Content: "plan", RunType: result.runType, IdempotencyKey: "reject-existing"})
			if !errors.Is(err, apperr.ErrInvalidArgument) || len(aggregate.Candidates) != 0 {
				t.Fatalf("untrusted EXISTING merge aggregate=%+v err=%v", aggregate, err)
			}
		})
	}

	t.Run("outside top 100 tool evidence", func(t *testing.T) {
		lots := make([]*auctionv1.Lot, 0, 101)
		lots = append(lots, &auctionv1.Lot{Id: "lot-too-old", RoomId: "room-1", MainAccountId: actor.TenantID, Status: auctionv1.LotStatus_LOT_STATUS_READY, Version: 1})
		for i := int64(1); i <= 100; i++ {
			lots = append(lots, &auctionv1.Lot{Id: fmt.Sprintf("lot-new-%03d", i), RoomId: "room-1", MainAccountId: actor.TenantID, Status: auctionv1.LotStatus_LOT_STATUS_READY, Version: 1, UpdatedAtUnixMs: i})
		}
		inventory := trustedInventoryRoomFinder{room: &auction.Room{ID: "room-1", MainAccountID: actor.TenantID}, lots: lots}
		result := `{"assistant_message":"bad","candidate_patches":[{"candidate_id":"lot-too-old","source_type":"EXISTING","fields":{"existingLotId":"lot-too-old"},"field_sources":{"existingLotId":"EXISTING"}}],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`
		repo := newConversationMemoryRepo()
		service, _ := NewService(repo, inventory, &conversationRuntime{runType: agentv1.RunType_RUN_TYPE_SESSION_PLAN, complete: result})
		created, _ := service.CreateSession(context.Background(), actor, "room-1")
		aggregate, _, err := service.SubmitMessage(context.Background(), actor, SubmitCommand{SessionID: created.Session.ID, ExpectedVersion: 1, Content: "plan", RunType: agentv1.RunType_RUN_TYPE_SESSION_PLAN, IdempotencyKey: "reject-old"})
		if !errors.Is(err, apperr.ErrInvalidArgument) || len(aggregate.Candidates) != 0 {
			t.Fatalf("non-evidence EXISTING merge aggregate=%+v err=%v", aggregate, err)
		}
	})
}

func TestAgentSessionPlanPersistsExplanationWhenTrustedInventoryLotIsIncomplete(t *testing.T) {
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	incomplete := &auctionv1.Lot{
		Id: "lot-incomplete", RoomId: "room-1", MainAccountId: actor.TenantID,
		Title: "Incomplete draft", Status: auctionv1.LotStatus_LOT_STATUS_DRAFT,
		QueueStatus: auctionv1.LotQueueStatus_LOT_QUEUE_STATUS_NONE, Version: 1,
		Rule: &auctionv1.BidRule{StartPrice: &auctionv1.Money{Amount: 1000, Currency: "CNY"}},
	}
	inventory := trustedInventoryRoomFinder{
		room: &auction.Room{ID: "room-1", MainAccountID: actor.TenantID},
		lots: []*auctionv1.Lot{incomplete},
	}
	result := `{"assistant_message":"generated","candidate_patches":[{"candidate_id":"lot-incomplete","source_type":"EXISTING","fields":{"existingLotId":"lot-incomplete"},"field_sources":{"existingLotId":"EXISTING"}}],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`
	repo := newConversationMemoryRepo()
	service, _ := NewService(repo, inventory, &conversationRuntime{runType: agentv1.RunType_RUN_TYPE_SESSION_PLAN, complete: result})
	created, _ := service.CreateSession(context.Background(), actor, "room-1")

	aggregate, _, err := service.SubmitMessage(context.Background(), actor, SubmitCommand{
		SessionID: created.Session.ID, ExpectedVersion: 1, Content: "plan five lots",
		RunType: agentv1.RunType_RUN_TYPE_SESSION_PLAN, IdempotencyKey: "incomplete-plan",
	})

	if err != nil {
		t.Fatalf("SubmitMessage() error = %v", err)
	}
	if len(aggregate.Candidates) != 0 || aggregate.Session.Status != StatusCollectingInput {
		t.Fatalf("aggregate = %+v, want visible explanation without invalid candidates", aggregate)
	}
	if len(aggregate.Messages) != 2 || !strings.Contains(aggregate.Messages[1].Content, "从未开拍") {
		t.Fatalf("assistant explanation = %+v", aggregate.Messages)
	}
}

func TestAgentSessionSubmitMergesCompletedTurnAndIsIdempotent(t *testing.T) {
	repo := newConversationMemoryRepo()
	runtime := &conversationRuntime{complete: `{"assistant_message":"Draft is ready","candidate_patches":[{"candidate_id":"candidate-1","source_type":"NEW","fields":{"title":"Moon vase","startPriceAmount":0,"durationSeconds":120,"mainImageUrl":"https://example.test/a.jpg","minIncrementAmount":100,"antiSnipeWindowSeconds":30,"antiSnipeExtendSeconds":10,"maxExtendCount":2,"depositAmount":0},"field_sources":{"title":"AI","startPriceAmount":"AI","durationSeconds":"AI","mainImageUrl":"AI","minIncrementAmount":"AI","antiSnipeWindowSeconds":"AI","antiSnipeExtendSeconds":"AI","maxExtendCount":"AI","depositAmount":"AI"}}],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`}
	service, err := NewService(repo, roomFinder{room: &auction.Room{ID: "room-1", MainAccountID: "tenant-a"}}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	created, err := service.CreateSession(context.Background(), actor, "room-1")
	if err != nil {
		t.Fatal(err)
	}
	merged, run, err := service.SubmitMessage(context.Background(), actor, SubmitCommand{SessionID: created.Session.ID, ExpectedVersion: created.Session.Version, Content: "make a draft", RunType: agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST, IdempotencyKey: "turn-1"})
	if err != nil {
		t.Fatalf("SubmitMessage() error = %v", err)
	}
	if merged.Session.Status != StatusReadyForReview || len(merged.Messages) != 2 || len(merged.Candidates) != 1 || run.Status != agentcontrol.StatusCompleted {
		t.Fatalf("merged turn = %+v run=%+v", merged, run)
	}
	if !merged.Candidates[0].Candidate.StartPriceSpecified || merged.Candidates[0].Candidate.Draft.Title != "Moon vase" {
		t.Fatalf("candidate was not strictly merged: %+v", merged.Candidates[0])
	}
	if merged.Candidates[0].ProvenanceVersion != CurrentProvenanceVersion {
		t.Fatalf("new candidate provenance version = %d, want %d", merged.Candidates[0].ProvenanceVersion, CurrentProvenanceVersion)
	}
	replay, replayRun, err := service.SubmitMessage(context.Background(), actor, SubmitCommand{SessionID: created.Session.ID, ExpectedVersion: created.Session.Version, Content: "make a draft", RunType: agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST, IdempotencyKey: "turn-1"})
	if err != nil || replayRun.ID != run.ID || len(replay.Messages) != 2 || runtime.starts != 1 {
		t.Fatalf("idempotent replay = aggregate=%+v run=%+v starts=%d err=%v", replay, replayRun, runtime.starts, err)
	}
}

func TestAgentSessionInvalidCompletedTurnPreservesDraftAndAllowsRetry(t *testing.T) {
	repo := newConversationMemoryRepo()
	runtime := &conversationRuntime{complete: `{"assistant_message":"bad","candidate_patches":[],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`}
	service, _ := NewService(repo, roomFinder{room: &auction.Room{ID: "room-1", MainAccountID: "tenant-a"}}, runtime)
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	created, _ := service.CreateSession(context.Background(), actor, "room-1")
	aggregate, _, err := service.SubmitMessage(context.Background(), actor, SubmitCommand{SessionID: created.Session.ID, ExpectedVersion: 1, Content: "draft", RunType: agentv1.RunType_RUN_TYPE_SESSION_PLAN, IdempotencyKey: "bad-1"})
	if !errors.Is(err, apperr.ErrInvalidArgument) || len(aggregate.Messages) != 1 || len(aggregate.Candidates) != 0 || aggregate.Session.Status != StatusCollectingInput {
		t.Fatalf("invalid completed turn mutated aggregate=%+v err=%v", aggregate, err)
	}
}

func TestAgentSessionNeedsRevisionCannotBecomeReadyUntilAggregateIsValid(t *testing.T) {
	repo := newConversationMemoryRepo()
	invalidCandidate := serviceValidCandidate("candidate-invalid")
	invalidCandidate.Draft.Title = ""
	valid := serviceValidCandidate("candidate-valid")
	repo.sessions["session-1"] = SessionAggregate{Session: Session{ID: "session-1", TenantID: "tenant-a", OperatorID: "operator-a", RoomID: "room-1", Status: StatusNeedsRevision, Version: 4}, Candidates: []DraftCandidate{{Candidate: invalidCandidate, Position: 1, Version: 1}, {Candidate: valid, Position: 2, Version: 1}}}
	service, _ := NewService(repo, roomFinder{room: &auction.Room{ID: "room-1", MainAccountID: "tenant-a"}}, &conversationRuntime{})
	patched, err := service.PatchCandidate(context.Background(), Actor{TenantID: "tenant-a", OperatorID: "operator-a"}, PatchCandidateCommand{SessionID: "session-1", CandidateID: "candidate-valid", ExpectedSessionVersion: 4, ExpectedCandidateVersion: 1, Fields: map[string]json.RawMessage{"description": json.RawMessage(`"edited"`)}})
	if err != nil || patched.Session.Status != StatusNeedsRevision {
		t.Fatalf("invalid aggregate transitioned ready: status=%s err=%v", patched.Session.Status, err)
	}
	patched, err = service.PatchCandidate(context.Background(), Actor{TenantID: "tenant-a", OperatorID: "operator-a"}, PatchCandidateCommand{SessionID: "session-1", CandidateID: "candidate-invalid", ExpectedSessionVersion: 5, ExpectedCandidateVersion: 1, Fields: map[string]json.RawMessage{"title": json.RawMessage(`"fixed"`)}})
	if err != nil || patched.Session.Status != StatusReadyForReview {
		t.Fatalf("complete aggregate did not become ready: status=%s err=%v", patched.Session.Status, err)
	}
}

func TestAgentSessionPatchCandidateUsesFieldWhitelistCentsAndUserSources(t *testing.T) {
	repo := newConversationMemoryRepo()
	candidate := serviceValidCandidate("candidate-1")
	repo.sessions["session-1"] = SessionAggregate{
		Session:    Session{ID: "session-1", TenantID: "tenant-a", OperatorID: "operator-a", RoomID: "room-1", Status: StatusReadyForReview, Version: 7, ActiveSnapshotVersion: 3},
		Candidates: []DraftCandidate{{Candidate: candidate, Position: 1, SelectedByDefault: true, FieldSourcesJSON: `{"title":"AI","startPriceAmount":"AI"}`, ValidationJSON: `{}`, Version: 4}},
	}
	service, _ := NewService(repo, roomFinder{room: &auction.Room{ID: "room-1", MainAccountID: "tenant-a"}}, &conversationRuntime{})
	fields := map[string]json.RawMessage{
		"title": json.RawMessage(`"Ocean Heart"`), "startPriceAmount": json.RawMessage(`10000`),
		"durationSeconds": json.RawMessage(`3000`), "description": json.RawMessage(`"user edited"`),
		"capPriceAmount": json.RawMessage(`100000`), "tags": json.RawMessage(`["jewel"]`),
	}
	patched, err := service.PatchCandidate(context.Background(), Actor{TenantID: "tenant-a", OperatorID: "operator-a"}, PatchCandidateCommand{SessionID: "session-1", CandidateID: "candidate-1", ExpectedSessionVersion: 7, ExpectedCandidateVersion: 4, Fields: fields})
	if err != nil {
		t.Fatal(err)
	}
	got := patched.Candidates[0]
	if got.Candidate.Draft.StartPriceAmount != 10000 || got.Candidate.Draft.CapPriceAmount == nil || *got.Candidate.Draft.CapPriceAmount != 100000 {
		t.Fatalf("HTTP contract cents were converted: %+v", got.Candidate.Draft)
	}
	sources := parseSources(got.FieldSourcesJSON)
	for field := range fields {
		if sources[field] != "USER" {
			t.Fatalf("source[%s]=%q, want USER", field, sources[field])
		}
	}
	if got.Version != 5 || patched.Session.Version != 8 || patched.Session.ActiveSnapshotVersion != 0 {
		t.Fatalf("versions/snapshot = candidate %d session %d snapshot %d", got.Version, patched.Session.Version, patched.Session.ActiveSnapshotVersion)
	}

	before := cloneAggregate(repo.sessions["session-1"])
	_, err = service.PatchCandidate(context.Background(), Actor{TenantID: "tenant-a", OperatorID: "operator-a"}, PatchCandidateCommand{SessionID: "session-1", CandidateID: "candidate-1", ExpectedSessionVersion: 8, ExpectedCandidateVersion: 5, Fields: map[string]json.RawMessage{"existingLotId": json.RawMessage(`"lot-spoof"`)}})
	if !errors.Is(err, apperr.ErrInvalidArgument) || repo.sessions["session-1"].Session.Version != before.Session.Version {
		t.Fatalf("unknown/identity field mutated aggregate: err=%v aggregate=%+v", err, repo.sessions["session-1"])
	}
	patched, err = service.PatchCandidate(context.Background(), Actor{TenantID: "tenant-a", OperatorID: "operator-a"}, PatchCandidateCommand{SessionID: "session-1", CandidateID: "candidate-1", ExpectedSessionVersion: 8, ExpectedCandidateVersion: 5, Fields: map[string]json.RawMessage{"title": json.RawMessage(`null`)}})
	if err != nil || patched.Session.Status != StatusNeedsRevision || patched.Candidates[0].Candidate.Draft.Title != "" || parseSources(patched.Candidates[0].FieldSourcesJSON)["title"] != "USER" {
		t.Fatalf("required-field removal did not invalidate review: aggregate=%+v err=%v", patched, err)
	}
}

func TestAgentSessionPatchCandidateRejectsStaleCandidateVersion(t *testing.T) {
	repo := newConversationMemoryRepo()
	candidate := serviceValidCandidate("candidate-1")
	repo.sessions["session-1"] = SessionAggregate{Session: Session{ID: "session-1", TenantID: "tenant-a", OperatorID: "operator-a", Status: StatusReadyForReview, Version: 2}, Candidates: []DraftCandidate{{Candidate: candidate, Position: 1, Version: 3}}}
	service, _ := NewService(repo, roomFinder{}, &conversationRuntime{})
	_, err := service.PatchCandidate(context.Background(), Actor{TenantID: "tenant-a", OperatorID: "operator-a"}, PatchCandidateCommand{SessionID: "session-1", CandidateID: "candidate-1", ExpectedSessionVersion: 2, ExpectedCandidateVersion: 2, Fields: map[string]json.RawMessage{"title": json.RawMessage(`"stale"`)}})
	if !errors.Is(err, apperr.ErrAgentSessionVersionConflict) || repo.sessions["session-1"].Session.Version != 2 {
		t.Fatalf("stale candidate patch err=%v aggregate=%+v", err, repo.sessions["session-1"])
	}
}

func TestAgentSessionPatchCandidateAllowedFieldWhitelist(t *testing.T) {
	candidate := serviceValidCandidate("candidate-1")
	allowed := map[string]json.RawMessage{
		"title": json.RawMessage(`"title"`), "startPriceAmount": json.RawMessage(`123`),
		"durationSeconds": json.RawMessage(`180`), "minIncrementAmount": json.RawMessage(`10`),
		"capPriceAmount": json.RawMessage(`1000`), "category": json.RawMessage(`"jewelry"`),
		"description": json.RawMessage(`"description"`), "tags": json.RawMessage(`["one","two"]`),
		"stock": json.RawMessage(`2`), "antiSnipeWindowSeconds": json.RawMessage(`20`),
		"antiSnipeExtendSeconds": json.RawMessage(`15`), "maxExtendCount": json.RawMessage(`3`),
		"depositAmount": json.RawMessage(`50`),
	}
	for field, value := range allowed {
		t.Run(field, func(t *testing.T) {
			copy := candidate
			if err := setUserCandidateField(&copy, field, value); err != nil {
				t.Fatalf("allowed field rejected: %v", err)
			}
		})
	}
	for _, field := range []string{"id", "sourceType", "existingLotId", "existingLotReadVersion", "mainImageAssetId", "mainImageUrl", "position", "selectedByDefault", "version"} {
		t.Run("reject_"+field, func(t *testing.T) {
			copy := candidate
			if err := setUserCandidateField(&copy, field, json.RawMessage(`1`)); !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Fatalf("protected field error=%v", err)
			}
		})
	}
	if err := setUserCandidateField(&candidate, "capPriceAmount", json.RawMessage(`null`)); err != nil || candidate.Draft.CapPriceAmount != nil {
		t.Fatalf("merge-patch null did not remove optional cap price: %+v err=%v", candidate.Draft.CapPriceAmount, err)
	}
}

func TestAgentSessionPatchCandidateNullPersistsInvalidDraftAndProvenance(t *testing.T) {
	for _, field := range []string{"title", "startPriceAmount", "durationSeconds", "minIncrementAmount", "capPriceAmount", "category", "description", "tags", "stock", "antiSnipeWindowSeconds", "antiSnipeExtendSeconds", "maxExtendCount", "depositAmount"} {
		t.Run(field, func(t *testing.T) {
			repo := newConversationMemoryRepo()
			candidate := serviceValidCandidate("candidate-1")
			capAmount := int64(1000)
			candidate.Draft.CapPriceAmount = &capAmount
			candidate.Draft.Category = "category"
			candidate.Draft.Description = "description"
			candidate.Draft.Tags = []string{"tag"}
			candidate.Draft.Stock = 2
			repo.sessions["session-1"] = SessionAggregate{
				Session:    Session{ID: "session-1", TenantID: "tenant-a", OperatorID: "operator-a", Status: StatusReadyForReview, Version: 7, ActiveSnapshotVersion: 4},
				Candidates: []DraftCandidate{{Candidate: candidate, Position: 1, FieldSourcesJSON: `{}`, ValidationJSON: `{}`, Version: 3}},
			}
			service, _ := NewService(repo, roomFinder{}, &conversationRuntime{})
			patched, err := service.PatchCandidate(context.Background(), Actor{TenantID: "tenant-a", OperatorID: "operator-a"}, PatchCandidateCommand{
				SessionID: "session-1", CandidateID: "candidate-1", ExpectedSessionVersion: 7, ExpectedCandidateVersion: 3,
				Fields: map[string]json.RawMessage{field: json.RawMessage(`null`)},
			})
			if err != nil {
				t.Fatalf("null patch rejected: %v", err)
			}
			got := patched.Candidates[0]
			if got.Version != 4 || patched.Session.Version != 8 || patched.Session.ActiveSnapshotVersion != 0 || parseSources(got.FieldSourcesJSON)[field] != "USER" {
				t.Fatalf("coupled update not persisted: session=%+v candidate=%+v", patched.Session, got)
			}
			if got.ValidationJSON == "" || got.ValidationJSON == `{}` {
				t.Fatalf("validation not persisted: %q", got.ValidationJSON)
			}
			if ValidateCandidate(got.Candidate) != nil && patched.Session.Status != StatusNeedsRevision {
				t.Fatalf("invalid draft status=%s validation=%s", patched.Session.Status, got.ValidationJSON)
			}
			if ValidateCandidate(got.Candidate) == nil && patched.Session.Status != StatusReadyForReview {
				t.Fatalf("valid optional removal status=%s", patched.Session.Status)
			}
		})
	}
}

func TestAgentSessionSubmitRejectsActiveAndTerminalSessions(t *testing.T) {
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	for name, status := range map[string]SessionStatus{"active": StatusCollectingInput, "cancelled": StatusCancelled, "applying": StatusApplying, "applied": StatusApplied} {
		t.Run(name, func(t *testing.T) {
			repo := newConversationMemoryRepo()
			runs := []agentcontrol.Run(nil)
			if name == "active" {
				runs = []agentcontrol.Run{{ID: "run-active", Status: agentcontrol.StatusPlanning, SessionVersion: 1}}
			}
			repo.sessions["session-1"] = SessionAggregate{Session: Session{ID: "session-1", TenantID: actor.TenantID, OperatorID: actor.OperatorID, RoomID: "room-1", Status: status, Version: 1}, Runs: runs}
			service, _ := NewService(repo, roomFinder{room: &auction.Room{ID: "room-1", MainAccountID: actor.TenantID}}, &conversationRuntime{})
			_, _, err := service.SubmitMessage(context.Background(), actor, SubmitCommand{SessionID: "session-1", ExpectedVersion: 1, Content: "next", RunType: agentv1.RunType_RUN_TYPE_SESSION_PLAN, IdempotencyKey: "next-1"})
			if !errors.Is(err, apperr.ErrConflict) && !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Fatalf("SubmitMessage() err=%v", err)
			}
		})
	}
}

func TestAgentSessionSSEComposesRunLocalSequences(t *testing.T) {
	repo := newConversationMemoryRepo()
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	repo.sessions["session-1"] = SessionAggregate{Session: Session{ID: "session-1", TenantID: actor.TenantID, OperatorID: actor.OperatorID, Version: 3}, Runs: []agentcontrol.Run{{ID: "run-1", Status: agentcontrol.StatusCompleted}, {ID: "run-2", Status: agentcontrol.StatusPlanning}}}
	runtime := &eventRuntime{events: map[string][]agentcontrol.RuntimeEvent{"run-1": {{RunID: "run-1", Seq: 1}, {RunID: "run-1", Seq: 2}}, "run-2": {{RunID: "run-2", Seq: 1}, {RunID: "run-2", Seq: 2}}}}
	service, _ := NewService(repo, roomFinder{}, runtime)
	stream, err := service.StreamEvents(context.Background(), actor, "session-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for _, want := range []int64{2, 3, 4} {
		event, err := stream.Recv()
		if err != nil || event.Seq != want {
			t.Fatalf("event=(%+v,%v), want seq %d", event, err, want)
		}
	}
}

func TestAgentSessionSSERejectsGappedLocalSequence(t *testing.T) {
	repo := newConversationMemoryRepo()
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	repo.sessions["s"] = SessionAggregate{Session: Session{ID: "s", TenantID: actor.TenantID, OperatorID: actor.OperatorID}, Runs: []agentcontrol.Run{{ID: "r", Status: agentcontrol.StatusPlanning}}}
	service, _ := NewService(repo, roomFinder{}, &eventRuntime{events: map[string][]agentcontrol.RuntimeEvent{"r": {{RunID: "r", Seq: 2}}}})
	stream, _ := service.StreamEvents(context.Background(), actor, "s", 0)
	_, err := stream.Recv()
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("gap err=%v", err)
	}
}

func TestAgentSessionRejectsBlankTerminalAssistantMessage(t *testing.T) {
	_, err := decodeTurnResult(`{"assistant_message":" ","candidate_patches":[],"required_inputs":[],"review_readiness":"NEEDS_INPUT","warnings":[]}`)
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("decodeTurnResult() err=%v", err)
	}
}

func TestAgentSessionRecoverMergesLegacyCompletedRunsInOrderExactlyOnce(t *testing.T) {
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	repo := newConversationMemoryRepo()
	run1 := agentcontrol.Run{ID: "run-1", Status: agentcontrol.StatusCompleted, SessionVersion: 2, CreatedAtUnixMs: 10, TurnResultJSON: `{"assistant_message":"first","candidate_patches":[],"required_inputs":[],"review_readiness":"NEEDS_INPUT","warnings":[]}`}
	run2 := agentcontrol.Run{ID: "run-2", Status: agentcontrol.StatusCompleted, SessionVersion: 3, CreatedAtUnixMs: 20, TurnResultJSON: `{"assistant_message":"second","candidate_patches":[],"required_inputs":[],"review_readiness":"NEEDS_INPUT","warnings":[]}`}
	repo.sessions["session-1"] = SessionAggregate{Session: Session{ID: "session-1", TenantID: actor.TenantID, OperatorID: actor.OperatorID, RoomID: "room-1", Status: StatusCollectingInput, Version: 4}, Runs: []agentcontrol.Run{run2, run1}}
	runtime := &conversationRuntime{runs: map[string]agentcontrol.Run{"run-1": run1, "run-2": run2}}
	service, _ := NewService(repo, roomFinder{}, runtime)
	aggregate, err := service.Recover(context.Background(), actor, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregate.Messages) != 2 || aggregate.Messages[0].RunID != "run-1" || aggregate.Messages[1].RunID != "run-2" {
		t.Fatalf("legacy recovery=%+v", aggregate.Messages)
	}
	again, err := service.Recover(context.Background(), actor, "session-1")
	if err != nil || len(again.Messages) != 2 {
		t.Fatalf("repeat recovery=%+v err=%v", again.Messages, err)
	}
}

func TestAgentSessionRecoverBlankAssistantPreservesRetryableAggregate(t *testing.T) {
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	repo := newConversationMemoryRepo()
	run := agentcontrol.Run{ID: "run-1", Status: agentcontrol.StatusCompleted, SessionVersion: 1, TurnResultJSON: `{"assistant_message":"","candidate_patches":[],"required_inputs":[],"review_readiness":"NEEDS_INPUT","warnings":[]}`}
	repo.sessions["s"] = SessionAggregate{Session: Session{ID: "s", TenantID: actor.TenantID, OperatorID: actor.OperatorID, Status: StatusCollectingInput, Version: 1}, Runs: []agentcontrol.Run{run}}
	service, _ := NewService(repo, roomFinder{}, &conversationRuntime{runs: map[string]agentcontrol.Run{"run-1": run}})
	got, err := service.Recover(context.Background(), actor, "s")
	if err != nil || got.Session.Version != 1 || got.Session.ConversationSummary != "" || len(got.Messages) != 0 || len(got.Candidates) != 0 || got.Session.Status != StatusCollectingInput {
		t.Fatalf("blank recovery=%+v err=%v", got, err)
	}
	run.TurnResultJSON = `{"assistant_message":"retry succeeds","candidate_patches":[],"required_inputs":[],"review_readiness":"NEEDS_INPUT","warnings":[]}`
	runtime := service.runtime.(*conversationRuntime)
	runtime.runs[run.ID] = run
	retried, err := service.Recover(context.Background(), actor, "s")
	if err != nil || retried.Session.Version != 2 || len(retried.Messages) != 1 || retried.Messages[0].Content != "retry succeeds" {
		t.Fatalf("valid recovery retry=%+v err=%v", retried, err)
	}
}

func TestAgentSessionRecoverRetriesMergeCASDeterministically(t *testing.T) {
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	run := agentcontrol.Run{ID: "run-cas", Status: agentcontrol.StatusCompleted, SessionVersion: 2, TurnResultJSON: `{"assistant_message":"merged once","candidate_patches":[],"required_inputs":[],"review_readiness":"NEEDS_INPUT","warnings":[]}`}
	base := newConversationMemoryRepo()
	base.sessions["s"] = SessionAggregate{Session: Session{ID: "s", TenantID: actor.TenantID, OperatorID: actor.OperatorID, Status: StatusCollectingInput, Version: 4}, Runs: []agentcontrol.Run{run}}
	repo := &mergeConflictRepository{Repository: base, conflictsRemaining: 1}
	service, _ := NewService(repo, roomFinder{}, &conversationRuntime{runs: map[string]agentcontrol.Run{run.ID: run}})
	got, err := service.Recover(context.Background(), actor, "s")
	if err != nil || repo.mergeCalls != 2 || got.Session.Version != 5 || len(got.Messages) != 1 || got.Messages[0].RunID != run.ID {
		t.Fatalf("Recover() after first CAS conflict = %+v calls=%d err=%v", got, repo.mergeCalls, err)
	}
	again, err := service.Recover(context.Background(), actor, "s")
	if err != nil || repo.mergeCalls != 2 || again.Session.Version != 5 || len(again.Messages) != 1 {
		t.Fatalf("repeated Recover() = %+v calls=%d err=%v", again, repo.mergeCalls, err)
	}
}

func TestAgentSessionRecoverExhaustsMergeCASWithoutStaleSuccess(t *testing.T) {
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	run := agentcontrol.Run{ID: "run-cas", Status: agentcontrol.StatusCompleted, SessionVersion: 2, TurnResultJSON: `{"assistant_message":"must not merge","candidate_patches":[],"required_inputs":[],"review_readiness":"NEEDS_INPUT","warnings":[]}`}
	base := newConversationMemoryRepo()
	base.sessions["s"] = SessionAggregate{Session: Session{ID: "s", TenantID: actor.TenantID, OperatorID: actor.OperatorID, Status: StatusCollectingInput, Version: 4}, Runs: []agentcontrol.Run{run}}
	repo := &mergeConflictRepository{Repository: base, conflictsRemaining: 10}
	service, _ := NewService(repo, roomFinder{}, &conversationRuntime{runs: map[string]agentcontrol.Run{run.ID: run}})
	got, err := service.Recover(context.Background(), actor, "s")
	if !errors.Is(err, apperr.ErrAgentSessionVersionConflict) || repo.mergeCalls != 3 || got.Session.Version != 4 || len(got.Messages) != 0 {
		t.Fatalf("Recover() exhausted CAS = %+v calls=%d err=%v", got, repo.mergeCalls, err)
	}
	persisted, findErr := base.FindSession(context.Background(), actor.TenantID, "s")
	if findErr != nil || persisted.Session.Version != 4 || len(persisted.Messages) != 0 || len(persisted.Candidates) != 0 {
		t.Fatalf("persisted aggregate after exhausted CAS = %+v err=%v", persisted, findErr)
	}
}

func TestAgentSessionSubmitBlankTerminalResultRemainsRetryable(t *testing.T) {
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	repo := newConversationMemoryRepo()
	runtime := &projectingConversationRuntime{repo: repo, results: []string{
		`{"assistant_message":"","candidate_patches":[],"required_inputs":[],"review_readiness":"NEEDS_INPUT","warnings":[]}`,
		`{"assistant_message":"valid retry","candidate_patches":[],"required_inputs":[],"review_readiness":"NEEDS_INPUT","warnings":[]}`,
	}}
	service, _ := NewService(repo, roomFinder{room: &auction.Room{ID: "room-1", MainAccountID: actor.TenantID}}, runtime)
	created, err := service.CreateSession(context.Background(), actor, "room-1")
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := service.SubmitMessage(context.Background(), actor, SubmitCommand{SessionID: created.Session.ID, ExpectedVersion: 1, Content: "blank", RunType: agentv1.RunType_RUN_TYPE_SESSION_PLAN, IdempotencyKey: "blank"})
	if !errors.Is(err, apperr.ErrInvalidArgument) || first.Session.Version != 2 || len(first.Messages) != 1 || len(first.Candidates) != 0 {
		t.Fatalf("blank SubmitMessage() = %+v err=%v", first, err)
	}
	retried, _, err := service.SubmitMessage(context.Background(), actor, SubmitCommand{SessionID: created.Session.ID, ExpectedVersion: 2, Content: "retry", RunType: agentv1.RunType_RUN_TYPE_SESSION_PLAN, IdempotencyKey: "retry"})
	if err != nil || retried.Session.Version != 4 || len(retried.Messages) != 3 || retried.Messages[2].Content != "valid retry" {
		t.Fatalf("retry SubmitMessage() = %+v err=%v", retried, err)
	}
}

func TestAgentSessionRecoverDoesNotResurrectTerminalSession(t *testing.T) {
	for _, state := range []SessionStatus{StatusCancelled, StatusApplying, StatusApplied} {
		t.Run(string(state), func(t *testing.T) {
			actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
			repo := newConversationMemoryRepo()
			run := agentcontrol.Run{ID: "old", Status: agentcontrol.StatusCompleted, TurnResultJSON: `{"assistant_message":"old","candidate_patches":[],"required_inputs":[],"review_readiness":"NEEDS_INPUT","warnings":[]}`}
			repo.sessions["s"] = SessionAggregate{Session: Session{ID: "s", TenantID: actor.TenantID, OperatorID: actor.OperatorID, Status: state, Version: 9}, Runs: []agentcontrol.Run{run}}
			service, _ := NewService(repo, roomFinder{}, &conversationRuntime{runs: map[string]agentcontrol.Run{"old": run}})
			got, err := service.Recover(context.Background(), actor, "s")
			if err != nil || got.Session.Version != 9 || got.Session.Status != state || len(got.Messages) != 0 || len(got.Candidates) != 0 {
				t.Fatalf("terminal recovery=%+v err=%v", got, err)
			}
		})
	}
}

func TestAgentSessionMaterializesTerminalBeforeNextSubmit(t *testing.T) {
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	repo := newConversationMemoryRepo()
	old := agentcontrol.Run{ID: "old", Status: agentcontrol.StatusCompleted, SessionVersion: 1, TurnResultJSON: `{"assistant_message":"done","candidate_patches":[],"required_inputs":[],"review_readiness":"NEEDS_INPUT","warnings":[]}`}
	repo.sessions["s"] = SessionAggregate{Session: Session{ID: "s", TenantID: actor.TenantID, OperatorID: actor.OperatorID, Status: StatusCollectingInput, Version: 1}, Runs: []agentcontrol.Run{old}}
	runtime := &conversationRuntime{complete: `{"assistant_message":"next","candidate_patches":[],"required_inputs":[],"review_readiness":"NEEDS_INPUT","warnings":[]}`, runs: map[string]agentcontrol.Run{"old": old}}
	service, _ := NewService(repo, roomFinder{}, runtime)
	got, _, err := service.SubmitMessage(context.Background(), actor, SubmitCommand{SessionID: "s", ExpectedVersion: 2, Content: "next", RunType: agentv1.RunType_RUN_TYPE_SESSION_PLAN, IdempotencyKey: "next"})
	if err != nil || len(got.Messages) != 3 {
		t.Fatalf("materialize/submit=%+v err=%v", got, err)
	}
}

func serviceValidCandidate(id string) Candidate {
	return Candidate{ID: id, SourceType: SourceNew, StartPriceSpecified: true, Draft: CandidateDraft{Title: "valid", StartPriceAmount: 0, DurationSeconds: 120, MainImageURL: "https://example.test/a.jpg", MinIncrementAmount: 1, AntiSnipeWindowSeconds: 30, AntiSnipeExtendSeconds: 10, MaxExtendCount: 1, DepositAmount: 0}}
}

type roomFinder struct{ room *auction.Room }

func (f roomFinder) FindRoomByID(context.Context, string) (*auction.Room, bool, error) {
	return f.room, true, nil
}

type trustedInventoryRoomFinder struct {
	room *auction.Room
	lots []*auctionv1.Lot
}

func (f trustedInventoryRoomFinder) FindRoomByID(context.Context, string) (*auction.Room, bool, error) {
	return f.room, true, nil
}

func (f trustedInventoryRoomFinder) ListLots(_ context.Context, query auction.LotQuery) (auction.LotList, error) {
	filtered := make([]*auctionv1.Lot, 0, len(f.lots))
	for _, lot := range f.lots {
		if lot.GetMainAccountId() == query.MainAccountID && lot.GetStatus() == query.Status {
			filtered = append(filtered, lot)
		}
	}
	return auction.LotList{Lots: filtered}, nil
}

type conversationRuntime struct {
	complete string
	starts   int
	runs     map[string]agentcontrol.Run
	runType  agentv1.RunType
}

func (r *conversationRuntime) StartPersistedSessionRun(_ context.Context, _ agentcontrol.Actor, id string) (agentcontrol.Run, error) {
	r.starts++
	return agentcontrol.Run{ID: id, Status: agentcontrol.StatusCompleted, SessionVersion: 2, RunType: r.runType, TurnResultJSON: r.complete}, nil
}
func (r *conversationRuntime) GetRun(_ context.Context, _ agentcontrol.Actor, id string) (agentcontrol.Run, error) {
	if run, ok := r.runs[id]; ok {
		return run, nil
	}
	return agentcontrol.Run{}, nil
}
func (r *conversationRuntime) StreamRunEvents(context.Context, agentcontrol.Actor, string, int64) (agentcontrol.EventStream, error) {
	return nil, apperr.ErrNotFound
}

type eventRuntime struct {
	events map[string][]agentcontrol.RuntimeEvent
}

func (r *eventRuntime) StartPersistedSessionRun(context.Context, agentcontrol.Actor, string) (agentcontrol.Run, error) {
	return agentcontrol.Run{}, nil
}
func (r *eventRuntime) GetRun(context.Context, agentcontrol.Actor, string) (agentcontrol.Run, error) {
	return agentcontrol.Run{}, nil
}
func (r *eventRuntime) StreamRunEvents(_ context.Context, _ agentcontrol.Actor, id string, _ int64) (agentcontrol.EventStream, error) {
	return &sliceEventStream{events: append([]agentcontrol.RuntimeEvent(nil), r.events[id]...)}, nil
}

type sliceEventStream struct{ events []agentcontrol.RuntimeEvent }

func (s *sliceEventStream) Recv() (agentcontrol.RuntimeEvent, error) {
	if len(s.events) == 0 {
		return agentcontrol.RuntimeEvent{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}
func (s *sliceEventStream) Close() error { return nil }

type conversationMemoryRepo struct{ sessions map[string]SessionAggregate }

type mergeConflictRepository struct {
	Repository
	conflictsRemaining int
	mergeCalls         int
}

func (r *mergeConflictRepository) MergeTurnResult(ctx context.Context, command MergeTurnCommand) (SessionAggregate, error) {
	r.mergeCalls++
	if r.conflictsRemaining > 0 {
		r.conflictsRemaining--
		return SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
	}
	return r.Repository.MergeTurnResult(ctx, command)
}

type projectingConversationRuntime struct {
	repo    *conversationMemoryRepo
	results []string
	starts  int
}

func (r *projectingConversationRuntime) StartPersistedSessionRun(_ context.Context, _ agentcontrol.Actor, id string) (agentcontrol.Run, error) {
	result := r.results[r.starts]
	r.starts++
	var projected agentcontrol.Run
	for sessionID, aggregate := range r.repo.sessions {
		for i := range aggregate.Runs {
			if aggregate.Runs[i].ID != id {
				continue
			}
			aggregate.Runs[i].Status = agentcontrol.StatusCompleted
			aggregate.Runs[i].TurnResultJSON = result
			projected = aggregate.Runs[i]
			r.repo.sessions[sessionID] = aggregate
		}
	}
	return projected, nil
}
func (r *projectingConversationRuntime) GetRun(_ context.Context, _ agentcontrol.Actor, id string) (agentcontrol.Run, error) {
	for _, aggregate := range r.repo.sessions {
		for _, run := range aggregate.Runs {
			if run.ID == id {
				return run, nil
			}
		}
	}
	return agentcontrol.Run{}, apperr.ErrNotFound
}
func (r *projectingConversationRuntime) StreamRunEvents(context.Context, agentcontrol.Actor, string, int64) (agentcontrol.EventStream, error) {
	return nil, apperr.ErrNotFound
}

func newConversationMemoryRepo() *conversationMemoryRepo {
	return &conversationMemoryRepo{sessions: map[string]SessionAggregate{}}
}
func (r *conversationMemoryRepo) CreateSession(_ context.Context, s Session) error {
	r.sessions[s.ID] = SessionAggregate{Session: s}
	return nil
}
func (r *conversationMemoryRepo) FindSession(_ context.Context, tenant, id string) (SessionAggregate, error) {
	value, ok := r.sessions[id]
	if !ok || value.Session.TenantID != tenant {
		return SessionAggregate{}, apperr.ErrNotFound
	}
	return cloneAggregate(value), nil
}
func (r *conversationMemoryRepo) AppendUserMessageAndStartRun(_ context.Context, c StartTurnCommand) (SessionAggregate, error) {
	a, err := r.FindSession(context.Background(), c.TenantID, c.SessionID)
	if err != nil {
		return SessionAggregate{}, err
	}
	if a.Session.Version != c.ExpectedVersion {
		return SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
	}
	a.Session.Version++
	a.Session.Status = StatusCollectingInput
	c.Run.SessionVersion = a.Session.Version
	a.Messages = append(a.Messages, Message{SessionID: c.SessionID, Seq: int64(len(a.Messages) + 1), Role: MessageRoleUser, Content: c.Message.Content})
	a.Runs = append(a.Runs, c.Run)
	r.sessions[c.SessionID] = a
	return cloneAggregate(a), nil
}
func (r *conversationMemoryRepo) MergeTurnResult(_ context.Context, c MergeTurnCommand) (SessionAggregate, error) {
	a, err := r.FindSession(context.Background(), c.TenantID, c.SessionID)
	if err != nil {
		return SessionAggregate{}, err
	}
	if a.Session.Version != c.ExpectedVersion {
		return SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
	}
	a.Session.Version++
	a.Session.Status = c.Status
	a.Session.ConversationSummary = c.ConversationSummary
	a.Candidates = c.Candidates
	a.Messages = append(a.Messages, Message{SessionID: c.SessionID, Seq: int64(len(a.Messages) + 1), Role: MessageRoleAssistant, Content: c.AssistantMessage.Content, RunID: c.RunID})
	r.sessions[c.SessionID] = a
	return cloneAggregate(a), nil
}
func (r *conversationMemoryRepo) PatchCandidate(_ context.Context, c PatchCandidateCommand) (SessionAggregate, error) {
	a, err := r.FindSession(context.Background(), c.TenantID, c.SessionID)
	if err != nil {
		return SessionAggregate{}, err
	}
	if a.Session.OperatorID != c.OperatorID || a.Session.Version != c.ExpectedSessionVersion {
		return SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
	}
	found := false
	for i := range a.Candidates {
		if a.Candidates[i].Candidate.ID == c.CandidateID {
			if a.Candidates[i].Version != c.ExpectedCandidateVersion {
				return SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
			}
			c.Candidate.Version = a.Candidates[i].Version + 1
			a.Candidates[i] = c.Candidate
			found = true
		}
	}
	if !found {
		return SessionAggregate{}, apperr.ErrNotFound
	}
	a.Session.Version++
	a.Session.Status = c.Status
	a.Session.ActiveSnapshotVersion = 0
	r.sessions[c.SessionID] = a
	return cloneAggregate(a), nil
}
func (r *conversationMemoryRepo) CancelSession(_ context.Context, tenant, operator, id string, version int64) error {
	a, err := r.FindSession(context.Background(), tenant, id)
	if err != nil {
		return err
	}
	if a.Session.OperatorID != operator || a.Session.Version != version {
		return apperr.ErrAgentSessionVersionConflict
	}
	a.Session.Status = StatusCancelled
	a.Session.Version++
	r.sessions[id] = a
	return nil
}
func cloneAggregate(a SessionAggregate) SessionAggregate {
	a.Messages = append([]Message(nil), a.Messages...)
	a.Candidates = append([]DraftCandidate(nil), a.Candidates...)
	a.Runs = append([]agentcontrol.Run(nil), a.Runs...)
	return a
}
