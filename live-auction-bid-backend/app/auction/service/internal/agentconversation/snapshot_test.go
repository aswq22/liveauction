package agentconversation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

func TestReviewSnapshotRequiresExplicitNonemptyMatchingSelectionAndOrder(t *testing.T) {
	aggregate := snapshotAggregate()
	service, _ := NewService(&snapshotMemoryRepository{conversationMemoryRepo: newConversationMemoryRepo(), aggregate: aggregate}, roomFinder{}, &conversationRuntime{})
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	tests := []struct {
		name     string
		selected []string
		ordered  []string
	}{
		{"empty", nil, nil},
		{"set mismatch", []string{"new-1"}, []string{"existing-1"}},
		{"selected duplicate", []string{"new-1", "new-1"}, []string{"new-1"}},
		{"ordered duplicate", []string{"new-1"}, []string{"new-1", "new-1"}},
		{"unknown", []string{"missing"}, []string{"missing"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := service.CreateReviewSnapshot(context.Background(), actor, CreateReviewSnapshotInput{SessionID: "session-1", ExpectedSessionVersion: 7, SelectedCandidateIDs: tt.selected, OrderedCandidateIDs: tt.ordered})
			if !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Fatalf("CreateReviewSnapshot() error = %v, want invalid argument", err)
			}
		})
	}
}

func containsSnapshotSecretOrRawImage(value string) bool {
	return strings.Contains(value, "signed.example") || strings.Contains(value, "do-not-store") || strings.Contains(value, "mainImageUrl")
}

func TestReviewSnapshotCanonicalHashIsStableAndCapturesOrderVersionsSourcesAndAssets(t *testing.T) {
	aggregate := snapshotAggregate()
	first, err := BuildReviewSnapshot(aggregate, "operator-a", []string{"existing-1", "new-1"}, []string{"new-1", "existing-1"})
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild field provenance with a different map insertion order. Canonical
	// JSON and its SHA-256 must remain byte-for-byte stable.
	aggregate.Candidates[1].FieldSourcesJSON = completeSnapshotSources("USER")
	second, err := BuildReviewSnapshot(aggregate, "operator-a", []string{"new-1", "existing-1"}, []string{"new-1", "existing-1"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentHash != second.ContentHash || string(first.SnapshotJSON) != string(second.SnapshotJSON) {
		t.Fatalf("canonical snapshot changed:\n%s\n%s", first.SnapshotJSON, second.SnapshotJSON)
	}
	if len(first.Candidates) != 2 || first.Candidates[0].CandidateID != "new-1" || first.Candidates[1].CandidateID != "existing-1" {
		t.Fatalf("candidate order = %#v", first.Candidates)
	}
	if first.Candidates[1].ExistingLotID != "lot-7" || first.Candidates[1].ExistingLotReadVersion == nil || *first.Candidates[1].ExistingLotReadVersion != 23 {
		t.Fatalf("existing lot identity/version missing: %#v", first.Candidates[1])
	}
	if got := first.Candidates[0].AssetIDs; len(got) != 1 || got[0] != "asset-1" {
		t.Fatalf("asset IDs = %#v", got)
	}
	if string(first.SnapshotJSON) == "" || json.Valid(first.SnapshotJSON) == false {
		t.Fatal("snapshot JSON is not valid")
	}
	if containsSnapshotSecretOrRawImage(string(first.SnapshotJSON)) {
		t.Fatalf("snapshot contains unstable or sensitive content: %s", first.SnapshotJSON)
	}
}

func TestReviewSnapshotRequiresCompleteExactExecutableFieldProvenance(t *testing.T) {
	for _, tt := range []struct {
		name    string
		sources string
	}{
		{"missing", `{"title":"USER"}`},
		{"unknown", strings.TrimSuffix(completeSnapshotSources("USER"), "}") + `,"providerSecret":"USER"}`},
		{"empty", strings.Replace(completeSnapshotSources("USER"), `"title":"USER"`, `"title":""`, 1)},
		{"invalid", strings.Replace(completeSnapshotSources("USER"), `"title":"USER"`, `"title":"MODEL"`, 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			aggregate := snapshotAggregate()
			aggregate.Candidates[0].FieldSourcesJSON = tt.sources
			if _, err := BuildReviewSnapshot(aggregate, "operator-a", []string{"existing-1"}, []string{"existing-1"}); !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Fatalf("BuildReviewSnapshot() error = %v", err)
			}
		})
	}
	aggregate := snapshotAggregate()
	if _, err := BuildReviewSnapshot(aggregate, "operator-a", []string{"new-1", "existing-1"}, []string{"existing-1", "new-1"}); err != nil {
		t.Fatalf("complete current provenance rejected: %v", err)
	}
}

func TestReviewSnapshotNormalizesOnlyLegacyMainImageURLProvenance(t *testing.T) {
	aggregate := snapshotAggregate()
	canonical, err := BuildReviewSnapshot(aggregate, "operator-a", []string{"new-1"}, []string{"new-1"})
	if err != nil {
		t.Fatal(err)
	}
	aggregate.Candidates[1].FieldSourcesJSON = strings.TrimSuffix(completeSnapshotSources("USER"), "}") + `,"mainImageUrl":"USER"}`
	legacy, err := BuildReviewSnapshot(aggregate, "operator-a", []string{"new-1"}, []string{"new-1"})
	if err != nil {
		t.Fatalf("legacy display provenance rejected: %v", err)
	}
	if legacy.ContentHash != canonical.ContentHash || string(legacy.SnapshotJSON) != string(canonical.SnapshotJSON) {
		t.Fatalf("legacy display provenance changed canonical content:\n%s\n%s", canonical.SnapshotJSON, legacy.SnapshotJSON)
	}
	aggregate.Candidates[1].ProvenanceVersion = CurrentProvenanceVersion
	if _, err := BuildReviewSnapshot(aggregate, "operator-a", []string{"new-1"}, []string{"new-1"}); !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("current candidate accepted legacy display provenance: %v", err)
	}
}

