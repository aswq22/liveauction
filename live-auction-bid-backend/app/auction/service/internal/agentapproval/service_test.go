package agentapproval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"live-auction-bid/backend/app/auction/service/internal/agentconversation"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

type snapshotReaderStub struct {
	snapshot agentconversation.ReviewSnapshot
	err      error
	calls    int
}

func (s *snapshotReaderStub) FindReviewSnapshot(_ context.Context, tenantID, operatorID, sessionID string, version int64) (agentconversation.ReviewSnapshot, error) {
	s.calls++
	return s.snapshot, s.err
}

type transactionRepositoryStub struct {
	execution   Execution
	findErr     error
	findResults []struct {
		execution Execution
		err       error
	}
	applyErr   error
	findCalls  int
	applyCalls int
	command    ApplyCommand
	onFind     func(int)
	onApply    func()
}

func (r *transactionRepositoryStub) FindExecutionByKey(_ context.Context, sessionID string, snapshotVersion int64, key string) (Execution, error) {
	r.findCalls++
	if r.onFind != nil {
		r.onFind(r.findCalls)
	}
	if len(r.findResults) > 0 {
		result := r.findResults[0]
		r.findResults = r.findResults[1:]
		return result.execution, result.err
	}
	return r.execution, r.findErr
}

func (r *transactionRepositoryStub) Apply(_ context.Context, command ApplyCommand) (Execution, error) {
	r.applyCalls++
	r.command = command
	if r.onApply != nil {
		r.onApply()
	}
	return r.execution, r.applyErr
}

func validCommand() ApplyCommand {
	return ApplyCommand{
		TenantID: "tenant-1", OperatorID: "operator-1", SessionID: "session-1",
		SnapshotVersion: 3, ContentHash: strings.Repeat("a", 64),
		SelectedCandidateIDs: []string{"candidate-1", "candidate-2"},
		OrderedCandidateIDs:  []string{"candidate-2", "candidate-1"},
		IdempotencyKey:       "apply-request-1",
	}
}

func validSnapshot() agentconversation.ReviewSnapshot {
	c := validCommand()
	return agentconversation.ReviewSnapshot{
		TenantID: c.TenantID, OperatorID: c.OperatorID, CreatedBy: c.OperatorID,
		SessionID: c.SessionID, SnapshotVersion: c.SnapshotVersion, ContentHash: c.ContentHash,
		SelectedCandidateIDs: append([]string(nil), c.SelectedCandidateIDs...),
		OrderedCandidateIDs:  append([]string(nil), c.OrderedCandidateIDs...),
	}
}

func TestRequestFingerprintBindsApplyTarget(t *testing.T) {
	queue := validCommand()
	queue.Target = ApplyTargetQueue
	inventory := queue
	inventory.Target = ApplyTargetInventoryOnly
	if RequestFingerprint(queue) == RequestFingerprint(inventory) {
		t.Fatal("queue and inventory-only requests must not share a fingerprint")
	}
}

func newTestService(reader *snapshotReaderStub, repo *transactionRepositoryStub) *Service {
	if reader == nil {
		reader = &snapshotReaderStub{snapshot: validSnapshot()}
	}
	if repo == nil {
		repo = &transactionRepositoryStub{findErr: apperr.ErrNotFound}
	}
	return NewService(reader, repo)
}

