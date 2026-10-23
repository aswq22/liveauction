package agentconversation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	agentv1 "live-auction-bid/backend/api/agent/v1"
	auctionv1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
	"live-auction-bid/backend/app/auction/service/internal/biz/auction"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
	"live-auction-bid/backend/app/auction/service/internal/pkg/idgen"
)

const MaxMessageBytes = 8 * 1024

type Actor struct{ TenantID, OperatorID string }

type RoomFinder interface {
	FindRoomByID(context.Context, string) (*auction.Room, bool, error)
}

// InventoryFinder is the authoritative Go-side inventory boundary used when a
// SESSION_PLAN asks to introduce an existing lot. Runtime/model JSON is never
// accepted as proof of ownership or as the source of the lot read version.
type InventoryFinder interface {
	ListLots(context.Context, auction.LotQuery) (auction.LotList, error)
}

// Runtime is intentionally the narrow session-aware gateway surface.  The
// conversation service owns all durable session mutations; the gateway only
// starts/projections the run that the repository has already recorded.
type Runtime interface {
	StartPersistedSessionRun(context.Context, agentcontrol.Actor, string) (agentcontrol.Run, error)
	GetRun(context.Context, agentcontrol.Actor, string) (agentcontrol.Run, error)
	StreamRunEvents(context.Context, agentcontrol.Actor, string, int64) (agentcontrol.EventStream, error)
}

type Service struct {
	repository Repository
	rooms      RoomFinder
	inventory  InventoryFinder
	runtime    Runtime
	now        func() time.Time
}

func NewService(repository Repository, rooms RoomFinder, runtime Runtime) (*Service, error) {
	if repository == nil || rooms == nil || runtime == nil {
		return nil, errors.New("agent conversation dependencies are required")
	}
	inventory, _ := rooms.(InventoryFinder)
	return &Service{repository: repository, rooms: rooms, inventory: inventory, runtime: runtime, now: time.Now}, nil
}

func (s *Service) CreateSession(ctx context.Context, actor Actor, roomID string) (SessionAggregate, error) {
	actor, err := normalizeActor(actor)
	if err != nil {
		return SessionAggregate{}, err
	}
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return SessionAggregate{}, invalid("room id is required")
	}
	room, _, err := s.rooms.FindRoomByID(ctx, roomID)
	if err != nil {
		return SessionAggregate{}, err
	}
	if room == nil || strings.TrimSpace(room.MainAccountID) != actor.TenantID {
		return SessionAggregate{}, apperr.ErrPermissionDenied
	}
	now := s.now().UnixMilli()
	session := Session{ID: idgen.New("agent-session"), TenantID: actor.TenantID, OperatorID: actor.OperatorID, RoomID: roomID, Status: StatusCollectingInput, Version: 1, CreatedAtUnixMs: now, UpdatedAtUnixMs: now}
	if err := s.repository.CreateSession(ctx, session); err != nil {
		return SessionAggregate{}, err
	}
	return s.repository.FindSession(ctx, actor.TenantID, session.ID)
}

func (s *Service) Recover(ctx context.Context, actor Actor, sessionID string) (SessionAggregate, error) {
	actor, err := normalizeActor(actor)
	if err != nil {
		return SessionAggregate{}, err
	}
	aggregate, err := s.authorizeSession(ctx, actor, sessionID)
	if err != nil {
		return SessionAggregate{}, err
	}
	if !sessionMergeEligible(aggregate.Session.Status) {
		return aggregate, nil
	}
	sortRuns(aggregate.Runs)
	for _, stored := range aggregate.Runs {
		if mergedRun(aggregate.Messages, stored.ID) {
			continue
		}
		run, getErr := s.runtime.GetRun(ctx, toRunActor(actor), stored.ID)
		if getErr != nil {
			continue
		} // durable state is still recoverable; do not leak runtime details.
		if run.Status == agentcontrol.StatusCompleted && run.TurnResultJSON != "" {
			aggregate, err = s.mergeRunWithRetry(ctx, actor, sessionID, aggregate, run)
			if err != nil {
				slog.Error("agent session completed turn merge failed", "session_id", sessionID, "run_id", run.ID, "error", err)
			}
			if err != nil && !errors.Is(err, apperr.ErrInvalidArgument) {
				return aggregate, err
			}
		}
	}
	return s.authorizeSession(ctx, actor, sessionID)
}

func (s *Service) mergeRunWithRetry(ctx context.Context, actor Actor, sessionID string, aggregate SessionAggregate, run agentcontrol.Run) (SessionAggregate, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if mergedRun(aggregate.Messages, run.ID) {
			return aggregate, nil
		}
		merged, err := s.mergeCompletedTurn(ctx, actor, aggregate, run)
		if err == nil {
			return merged, nil
		}
		if !errors.Is(err, apperr.ErrAgentSessionVersionConflict) {
			return aggregate, err
		}
		aggregate, err = s.authorizeSession(ctx, actor, sessionID)
		if err != nil {
			return SessionAggregate{}, err
		}
	}
	return aggregate, apperr.ErrAgentSessionVersionConflict
}

