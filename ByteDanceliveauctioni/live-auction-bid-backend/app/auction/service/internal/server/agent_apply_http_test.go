package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	auctionv1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/agentapproval"
	"live-auction-bid/backend/app/auction/service/internal/agentconversation"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
	"live-auction-bid/backend/app/auction/service/internal/pkg/requestctx"
)

func (f *sessionHandlerFixture) rebuildHandlerWithApproval(t *testing.T, approval *agentapproval.Service) {
	t.Helper()
	service, err := agentconversation.NewService(f.repo, f.room, f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	f.handler = NewAgentSessionHTTPHandler(service, f.auth, AgentHTTPOptions{}, approval)
}

func TestAgentApplyHTTPPermissionAndExactResponse(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate})
	snapshot := applyHTTPSnapshot(agentconversation.SourceNew)
	fixture.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: snapshot.SessionID, TenantID: snapshot.TenantID, OperatorID: snapshot.OperatorID, RoomID: snapshot.RoomID, Status: agentconversation.StatusReadyForReview, Version: 3, ActiveSnapshotVersion: 1}})
	repository := &applyHTTPRepository{execution: agentapproval.Execution{ID: "exec-1", TenantID: "merchant-a", OperatorID: "operator-a", SessionID: "session-apply", SnapshotVersion: 1, IdempotencyKey: "apply-key", Status: agentapproval.ExecutionApplied, SelectedCandidateIDs: []string{"candidate-1"}, OrderedCandidateIDs: []string{"candidate-1"}, CreatedLotIDs: []string{"lot-1"}, QueuePositions: []int32{7}}}
	repository.execution.RequestFingerprint = agentapproval.RequestFingerprint(applyHTTPCommand(snapshot))
	approval := agentapproval.NewService(applyHTTPSnapshotReader{snapshot: snapshot}, repository)
	fixture.rebuildHandlerWithApproval(t, approval)

	request := httptest.NewRequest(http.MethodPost, agentSessionsPath+"/session-apply/apply", strings.NewReader(applyHTTPBody(snapshot)))
	request = request.WithContext(requestctx.WithRequestContext(request.Context(), requestctx.RequestContext{RequestID: "req-1", TraceID: "0123456789abcdef0123456789abcdef"}))
	request.Header.Set("Authorization", "Bearer "+fixture.token)
	request.Header.Set("Idempotency-Key", "apply-key")
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	if sessionResultCode(t, response) != 0 {
		t.Fatalf("apply=%s", response.Body.String())
	}
	var envelope struct {
		ExecutionID   string   `json:"executionId"`
		Target        string   `json:"target"`
		CreatedLotIDs []string `json:"createdLotIds"`
		QueuedLots    []struct {
			LotID         string `json:"lotId"`
			QueuePosition int32  `json:"queuePosition"`
		} `json:"queuedLots"`
	}
	decodeSessionEnvelope(t, response, &envelope)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 5 || raw["executionId"] == nil || raw["target"] == nil || raw["createdLotIds"] == nil || raw["queuedLots"] == nil || raw["result"] == nil {
		t.Fatalf("raw keys=%v", raw)
	}
	var rawQueued []map[string]json.RawMessage
	if err := json.Unmarshal(raw["queuedLots"], &rawQueued); err != nil {
		t.Fatal(err)
	}
	if len(rawQueued) != 1 || len(rawQueued[0]) != 2 || rawQueued[0]["lotId"] == nil || rawQueued[0]["queuePosition"] == nil {
		t.Fatalf("queued raw=%v", rawQueued)
	}
	if envelope.ExecutionID != "exec-1" || envelope.Target != string(agentapproval.ApplyTargetQueue) || len(envelope.CreatedLotIDs) != 1 || envelope.CreatedLotIDs[0] != "lot-1" || len(envelope.QueuedLots) != 1 || envelope.QueuedLots[0].LotID != "lot-1" || envelope.QueuedLots[0].QueuePosition != 7 {
		t.Fatalf("response=%s", response.Body.String())
	}
	if repository.calls != 1 || repository.last.TenantID != "merchant-a" || repository.last.OperatorID != "operator-a" || repository.last.SessionID != "session-apply" || repository.last.TraceID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("trusted command=%+v calls=%d", repository.last, repository.calls)
	}
}