func TestApplyRejectsInvalidIdentityVersionHashKeyAndCollectionsBeforeIO(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ApplyCommand)
	}{
		{"blank tenant", func(c *ApplyCommand) { c.TenantID = " " }},
		{"blank operator", func(c *ApplyCommand) { c.OperatorID = "" }},
		{"blank session", func(c *ApplyCommand) { c.SessionID = "\t" }},
		{"unsafe tenant", func(c *ApplyCommand) { c.TenantID = "tenant/1" }},
		{"nonpositive snapshot", func(c *ApplyCommand) { c.SnapshotVersion = 0 }},
		{"short hash", func(c *ApplyCommand) { c.ContentHash = "abc" }},
		{"uppercase hash", func(c *ApplyCommand) { c.ContentHash = strings.Repeat("A", 64) }},
		{"short key", func(c *ApplyCommand) { c.IdempotencyKey = "short" }},
		{"unsafe key", func(c *ApplyCommand) { c.IdempotencyKey = "apply/key" }},
		{"long key", func(c *ApplyCommand) { c.IdempotencyKey = strings.Repeat("a", 129) }},
		{"empty selected", func(c *ApplyCommand) { c.SelectedCandidateIDs = nil }},
		{"empty ordered", func(c *ApplyCommand) { c.OrderedCandidateIDs = nil }},
		{"duplicate selected", func(c *ApplyCommand) { c.SelectedCandidateIDs = []string{"candidate-1", "candidate-1"} }},
		{"duplicate ordered", func(c *ApplyCommand) { c.OrderedCandidateIDs = []string{"candidate-1", "candidate-1"} }},
		{"different sets", func(c *ApplyCommand) { c.OrderedCandidateIDs = []string{"candidate-1", "candidate-3"} }},
		{"unsafe candidate", func(c *ApplyCommand) {
			c.SelectedCandidateIDs[0] = "candidate/1"
			c.OrderedCandidateIDs[1] = "candidate/1"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command := validCommand()
			tt.mutate(&command)
			reader := &snapshotReaderStub{snapshot: validSnapshot()}
			repo := &transactionRepositoryStub{findErr: apperr.ErrNotFound}
			_, err := newTestService(reader, repo).Apply(context.Background(), command)
			if !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Fatalf("Apply() error = %v, want invalid argument", err)
			}
			if reader.calls != 0 || repo.findCalls != 0 || repo.applyCalls != 0 {
				t.Fatalf("invalid request performed IO: reader=%d find=%d apply=%d", reader.calls, repo.findCalls, repo.applyCalls)
			}
		})
	}
}

func TestApplyNormalizesSafeWhitespaceAndSelectedSet(t *testing.T) {
	command := validCommand()
	command.TenantID = " tenant-1 "
	command.OperatorID = " operator-1 "
	command.SessionID = " session-1 "
	command.ContentHash = " " + command.ContentHash + " "
	command.IdempotencyKey = " apply-request-1 "
	command.SelectedCandidateIDs = []string{" candidate-2 ", "candidate-1"}
	command.OrderedCandidateIDs = []string{" candidate-2", "candidate-1 "}
	canonical := validCommand()
	repo := &transactionRepositoryStub{findErr: apperr.ErrNotFound, execution: executionFor(canonical, RequestFingerprint(canonical))}
	result, err := newTestService(nil, repo).Apply(context.Background(), command)
	if err != nil || result.ID != "execution-1" {
		t.Fatalf("Apply() = (%+v, %v)", result, err)
	}
	if got := strings.Join(repo.command.SelectedCandidateIDs, ","); got != "candidate-1,candidate-2" {
		t.Fatalf("canonical selected IDs = %q", got)
	}
	if got := strings.Join(repo.command.OrderedCandidateIDs, ","); got != "candidate-2,candidate-1" {
		t.Fatalf("preserved order = %q", got)
	}
}

func TestApplyRequiresExactImmutableSnapshotEnvelope(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*agentconversation.ReviewSnapshot)
	}{
		{"tenant", func(s *agentconversation.ReviewSnapshot) { s.TenantID = "tenant-2" }},
		{"operator", func(s *agentconversation.ReviewSnapshot) { s.OperatorID = "operator-2" }},
		{"creator", func(s *agentconversation.ReviewSnapshot) { s.CreatedBy = "operator-2" }},
		{"session", func(s *agentconversation.ReviewSnapshot) { s.SessionID = "session-2" }},
		{"version", func(s *agentconversation.ReviewSnapshot) { s.SnapshotVersion++ }},
		{"hash", func(s *agentconversation.ReviewSnapshot) { s.ContentHash = strings.Repeat("b", 64) }},
		{"selected", func(s *agentconversation.ReviewSnapshot) { s.SelectedCandidateIDs = []string{"candidate-1"} }},
		{"ordered", func(s *agentconversation.ReviewSnapshot) {
			s.OrderedCandidateIDs = []string{"candidate-1", "candidate-2"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := validSnapshot()
			tt.mutate(&snapshot)
			repo := &transactionRepositoryStub{findErr: apperr.ErrNotFound}
			_, err := newTestService(&snapshotReaderStub{snapshot: snapshot}, repo).Apply(context.Background(), validCommand())
			if !errors.Is(err, apperr.ErrAgentReviewSnapshotStale) {
				t.Fatalf("Apply() error = %v, want stale snapshot", err)
			}
			if repo.findCalls != 0 || repo.applyCalls != 0 {
				t.Fatalf("snapshot mismatch reached transactional repository")
			}
		})
	}
}