type SubmitCommand struct {
	SessionID       string
	ExpectedVersion int64
	Content         string
	RunType         agentv1.RunType
	IdempotencyKey  string
}

func (s *Service) SubmitMessage(ctx context.Context, actor Actor, command SubmitCommand) (SessionAggregate, agentcontrol.Run, error) {
	actor, err := normalizeActor(actor)
	if err != nil {
		return SessionAggregate{}, agentcontrol.Run{}, err
	}
	command.SessionID = strings.TrimSpace(command.SessionID)
	command.Content = strings.TrimSpace(command.Content)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	if command.SessionID == "" || command.ExpectedVersion <= 0 || command.Content == "" || len(command.Content) > MaxMessageBytes || !utf8.ValidString(command.Content) {
		return SessionAggregate{}, agentcontrol.Run{}, invalid("invalid session message")
	}
	if !validIdempotencyKey(command.IdempotencyKey) {
		return SessionAggregate{}, agentcontrol.Run{}, invalid("invalid idempotency key")
	}
	if command.RunType != agentv1.RunType_RUN_TYPE_SESSION_PLAN && command.RunType != agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST {
		return SessionAggregate{}, agentcontrol.Run{}, invalid("session run type is required")
	}
	aggregate, err := s.Recover(ctx, actor, command.SessionID)
	if err != nil {
		return SessionAggregate{}, agentcontrol.Run{}, err
	}
	input, err := encodeTurnInput(command.Content, aggregate.Candidates)
	if err != nil {
		return SessionAggregate{}, agentcontrol.Run{}, err
	}
	for _, existing := range aggregate.Runs {
		if existing.IdempotencyKey == command.IdempotencyKey {
			if turnInputMessage(existing.TurnInputJSON) != command.Content || existing.RunType != command.RunType {
				return SessionAggregate{}, agentcontrol.Run{}, apperr.ErrConflict
			}
			return aggregate, existing, nil
		}
	}
	if aggregate.Session.Status == StatusCancelled || aggregate.Session.Status == StatusApplied || aggregate.Session.Status == StatusApplying {
		return SessionAggregate{}, agentcontrol.Run{}, invalid("session cannot accept a new turn")
	}
	if active := activeRun(aggregate.Runs); active != nil {
		return aggregate, *active, apperr.ErrConflict
	}
	if aggregate.Session.Version != command.ExpectedVersion {
		return SessionAggregate{}, agentcontrol.Run{}, apperr.ErrAgentSessionVersionConflict
	}
	trace, err := newTraceID()
	if err != nil {
		return SessionAggregate{}, agentcontrol.Run{}, err
	}
	now := s.now().UnixMilli()
	run := agentcontrol.Run{ID: idgen.New("run"), TenantID: actor.TenantID, OperatorID: actor.OperatorID, TraceID: trace, Goal: "Continue agent session", IdempotencyKey: command.IdempotencyKey, Status: agentcontrol.StatusCreated, Version: 1, RunType: command.RunType, SessionID: command.SessionID, SessionVersion: aggregate.Session.Version + 1, TurnInputJSON: input, CreatedAtUnixMs: now, UpdatedAtUnixMs: now}
	aggregate, err = s.repository.AppendUserMessageAndStartRun(ctx, StartTurnCommand{TenantID: actor.TenantID, OperatorID: actor.OperatorID, SessionID: command.SessionID, ExpectedVersion: command.ExpectedVersion, Message: Message{Content: command.Content, CreatedAtUnixMs: now}, Run: run})
	if err != nil {
		return SessionAggregate{}, agentcontrol.Run{}, err
	}
	for _, candidate := range aggregate.Runs {
		if candidate.ID == run.ID {
			run = candidate
			break
		}
	}
	run, err = s.runtime.StartPersistedSessionRun(ctx, toRunActor(actor), run.ID)
	if err != nil {
		return aggregate, run, err
	}
	if run.Status == agentcontrol.StatusCompleted && run.TurnResultJSON != "" {
		if aggregate, err = s.mergeRunWithRetry(ctx, actor, command.SessionID, aggregate, run); err != nil {
			return aggregate, run, err
		}
	}
	return aggregate, run, nil
}

func (s *Service) Cancel(ctx context.Context, actor Actor, sessionID string, expectedVersion int64) error {
	actor, err := normalizeActor(actor)
	if err != nil {
		return err
	}
	aggregate, err := s.authorizeSession(ctx, actor, sessionID)
	if err != nil {
		return err
	}
	if aggregate.Session.Status == StatusCancelled {
		return nil
	}
	if expectedVersion <= 0 {
		return invalid("expected version is required")
	}
	return s.repository.CancelSession(ctx, actor.TenantID, actor.OperatorID, sessionID, expectedVersion)
}