func TestAgentApplyHTTPConditionalCreatePermissionAndStrictBoundary(t *testing.T) {
	for _, tc := range []struct {
		name        string
		source      agentconversation.SourceType
		permissions []string
		want        int
	}{
		{"existing only needs queue", agentconversation.SourceExisting, []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue}, 0},
		{"new needs create", agentconversation.SourceNew, []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue}, 403001},
		{"queue always required", agentconversation.SourceExisting, []string{userbiz.PermissionLotViewAdmin}, 403001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newAgentSessionHandlerFixture(t, "merchant-a", tc.permissions)
			snapshot := applyHTTPSnapshot(tc.source)
			fixture.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: snapshot.SessionID, TenantID: snapshot.TenantID, OperatorID: snapshot.OperatorID, RoomID: snapshot.RoomID, Status: agentconversation.StatusReadyForReview, Version: 3, ActiveSnapshotVersion: 1}})
			repo := &applyHTTPRepository{}
			command := applyHTTPCommand(snapshot)
			created := []string{"lot-1"}
			if tc.source == agentconversation.SourceExisting {
				created = []string{}
			}
			repo.execution = agentapproval.Execution{ID: "exec", TenantID: command.TenantID, OperatorID: command.OperatorID, SessionID: command.SessionID, SnapshotVersion: 1, IdempotencyKey: "apply-key", RequestFingerprint: agentapproval.RequestFingerprint(command), Status: agentapproval.ExecutionApplied, SelectedCandidateIDs: command.SelectedCandidateIDs, OrderedCandidateIDs: command.OrderedCandidateIDs, CreatedLotIDs: created, QueuePositions: []int32{1}}
			fixture.rebuildHandlerWithApproval(t, agentapproval.NewService(applyHTTPSnapshotReader{snapshot: snapshot}, repo))
			response := fixture.requestWithHeaders(http.MethodPost, agentSessionsPath+"/session-apply/apply", applyHTTPBody(snapshot), map[string]string{"Idempotency-Key": "apply-key"})
			if got := sessionResultCode(t, response); got != tc.want {
				t.Fatalf("got=%d body=%s", got, response.Body.String())
			}
			if tc.want != 0 && repo.calls != 0 {
				t.Fatalf("denied request mutated: calls=%d", repo.calls)
			}
		})
	}

	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate})
	snapshot := applyHTTPSnapshot(agentconversation.SourceNew)
	repo := &applyHTTPRepository{}
	fixture.rebuildHandlerWithApproval(t, agentapproval.NewService(applyHTTPSnapshotReader{snapshot: snapshot}, repo))
	path := agentSessionsPath + "/session-apply/apply"
	for _, tc := range []struct{ body, key string }{
		{applyHTTPBody(snapshot), ""},
		{`{"snapshotVersion":1,"contentHash":"` + snapshot.ContentHash + `","selectedCandidateIds":["candidate-1"],"orderedCandidateIds":["candidate-1"],"tenantId":"evil"}`, "apply-key"},
		{`{"snapshotVersion":1,"contentHash":"bad","selectedCandidateIds":["candidate-1"],"orderedCandidateIds":["candidate-1"]}`, "apply-key"},
	} {
		response := fixture.requestWithHeaders(http.MethodPost, path, tc.body, map[string]string{"Idempotency-Key": tc.key})
		if sessionResultCode(t, response) != 400001 {
			t.Fatalf("strict=%s", response.Body.String())
		}
	}
	wrong := fixture.request(http.MethodGet, path, "")
	if wrong.Code != http.StatusMethodNotAllowed || wrong.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("method=%d allow=%q", wrong.Code, wrong.Header().Get("Allow"))
	}
}

