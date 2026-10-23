package agentconversation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"

	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

type CreateReviewSnapshotInput struct {
	SessionID              string
	ExpectedSessionVersion int64
	SelectedCandidateIDs   []string
	OrderedCandidateIDs    []string
}

type CreateReviewSnapshotCommand struct {
	TenantID               string
	OperatorID             string
	SessionID              string
	ExpectedSessionVersion int64
	Snapshot               ReviewSnapshot
}

type ReviewSnapshotRepository interface {
	AuthorizeReviewSnapshot(context.Context, CreateReviewSnapshotCommand) error
	CreateReviewSnapshot(context.Context, CreateReviewSnapshotCommand) (ReviewSnapshot, error)
	FindReviewSnapshot(context.Context, string, string, string, int64) (ReviewSnapshot, error)
}

type ReviewSnapshot struct {
	SessionID            string              `json:"sessionId"`
	TenantID             string              `json:"tenantId"`
	OperatorID           string              `json:"operatorId"`
	RoomID               string              `json:"roomId"`
	SnapshotVersion      int64               `json:"snapshotVersion"`
	ContentHash          string              `json:"contentHash"`
	SnapshotJSON         json.RawMessage     `json:"-"`
	SelectedCandidateIDs []string            `json:"selectedCandidateIds"`
	OrderedCandidateIDs  []string            `json:"orderedCandidateIds"`
	Candidates           []SnapshotCandidate `json:"candidates"`
	CreatedBy            string              `json:"createdBy"`
	CreatedAtUnixMs      int64               `json:"createdAtUnixMs"`
}

type SnapshotFieldSource struct {
	Field  string `json:"field"`
	Source string `json:"source"`
}

type SnapshotCandidate struct {
	CandidateID            string                 `json:"candidateId"`
	CandidateVersion       int64                  `json:"candidateVersion"`
	SourceType             SourceType             `json:"sourceType"`
	ExistingLotID          string                 `json:"existingLotId,omitempty"`
	ExistingLotReadVersion *int64                 `json:"existingLotReadVersion,omitempty"`
	Draft                  SnapshotCandidateDraft `json:"draft"`
	FieldSources           []SnapshotFieldSource  `json:"fieldSources"`
	AssetIDs               []string               `json:"assetIds"`
}

// SnapshotCandidateDraft deliberately excludes signed/public image URLs. The
// durable asset identity is captured by ID and is re-authorized during Apply.
type SnapshotCandidateDraft struct {
	Title                  string   `json:"title"`
	StartPriceAmount       int64    `json:"startPriceAmount"`
	DurationSeconds        int32    `json:"durationSeconds"`
	MainImageAssetID       string   `json:"mainImageAssetId"`
	MinIncrementAmount     int64    `json:"minIncrementAmount"`
	CapPriceAmount         *int64   `json:"capPriceAmount,omitempty"`
	Category               string   `json:"category"`
	Description            string   `json:"description"`
	Tags                   []string `json:"tags"`
	Stock                  int32    `json:"stock"`
	AntiSnipeWindowSeconds int32    `json:"antiSnipeWindowSeconds"`
	AntiSnipeExtendSeconds int32    `json:"antiSnipeExtendSeconds"`
	MaxExtendCount         int32    `json:"maxExtendCount"`
	DepositAmount          int64    `json:"depositAmount"`
}

type canonicalReviewSnapshot struct {
	SessionID            string              `json:"sessionId"`
	TenantID             string              `json:"tenantId"`
	OperatorID           string              `json:"operatorId"`
	RoomID               string              `json:"roomId"`
	SelectedCandidateIDs []string            `json:"selectedCandidateIds"`
	OrderedCandidateIDs  []string            `json:"orderedCandidateIds"`
	Candidates           []SnapshotCandidate `json:"candidates"`
}