func (s *Service) StreamEvents(ctx context.Context, actor Actor, sessionID string, afterSeq int64) (agentcontrol.EventStream, error) {
	actor, err := normalizeActor(actor)
	if err != nil {
		return nil, err
	}
	if afterSeq < 0 {
		return nil, invalid("after_seq must be nonnegative")
	}
	aggregate, err := s.authorizeSession(ctx, actor, sessionID)
	if err != nil {
		return nil, err
	}
	if len(aggregate.Runs) == 0 {
		return nil, apperr.ErrNotFound
	}
	runs := append([]agentcontrol.Run(nil), aggregate.Runs...)
	sortRuns(runs)
	return &sessionEventStream{ctx: ctx, runtime: s.runtime, actor: toRunActor(actor), runs: runs, after: afterSeq}, nil
}

// sessionEventStream serializes run-local durable streams in creation order.
// Each completed run is exhausted before the next begins, so its contribution
// to the session-global sequence is immutable once a later run exists.
type sessionEventStream struct {
	ctx                      context.Context
	runtime                  Runtime
	actor                    agentcontrol.Actor
	runs                     []agentcontrol.Run
	after, offset, lastLocal int64
	index                    int
	current                  agentcontrol.EventStream
}

func (s *sessionEventStream) Recv() (agentcontrol.RuntimeEvent, error) {
	for s.index < len(s.runs) {
		if s.current == nil {
			stream, err := s.runtime.StreamRunEvents(s.ctx, s.actor, s.runs[s.index].ID, 0)
			if err != nil {
				return agentcontrol.RuntimeEvent{}, err
			}
			s.current = stream
		}
		event, err := s.current.Recv()
		if err != nil {
			_ = s.current.Close()
			s.current = nil
			if errors.Is(err, io.EOF) {
				s.offset += s.lastLocal
				s.lastLocal = 0
				s.index++
				continue
			}
			return agentcontrol.RuntimeEvent{}, err
		}
		if event.Seq <= 0 || event.Seq != s.lastLocal+1 {
			return agentcontrol.RuntimeEvent{}, invalid("invalid event sequence")
		}
		s.lastLocal = event.Seq
		global := s.offset + event.Seq
		if global <= s.after {
			continue
		}
		event.Seq = global
		return event, nil
	}
	return agentcontrol.RuntimeEvent{}, io.EOF
}
func (s *sessionEventStream) Close() error {
	if s.current != nil {
		return s.current.Close()
	}
	return nil
}

func (s *Service) PatchCandidate(ctx context.Context, actor Actor, command PatchCandidateCommand) (SessionAggregate, error) {
	actor, err := normalizeActor(actor)
	if err != nil {
		return SessionAggregate{}, err
	}
	aggregate, err := s.authorizeSession(ctx, actor, command.SessionID)
	if err != nil {
		return SessionAggregate{}, err
	}
	if command.ExpectedSessionVersion != aggregate.Session.Version {
		return SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
	}
	if command.ExpectedCandidateVersion <= 0 || len(command.Fields) == 0 {
		return SessionAggregate{}, invalid("candidate patch fields are required")
	}
	if !sessionMergeEligible(aggregate.Session.Status) {
		return SessionAggregate{}, apperr.ErrConflict
	}
	index := -1
	for i := range aggregate.Candidates {
		if aggregate.Candidates[i].Candidate.ID == strings.TrimSpace(command.CandidateID) {
			index = i
			break
		}
	}
	if index < 0 {
		return SessionAggregate{}, apperr.ErrNotFound
	}
	current := aggregate.Candidates[index]
	if current.Version != command.ExpectedCandidateVersion {
		return SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
	}
	replacement := current
	sources := parseSources(replacement.FieldSourcesJSON)
	for name, raw := range command.Fields {
		if err := setUserCandidateField(&replacement.Candidate, name, raw); err != nil {
			return SessionAggregate{}, err
		}
		sources[name] = "USER"
	}
	replacement.FieldSourcesJSON = fieldSourcesJSON(sources)
	validation, validationErr := candidateValidationJSON(replacement.Candidate)
	if validationErr != nil {
		return SessionAggregate{}, validationErr
	}
	replacement.ValidationJSON = validation
	candidates := replaceCandidate(aggregate.Candidates, replacement)
	if allCandidatesValid(candidates) {
		command.Status = StatusReadyForReview
	} else if aggregate.Session.Status == StatusCollectingInput {
		command.Status = StatusCollectingInput
	} else {
		command.Status = StatusNeedsRevision
	}
	command.Candidate = replacement
	command.TenantID, command.OperatorID, command.UpdatedAtUnixMs = actor.TenantID, actor.OperatorID, s.now().UnixMilli()
	return s.repository.PatchCandidate(ctx, command)
}

