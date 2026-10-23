package agentapproval

import (
	"context"
	"errors"
	"testing"

	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

type approvalObserverSpy struct {
	begins, hits       int
	attempts, outcomes []string
}

func (o *approvalObserverSpy) BeginApplyTransaction(ctx context.Context) (context.Context, func(string)) {
	o.begins++
	return ctx, func(result string) { o.attempts = append(o.attempts, result) }
}

func TestApplyFinalOutcomeRecordedAfterResolution(t *testing.T) {
	command := validCommand()
	fingerprint := RequestFingerprint(command)
	for _, tc := range []struct {
		name string
		repo *transactionRepositoryStub
		want string
	}{
		{"fresh", &transactionRepositoryStub{findErr: apperr.ErrNotFound, execution: executionFor(command, fingerprint)}, "success"},
		{"pre-read replay", &transactionRepositoryStub{execution: executionFor(command, fingerprint)}, "idempotent_hit"},
		{"malformed success", &transactionRepositoryStub{findErr: apperr.ErrNotFound, execution: Execution{Status: ExecutionApplying}}, "rollback"},
		{"concurrent replay", &transactionRepositoryStub{findResults: []struct {
			execution Execution
			err       error
		}{{err: apperr.ErrNotFound}, {execution: executionFor(command, fingerprint)}}, applyErr: apperr.ErrConflict}, "idempotent_hit"},
		{"stale conflict", &transactionRepositoryStub{findErr: apperr.ErrNotFound, applyErr: apperr.ErrAgentCandidateChanged}, "snapshot_conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observer := &approvalObserverSpy{}
			_, _ = NewService(&snapshotReaderStub{snapshot: validSnapshot()}, tc.repo).WithObserver(observer).Apply(context.Background(), command)
			if len(observer.outcomes) != 1 || observer.outcomes[0] != tc.want {
				t.Fatalf("outcomes=%v attempts=%v", observer.outcomes, observer.attempts)
			}
		})
	}
}

func TestApplyPreflightStaleRecordsOnlySnapshotConflict(t *testing.T) {
	observer := &approvalObserverSpy{}
	repo := &revisionRepoStub{transactionRepositoryStub: transactionRepositoryStub{findErr: apperr.ErrNotFound}}
	reader := &snapshotReaderStub{err: apperr.ErrAgentReviewSnapshotStale}
	_, err := NewService(reader, repo).WithObserver(observer).Apply(context.Background(), validCommand())
	if !errors.Is(err, apperr.ErrAgentReviewSnapshotStale) {
		t.Fatalf("err=%v", err)
	}
	if observer.begins != 0 || len(observer.outcomes) != 1 || observer.outcomes[0] != "snapshot_conflict" || repo.invalidations != 1 {
		t.Fatalf("observer=%+v invalidations=%d", observer, repo.invalidations)
	}
}
func (o *approvalObserverSpy) RecordApplyOutcome(result string) {
	o.outcomes = append(o.outcomes, result)
	if result == "idempotent_hit" {
		o.hits++
	}
}

type revisionRepoStub struct {
	transactionRepositoryStub
	invalidations    int
	invalidated      ApplyCommand
	invalidationErrs []error
	sawDetached      bool
}

func (r *revisionRepoStub) InvalidateReviewSnapshot(ctx context.Context, c ApplyCommand, _ error) error {
	r.invalidations++
	r.invalidated = c
	if ctx.Err() == nil {
		r.sawDetached = true
	}
	if len(r.invalidationErrs) > 0 {
		err := r.invalidationErrs[0]
		r.invalidationErrs = r.invalidationErrs[1:]
		return err
	}
	return nil
}