// ValidateReviewSnapshotIntegrity is the persistence-boundary check: the JSON
// must exactly represent the immutable envelope and its digest must match.
func ValidateReviewSnapshotIntegrity(snapshot ReviewSnapshot) error {
	if snapshot.SessionID == "" || snapshot.TenantID == "" || snapshot.OperatorID == "" || snapshot.RoomID == "" || snapshot.CreatedBy != snapshot.OperatorID {
		return invalid("invalid review snapshot identity")
	}
	decoder := json.NewDecoder(bytes.NewReader(snapshot.SnapshotJSON))
	decoder.DisallowUnknownFields()
	var decoded canonicalReviewSnapshot
	if err := decoder.Decode(&decoded); err != nil {
		return invalid("invalid review snapshot content")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return invalid("invalid review snapshot content")
	}
	want := canonicalReviewSnapshot{SessionID: snapshot.SessionID, TenantID: snapshot.TenantID, OperatorID: snapshot.OperatorID, RoomID: snapshot.RoomID, SelectedCandidateIDs: snapshot.SelectedCandidateIDs, OrderedCandidateIDs: snapshot.OrderedCandidateIDs, Candidates: snapshot.Candidates}
	if !reflect.DeepEqual(decoded, want) {
		return invalid("review snapshot envelope does not match content")
	}
	selected, ordered, err := validateSnapshotSelection(decoded.SelectedCandidateIDs, decoded.OrderedCandidateIDs)
	if err != nil || !reflect.DeepEqual(selected, decoded.SelectedCandidateIDs) || !reflect.DeepEqual(ordered, decoded.OrderedCandidateIDs) || len(decoded.Candidates) != len(ordered) {
		return invalid("review snapshot selection is not canonical")
	}
	for i := range decoded.Candidates {
		if decoded.Candidates[i].CandidateID != ordered[i] {
			return invalid("review snapshot candidate order does not match")
		}
	}
	canonical, err := json.Marshal(decoded)
	if err != nil || !bytes.Equal(canonical, snapshot.SnapshotJSON) {
		return invalid("review snapshot content is not canonical")
	}
	digest := sha256.Sum256(canonical)
	if snapshot.ContentHash != hex.EncodeToString(digest[:]) {
		return invalid("review snapshot content hash mismatch")
	}
	return nil
}

func (s *Service) CreateReviewSnapshot(ctx context.Context, actor Actor, input CreateReviewSnapshotInput) (ReviewSnapshot, error) {
	actor, err := normalizeActor(actor)
	if err != nil {
		return ReviewSnapshot{}, err
	}
	input.SessionID = strings.TrimSpace(input.SessionID)
	if input.SessionID == "" || input.ExpectedSessionVersion <= 0 {
		return ReviewSnapshot{}, invalid("session and expected version are required")
	}
	aggregate, err := s.authorizeSession(ctx, actor, input.SessionID)
	if err != nil {
		return ReviewSnapshot{}, err
	}
	if aggregate.Session.Version != input.ExpectedSessionVersion {
		return ReviewSnapshot{}, apperr.ErrAgentSessionVersionConflict
	}
	if aggregate.Session.Status != StatusReadyForReview {
		return ReviewSnapshot{}, apperr.ErrAgentReviewSnapshotStale
	}
	snapshot, err := BuildReviewSnapshot(aggregate, actor.OperatorID, input.SelectedCandidateIDs, input.OrderedCandidateIDs)
	if err != nil {
		return ReviewSnapshot{}, err
	}
	snapshot.CreatedAtUnixMs = s.now().UnixMilli()
	repository, ok := s.repository.(ReviewSnapshotRepository)
	if !ok {
		return ReviewSnapshot{}, errors.New("review snapshot persistence is unavailable")
	}
	command := CreateReviewSnapshotCommand{
		TenantID: actor.TenantID, OperatorID: actor.OperatorID, SessionID: input.SessionID,
		ExpectedSessionVersion: input.ExpectedSessionVersion, Snapshot: snapshot,
	}
	if err := repository.AuthorizeReviewSnapshot(ctx, command); err != nil {
		return ReviewSnapshot{}, err
	}
	return repository.CreateReviewSnapshot(ctx, command)
}

func (s *Service) GetReviewSnapshot(ctx context.Context, actor Actor, sessionID string, snapshotVersion int64) (ReviewSnapshot, error) {
	actor, err := normalizeActor(actor)
	if err != nil {
		return ReviewSnapshot{}, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || snapshotVersion <= 0 {
		return ReviewSnapshot{}, invalid("session and snapshot version are required")
	}
	aggregate, err := s.authorizeSession(ctx, actor, sessionID)
	if err != nil {
		return ReviewSnapshot{}, err
	}
	if aggregate.Session.Status != StatusReadyForReview || aggregate.Session.ActiveSnapshotVersion != snapshotVersion {
		return ReviewSnapshot{}, apperr.ErrAgentReviewSnapshotStale
	}
	repository, ok := s.repository.(ReviewSnapshotRepository)
	if !ok {
		return ReviewSnapshot{}, errors.New("review snapshot persistence is unavailable")
	}
	snapshot, err := repository.FindReviewSnapshot(ctx, actor.TenantID, actor.OperatorID, sessionID, snapshotVersion)
	if err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			return ReviewSnapshot{}, apperr.ErrAgentReviewSnapshotStale
		}
		return ReviewSnapshot{}, err
	}
	if snapshot.SnapshotVersion != snapshotVersion || snapshot.SessionID != sessionID || snapshot.TenantID != actor.TenantID || snapshot.OperatorID != actor.OperatorID || snapshot.CreatedBy != actor.OperatorID || snapshot.RoomID != aggregate.Session.RoomID || ValidateReviewSnapshotIntegrity(snapshot) != nil {
		return ReviewSnapshot{}, apperr.ErrAgentReviewSnapshotStale
	}
	return snapshot, nil
}