func (s *Service) BindCandidateImage(ctx context.Context, actor Actor, command BindCandidateImageCommand) (SessionAggregate, error) {
	actor, err := normalizeActor(actor)
	if err != nil {
		return SessionAggregate{}, err
	}
	command.SessionID = strings.TrimSpace(command.SessionID)
	command.CandidateID = strings.TrimSpace(command.CandidateID)
	command.AssetID = strings.TrimSpace(command.AssetID)
	if command.SessionID == "" || command.CandidateID == "" || command.AssetID == "" || command.ExpectedSessionVersion <= 0 || command.ExpectedCandidateVersion <= 0 {
		return SessionAggregate{}, invalid("session, candidate, asset and versions are required")
	}
	aggregate, err := s.authorizeSession(ctx, actor, command.SessionID)
	if err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			return SessionAggregate{}, apperr.ErrPermissionDenied
		}
		return SessionAggregate{}, err
	}
	if aggregate.Session.Version != command.ExpectedSessionVersion {
		return SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
	}
	if !sessionMergeEligible(aggregate.Session.Status) {
		return SessionAggregate{}, apperr.ErrConflict
	}
	found := false
	for _, candidate := range aggregate.Candidates {
		if candidate.Candidate.ID == command.CandidateID {
			found = true
			if candidate.Version != command.ExpectedCandidateVersion {
				return SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
			}
			break
		}
	}
	if !found {
		return SessionAggregate{}, apperr.ErrNotFound
	}
	binder, ok := s.repository.(CandidateImageBinder)
	if !ok {
		return SessionAggregate{}, errors.New("candidate image binding is unavailable")
	}
	command.TenantID, command.OperatorID, command.UpdatedAtUnixMs = actor.TenantID, actor.OperatorID, s.now().UnixMilli()
	return binder.BindCandidateImage(ctx, command)
}

func setUserCandidateField(candidate *Candidate, name string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return invalid("invalid candidate field")
	}
	if !userEditableCandidateField(name) {
		return invalid("unknown candidate field")
	}
	// RFC 7396 null removes a member. For the typed draft this maps to its zero
	// value; explicit start-price presence is cleared as well.
	if string(raw) == "null" {
		switch name {
		case "title":
			candidate.Draft.Title = ""
		case "startPriceAmount":
			candidate.Draft.StartPriceAmount, candidate.StartPriceSpecified = 0, false
		case "durationSeconds":
			candidate.Draft.DurationSeconds = 0
		case "minIncrementAmount":
			candidate.Draft.MinIncrementAmount = 0
		case "capPriceAmount":
			candidate.Draft.CapPriceAmount = nil
		case "category":
			candidate.Draft.Category = ""
		case "description":
			candidate.Draft.Description = ""
		case "tags":
			candidate.Draft.Tags = nil
		case "stock":
			candidate.Draft.Stock = 0
		case "antiSnipeWindowSeconds":
			candidate.Draft.AntiSnipeWindowSeconds = 0
		case "antiSnipeExtendSeconds":
			candidate.Draft.AntiSnipeExtendSeconds = 0
		case "maxExtendCount":
			candidate.Draft.MaxExtendCount = 0
		case "depositAmount":
			candidate.Draft.DepositAmount = 0
		default:
			return invalid("unknown candidate field")
		}
		return nil
	}
	return setCandidateField(candidate, name, raw)
}

func userEditableCandidateField(name string) bool {
	switch name {
	case "title", "startPriceAmount", "durationSeconds", "minIncrementAmount", "capPriceAmount", "category", "description", "tags", "stock", "antiSnipeWindowSeconds", "antiSnipeExtendSeconds", "maxExtendCount", "depositAmount":
		return true
	default:
		return false
	}
}