func TestApplySameKeyAndCanonicalPayloadReturnsOriginalExecution(t *testing.T) {
	command := validCommand()
	fingerprint := RequestFingerprint(command)
	want := executionFor(command, fingerprint)
	want.CreatedLotIDs = []string{"lot-1"}
	repo := &transactionRepositoryStub{execution: want}
	got, err := newTestService(nil, repo).Apply(context.Background(), command)
	if err != nil || got.ID != want.ID || len(got.CreatedLotIDs) != 1 {
		t.Fatalf("Apply() = (%+v, %v), want original", got, err)
	}
	if repo.applyCalls != 0 {
		t.Fatalf("Apply called repository.Apply %d times on replay", repo.applyCalls)
	}
}

func TestApplySameKeyDifferentPayloadConflicts(t *testing.T) {
	command := validCommand()
	different := command
	different.ContentHash = strings.Repeat("b", 64)
	repo := &transactionRepositoryStub{execution: executionFor(command, RequestFingerprint(different))}
	_, err := newTestService(nil, repo).Apply(context.Background(), validCommand())
	if !errors.Is(err, apperr.ErrConflict) {
		t.Fatalf("Apply() error = %v, want conflict", err)
	}
	if repo.applyCalls != 0 {
		t.Fatalf("mismatched replay called Apply")
	}
}