func TestReviewSnapshotBackfillsOnlyMissingLegacyOptionalZeroValueProvenance(t *testing.T) {
	aggregate := snapshotAggregate()
	candidate := &aggregate.Candidates[1]
	candidate.Candidate.Draft.CapPriceAmount = nil
	candidate.Candidate.Draft.Category = ""
	candidate.Candidate.Draft.Description = ""
	candidate.Candidate.Draft.Tags = nil
	candidate.Candidate.Draft.DepositAmount = 0
	sources := parseSources(candidate.FieldSourcesJSON)
	for _, field := range []string{"capPriceAmount", "category", "description", "tags", "depositAmount"} {
		delete(sources, field)
	}
	candidate.FieldSourcesJSON = fieldSourcesJSON(sources)

	snapshot, err := BuildReviewSnapshot(aggregate, "operator-a", []string{"new-1"}, []string{"new-1"})
	if err != nil {
		t.Fatalf("valid recovered legacy candidate rejected: %v", err)
	}
	for _, source := range snapshot.Candidates[0].FieldSources {
		if source.Field == "category" || source.Field == "description" || source.Field == "tags" || source.Field == "depositAmount" || source.Field == "capPriceAmount" {
			if source.Source != "AI" {
				t.Fatalf("legacy optional source %s = %s, want AI", source.Field, source.Source)
			}
		}
	}

	candidate.Candidate.Draft.Category = "not-empty"
	if _, err := BuildReviewSnapshot(aggregate, "operator-a", []string{"new-1"}, []string{"new-1"}); !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("missing provenance for non-default value error = %v", err)
	}

	candidate.Candidate.Draft.Category = ""
	candidate.ProvenanceVersion = CurrentProvenanceVersion
	if _, err := BuildReviewSnapshot(aggregate, "operator-a", []string{"new-1"}, []string{"new-1"}); !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("current candidate received legacy provenance backfill: %v", err)
	}
	candidate.ProvenanceVersion = 99
	if _, err := BuildReviewSnapshot(aggregate, "operator-a", []string{"new-1"}, []string{"new-1"}); !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("unknown provenance version accepted: %v", err)
	}
}