func candidateValidationJSON(candidate Candidate) (string, error) {
	missing := MissingRequiredInputs(candidate)
	errorsList := make([]string, 0, 1)
	if len(missing) == 0 {
		if err := ValidateCandidate(candidate); err != nil {
			errorsList = append(errorsList, err.Error())
		}
	}
	raw, err := json.Marshal(map[string]any{"requiredInputs": missing, "errors": errorsList})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (s *Service) authorizeSession(ctx context.Context, actor Actor, sessionID string) (SessionAggregate, error) {
	aggregate, err := s.repository.FindSession(ctx, actor.TenantID, strings.TrimSpace(sessionID))
	if err != nil {
		return SessionAggregate{}, err
	}
	if aggregate.Session.OperatorID != actor.OperatorID {
		return SessionAggregate{}, apperr.ErrPermissionDenied
	}
	return aggregate, nil
}

func (s *Service) mergeCompletedTurn(ctx context.Context, actor Actor, aggregate SessionAggregate, run agentcontrol.Run) (SessionAggregate, error) {
	if !sessionMergeEligible(aggregate.Session.Status) {
		return aggregate, apperr.ErrConflict
	}
	result, err := decodeTurnResult(run.TurnResultJSON)
	if err != nil {
		return aggregate, err
	}
	baseCandidates := aggregate.Candidates
	if run.RunType == agentv1.RunType_RUN_TYPE_SESSION_PLAN {
		// A plan is a replaceable view over inventory, not another group of lot drafts.
		baseCandidates = nil
	} else {
		// Returning to lot creation closes any prior plan workspace.
		baseCandidates = make([]DraftCandidate, 0, len(aggregate.Candidates))
		for _, candidate := range aggregate.Candidates {
			if candidate.Candidate.SourceType == SourceNew {
				baseCandidates = append(baseCandidates, candidate)
			}
		}
	}
	candidates, err := s.mergeCandidates(ctx, actor, aggregate.Session.ID, aggregate.Session.RoomID, baseCandidates, result, run.RunType)
	if err != nil {
		return aggregate, err
	}
	if run.RunType == agentv1.RunType_RUN_TYPE_SESSION_PLAN {
		// The inventory reader can expose an unfinished DRAFT lot so the model can
		// explain the available inventory. Such a lot must never make the entire
		// conversational turn disappear merely because it cannot be appended to
		// the queue yet. Keep the prior candidates, omit newly introduced invalid
		// inventory lots, and persist an actionable assistant message.
		currentIDs := make(map[string]struct{}, len(baseCandidates))
		for _, candidate := range baseCandidates {
			currentIDs[candidate.Candidate.ID] = struct{}{}
		}
		filtered := make([]DraftCandidate, 0, len(candidates))
		skipped := 0
		for _, candidate := range candidates {
			_, existed := currentIDs[candidate.Candidate.ID]
			if !existed && candidate.Candidate.SourceType == SourceExisting && ValidateCandidate(candidate.Candidate) != nil {
				skipped++
				continue
			}
			filtered = append(filtered, candidate)
		}
		candidates = filtered
		if skipped > 0 {
			if len(candidates) == len(baseCandidates) {
				result.AssistantMessage = "当前库存中没有足够的可排品拍品。只有信息完整、尚未入队且从未开拍的库存拍品可以参与排品；请先补充库存后再生成方案。"
			} else {
				result.AssistantMessage += fmt.Sprintf(" 另有 %d 件库存拍品信息不完整，已从本次方案中排除。", skipped)
			}
			result.Readiness = "NEEDS_INPUT"
		}
	}
	status := StatusCollectingInput
	if result.Readiness == "READY_FOR_REVIEW" {
		if !allCandidatesValid(candidates) {
			return aggregate, invalid("runtime result is not review-ready")
		}
		status = StatusReadyForReview
	} else if aggregate.Session.Status == StatusNeedsRevision {
		status = StatusNeedsRevision
	}
	if !CanTransition(aggregate.Session.Status, status) && aggregate.Session.Status != status {
		return aggregate, invalid("invalid session transition")
	}
	return s.repository.MergeTurnResult(ctx, MergeTurnCommand{TenantID: actor.TenantID, OperatorID: actor.OperatorID, SessionID: aggregate.Session.ID, RunID: run.ID, ExpectedVersion: aggregate.Session.Version, AssistantMessage: Message{Content: result.AssistantMessage, RunID: run.ID, CreatedAtUnixMs: s.now().UnixMilli()}, Candidates: candidates, ConversationSummary: result.AssistantMessage, Status: status, UpdatedAtUnixMs: s.now().UnixMilli()})
}

type turnResult struct {
	AssistantMessage string           `json:"assistant_message"`
	CandidatePatches []candidatePatch `json:"candidate_patches"`
	RequiredInputs   []string         `json:"required_inputs"`
	Readiness        string           `json:"review_readiness"`
	Warnings         []string         `json:"warnings"`
}
type candidatePatch struct {
	CandidateID  string                     `json:"candidate_id"`
	SourceType   SourceType                 `json:"source_type"`
	Fields       map[string]json.RawMessage `json:"fields"`
	FieldSources map[string]string          `json:"field_sources"`
}

func decodeTurnResult(raw string) (turnResult, error) {
	if len(raw) == 0 || len(raw) > 1024*1024 {
		return turnResult{}, invalid("invalid turn result")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	var result turnResult
	if err := d.Decode(&result); err != nil {
		return turnResult{}, invalid("invalid turn result")
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		return turnResult{}, invalid("invalid turn result")
	}
	if strings.TrimSpace(result.AssistantMessage) == "" || result.Readiness != "NEEDS_INPUT" && result.Readiness != "READY_FOR_REVIEW" || result.CandidatePatches == nil || result.RequiredInputs == nil || result.Warnings == nil {
		return turnResult{}, invalid("invalid turn result")
	}
	seen := map[string]bool{}
	for _, patch := range result.CandidatePatches {
		if strings.TrimSpace(patch.CandidateID) == "" || seen[patch.CandidateID] || (patch.SourceType != SourceNew && patch.SourceType != SourceExisting) || patch.Fields == nil || patch.FieldSources == nil {
			return turnResult{}, invalid("invalid turn result")
		}
		seen[patch.CandidateID] = true
		for name, source := range patch.FieldSources {
			if _, ok := patch.Fields[name]; !ok || !validSource(source) {
				return turnResult{}, invalid("invalid turn result")
			}
		}
	}
	for _, required := range result.RequiredInputs {
		switch RequiredInput(required) {
		case RequiredTitle, RequiredStartPrice, RequiredDuration, RequiredMainImage, RequiredAmbiguousCandidate:
		default:
			return turnResult{}, invalid("invalid turn result")
		}
	}
	return result, nil
}

func (s *Service) mergeCandidates(ctx context.Context, actor Actor, sessionID, roomID string, current []DraftCandidate, result turnResult, runType agentv1.RunType) ([]DraftCandidate, error) {
	out := append([]DraftCandidate(nil), current...)
	indexes := map[string]int{}
	for i := range out {
		indexes[out[i].Candidate.ID] = i
	}
	for _, patch := range result.CandidatePatches {
		i, exists := indexes[patch.CandidateID]
		if !exists {
			if patch.SourceType == SourceExisting {
				if runType != agentv1.RunType_RUN_TYPE_SESSION_PLAN {
					return nil, invalid("runtime cannot introduce existing candidate")
				}
				candidate, err := s.trustedExistingCandidate(ctx, actor, patch, sessionID, roomID, int32(len(out)+1))
				if err != nil {
					return nil, err
				}
				out = append(out, candidate)
				indexes[patch.CandidateID] = len(out) - 1
				continue
			}
			candidate := DraftCandidate{Candidate: Candidate{ID: patch.CandidateID, SourceType: SourceNew}, Position: int32(len(out) + 1), SelectedByDefault: true, ProvenanceVersion: CurrentProvenanceVersion, Version: 1}
			if err := applyPatch(&candidate, patch, nil); err != nil {
				return nil, err
			}
			candidate.FieldSourcesJSON = fieldSourcesJSON(approvalFieldSources(patch.FieldSources))
			out = append(out, candidate)
			indexes[patch.CandidateID] = len(out) - 1
			continue
		}
		if out[i].Candidate.SourceType != patch.SourceType {
			return nil, invalid("runtime cannot change candidate source type")
		}
		if patch.SourceType == SourceExisting {
			if runType != agentv1.RunType_RUN_TYPE_SESSION_PLAN {
				return nil, invalid("runtime cannot update existing candidate")
			}
			trusted, err := s.trustedExistingCandidate(ctx, actor, patch, sessionID, roomID, out[i].Position)
			if err != nil {
				return nil, err
			}
			trusted.SelectedByDefault = out[i].SelectedByDefault
			trusted.Version = out[i].Version
			out[i] = trusted
			continue
		}
		sources := parseSources(out[i].FieldSourcesJSON)
		if err := applyPatch(&out[i], patch, sources); err != nil {
			return nil, err
		}
		out[i].FieldSourcesJSON = fieldSourcesJSON(approvalFieldSources(sources))
	}
	return out, nil
}

var trustedExistingStatuses = []auctionv1.LotStatus{
	auctionv1.LotStatus_LOT_STATUS_DRAFT,
	auctionv1.LotStatus_LOT_STATUS_READY,
}

func (s *Service) trustedExistingCandidate(ctx context.Context, actor Actor, patch candidatePatch, sessionID, roomID string, position int32) (DraftCandidate, error) {
	if s.inventory == nil {
		return DraftCandidate{}, invalid("trusted inventory is unavailable")
	}
	var requestedID string
	raw, ok := patch.Fields["existingLotId"]
	if !ok || json.Unmarshal(raw, &requestedID) != nil {
		return DraftCandidate{}, invalid("existing lot id is required")
	}
	requestedID = strings.TrimSpace(requestedID)
	if requestedID == "" || strings.TrimSpace(sessionID) == "" || patch.FieldSources["existingLotId"] != "EXISTING" {
		return DraftCandidate{}, invalid("existing lot identity is not trusted")
	}
	visible := make([]*auctionv1.Lot, 0, 100)
	seen := make(map[string]struct{})
	for _, status := range trustedExistingStatuses {
		list, err := s.inventory.ListLots(ctx, auction.LotQuery{Page: 1, PageSize: 100, MainAccountID: actor.TenantID, Status: status, OrderByUpdatedAt: true})
		if err != nil {
			return DraftCandidate{}, fmt.Errorf("list trusted inventory status %s: %w", status, err)
		}
		for _, lot := range list.Lots {
			if lot == nil || lot.GetMainAccountId() != actor.TenantID || lot.GetStatus() != status || lot.GetStartedAtUnixMs() != 0 || lot.GetQueueStatus() != auctionv1.LotQueueStatus_LOT_QUEUE_STATUS_NONE || lot.GetQueuePosition() != 0 {
				continue
			}
			if _, duplicate := seen[lot.GetId()]; duplicate {
				continue
			}
			seen[lot.GetId()] = struct{}{}
			visible = append(visible, lot)
		}
	}
	// QueryInventory performs the same per-status union, global newest-first
	// sort and top-100 truncation. Reproduce it here so a guessed tenant lot
	// outside the evidence actually visible to SESSION_PLAN cannot be admitted.
	sort.SliceStable(visible, func(i, j int) bool {
		if visible[i].GetUpdatedAtUnixMs() == visible[j].GetUpdatedAtUnixMs() {
			return visible[i].GetId() < visible[j].GetId()
		}
		return visible[i].GetUpdatedAtUnixMs() > visible[j].GetUpdatedAtUnixMs()
	})
	if len(visible) > 100 {
		visible = visible[:100]
	}
	var trusted *auctionv1.Lot
	for _, lot := range visible {
		if lot.GetId() == requestedID && lot.GetRoomId() == roomID {
			trusted = lot
			break
		}
	}
	if trusted == nil || trusted.GetVersion() <= 0 {
		return DraftCandidate{}, invalid("existing lot is not in trusted inventory")
	}
	candidate := candidateFromTrustedLot(trusted)
	candidate.ID = existingSessionCandidateID(sessionID, requestedID)
	validation, err := candidateValidationJSON(candidate)
	if err != nil {
		return DraftCandidate{}, err
	}
	sources := map[string]string{}
	for _, name := range approvalCandidateFieldKeys {
		sources[name] = "EXISTING"
	}
	return DraftCandidate{Candidate: candidate, Position: position, SelectedByDefault: true, FieldSourcesJSON: fieldSourcesJSON(sources), ValidationJSON: validation, ProvenanceVersion: CurrentProvenanceVersion, Version: 1}, nil
}

func existingSessionCandidateID(sessionID, lotID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sessionID) + "\x00" + strings.TrimSpace(lotID)))
	return "existing-" + hex.EncodeToString(sum[:20])
}