func BuildReviewSnapshot(aggregate SessionAggregate, operatorID string, selectedIDs, orderedIDs []string) (ReviewSnapshot, error) {
	selected, ordered, err := validateSnapshotSelection(selectedIDs, orderedIDs)
	if err != nil {
		return ReviewSnapshot{}, err
	}
	byID := make(map[string]DraftCandidate, len(aggregate.Candidates))
	for _, candidate := range aggregate.Candidates {
		if candidate.Candidate.ID == "" {
			return ReviewSnapshot{}, invalid("candidate id is required")
		}
		if _, duplicate := byID[candidate.Candidate.ID]; duplicate {
			return ReviewSnapshot{}, invalid("duplicate candidate id")
		}
		if err := ValidateCandidate(candidate.Candidate); err != nil {
			return ReviewSnapshot{}, err
		}
		if candidate.Candidate.SourceType == SourceNew && strings.TrimSpace(candidate.Candidate.Draft.MainImageAssetID) == "" {
			return ReviewSnapshot{}, apperr.ErrAgentImageAssetInvalid
		}
		if candidate.Candidate.SourceType == SourceExisting && strings.TrimSpace(candidate.Candidate.Draft.MainImageAssetID) != "" {
			return ReviewSnapshot{}, apperr.ErrAgentImageAssetInvalid
		}
		byID[candidate.Candidate.ID] = candidate
	}
	candidates := make([]SnapshotCandidate, 0, len(ordered))
	for _, id := range ordered {
		candidate, ok := byID[id]
		if !ok {
			return ReviewSnapshot{}, invalid("unknown selected candidate")
		}
		converted, err := snapshotCandidate(candidate)
		if err != nil {
			return ReviewSnapshot{}, err
		}
		candidates = append(candidates, converted)
	}
	canonical := canonicalReviewSnapshot{
		SessionID: aggregate.Session.ID, TenantID: aggregate.Session.TenantID,
		OperatorID: operatorID, RoomID: aggregate.Session.RoomID,
		SelectedCandidateIDs: selected, OrderedCandidateIDs: ordered, Candidates: candidates,
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return ReviewSnapshot{}, err
	}
	digest := sha256.Sum256(payload)
	return ReviewSnapshot{
		SessionID: aggregate.Session.ID, TenantID: aggregate.Session.TenantID, OperatorID: operatorID, RoomID: aggregate.Session.RoomID,
		ContentHash: hex.EncodeToString(digest[:]), SnapshotJSON: payload,
		SelectedCandidateIDs: selected, OrderedCandidateIDs: ordered, Candidates: candidates, CreatedBy: operatorID,
	}, nil
}

func validateSnapshotSelection(selectedIDs, orderedIDs []string) ([]string, []string, error) {
	if len(selectedIDs) == 0 || len(orderedIDs) == 0 || len(selectedIDs) != len(orderedIDs) {
		return nil, nil, invalid("explicit nonempty matching selection and order are required")
	}
	selectedSet := make(map[string]struct{}, len(selectedIDs))
	selected := make([]string, len(selectedIDs))
	for i, raw := range selectedIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, nil, invalid("candidate id is required")
		}
		if _, duplicate := selectedSet[id]; duplicate {
			return nil, nil, invalid("duplicate selected candidate")
		}
		selectedSet[id], selected[i] = struct{}{}, id
	}
	orderedSet := make(map[string]struct{}, len(orderedIDs))
	ordered := make([]string, len(orderedIDs))
	for i, raw := range orderedIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, nil, invalid("candidate id is required")
		}
		if _, duplicate := orderedSet[id]; duplicate {
			return nil, nil, invalid("duplicate ordered candidate")
		}
		if _, selected := selectedSet[id]; !selected {
			return nil, nil, invalid("selected and ordered candidate sets differ")
		}
		orderedSet[id], ordered[i] = struct{}{}, id
	}
	if len(orderedSet) != len(selectedSet) {
		return nil, nil, invalid("selected and ordered candidate sets differ")
	}
	// Selection is a set; canonicalize it independently from the requested
	// batch order, which remains significant and is preserved above.
	sort.Strings(selected)
	return selected, ordered, nil
}