func TestReviewSnapshotRejectsIneligibleSessionOrInvalidCandidate(t *testing.T) {
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	for _, mutate := range []func(*SessionAggregate){
		func(a *SessionAggregate) { a.Session.Status = StatusCollectingInput },
		func(a *SessionAggregate) { a.Candidates[0].Candidate.Draft.Title = "" },
	} {
		a := snapshotAggregate()
		mutate(&a)
		repo := &snapshotMemoryRepository{conversationMemoryRepo: newConversationMemoryRepo(), aggregate: a}
		service, _ := NewService(repo, roomFinder{}, &conversationRuntime{})
		_, err := service.CreateReviewSnapshot(context.Background(), actor, CreateReviewSnapshotInput{SessionID: "session-1", ExpectedSessionVersion: 7, SelectedCandidateIDs: []string{"new-1"}, OrderedCandidateIDs: []string{"new-1"}})
		if err == nil {
			t.Fatal("CreateReviewSnapshot() accepted ineligible content")
		}
	}
}

func TestReviewSnapshotServiceReauthorizesBeforePersistence(t *testing.T) {
	aggregate := snapshotAggregate()
	repo := &snapshotMemoryRepository{conversationMemoryRepo: newConversationMemoryRepo(), aggregate: aggregate, authorizeErr: apperr.ErrAgentImageAssetInvalid}
	service, _ := NewService(repo, roomFinder{}, &conversationRuntime{})
	_, err := service.CreateReviewSnapshot(context.Background(), Actor{TenantID: "tenant-a", OperatorID: "operator-a"}, CreateReviewSnapshotInput{SessionID: "session-1", ExpectedSessionVersion: 7, SelectedCandidateIDs: []string{"new-1"}, OrderedCandidateIDs: []string{"new-1"}})
	if !errors.Is(err, apperr.ErrAgentImageAssetInvalid) || repo.authorizeCalls != 1 || len(repo.created) != 0 {
		t.Fatalf("preflight err=%v calls=%d created=%d", err, repo.authorizeCalls, len(repo.created))
	}
}

func TestReviewSnapshotExistingCandidateCannotInjectDraftUploadAsset(t *testing.T) {
	aggregate := snapshotAggregate()
	aggregate.Candidates[0].Candidate.Draft.MainImageAssetID = "asset-injected"
	if _, err := BuildReviewSnapshot(aggregate, "operator-a", []string{"existing-1"}, []string{"existing-1"}); !errors.Is(err, apperr.ErrAgentImageAssetInvalid) {
		t.Fatalf("BuildReviewSnapshot() error = %v", err)
	}
}