func candidateFromTrustedLot(lot *auctionv1.Lot) Candidate {
	version := lot.GetVersion()
	rule := lot.GetRule()
	var capAmount *int64
	if capPrice := rule.GetCapPrice(); capPrice != nil {
		value := capPrice.GetAmount()
		capAmount = &value
	}
	return Candidate{
		ID: lot.GetId(), SourceType: SourceExisting, ExistingLotID: lot.GetId(), ExistingLotReadVersion: &version, StartPriceSpecified: rule.GetStartPrice() != nil,
		Draft: CandidateDraft{
			Title: lot.GetTitle(), StartPriceAmount: rule.GetStartPrice().GetAmount(), DurationSeconds: rule.GetDurationSeconds(), MainImageURL: lot.GetImageUrl(),
			MinIncrementAmount: rule.GetMinIncrement().GetAmount(), CapPriceAmount: capAmount, Category: lot.GetCategory(), Description: lot.GetDescription(), Tags: append([]string(nil), lot.GetTags()...), Stock: lot.GetStock(),
			AntiSnipeWindowSeconds: rule.GetAntiSnipeWindowSeconds(), AntiSnipeExtendSeconds: rule.GetAntiSnipeExtendSeconds(), MaxExtendCount: rule.GetMaxExtendCount(), DepositAmount: lot.GetDepositAmount().GetAmount(),
		},
	}
}