func TestAgentApplyHTTPInventoryOnlyNeedsCreateButNotQueuePermission(t *testing.T) {
	snapshot := applyHTTPSnapshot(agentconversation.SourceNew)
	bodyBytes, _ := json.Marshal(map[string]any{"snapshotVersion": snapshot.SnapshotVersion, "contentHash": snapshot.ContentHash, "selectedCandidateIds": snapshot.SelectedCandidateIDs, "orderedCandidateIds": snapshot.OrderedCandidateIDs, "target": agentapproval.ApplyTargetInventoryOnly})
	body := string(bodyBytes)

	for _, tc := range []struct {
		name        string
		permissions []string
		want        int
	}{
		{"create permission is sufficient", []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotCreate}, 0},
		{"create permission is required", []string{userbiz.PermissionLotViewAdmin}, 403001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newAgentSessionHandlerFixture(t, "merchant-a", tc.permissions)
			fixture.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: snapshot.SessionID, TenantID: snapshot.TenantID, OperatorID: snapshot.OperatorID, RoomID: snapshot.RoomID, Status: agentconversation.StatusReadyForReview, Version: 3, ActiveSnapshotVersion: 1}})
			command := applyHTTPCommand(snapshot)
			command.Target = agentapproval.ApplyTargetInventoryOnly
			repo := &applyHTTPRepository{execution: agentapproval.Execution{ID: "exec-inventory", TenantID: command.TenantID, OperatorID: command.OperatorID, SessionID: command.SessionID, SnapshotVersion: 1, IdempotencyKey: "apply-key", RequestFingerprint: agentapproval.RequestFingerprint(command), Status: agentapproval.ExecutionApplied, SelectedCandidateIDs: command.SelectedCandidateIDs, OrderedCandidateIDs: command.OrderedCandidateIDs, CreatedLotIDs: []string{"lot-1"}, QueuePositions: []int32{}}}
			fixture.rebuildHandlerWithApproval(t, agentapproval.NewService(applyHTTPSnapshotReader{snapshot: snapshot}, repo))
			response := fixture.requestWithHeaders(http.MethodPost, agentSessionsPath+"/session-apply/apply", body, map[string]string{"Idempotency-Key": "apply-key"})
			if got := sessionResultCode(t, response); got != tc.want {
				t.Fatalf("got=%d body=%s", got, response.Body.String())
			}
			if tc.want == 0 && (!containsFold(response.Body.String(), "INVENTORY_ONLY") || !containsFold(response.Body.String(), `"queuedLots":[]`)) {
				t.Fatalf("inventory-only response=%s", response.Body.String())
			}
			if tc.want != 0 && repo.calls != 0 {
				t.Fatalf("denied request mutated: calls=%d", repo.calls)
			}
		})
	}
}

func TestAgentApplyHTTPRedactsRepositoryFailure(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate})
	snapshot := applyHTTPSnapshot(agentconversation.SourceNew)
	fixture.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: snapshot.SessionID, TenantID: snapshot.TenantID, OperatorID: snapshot.OperatorID, RoomID: snapshot.RoomID, Status: agentconversation.StatusReadyForReview, Version: 3, ActiveSnapshotVersion: 1}})
	repo := &applyHTTPRepository{err: errors.New("SQL prompt provider sk-secret")}
	fixture.rebuildHandlerWithApproval(t, agentapproval.NewService(applyHTTPSnapshotReader{snapshot: snapshot}, repo))
	response := fixture.requestWithHeaders(http.MethodPost, agentSessionsPath+"/session-apply/apply", applyHTTPBody(snapshot), map[string]string{"Idempotency-Key": "apply-key"})
	for _, secret := range []string{"sql", "prompt", "provider", "sk-secret"} {
		if containsFold(response.Body.String(), secret) {
			t.Fatalf("leak %q: %s", secret, response.Body.String())
		}
	}
}

