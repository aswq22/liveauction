package agentapproval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"live-auction-bid/backend/app/auction/service/internal/agentconversation"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

const (
	maxSafeIDLength         = 128
	minIdempotencyKeyLength = 8
)

var (
	safeID             = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	safeIdempotencyKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]*$`)
)

// SnapshotReader is deliberately read-only. It supports an early exact
// envelope check for deterministic client errors; Apply remains responsible
// for repeating all checks under its transaction locks.
type SnapshotReader interface {
	FindReviewSnapshot(ctx context.Context, tenantID, operatorID, sessionID string, snapshotVersion int64) (agentconversation.ReviewSnapshot, error)
}

type Service struct {
	snapshots  SnapshotReader
	repository TransactionalRepository
	observer   Observer
}

type Observer interface {
	BeginApplyTransaction(context.Context) (context.Context, func(result string))
	RecordApplyOutcome(result string)
}

type RevisionInvalidator interface {
	InvalidateReviewSnapshot(context.Context, ApplyCommand, error) error
}

func (s *Service) WithObserver(observer Observer) *Service {
	if s != nil {
		s.observer = observer
	}
	return s
}

// Requirements performs the immutable, read-only authorization preflight used
// by transports before checking conditional permissions. Apply deliberately
// repeats this check and every mutable check at the transactional boundary.
func (s *Service) Requirements(ctx context.Context, command ApplyCommand) (agentconversation.ReviewSnapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return agentconversation.ReviewSnapshot{}, false, err
	}
	normalized, err := normalizeCommand(command)
	if err != nil {
		return agentconversation.ReviewSnapshot{}, false, err
	}
	if s == nil || s.snapshots == nil || s.repository == nil {
		if s != nil {
			s.recordOutcome("rollback")
		}
		return agentconversation.ReviewSnapshot{}, false, apperr.ErrAgentBatchApplyFailed
	}
	snapshot, err := s.snapshots.FindReviewSnapshot(ctx, normalized.TenantID, normalized.OperatorID, normalized.SessionID, normalized.SnapshotVersion)
	if err != nil {
		if isRevisionConflict(err) {
			if s.invalidateRevision(ctx, normalized, err) != nil {
				s.recordOutcome("rollback")
				return agentconversation.ReviewSnapshot{}, false, apperr.ErrAgentBatchApplyFailed
			}
		}
		s.recordAdmittedRequirementsFailure(err)
		return agentconversation.ReviewSnapshot{}, false, safeRepositoryError(err)
	}
	if !snapshotMatchesCommand(snapshot, normalized) {
		s.recordOutcome("snapshot_conflict")
		return agentconversation.ReviewSnapshot{}, false, apperr.ErrAgentReviewSnapshotStale
	}
	requiresCreate := false
	for _, candidate := range snapshot.Candidates {
		if candidate.SourceType == agentconversation.SourceNew {
			requiresCreate = true
			break
		}
	}
	return snapshot, requiresCreate, nil
}

func NewService(snapshots SnapshotReader, repository TransactionalRepository) *Service {
	return &Service{snapshots: snapshots, repository: repository}
}

func (s *Service) Apply(ctx context.Context, command ApplyCommand) (Execution, error) {
	if err := ctx.Err(); err != nil {
		return Execution{}, err
	}
	if s == nil || s.snapshots == nil || s.repository == nil {
		return Execution{}, apperr.ErrAgentBatchApplyFailed
	}
	command, err := normalizeCommand(command)
	if err != nil {
		return Execution{}, err
	}

	snapshot, err := s.snapshots.FindReviewSnapshot(ctx, command.TenantID, command.OperatorID, command.SessionID, command.SnapshotVersion)
	if err != nil {
		if errors.Is(err, apperr.ErrAgentReviewSnapshotStale) {
			if s.invalidateRevision(ctx, command, err) != nil {
				s.recordOutcome("rollback")
				return Execution{}, apperr.ErrAgentBatchApplyFailed
			}
			s.recordOutcome("snapshot_conflict")
		}
		return Execution{}, safeRepositoryError(err)
	}
	if err := ctx.Err(); err != nil {
		return Execution{}, err
	}
	if !snapshotMatchesCommand(snapshot, command) {
		// The immutable snapshot itself is still current; this is a changed
		// request envelope (for example, same key/different payload), not a
		// durable snapshot conflict and must not invalidate review state.
		s.recordOutcome("rollback")
		return Execution{}, apperr.ErrAgentReviewSnapshotStale
	}

	fingerprint := RequestFingerprint(command)
	if existing, found, err := s.findExisting(ctx, command, fingerprint); err != nil || found {
		if found {
			existing.Target = command.Target
			existing.IdempotentReplay = true
			s.recordOutcome("idempotent_hit")
		} else if err != nil {
			s.recordOutcome("rollback")
		}
		return existing, err
	}
	if err := ctx.Err(); err != nil {
		return Execution{}, err
	}

	transactionCtx := ctx
	end := func(string) {}
	if s.observer != nil {
		transactionCtx, end = s.observer.BeginApplyTransaction(ctx)
	}
	execution, err := s.repository.Apply(transactionCtx, command)
	execution.Target = command.Target
	attemptResult := "success"
	if execution.IdempotentReplay {
		attemptResult = "idempotent_hit"
	}
	if err != nil {
		attemptResult = "rollback"
		if errors.Is(err, apperr.ErrConflict) || errors.Is(err, apperr.ErrAgentBatchAlreadyApplied) {
			attemptResult = "conflict"
		}
		if errors.Is(err, apperr.ErrAgentReviewSnapshotStale) || errors.Is(err, apperr.ErrAgentCandidateChanged) || errors.Is(err, apperr.ErrAgentImageAssetInvalid) {
			attemptResult = "snapshot_conflict"
		}
	}
	end(attemptResult)
	if err == nil {
		if err := validateCompatibleExecution(execution, command, fingerprint); err != nil {
			s.recordOutcome("rollback")
			return Execution{}, err
		}
		if execution.IdempotentReplay {
			s.recordOutcome("idempotent_hit")
		} else {
			s.recordOutcome("success")
		}
		return execution, nil
	}
	if errors.Is(err, apperr.ErrAgentReviewSnapshotStale) || errors.Is(err, apperr.ErrAgentCandidateChanged) || errors.Is(err, apperr.ErrAgentImageAssetInvalid) {
		if s.invalidateRevision(ctx, command, err) != nil {
			s.recordOutcome("rollback")
			return Execution{}, apperr.ErrAgentBatchApplyFailed
		}
	}
	// A concurrent caller can win the execution-key insert after our first
	// lookup. Re-read only for idempotency/terminal conflicts and still require
	// the complete request fingerprint before returning its result.
	if errors.Is(err, apperr.ErrConflict) || errors.Is(err, apperr.ErrAgentBatchAlreadyApplied) {
		if existing, found, replayErr := s.findExisting(ctx, command, fingerprint); replayErr != nil || found {
			if found {
				existing.IdempotentReplay = true
				s.recordOutcome("idempotent_hit")
			} else {
				s.recordOutcome("rollback")
			}
			return existing, replayErr
		}
	}
	if errors.Is(err, apperr.ErrAgentReviewSnapshotStale) || errors.Is(err, apperr.ErrAgentCandidateChanged) || errors.Is(err, apperr.ErrAgentImageAssetInvalid) {
		s.recordOutcome("snapshot_conflict")
	} else {
		s.recordOutcome("rollback")
	}
	return Execution{}, safeRepositoryError(err)
}

func (s *Service) recordOutcome(result string) {
	if s.observer != nil {
		s.observer.RecordApplyOutcome(result)
	}
}

func (s *Service) recordAdmittedRequirementsFailure(err error) {
	if isRevisionConflict(err) {
		s.recordOutcome("snapshot_conflict")
		return
	}
	for _, excluded := range []error{
		context.Canceled, context.DeadlineExceeded,
		apperr.ErrInvalidArgument, apperr.ErrUnauthenticated, apperr.ErrPermissionDenied, apperr.ErrNotFound,
	} {
		if errors.Is(err, excluded) {
			return
		}
	}
	s.recordOutcome("rollback")
}

func isRevisionConflict(err error) bool {
	return errors.Is(err, apperr.ErrAgentReviewSnapshotStale) ||
		errors.Is(err, apperr.ErrAgentCandidateChanged) ||
		errors.Is(err, apperr.ErrAgentImageAssetInvalid)
}

func (s *Service) invalidateRevision(ctx context.Context, command ApplyCommand, cause error) error {
	if invalidator, ok := s.repository.(RevisionInvalidator); ok {
		detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if err = invalidator.InvalidateReviewSnapshot(detached, command, cause); err == nil {
				return nil
			}
			if attempt < 2 {
				select {
				case <-detached.Done():
					return err
				case <-time.After(time.Duration(attempt+1) * 10 * time.Millisecond):
				}
			}
		}
		slog.Error("agent review invalidation exhausted", "session_id", command.SessionID, "error_code", "AGENT_REVISION_RECONCILE_FAILED")
		return err
	}
	return nil
}

func (s *Service) findExisting(ctx context.Context, command ApplyCommand, fingerprint string) (Execution, bool, error) {
	existing, err := s.repository.FindExecutionByKey(ctx, command.SessionID, command.SnapshotVersion, command.IdempotencyKey)
	if err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			return Execution{}, false, nil
		}
		return Execution{}, false, safeRepositoryError(err)
	}
	if err := validateCompatibleExecution(existing, command, fingerprint); err != nil {
		return Execution{}, false, err
	}
	return existing, true, nil
}

// validateCompatibleExecution is the single trust boundary for both a fresh
// transactional result and every idempotency replay/re-read. Only a fully
// materialized APPLIED execution for this exact canonical request is success.
func validateCompatibleExecution(execution Execution, command ApplyCommand, fingerprint string) error {
	if execution.Status != ExecutionApplied ||
		execution.TenantID != command.TenantID || execution.OperatorID != command.OperatorID ||
		execution.SessionID != command.SessionID || execution.SnapshotVersion != command.SnapshotVersion ||
		execution.IdempotencyKey != command.IdempotencyKey || execution.RequestFingerprint == "" ||
		execution.RequestFingerprint != fingerprint ||
		!reflect.DeepEqual(execution.SelectedCandidateIDs, command.SelectedCandidateIDs) ||
		!reflect.DeepEqual(execution.OrderedCandidateIDs, command.OrderedCandidateIDs) {
		return apperr.ErrConflict
	}
	return nil
}

func normalizeCommand(command ApplyCommand) (ApplyCommand, error) {
	command.TenantID = strings.TrimSpace(command.TenantID)
	command.OperatorID = strings.TrimSpace(command.OperatorID)
	command.SessionID = strings.TrimSpace(command.SessionID)
	command.ContentHash = strings.TrimSpace(command.ContentHash)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	if command.Target == "" {
		command.Target = ApplyTargetQueue
	}
	if command.Target != ApplyTargetQueue && command.Target != ApplyTargetInventoryOnly {
		return ApplyCommand{}, invalid("invalid apply target")
	}
	if !validSafeID(command.TenantID) || !validSafeID(command.OperatorID) || !validSafeID(command.SessionID) {
		return ApplyCommand{}, invalid("tenant, operator and session identities are required")
	}
	if command.SnapshotVersion <= 0 {
		return ApplyCommand{}, invalid("snapshot version must be positive")
	}
	if !validCanonicalHash(command.ContentHash) {
		return ApplyCommand{}, invalid("content hash must be canonical lowercase SHA-256")
	}
	if len(command.IdempotencyKey) < minIdempotencyKeyLength || len(command.IdempotencyKey) > maxSafeIDLength || !safeIdempotencyKey.MatchString(command.IdempotencyKey) {
		return ApplyCommand{}, invalid("invalid idempotency key")
	}
	selected, ordered, err := normalizeSelection(command.SelectedCandidateIDs, command.OrderedCandidateIDs)
	if err != nil {
		return ApplyCommand{}, err
	}
	command.SelectedCandidateIDs = selected
	command.OrderedCandidateIDs = ordered
	return command, nil
}

func normalizeSelection(selectedRaw, orderedRaw []string) ([]string, []string, error) {
	if len(selectedRaw) == 0 || len(selectedRaw) != len(orderedRaw) {
		return nil, nil, invalid("explicit nonempty matching selection and order are required")
	}
	selected := make([]string, len(selectedRaw))
	selectedSet := make(map[string]struct{}, len(selectedRaw))
	for i, raw := range selectedRaw {
		id := strings.TrimSpace(raw)
		if !validSafeID(id) {
			return nil, nil, invalid("invalid selected candidate id")
		}
		if _, exists := selectedSet[id]; exists {
			return nil, nil, invalid("duplicate selected candidate id")
		}
		selected[i] = id
		selectedSet[id] = struct{}{}
	}
	ordered := make([]string, len(orderedRaw))
	orderedSet := make(map[string]struct{}, len(orderedRaw))
	for i, raw := range orderedRaw {
		id := strings.TrimSpace(raw)
		if !validSafeID(id) {
			return nil, nil, invalid("invalid ordered candidate id")
		}
		if _, exists := orderedSet[id]; exists {
			return nil, nil, invalid("duplicate ordered candidate id")
		}
		if _, selected := selectedSet[id]; !selected {
			return nil, nil, invalid("selected and ordered candidate sets differ")
		}
		ordered[i] = id
		orderedSet[id] = struct{}{}
	}
	if len(orderedSet) != len(selectedSet) {
		return nil, nil, invalid("selected and ordered candidate sets differ")
	}
	sort.Strings(selected)
	return selected, ordered, nil
}

func snapshotMatchesCommand(snapshot agentconversation.ReviewSnapshot, command ApplyCommand) bool {
	return snapshot.TenantID == command.TenantID &&
		snapshot.OperatorID == command.OperatorID &&
		snapshot.CreatedBy == command.OperatorID &&
		snapshot.SessionID == command.SessionID &&
		snapshot.SnapshotVersion == command.SnapshotVersion &&
		snapshot.ContentHash == command.ContentHash &&
		reflect.DeepEqual(snapshot.SelectedCandidateIDs, command.SelectedCandidateIDs) &&
		reflect.DeepEqual(snapshot.OrderedCandidateIDs, command.OrderedCandidateIDs)
}

// RequestFingerprint is the deterministic identity of the authorized payload.
// The idempotency key itself is deliberately excluded: it names an attempt,
// while the fingerprint proves which normalized request that attempt represents.
func RequestFingerprint(command ApplyCommand) string {
	command, err := normalizeCommand(command)
	if err != nil {
		return ""
	}
	payload := struct {
		TenantID             string      `json:"tenantId"`
		OperatorID           string      `json:"operatorId"`
		SessionID            string      `json:"sessionId"`
		SnapshotVersion      int64       `json:"snapshotVersion"`
		ContentHash          string      `json:"contentHash"`
		SelectedCandidateIDs []string    `json:"selectedCandidateIds"`
		OrderedCandidateIDs  []string    `json:"orderedCandidateIds"`
		Target               ApplyTarget `json:"target"`
	}{command.TenantID, command.OperatorID, command.SessionID, command.SnapshotVersion, command.ContentHash, command.SelectedCandidateIDs, command.OrderedCandidateIDs, command.Target}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func validSafeID(value string) bool {
	return len(value) > 0 && len(value) <= maxSafeIDLength && safeID.MatchString(value)
}

func validCanonicalHash(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func safeRepositoryError(err error) error {
	if err == nil {
		return nil
	}
	for _, safe := range []error{
		context.Canceled, context.DeadlineExceeded,
		apperr.ErrNotFound, apperr.ErrConflict, apperr.ErrInvalidArgument,
		apperr.ErrPermissionDenied, apperr.ErrUnauthenticated,
		apperr.ErrAgentSessionVersionConflict, apperr.ErrAgentReviewSnapshotStale, apperr.ErrAgentCandidateChanged,
		apperr.ErrAgentImageAssetInvalid, apperr.ErrAgentBatchAlreadyApplied,
		apperr.ErrAgentBatchApplyFailed, apperr.ErrQueuePositionConflict,
	} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return apperr.ErrAgentBatchApplyFailed
}

func invalid(message string) error {
	return fmt.Errorf("%w: %s", apperr.ErrInvalidArgument, message)
}