func applyPatch(candidate *DraftCandidate, patch candidatePatch, existingSources map[string]string) error {
	if existingSources == nil {
		existingSources = map[string]string{}
	}
	for name, raw := range patch.Fields {
		if existingSources[name] == "USER" {
			continue
		}
		if err := setCandidateField(&candidate.Candidate, name, raw); err != nil {
			return err
		}
		source := patch.FieldSources[name]
		if source == "" {
			source = "AI"
		}
		existingSources[name] = source
	}
	return nil
}

func setCandidateField(candidate *Candidate, name string, raw json.RawMessage) error {
	decode := func(target any) error {
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.DisallowUnknownFields()
		if err := d.Decode(target); err != nil {
			return invalid("invalid candidate field")
		}
		return nil
	}
	switch name {
	case "title":
		return decode(&candidate.Draft.Title)
	case "startPriceAmount":
		candidate.StartPriceSpecified = true
		return decode(&candidate.Draft.StartPriceAmount)
	case "durationSeconds":
		return decode(&candidate.Draft.DurationSeconds)
	case "mainImageAssetId":
		return decode(&candidate.Draft.MainImageAssetID)
	case "mainImageUrl":
		return decode(&candidate.Draft.MainImageURL)
	case "minIncrementAmount":
		return decode(&candidate.Draft.MinIncrementAmount)
	case "capPriceAmount":
		var value *int64
		if err := decode(&value); err != nil {
			return err
		}
		candidate.Draft.CapPriceAmount = value
		return nil
	case "category":
		return decode(&candidate.Draft.Category)
	case "description":
		return decode(&candidate.Draft.Description)
	case "tags":
		return decode(&candidate.Draft.Tags)
	case "stock":
		return decode(&candidate.Draft.Stock)
	case "antiSnipeWindowSeconds":
		return decode(&candidate.Draft.AntiSnipeWindowSeconds)
	case "antiSnipeExtendSeconds":
		return decode(&candidate.Draft.AntiSnipeExtendSeconds)
	case "maxExtendCount":
		return decode(&candidate.Draft.MaxExtendCount)
	case "depositAmount":
		return decode(&candidate.Draft.DepositAmount)
	default:
		return invalid("unknown candidate field")
	}
}