func TestAgentApplyHTTPStableConflictsAndReplay(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"stale snapshot", apperr.ErrAgentReviewSnapshotStale, 409201},
		{"candidate changed", apperr.ErrAgentCandidateChanged, 409202},
		{"invalid asset", apperr.ErrAgentImageAssetInvalid, 400201},
		{"batch rollback", apperr.ErrAgentBatchApplyFailed, 500201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate})
			snapshot := applyHTTPSnapshot(agentconversation.SourceNew)
			repo := &applyHTTPRepository{err: tc.err}
			fixture.rebuildHandlerWithApproval(t, agentapproval.NewService(applyHTTPSnapshotReader{snapshot: snapshot}, repo))
			response := fixture.requestWithHeaders(http.MethodPost, agentSessionsPath+"/session-apply/apply", applyHTTPBody(snapshot), map[string]string{"Idempotency-Key": "apply-key"})
			if got := sessionResultCode(t, response); got != tc.code {
				t.Fatalf("got=%d body=%s", got, response.Body.String())
			}
		})
	}

	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate})
	snapshot := applyHTTPSnapshot(agentconversation.SourceNew)
	command := applyHTTPCommand(snapshot)
	execution := agentapproval.Execution{ID: "exec-replay", TenantID: command.TenantID, OperatorID: command.OperatorID, SessionID: command.SessionID, SnapshotVersion: 1, IdempotencyKey: "apply-key", RequestFingerprint: agentapproval.RequestFingerprint(command), Status: agentapproval.ExecutionApplied, SelectedCandidateIDs: command.SelectedCandidateIDs, OrderedCandidateIDs: command.OrderedCandidateIDs, CreatedLotIDs: []string{"lot-replay"}, QueuePositions: []int32{9}}
	repo := &applyHTTPRepository{existing: &execution}
	fixture.rebuildHandlerWithApproval(t, agentapproval.NewService(applyHTTPSnapshotReader{snapshot: snapshot}, repo))
	response := fixture.requestWithHeaders(http.MethodPost, agentSessionsPath+"/session-apply/apply", applyHTTPBody(snapshot), map[string]string{"Idempotency-Key": "apply-key"})
	if sessionResultCode(t, response) != 0 || repo.calls != 0 || !containsFold(response.Body.String(), "exec-replay") {
		t.Fatalf("replay=%s calls=%d", response.Body.String(), repo.calls)
	}
}

func TestAgentApplyHTTPConcealsCrossScope(t *testing.T) {
	owner := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate})
	snapshot := applyHTTPSnapshot(agentconversation.SourceNew)
	repo := &applyHTTPRepository{}
	approval := agentapproval.NewService(applyHTTPSnapshotReader{snapshot: snapshot}, repo)
	owner.rebuildHandlerWithApproval(t, approval)
	for name, user := range map[string]*auctionv1.User{
		"cross tenant":   {Id: "operator-b", MainAccountId: "merchant-b", RoleCodes: []string{userbiz.RoleOperator}, PermissionCodes: []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate}, Status: auctionv1.UserStatus_USER_STATUS_ACTIVE},
		"cross operator": {Id: "operator-b", MainAccountId: "merchant-a", RoleCodes: []string{userbiz.RoleOperator}, PermissionCodes: []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate}, Status: auctionv1.UserStatus_USER_STATUS_ACTIVE},
	} {
		t.Run(name, func(t *testing.T) {
			pair, err := owner.auth.IssueTokenPair(user)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, agentSessionsPath+"/session-apply/apply", strings.NewReader(applyHTTPBody(snapshot)))
			r.Header.Set("Authorization", "Bearer "+pair.AccessToken)
			r.Header.Set("Idempotency-Key", "apply-key")
			w := httptest.NewRecorder()
			owner.handler.ServeHTTP(w, r)
			if sessionResultCode(t, w) != 409201 || repo.calls != 0 {
				t.Fatalf("scope=%s calls=%d", w.Body.String(), repo.calls)
			}
			for _, secret := range []string{"merchant-a", "operator-a", "room-a", "candidate-1"} {
				if containsFold(w.Body.String(), secret) {
					t.Fatalf("leaked %s: %s", secret, w.Body.String())
				}
			}
		})
	}
}