func TestApplyRejectsMalformedSuccessfulRepositoryExecution(t *testing.T) {
	command := validCommand()
	tests := []struct {
		name   string
		mutate func(*Execution)
	}{
		{"cross tenant", func(e *Execution) { e.TenantID = "tenant-2" }},
		{"cross operator", func(e *Execution) { e.OperatorID = "operator-2" }},
		{"cross session", func(e *Execution) { e.SessionID = "session-2" }},
		{"wrong version", func(e *Execution) { e.SnapshotVersion++ }},
		{"wrong key", func(e *Execution) { e.IdempotencyKey = "apply-request-2" }},
		{"wrong fingerprint", func(e *Execution) {
			e.RequestFingerprint = RequestFingerprint(ApplyCommand{
				TenantID: command.TenantID, OperatorID: command.OperatorID, SessionID: command.SessionID,
				SnapshotVersion: command.SnapshotVersion, ContentHash: strings.Repeat("b", 64),
				SelectedCandidateIDs: command.SelectedCandidateIDs, OrderedCandidateIDs: command.OrderedCandidateIDs,
				IdempotencyKey: command.IdempotencyKey,
			})
		}},
		{"applying", func(e *Execution) { e.Status = ExecutionApplying }},
		{"failed", func(e *Execution) { e.Status = ExecutionFailed }},
		{"unknown status", func(e *Execution) { e.Status = ExecutionStatus("UNKNOWN") }},
		{"selected mismatch", func(e *Execution) { e.SelectedCandidateIDs = []string{"candidate-1"} }},
		{"ordered mismatch", func(e *Execution) { e.OrderedCandidateIDs = []string{"candidate-1", "candidate-2"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execution := executionFor(command, RequestFingerprint(command))
			tt.mutate(&execution)
			repo := &transactionRepositoryStub{findErr: apperr.ErrNotFound, execution: execution}
			got, err := newTestService(nil, repo).Apply(context.Background(), command)
			if !errors.Is(err, apperr.ErrConflict) || got.ID != "" {
				t.Fatalf("Apply() = (%+v, %v), want zero/conflict", got, err)
			}
			if repo.findCalls != 1 || repo.applyCalls != 1 {
				t.Fatalf("calls find/apply = %d/%d, want 1/1", repo.findCalls, repo.applyCalls)
			}
		})
	}
}

func TestApplyReplayRequiresFullyCompatibleAppliedExecution(t *testing.T) {
	command := validCommand()
	tests := []struct {
		name   string
		mutate func(*Execution)
	}{
		{"applying", func(e *Execution) { e.Status = ExecutionApplying }},
		{"failed", func(e *Execution) { e.Status = ExecutionFailed }},
		{"unknown", func(e *Execution) { e.Status = ExecutionStatus("UNKNOWN") }},
		{"cross tenant", func(e *Execution) { e.TenantID = "tenant-2" }},
		{"cross operator", func(e *Execution) { e.OperatorID = "operator-2" }},
		{"cross session", func(e *Execution) { e.SessionID = "session-2" }},
		{"wrong version", func(e *Execution) { e.SnapshotVersion++ }},
		{"wrong key", func(e *Execution) { e.IdempotencyKey = "apply-request-2" }},
		{"wrong fingerprint", func(e *Execution) { e.RequestFingerprint = strings.Repeat("0", 64) }},
		{"selected mismatch", func(e *Execution) { e.SelectedCandidateIDs = []string{"candidate-2"} }},
		{"ordered mismatch", func(e *Execution) { e.OrderedCandidateIDs = []string{"candidate-1", "candidate-2"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execution := executionFor(command, RequestFingerprint(command))
			tt.mutate(&execution)
			repo := &transactionRepositoryStub{execution: execution}
			got, err := newTestService(nil, repo).Apply(context.Background(), command)
			if !errors.Is(err, apperr.ErrConflict) || got.ID != "" || repo.applyCalls != 0 {
				t.Fatalf("replay = (%+v, %v), apply calls %d", got, err, repo.applyCalls)
			}
		})
	}
}

func TestRequestFingerprintCanonicalMutationMatrix(t *testing.T) {
	base := validCommand()
	want := RequestFingerprint(base)
	mutations := []struct {
		name   string
		mutate func(*ApplyCommand)
	}{
		{"tenant", func(c *ApplyCommand) { c.TenantID = "tenant-2" }},
		{"operator", func(c *ApplyCommand) { c.OperatorID = "operator-2" }},
		{"session", func(c *ApplyCommand) { c.SessionID = "session-2" }},
		{"snapshot version", func(c *ApplyCommand) { c.SnapshotVersion++ }},
		{"content hash", func(c *ApplyCommand) { c.ContentHash = strings.Repeat("b", 64) }},
		{"selected membership", func(c *ApplyCommand) {
			c.SelectedCandidateIDs = []string{"candidate-1", "candidate-3"}
			c.OrderedCandidateIDs = []string{"candidate-3", "candidate-1"}
		}},
		{"ordered order", func(c *ApplyCommand) { c.OrderedCandidateIDs = []string{"candidate-1", "candidate-2"} }},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			changed := base
			tt.mutate(&changed)
			if got := RequestFingerprint(changed); got == "" || got == want {
				t.Fatalf("changed fingerprint = %q, base %q", got, want)
			}
		})
	}
	keyOnly := base
	keyOnly.IdempotencyKey = "apply-request-2"
	if got := RequestFingerprint(keyOnly); got != want {
		t.Fatalf("idempotency-only fingerprint = %q, want %q", got, want)
	}
	equivalent := base
	equivalent.TenantID = " tenant-1 "
	equivalent.SelectedCandidateIDs = []string{" candidate-2 ", "candidate-1 "}
	equivalent.OrderedCandidateIDs = []string{" candidate-2 ", "candidate-1 "}
	if got := RequestFingerprint(equivalent); got != want {
		t.Fatalf("normalized equivalent fingerprint = %q, want %q", got, want)
	}
}

func TestApplyNeverUsesFingerprintOrKeyAloneAsAuthorization(t *testing.T) {
	command := validCommand()
	for _, mutate := range []func(*Execution){
		func(e *Execution) { e.TenantID = "tenant-2" },
		func(e *Execution) { e.OperatorID = "operator-2" },
		func(e *Execution) { e.SessionID = "session-2" },
		func(e *Execution) { e.SnapshotVersion++ },
		func(e *Execution) { e.IdempotencyKey = "apply-request-2" },
	} {
		execution := executionFor(command, RequestFingerprint(command))
		mutate(&execution)
		repo := &transactionRepositoryStub{execution: execution}
		_, err := newTestService(nil, repo).Apply(context.Background(), command)
		if !errors.Is(err, apperr.ErrConflict) || repo.applyCalls != 0 {
			t.Fatalf("identity-mismatched replay = %v, apply calls %d", err, repo.applyCalls)
		}
	}
}

func TestApplyRecoversCompatibleConcurrentReplayAfterConflict(t *testing.T) {
	command := validCommand()
	want := executionFor(command, RequestFingerprint(command))
	repo := &transactionRepositoryStub{applyErr: apperr.ErrConflict, findResults: []struct {
		execution Execution
		err       error
	}{
		{err: apperr.ErrNotFound}, {execution: want},
	}}
	got, err := newTestService(nil, repo).Apply(context.Background(), command)
	if err != nil || got.ID != want.ID {
		t.Fatalf("Apply() = (%+v, %v), want concurrent replay", got, err)
	}
	if repo.findCalls != 2 || repo.applyCalls != 1 {
		t.Fatalf("calls find/apply = %d/%d, want 2/1", repo.findCalls, repo.applyCalls)
	}
}