func encodeTurnInput(message string, candidates []DraftCandidate) (string, error) {
	patches := make([]map[string]any, 0, len(candidates))
	for _, candidate := range candidates {
		fields := candidateFields(candidate.Candidate)
		patches = append(patches, map[string]any{"candidate_id": candidate.Candidate.ID, "source_type": candidate.Candidate.SourceType, "fields": fields, "field_sources": parseSources(candidate.FieldSourcesJSON)})
	}
	raw, err := json.Marshal(map[string]any{"message": message, "candidate_patches": patches})
	return string(raw), err
}
func turnInputMessage(raw string) string {
	var value struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(raw), &value) != nil {
		return ""
	}
	return value.Message
}
func candidateFields(c Candidate) map[string]any {
	return map[string]any{"title": c.Draft.Title, "startPriceAmount": c.Draft.StartPriceAmount, "durationSeconds": c.Draft.DurationSeconds, "mainImageAssetId": c.Draft.MainImageAssetID, "mainImageUrl": c.Draft.MainImageURL, "minIncrementAmount": c.Draft.MinIncrementAmount, "capPriceAmount": c.Draft.CapPriceAmount, "category": c.Draft.Category, "description": c.Draft.Description, "tags": c.Draft.Tags, "stock": c.Draft.Stock, "antiSnipeWindowSeconds": c.Draft.AntiSnipeWindowSeconds, "antiSnipeExtendSeconds": c.Draft.AntiSnipeExtendSeconds, "maxExtendCount": c.Draft.MaxExtendCount, "depositAmount": c.Draft.DepositAmount, "existingLotId": c.ExistingLotID, "existingLotReadVersion": c.ExistingLotReadVersion}
}
func allCandidatesValid(candidates []DraftCandidate) bool {
	if len(candidates) == 0 {
		return false
	}
	for _, candidate := range candidates {
		if ValidateCandidate(candidate.Candidate) != nil {
			return false
		}
	}
	return true
}
func activeRun(runs []agentcontrol.Run) *agentcontrol.Run {
	for i := range runs {
		if runs[i].Status != agentcontrol.StatusCompleted && runs[i].Status != agentcontrol.StatusFailed && runs[i].Status != agentcontrol.StatusCancelled {
			return &runs[i]
		}
	}
	return nil
}
func sessionMergeEligible(status SessionStatus) bool {
	return status == StatusCollectingInput || status == StatusReadyForReview || status == StatusNeedsRevision
}
func sortRuns(runs []agentcontrol.Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].CreatedAtUnixMs == runs[j].CreatedAtUnixMs {
			return runs[i].ID < runs[j].ID
		}
		return runs[i].CreatedAtUnixMs < runs[j].CreatedAtUnixMs
	})
}
func mergedRun(messages []Message, runID string) bool {
	for _, message := range messages {
		if message.Role == MessageRoleAssistant && message.RunID == runID {
			return true
		}
	}
	return false
}
func replaceCandidate(current []DraftCandidate, replacement DraftCandidate) []DraftCandidate {
	out := append([]DraftCandidate(nil), current...)
	for i := range out {
		if out[i].Candidate.ID == replacement.Candidate.ID {
			out[i] = replacement
			return out
		}
	}
	return append(out, replacement)
}
func parseSources(raw string) map[string]string {
	out := map[string]string{}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}
func fieldSourcesJSON(value map[string]string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
func validSource(value string) bool {
	return value == "USER" || value == "AI" || value == "SYSTEM" || value == "EXISTING"
}
func normalizeActor(actor Actor) (Actor, error) {
	actor.TenantID = strings.TrimSpace(actor.TenantID)
	actor.OperatorID = strings.TrimSpace(actor.OperatorID)
	if actor.TenantID == "" || actor.OperatorID == "" {
		return Actor{}, apperr.ErrPermissionDenied
	}
	return actor, nil
}
func toRunActor(actor Actor) agentcontrol.Actor {
	return agentcontrol.Actor{TenantID: actor.TenantID, OperatorID: actor.OperatorID}
}
func invalid(message string) error { return fmt.Errorf("%w: %s", apperr.ErrInvalidArgument, message) }
func validIdempotencyKey(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '~') {
			return false
		}
	}
	return true
}
func newTraceID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