func snapshotCandidate(candidate DraftCandidate) (SnapshotCandidate, error) {
	sources := map[string]string{}
	if strings.TrimSpace(candidate.FieldSourcesJSON) != "" {
		if err := json.Unmarshal([]byte(candidate.FieldSourcesJSON), &sources); err != nil {
			return SnapshotCandidate{}, invalid("invalid candidate field sources")
		}
	}
	// Early conversational drafts did not persist provenance for optional
	// fields whose value was the executable zero value. Those drafts are still
	// recoverable and the UI correctly considers them valid. Attribute only
	// those exact legacy defaults to AI; never synthesize provenance for a
	// non-default value or for a required field.
	if candidate.ProvenanceVersion == 0 {
		BackfillLegacyOptionalSources(candidate.Candidate.Draft, sources)
	} else if candidate.ProvenanceVersion != CurrentProvenanceVersion {
		return SnapshotCandidate{}, invalid("unsupported candidate provenance version")
	}
	// Task 8 persisted mainImageUrl provenance before approval snapshots
	// deliberately excluded display/signed URLs. Accept exactly that one known
	// legacy surplus key, validate its source, and omit it from canonical data.
	if candidate.ProvenanceVersion == 0 && len(sources) == len(approvalCandidateFieldKeys)+1 {
		if legacy, ok := sources["mainImageUrl"]; ok && validSource(legacy) {
			delete(sources, "mainImageUrl")
		}
	}
	if len(sources) != len(approvalCandidateFieldKeys) {
		return SnapshotCandidate{}, invalid("candidate field sources must cover every executable field")
	}
	provenance := make([]SnapshotFieldSource, 0, len(approvalCandidateFieldKeys))
	for _, field := range approvalCandidateFieldKeys {
		source, exists := sources[field]
		if !exists {
			return SnapshotCandidate{}, invalid("candidate field sources must cover every executable field")
		}
		if !validSource(source) {
			return SnapshotCandidate{}, invalid("invalid candidate field source")
		}
		provenance = append(provenance, SnapshotFieldSource{Field: field, Source: source})
	}
	tags := append([]string(nil), candidate.Candidate.Draft.Tags...)
	if tags == nil {
		tags = []string{}
	}
	assetIDs := []string{}
	if candidate.Candidate.Draft.MainImageAssetID != "" {
		assetIDs = append(assetIDs, candidate.Candidate.Draft.MainImageAssetID)
	}
	d := candidate.Candidate.Draft
	return SnapshotCandidate{
		CandidateID: candidate.Candidate.ID, CandidateVersion: candidate.Version, SourceType: candidate.Candidate.SourceType,
		ExistingLotID: candidate.Candidate.ExistingLotID, ExistingLotReadVersion: candidate.Candidate.ExistingLotReadVersion,
		Draft:        SnapshotCandidateDraft{Title: d.Title, StartPriceAmount: d.StartPriceAmount, DurationSeconds: d.DurationSeconds, MainImageAssetID: d.MainImageAssetID, MinIncrementAmount: d.MinIncrementAmount, CapPriceAmount: d.CapPriceAmount, Category: d.Category, Description: d.Description, Tags: tags, Stock: d.Stock, AntiSnipeWindowSeconds: d.AntiSnipeWindowSeconds, AntiSnipeExtendSeconds: d.AntiSnipeExtendSeconds, MaxExtendCount: d.MaxExtendCount, DepositAmount: d.DepositAmount},
		FieldSources: provenance, AssetIDs: assetIDs,
	}, nil
}

// BackfillLegacyOptionalSources applies the one compatibility normalization
// shared by snapshot construction and the transactional Apply recheck.
func BackfillLegacyOptionalSources(draft CandidateDraft, sources map[string]string) {
	if _, ok := sources["capPriceAmount"]; !ok && draft.CapPriceAmount == nil {
		sources["capPriceAmount"] = "AI"
	}
	if _, ok := sources["category"]; !ok && strings.TrimSpace(draft.Category) == "" {
		sources["category"] = "AI"
	}
	if _, ok := sources["description"]; !ok && strings.TrimSpace(draft.Description) == "" {
		sources["description"] = "AI"
	}
	if _, ok := sources["tags"]; !ok && len(draft.Tags) == 0 {
		sources["tags"] = "AI"
	}
	if _, ok := sources["depositAmount"]; !ok && draft.DepositAmount == 0 {
		sources["depositAmount"] = "AI"
	}
}

var approvalCandidateFieldKeys = []string{
	"antiSnipeExtendSeconds", "antiSnipeWindowSeconds", "capPriceAmount", "category",
	"depositAmount", "description", "durationSeconds", "mainImageAssetId", "maxExtendCount",
	"minIncrementAmount", "startPriceAmount", "stock", "tags", "title",
}

func approvalFieldSources(sources map[string]string) map[string]string {
	filtered := make(map[string]string, len(approvalCandidateFieldKeys))
	for _, field := range approvalCandidateFieldKeys {
		if source, ok := sources[field]; ok {
			filtered[field] = source
		}
	}
	return filtered
}