func TestApplyPropagatesSafeRepositoryErrorsAndContextCancellation(t *testing.T) {
	t.Run("snapshot not found", func(t *testing.T) {
		repo := &transactionRepositoryStub{}
		_, err := newTestService(&snapshotReaderStub{err: apperr.ErrNotFound}, repo).Apply(context.Background(), validCommand())
		if !errors.Is(err, apperr.ErrNotFound) || repo.findCalls != 0 {
			t.Fatalf("error/calls = %v/%d", err, repo.findCalls)
		}
	})
	t.Run("transaction conflict", func(t *testing.T) {
		repo := &transactionRepositoryStub{findErr: apperr.ErrNotFound, applyErr: apperr.ErrAgentCandidateChanged}
		_, err := newTestService(nil, repo).Apply(context.Background(), validCommand())
		if !errors.Is(err, apperr.ErrAgentCandidateChanged) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("unknown storage error is masked", func(t *testing.T) {
		repo := &transactionRepositoryStub{findErr: apperr.ErrNotFound, applyErr: errors.New("driver: secret dsn")}
		_, err := newTestService(nil, repo).Apply(context.Background(), validCommand())
		if err != apperr.ErrAgentBatchApplyFailed {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("cancelled before IO", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reader := &snapshotReaderStub{snapshot: validSnapshot()}
		repo := &transactionRepositoryStub{findErr: apperr.ErrNotFound}
		_, err := newTestService(reader, repo).Apply(ctx, validCommand())
		if !errors.Is(err, context.Canceled) || reader.calls != 0 || repo.findCalls != 0 || repo.applyCalls != 0 {
			t.Fatalf("cancel result = %v, calls=%d/%d/%d", err, reader.calls, repo.findCalls, repo.applyCalls)
		}
	})
	t.Run("cancelled after initial find", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		repo := &transactionRepositoryStub{findErr: apperr.ErrNotFound, onFind: func(call int) { cancel() }}
		_, err := newTestService(nil, repo).Apply(ctx, validCommand())
		if !errors.Is(err, context.Canceled) || repo.findCalls != 1 || repo.applyCalls != 0 {
			t.Fatalf("cancel result = %v, calls=%d/%d", err, repo.findCalls, repo.applyCalls)
		}
	})
	t.Run("apply cancellation is safe", func(t *testing.T) {
		repo := &transactionRepositoryStub{findErr: apperr.ErrNotFound, applyErr: context.DeadlineExceeded}
		_, err := newTestService(nil, repo).Apply(context.Background(), validCommand())
		if !errors.Is(err, context.DeadlineExceeded) || repo.findCalls != 1 || repo.applyCalls != 1 {
			t.Fatalf("deadline result = %v, calls=%d/%d", err, repo.findCalls, repo.applyCalls)
		}
	})
	t.Run("re-read cancellation is safe", func(t *testing.T) {
		repo := &transactionRepositoryStub{applyErr: apperr.ErrConflict, findResults: []struct {
			execution Execution
			err       error
		}{{err: apperr.ErrNotFound}, {err: context.Canceled}}}
		_, err := newTestService(nil, repo).Apply(context.Background(), validCommand())
		if !errors.Is(err, context.Canceled) || repo.findCalls != 2 || repo.applyCalls != 1 {
			t.Fatalf("cancel result = %v, calls=%d/%d", err, repo.findCalls, repo.applyCalls)
		}
	})
}

func executionFor(command ApplyCommand, fingerprint string) Execution {
	return Execution{
		ID: "execution-1", TenantID: command.TenantID, OperatorID: command.OperatorID,
		SessionID: command.SessionID, SnapshotVersion: command.SnapshotVersion,
		IdempotencyKey: command.IdempotencyKey, RequestFingerprint: fingerprint,
		Status:               ExecutionApplied,
		SelectedCandidateIDs: append([]string(nil), command.SelectedCandidateIDs...),
		OrderedCandidateIDs:  append([]string(nil), command.OrderedCandidateIDs...),
	}
}