func TestAgentApplyHTTPMetricAdmissionAndSingleFinalOutcome(t *testing.T) {
	for _, tc := range []struct {
		name             string
		permissions      []string
		source           agentconversation.SourceType
		readerErr        error
		applyErr         error
		invalidateErr    error
		body             string
		wantOutcome      string
		wantInvalidation int
		replay           bool
	}{
		{name: "requirements stale", permissions: []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate}, source: agentconversation.SourceNew, readerErr: apperr.ErrAgentReviewSnapshotStale, wantOutcome: "snapshot_conflict", wantInvalidation: 1},
		{name: "requirements internal", permissions: []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate}, source: agentconversation.SourceNew, readerErr: errors.New("database unavailable"), wantOutcome: "rollback"},
		{name: "reconciliation exhaustion", permissions: []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate}, source: agentconversation.SourceNew, readerErr: apperr.ErrAgentReviewSnapshotStale, invalidateErr: errors.New("transient"), wantOutcome: "rollback", wantInvalidation: 3},
		{name: "malformed", permissions: []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate}, source: agentconversation.SourceNew, body: `{"snapshotVersion":1,"contentHash":"bad","selectedCandidateIds":["candidate-1"],"orderedCandidateIds":["candidate-1"]}`},
		{name: "admitted envelope mismatch", permissions: []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate}, source: agentconversation.SourceNew, body: `{"snapshotVersion":1,"contentHash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","selectedCandidateIds":["candidate-1"],"orderedCandidateIds":["candidate-1"]}`, wantOutcome: "snapshot_conflict"},
		{name: "base queue denied", permissions: []string{userbiz.PermissionLotViewAdmin}, source: agentconversation.SourceExisting},
		{name: "conditional create denied", permissions: []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue}, source: agentconversation.SourceNew},
		{name: "fresh success", permissions: []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate}, source: agentconversation.SourceNew, wantOutcome: "success"},
		{name: "pre-read replay", permissions: []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate}, source: agentconversation.SourceNew, wantOutcome: "idempotent_hit", replay: true},
		{name: "apply conflict", permissions: []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionLotQueue, userbiz.PermissionLotCreate}, source: agentconversation.SourceNew, applyErr: apperr.ErrAgentCandidateChanged, wantOutcome: "snapshot_conflict", wantInvalidation: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newAgentSessionHandlerFixture(t, "merchant-a", tc.permissions)
			snapshot := applyHTTPSnapshot(tc.source)
			fixture.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: snapshot.SessionID, TenantID: snapshot.TenantID, OperatorID: snapshot.OperatorID, RoomID: snapshot.RoomID, Status: agentconversation.StatusReadyForReview, Version: 3, ActiveSnapshotVersion: 1}})
			command := applyHTTPCommand(snapshot)
			execution := agentapproval.Execution{ID: "exec-metric", TenantID: command.TenantID, OperatorID: command.OperatorID, SessionID: command.SessionID, SnapshotVersion: command.SnapshotVersion, IdempotencyKey: command.IdempotencyKey, RequestFingerprint: agentapproval.RequestFingerprint(command), Status: agentapproval.ExecutionApplied, SelectedCandidateIDs: command.SelectedCandidateIDs, OrderedCandidateIDs: command.OrderedCandidateIDs, CreatedLotIDs: []string{"lot-1"}, QueuePositions: []int32{1}}
			repo := &applyHTTPRepository{execution: execution, err: tc.applyErr, invalidateErr: tc.invalidateErr}
			if tc.replay {
				repo.existing = &execution
			}
			observer := &applyHTTPObserver{}
			approval := agentapproval.NewService(applyHTTPSnapshotReader{snapshot: snapshot, err: tc.readerErr}, repo).WithObserver(observer)
			fixture.rebuildHandlerWithApproval(t, approval)
			body := tc.body
			if body == "" {
				body = applyHTTPBody(snapshot)
			}
			_ = fixture.requestWithHeaders(http.MethodPost, agentSessionsPath+"/session-apply/apply", body, map[string]string{"Idempotency-Key": "apply-key"})
			if tc.wantOutcome == "" {
				if len(observer.outcomes) != 0 {
					t.Fatalf("excluded request outcomes=%v", observer.outcomes)
				}
			} else if len(observer.outcomes) != 1 || observer.outcomes[0] != tc.wantOutcome {
				t.Fatalf("outcomes=%v want=%s", observer.outcomes, tc.wantOutcome)
			}
			if repo.invalidations != tc.wantInvalidation {
				t.Fatalf("invalidations=%d want=%d", repo.invalidations, tc.wantInvalidation)
			}
		})
	}
}