func TestApplyObserverCoversOnlyTransactionAndReplay(t *testing.T) {
	observer := &approvalObserverSpy{}
	bad := validCommand()
	bad.ContentHash = "bad"
	service := newTestService(nil, nil).WithObserver(observer)
	_, _ = service.Apply(context.Background(), bad)
	if observer.begins != 0 || observer.hits != 0 {
		t.Fatalf("preflight observed: %+v", observer)
	}

	command := validCommand()
	repo := &transactionRepositoryStub{findErr: apperr.ErrNotFound, execution: executionFor(command, RequestFingerprint(command))}
	service = NewService(&snapshotReaderStub{snapshot: validSnapshot()}, repo).WithObserver(observer)
	if _, err := service.Apply(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if observer.begins != 1 || len(observer.attempts) != 1 || observer.attempts[0] != "success" || len(observer.outcomes) != 1 || observer.outcomes[0] != "success" {
		t.Fatalf("transaction observations=%+v", observer)
	}

	replay := executionFor(command, RequestFingerprint(command))
	repo = &transactionRepositoryStub{execution: replay}
	service = NewService(&snapshotReaderStub{snapshot: validSnapshot()}, repo).WithObserver(observer)
	if _, err := service.Apply(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if observer.begins != 1 || observer.hits != 1 || observer.outcomes[len(observer.outcomes)-1] != "idempotent_hit" {
		t.Fatalf("replay observations=%+v", observer)
	}
}

func TestApplyRevisionInvalidationDetachedRetryAndExhaustion(t *testing.T) {
	command := validCommand()
	ctx, cancel := context.WithCancel(context.Background())
	repo := &revisionRepoStub{transactionRepositoryStub: transactionRepositoryStub{findErr: apperr.ErrNotFound, applyErr: apperr.ErrAgentCandidateChanged, onApply: cancel}, invalidationErrs: []error{errors.New("transient")}}
	_, err := NewService(&snapshotReaderStub{snapshot: validSnapshot()}, repo).Apply(ctx, command)
	if !errors.Is(err, apperr.ErrAgentCandidateChanged) || repo.invalidations != 2 || !repo.sawDetached {
		t.Fatalf("err=%v invalidations=%d detached=%v", err, repo.invalidations, repo.sawDetached)
	}

	repo = &revisionRepoStub{transactionRepositoryStub: transactionRepositoryStub{findErr: apperr.ErrNotFound, applyErr: apperr.ErrAgentCandidateChanged}, invalidationErrs: []error{errors.New("one"), errors.New("two"), errors.New("three")}}
	_, err = NewService(&snapshotReaderStub{snapshot: validSnapshot()}, repo).Apply(context.Background(), command)
	if !errors.Is(err, apperr.ErrAgentBatchApplyFailed) || repo.invalidations != 3 {
		t.Fatalf("exhaustion err=%v calls=%d", err, repo.invalidations)
	}
	// A later request retries reconciliation and may complete it.
	repo.transactionRepositoryStub.findErr = apperr.ErrNotFound
	_, _ = NewService(&snapshotReaderStub{snapshot: validSnapshot()}, repo).Apply(context.Background(), command)
	if repo.invalidations != 4 {
		t.Fatalf("later retry calls=%d", repo.invalidations)
	}
}

func TestApplyStaleTransactionInvalidatesReviewOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"snapshot", apperr.ErrAgentReviewSnapshotStale, 1}, {"candidate", apperr.ErrAgentCandidateChanged, 1}, {"asset", apperr.ErrAgentImageAssetInvalid, 1}, {"permission", apperr.ErrPermissionDenied, 0}, {"internal", errors.New("db"), 0}, {"idempotency", apperr.ErrConflict, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := transactionRepositoryStub{findErr: apperr.ErrNotFound, applyErr: tc.err}
			repo := &revisionRepoStub{transactionRepositoryStub: base}
			_, _ = NewService(&snapshotReaderStub{snapshot: validSnapshot()}, repo).Apply(context.Background(), validCommand())
			if repo.invalidations != tc.want {
				t.Fatalf("invalidations=%d", repo.invalidations)
			}
		})
	}
}