func TestGetReviewSnapshotRequiresCurrentAuthorizedIntegrityValidSnapshot(t *testing.T) {
	aggregate := snapshotAggregate()
	snapshot, err := BuildReviewSnapshot(aggregate, "operator-a", []string{"new-1"}, []string{"new-1"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.SnapshotVersion = 3
	aggregate.Session.ActiveSnapshotVersion = 3
	repo := &snapshotMemoryRepository{conversationMemoryRepo: newConversationMemoryRepo(), aggregate: aggregate, found: snapshot}
	service, _ := NewService(repo, roomFinder{}, &conversationRuntime{})
	actor := Actor{TenantID: "tenant-a", OperatorID: "operator-a"}
	got, err := service.GetReviewSnapshot(context.Background(), actor, "session-1", 3)
	if err != nil || got.ContentHash != snapshot.ContentHash || repo.findCalls != 1 {
		t.Fatalf("GetReviewSnapshot() got=%+v err=%v calls=%d", got, err, repo.findCalls)
	}
	for _, mutate := range []func(*snapshotMemoryRepository){
		func(r *snapshotMemoryRepository) { r.aggregate.Session.ActiveSnapshotVersion = 2 },
		func(r *snapshotMemoryRepository) { r.aggregate.Session.Status = StatusNeedsRevision },
		func(r *snapshotMemoryRepository) { r.found.OperatorID = "operator-b"; r.found.CreatedBy = "operator-b" },
		func(r *snapshotMemoryRepository) { r.found.RoomID = "room-b" },
		func(r *snapshotMemoryRepository) { r.found.ContentHash = strings.Repeat("0", 64) },
	} {
		copyRepo := &snapshotMemoryRepository{conversationMemoryRepo: newConversationMemoryRepo(), aggregate: aggregate, found: snapshot}
		mutate(copyRepo)
		copyService, _ := NewService(copyRepo, roomFinder{}, &conversationRuntime{})
		if _, err := copyService.GetReviewSnapshot(context.Background(), actor, "session-1", 3); !errors.Is(err, apperr.ErrAgentReviewSnapshotStale) {
			t.Fatalf("GetReviewSnapshot() unsafe err=%v", err)
		}
	}
	if _, err := service.GetReviewSnapshot(context.Background(), Actor{TenantID: "tenant-a", OperatorID: "operator-b"}, "session-1", 3); !errors.Is(err, apperr.ErrPermissionDenied) {
		t.Fatalf("cross-operator err=%v", err)
	}
}

func snapshotAggregate() SessionAggregate {
	version := int64(23)
	newCandidate := DraftCandidate{Candidate: serviceValidCandidate("new-1"), Position: 2, Version: 4, FieldSourcesJSON: completeSnapshotSources("USER")}
	newCandidate.Candidate.Draft.MainImageURL = "https://signed.example/raw-image?secret=do-not-store"
	newCandidate.Candidate.Draft.MainImageAssetID = "asset-1"
	existing := DraftCandidate{Candidate: serviceValidCandidate("existing-1"), Position: 1, Version: 6, FieldSourcesJSON: completeSnapshotSources("EXISTING")}
	existing.Candidate.SourceType = SourceExisting
	existing.Candidate.ExistingLotID = "lot-7"
	existing.Candidate.ExistingLotReadVersion = &version
	return SessionAggregate{Session: Session{ID: "session-1", TenantID: "tenant-a", OperatorID: "operator-a", RoomID: "room-1", Status: StatusReadyForReview, Version: 7}, Candidates: []DraftCandidate{existing, newCandidate}}
}

func completeSnapshotSources(source string) string {
	fields := []string{"antiSnipeExtendSeconds", "antiSnipeWindowSeconds", "capPriceAmount", "category", "depositAmount", "description", "durationSeconds", "mainImageAssetId", "maxExtendCount", "minIncrementAmount", "startPriceAmount", "stock", "tags", "title"}
	values := make([]string, 0, len(fields))
	for _, field := range fields {
		values = append(values, `"`+field+`":"`+source+`"`)
	}
	return "{" + strings.Join(values, ",") + "}"
}

type snapshotMemoryRepository struct {
	*conversationMemoryRepo
	aggregate      SessionAggregate
	created        []ReviewSnapshot
	authorizeErr   error
	authorizeCalls int
	found          ReviewSnapshot
	findCalls      int
}

func (r *snapshotMemoryRepository) FindSession(context.Context, string, string) (SessionAggregate, error) {
	return cloneAggregate(r.aggregate), nil
}
func (r *snapshotMemoryRepository) CreateReviewSnapshot(_ context.Context, command CreateReviewSnapshotCommand) (ReviewSnapshot, error) {
	if command.ExpectedSessionVersion != r.aggregate.Session.Version {
		return ReviewSnapshot{}, apperr.ErrAgentSessionVersionConflict
	}
	command.Snapshot.SnapshotVersion = int64(len(r.created) + 1)
	r.created = append(r.created, command.Snapshot)
	r.aggregate.Session.Version++
	r.aggregate.Session.ActiveSnapshotVersion = command.Snapshot.SnapshotVersion
	return command.Snapshot, nil
}
func (r *snapshotMemoryRepository) AuthorizeReviewSnapshot(context.Context, CreateReviewSnapshotCommand) error {
	r.authorizeCalls++
	return r.authorizeErr
}
func (r *snapshotMemoryRepository) FindReviewSnapshot(context.Context, string, string, string, int64) (ReviewSnapshot, error) {
	r.findCalls++
	if r.found.SnapshotVersion == 0 {
		return ReviewSnapshot{}, apperr.ErrNotFound
	}
	return r.found, nil
}