type applyHTTPSnapshotReader struct {
	snapshot agentconversation.ReviewSnapshot
	err      error
}

type applyHTTPObserver struct {
	outcomes []string
}

func (o *applyHTTPObserver) BeginApplyTransaction(ctx context.Context) (context.Context, func(string)) {
	return ctx, func(string) {}
}
func (o *applyHTTPObserver) RecordApplyOutcome(outcome string) {
	o.outcomes = append(o.outcomes, outcome)
}

func (r applyHTTPSnapshotReader) FindReviewSnapshot(context.Context, string, string, string, int64) (agentconversation.ReviewSnapshot, error) {
	return r.snapshot, r.err
}

type applyHTTPRepository struct {
	execution     agentapproval.Execution
	err           error
	calls         int
	last          agentapproval.ApplyCommand
	existing      *agentapproval.Execution
	invalidateErr error
	invalidations int
}

func (r *applyHTTPRepository) InvalidateReviewSnapshot(context.Context, agentapproval.ApplyCommand, error) error {
	r.invalidations++
	return r.invalidateErr
}

func (r *applyHTTPRepository) Apply(_ context.Context, c agentapproval.ApplyCommand) (agentapproval.Execution, error) {
	r.calls++
	r.last = c
	return r.execution, r.err
}
func (r *applyHTTPRepository) FindExecutionByKey(context.Context, string, int64, string) (agentapproval.Execution, error) {
	if r.existing != nil {
		return *r.existing, nil
	}
	return agentapproval.Execution{}, apperr.ErrNotFound
}

func applyHTTPSnapshot(source agentconversation.SourceType) agentconversation.ReviewSnapshot {
	c := agentconversation.SnapshotCandidate{CandidateID: "candidate-1", CandidateVersion: 1, SourceType: source}
	if source == agentconversation.SourceExisting {
		version := int64(2)
		c.ExistingLotID = "lot-1"
		c.ExistingLotReadVersion = &version
	}
	return agentconversation.ReviewSnapshot{SessionID: "session-apply", TenantID: "merchant-a", OperatorID: "operator-a", RoomID: "room-a", SnapshotVersion: 1, ContentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SelectedCandidateIDs: []string{"candidate-1"}, OrderedCandidateIDs: []string{"candidate-1"}, Candidates: []agentconversation.SnapshotCandidate{c}, CreatedBy: "operator-a"}
}
func applyHTTPCommand(s agentconversation.ReviewSnapshot) agentapproval.ApplyCommand {
	return agentapproval.ApplyCommand{TenantID: s.TenantID, OperatorID: s.OperatorID, SessionID: s.SessionID, SnapshotVersion: s.SnapshotVersion, ContentHash: s.ContentHash, SelectedCandidateIDs: s.SelectedCandidateIDs, OrderedCandidateIDs: s.OrderedCandidateIDs, IdempotencyKey: "apply-key"}
}
func applyHTTPBody(s agentconversation.ReviewSnapshot) string {
	b, _ := json.Marshal(map[string]any{"snapshotVersion": s.SnapshotVersion, "contentHash": s.ContentHash, "selectedCandidateIds": s.SelectedCandidateIDs, "orderedCandidateIds": s.OrderedCandidateIDs})
	return string(b)
}
func containsFold(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if equalFoldASCII(value[i:i+len(part)], part) {
			return true
		}
	}
	return false
}
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x >= 'A' && x <= 'Z' {
			x += 32
		}
		if y >= 'A' && y <= 'Z' {
			y += 32
		}
		if x != y {
			return false
		}
	}
	return true
}
