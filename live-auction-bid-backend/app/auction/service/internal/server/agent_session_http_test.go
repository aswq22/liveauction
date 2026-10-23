package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	httptransport "github.com/go-kratos/kratos/v2/transport/http"
	auctionv1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
	"live-auction-bid/backend/app/auction/service/internal/agentconversation"
	"live-auction-bid/backend/app/auction/service/internal/biz/auction"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
	"live-auction-bid/backend/app/auction/service/internal/pkg/auth"
	appsvc "live-auction-bid/backend/app/auction/service/internal/service"
)

func TestAgentSessionHTTPRegistersOnlySessionPrefix(t *testing.T) {
	transport := httptransport.NewServer()
	registerAgentSessionHTTP(transport, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { _, _ = io.WriteString(w, request.URL.Path) }))
	response := httptest.NewRecorder()
	transport.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/admin/agent/sessions/session-1", nil))
	if response.Body.String() != "/api/admin/agent/sessions/session-1" {
		t.Fatalf("session route = %q", response.Body.String())
	}
	nonSession := httptest.NewRecorder()
	transport.ServeHTTP(nonSession, httptest.NewRequest(http.MethodGet, "/api/admin/agent/runs/run-1", nil))
	if nonSession.Body.String() == "/api/admin/agent/runs/run-1" {
		t.Fatal("session handler captured run route")
	}
}

func TestAgentSessionHTTPAuthAndMethods(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	noBearer := httptest.NewRecorder()
	fixture.handler.ServeHTTP(noBearer, httptest.NewRequest(http.MethodPost, agentSessionsPath, strings.NewReader(`{"roomId":"room-a"}`)))
	if sessionResultCode(t, noBearer) != 401001 {
		t.Fatalf("no bearer=%s", noBearer.Body.String())
	}
	denied := newAgentSessionHandlerFixture(t, "merchant-a", nil)
	response := denied.request(http.MethodPost, agentSessionsPath, `{"roomId":"room-a"}`)
	if sessionResultCode(t, response) != 403001 {
		t.Fatalf("permission=%s", response.Body.String())
	}
	wrong := fixture.request(http.MethodGet, agentSessionsPath, "")
	if wrong.Code != http.StatusMethodNotAllowed || wrong.Header().Get("Allow") != "POST" {
		t.Fatalf("method=%d allow=%q", wrong.Code, wrong.Header().Get("Allow"))
	}
	known := fixture.request(http.MethodGet, agentSessionsPath+"/missing/messages", `{}`)
	if known.Code != http.StatusMethodNotAllowed || known.Header().Get("Allow") != "POST" {
		t.Fatalf("known method=%d allow=%q", known.Code, known.Header().Get("Allow"))
	}
}

func TestAgentSessionHTTPCreateTenantAndStrictBody(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	for _, body := range []string{`{"roomId":"room-a","tenantId":"merchant-b"}`, `{"roomId":"room-a"} {}`, `{"roomId":"` + strings.Repeat("x", 70*1024) + `"}`} {
		response := fixture.request(http.MethodPost, agentSessionsPath, body)
		if sessionResultCode(t, response) != 400001 {
			t.Fatalf("body accepted: %s", response.Body.String())
		}
	}
	response := fixture.request(http.MethodPost, agentSessionsPath, `{"roomId":"room-a"}`)
	if sessionResultCode(t, response) != 0 {
		t.Fatalf("create=%s", response.Body.String())
	}
	if fixture.repo.lastTenant != "merchant-a" {
		t.Fatalf("tenant=%q", fixture.repo.lastTenant)
	}
	fixture.room.main = "merchant-b"
	denial := fixture.request(http.MethodPost, agentSessionsPath, `{"roomId":"room-a"}`)
	if sessionResultCode(t, denial) != 403001 {
		t.Fatalf("room denial=%s", denial.Body.String())
	}
}

func TestAgentSessionHTTPMessageIdempotencyAndConflict(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	created := fixture.request(http.MethodPost, agentSessionsPath, `{"roomId":"room-a"}`)
	var createEnvelope struct {
		Result struct {
			Code int `json:"code"`
		} `json:"result"`
		Session sessionHTTPResponse `json:"session"`
	}
	decodeSessionEnvelope(t, created, &createEnvelope)
	if createEnvelope.Result.Code != 0 || createEnvelope.Session.ID == "" {
		t.Fatalf("create=%s", created.Body.String())
	}

	path := agentSessionsPath + "/" + createEnvelope.Session.ID + "/messages"
	first := fixture.requestWithHeaders(http.MethodPost, path, `{"expectedVersion":1,"content":"  make a plan  ","runType":"SESSION_PLAN"}`, map[string]string{"Idempotency-Key": "turn-1"})
	var firstEnvelope struct {
		Result struct {
			Code int `json:"code"`
		} `json:"result"`
		Run agentRunResponse `json:"run"`
	}
	decodeSessionEnvelope(t, first, &firstEnvelope)
	if first.Code != http.StatusOK || firstEnvelope.Result.Code != 0 || firstEnvelope.Run.RunID == "" {
		t.Fatalf("first submit status=%d body=%s", first.Code, first.Body.String())
	}

	replay := fixture.requestWithHeaders(http.MethodPost, path, `{"expectedVersion":1,"content":"make a plan","runType":"SESSION_PLAN"}`, map[string]string{"Idempotency-Key": "turn-1"})
	var replayEnvelope struct {
		Result struct {
			Code int `json:"code"`
		} `json:"result"`
		Run agentRunResponse `json:"run"`
	}
	decodeSessionEnvelope(t, replay, &replayEnvelope)
	if replay.Code != http.StatusOK || replayEnvelope.Result.Code != 0 || replayEnvelope.Run != firstEnvelope.Run {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	if fixture.runtime.startCount() != 1 {
		t.Fatalf("runtime starts=%d, want 1", fixture.runtime.startCount())
	}

	conflict := fixture.requestWithHeaders(http.MethodPost, path, `{"expectedVersion":1,"content":"different plan","runType":"SESSION_PLAN"}`, map[string]string{"Idempotency-Key": "turn-1"})
	if conflict.Code != http.StatusOK || sessionResultCode(t, conflict) != int(appsvc.ResultCodeConflict) {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
}

func TestAgentSessionHTTPCancelCASAndRejectsFurtherMessages(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	session := createAgentHTTPSession(t, fixture)
	staleMessage := fixture.requestWithHeaders(http.MethodPost, agentSessionsPath+"/"+session.ID+"/messages", `{"expectedVersion":2,"content":"stale","runType":"SESSION_PLAN"}`, map[string]string{"Idempotency-Key": "stale-message"})
	if staleMessage.Code != http.StatusOK || sessionResultCode(t, staleMessage) != int(appsvc.ResultCodeConflict) {
		t.Fatalf("stale message status=%d body=%s", staleMessage.Code, staleMessage.Body.String())
	}
	path := agentSessionsPath + "/" + session.ID + "/cancel"

	stale := fixture.request(http.MethodPost, path, `{"expectedVersion":2}`)
	if stale.Code != http.StatusOK || sessionResultCode(t, stale) != int(appsvc.ResultCodeConflict) {
		t.Fatalf("stale cancel status=%d body=%s", stale.Code, stale.Body.String())
	}
	success := fixture.request(http.MethodPost, path, `{"expectedVersion":1}`)
	if success.Code != http.StatusOK || sessionResultCode(t, success) != 0 {
		t.Fatalf("cancel status=%d body=%s", success.Code, success.Body.String())
	}

	recovered := fixture.request(http.MethodGet, agentSessionsPath+"/"+session.ID, "")
	var recoveryEnvelope struct {
		Result struct {
			Code int `json:"code"`
		} `json:"result"`
		Session sessionHTTPResponse `json:"session"`
	}
	decodeSessionEnvelope(t, recovered, &recoveryEnvelope)
	if recoveryEnvelope.Result.Code != 0 || recoveryEnvelope.Session.Status != string(agentconversation.StatusCancelled) || recoveryEnvelope.Session.Version != 2 {
		t.Fatalf("cancel recovery=%s", recovered.Body.String())
	}

	submit := fixture.requestWithHeaders(http.MethodPost, agentSessionsPath+"/"+session.ID+"/messages", `{"expectedVersion":2,"content":"continue","runType":"SESSION_PLAN"}`, map[string]string{"Idempotency-Key": "after-cancel"})
	if submit.Code != http.StatusOK || sessionResultCode(t, submit) != int(appsvc.ResultCodeInvalidArgument) {
		t.Fatalf("post-cancel submit status=%d body=%s", submit.Code, submit.Body.String())
	}
	if fixture.runtime.startCount() != 0 {
		t.Fatalf("post-cancel runtime starts=%d", fixture.runtime.startCount())
	}
}

func TestAgentSessionHTTPRecoveryMaterializesTerminalAggregate(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	stored := agentcontrol.Run{ID: "run-recovery", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentcontrol.StatusCreated, Version: 1, SessionID: "session-recovery", SessionVersion: 2, CreatedAtUnixMs: 10}
	fixture.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: "session-recovery", TenantID: "merchant-a", OperatorID: "operator-a", RoomID: "room-a", Status: agentconversation.StatusCollectingInput, Version: 2}, Runs: []agentcontrol.Run{stored}})
	terminal := stored
	terminal.Status = agentcontrol.StatusCompleted
	terminal.TurnResultJSON = `{"assistant_message":"Draft is ready","candidate_patches":[{"candidate_id":"candidate-1","source_type":"NEW","fields":{"title":"Moon vase","startPriceAmount":0,"durationSeconds":120,"mainImageUrl":"https://example.test/a.jpg","minIncrementAmount":100,"antiSnipeWindowSeconds":30,"antiSnipeExtendSeconds":10,"maxExtendCount":2,"depositAmount":0},"field_sources":{"title":"AI","startPriceAmount":"AI","durationSeconds":"AI","mainImageUrl":"AI","minIncrementAmount":"AI","antiSnipeWindowSeconds":"AI","antiSnipeExtendSeconds":"AI","maxExtendCount":"AI","depositAmount":"AI"}}],"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}`
	fixture.runtime.setRun(terminal)

	response := fixture.request(http.MethodGet, agentSessionsPath+"/session-recovery", "")
	var envelope struct {
		Result struct {
			Code int `json:"code"`
		} `json:"result"`
		Session    sessionHTTPResponse                `json:"session"`
		Messages   []agentconversation.Message        `json:"messages"`
		Candidates []agentconversation.DraftCandidate `json:"candidates"`
		ActiveRun  *agentRunResponse                  `json:"activeRun"`
	}
	decodeSessionEnvelope(t, response, &envelope)
	if response.Code != http.StatusOK || envelope.Result.Code != 0 {
		t.Fatalf("recovery status=%d body=%s", response.Code, response.Body.String())
	}
	if envelope.Session.Status != string(agentconversation.StatusReadyForReview) || envelope.Session.Version != 3 || len(envelope.Messages) != 1 || envelope.Messages[0].Role != agentconversation.MessageRoleAssistant || envelope.Messages[0].Content != "Draft is ready" {
		t.Fatalf("materialized session/messages=%s", response.Body.String())
	}
	if len(envelope.Candidates) != 1 || envelope.Candidates[0].Candidate.Draft.Title != "Moon vase" || envelope.ActiveRun != nil {
		t.Fatalf("materialized candidates/active run=%s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"status":"CREATED"`) {
		t.Fatalf("recovery exposed stale run: %s", response.Body.String())
	}
}

func TestAgentSessionHTTPPatchCandidateLifecycle(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	candidate := agentconversation.DraftCandidate{
		Candidate: agentconversation.Candidate{ID: "candidate-1", SourceType: agentconversation.SourceNew, StartPriceSpecified: true, Draft: agentconversation.CandidateDraft{
			Title: "Draft", StartPriceAmount: 500, DurationSeconds: 120, MainImageURL: "https://example.test/a.jpg",
			MinIncrementAmount: 100, AntiSnipeWindowSeconds: 30, AntiSnipeExtendSeconds: 10, MaxExtendCount: 2,
		}},
		Position: 1, SelectedByDefault: true, FieldSourcesJSON: `{"title":"AI","startPriceAmount":"AI"}`, ValidationJSON: `{}`, Version: 4,
	}
	fixture.repo.put(agentconversation.SessionAggregate{
		Session:    agentconversation.Session{ID: "session-patch", TenantID: "merchant-a", OperatorID: "operator-a", RoomID: "room-a", Status: agentconversation.StatusReadyForReview, Version: 7, ActiveSnapshotVersion: 3},
		Candidates: []agentconversation.DraftCandidate{candidate},
	})
	path := agentSessionsPath + "/session-patch/candidates/candidate-1"
	response := fixture.requestWithHeaders(http.MethodPatch, path, `{"expectedSessionVersion":7,"fields":{"title":"Ocean Heart","startPriceAmount":10000}}`, map[string]string{"If-Match": "4"})
	if response.Code != http.StatusOK || sessionResultCode(t, response) != 0 {
		t.Fatalf("patch status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Session    sessionHTTPResponse                `json:"session"`
		Candidates []agentconversation.DraftCandidate `json:"candidates"`
	}
	decodeSessionEnvelope(t, response, &envelope)
	if envelope.Session.Version != 8 || envelope.Session.ActiveSnapshotVersion != 0 || len(envelope.Candidates) != 1 || envelope.Candidates[0].Version != 5 || envelope.Candidates[0].Candidate.Draft.StartPriceAmount != 10000 {
		t.Fatalf("patch response=%s", response.Body.String())
	}
	var sources map[string]string
	if err := json.Unmarshal([]byte(envelope.Candidates[0].FieldSourcesJSON), &sources); err != nil || sources["title"] != "USER" || sources["startPriceAmount"] != "USER" {
		t.Fatalf("sources=%v err=%v", sources, err)
	}

	staleCandidate := fixture.requestWithHeaders(http.MethodPatch, path, `{"expectedSessionVersion":8,"fields":{"title":"stale"}}`, map[string]string{"If-Match": "4"})
	if sessionResultCode(t, staleCandidate) != int(appsvc.ResultCodeConflict) {
		t.Fatalf("stale candidate=%s", staleCandidate.Body.String())
	}
	staleSession := fixture.requestWithHeaders(http.MethodPatch, path, `{"expectedSessionVersion":7,"fields":{"title":"stale"}}`, map[string]string{"If-Match": "5"})
	if sessionResultCode(t, staleSession) != int(appsvc.ResultCodeConflict) {
		t.Fatalf("stale session=%s", staleSession.Body.String())
	}
}

func TestAgentSessionHTTPPatchCandidateStrictContract(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	fixture.repo.put(agentconversation.SessionAggregate{
		Session:    agentconversation.Session{ID: "session-x", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentconversation.StatusCollectingInput, Version: 1},
		Candidates: []agentconversation.DraftCandidate{{Candidate: agentconversation.Candidate{ID: "candidate-x", SourceType: agentconversation.SourceNew}, Position: 1, Version: 1}},
	})
	path := agentSessionsPath + "/session-x/candidates/candidate-x"
	for name, testCase := range map[string][2]string{
		"missing if match":   {`{"expectedSessionVersion":1,"fields":{"title":"x"}}`, ""},
		"malformed if match": {`{"expectedSessionVersion":1,"fields":{"title":"x"}}`, `W/"1"`},
		"empty fields":       {`{"expectedSessionVersion":1,"fields":{}}`, "1"},
		"unknown body":       {`{"expectedSessionVersion":1,"fields":{"title":"x"},"tenantId":"other"}`, "1"},
		"unknown field":      {`{"expectedSessionVersion":1,"fields":{"sourceType":"EXISTING"}}`, "1"},
	} {
		t.Run(name, func(t *testing.T) {
			response := fixture.requestWithHeaders(http.MethodPatch, path, testCase[0], map[string]string{"If-Match": testCase[1]})
			if sessionResultCode(t, response) != int(appsvc.ResultCodeInvalidArgument) {
				t.Fatalf("response=%s", response.Body.String())
			}
		})
	}
	wrongMethod := fixture.request(http.MethodPost, path, `{}`)
	if wrongMethod.Code != http.StatusMethodNotAllowed || wrongMethod.Header().Get("Allow") != http.MethodPatch {
		t.Fatalf("method=%d allow=%q", wrongMethod.Code, wrongMethod.Header().Get("Allow"))
	}
}

func TestAgentSessionHTTPPatchCandidateAuthorizationIsolation(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	secretTitle := "private draft title"
	fixture.repo.put(agentconversation.SessionAggregate{
		Session:    agentconversation.Session{ID: "session-private", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentconversation.StatusCollectingInput, Version: 1},
		Candidates: []agentconversation.DraftCandidate{{Candidate: agentconversation.Candidate{ID: "candidate-private", SourceType: agentconversation.SourceNew, Draft: agentconversation.CandidateDraft{Title: secretTitle}}, Position: 1, Version: 1}},
	})
	path := agentSessionsPath + "/session-private/candidates/candidate-private"
	body := `{"expectedSessionVersion":1,"fields":{"title":"stolen"}}`

	unauthorizedRequest := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	unauthorizedRequest.Header.Set("If-Match", "1")
	unauthorized := httptest.NewRecorder()
	fixture.handler.ServeHTTP(unauthorized, unauthorizedRequest)
	if sessionResultCode(t, unauthorized) != int(appsvc.ResultCodeUnauthenticated) || strings.Contains(unauthorized.Body.String(), secretTitle) {
		t.Fatalf("unauthorized leaked data: %s", unauthorized.Body.String())
	}

	denied := newAgentSessionHandlerFixture(t, "merchant-a", nil)
	denied.repo = fixture.repo
	denied.runtime = newSessionHTTPRuntime(denied.repo)
	denied.rebuildHandler(t, 0)
	noPermission := denied.requestWithHeaders(http.MethodPatch, path, body, map[string]string{"If-Match": "1"})
	if sessionResultCode(t, noPermission) != int(appsvc.ResultCodePermissionDenied) || strings.Contains(noPermission.Body.String(), secretTitle) {
		t.Fatalf("permission denial leaked data: %s", noPermission.Body.String())
	}

	for name, user := range map[string]*auctionv1.User{
		"cross tenant":   {Id: "operator-b", MainAccountId: "merchant-b", RoleCodes: []string{userbiz.RoleOperator}, PermissionCodes: []string{userbiz.PermissionLotViewAdmin}, Status: auctionv1.UserStatus_USER_STATUS_ACTIVE},
		"cross operator": {Id: "operator-b", MainAccountId: "merchant-a", RoleCodes: []string{userbiz.RoleOperator}, PermissionCodes: []string{userbiz.PermissionLotViewAdmin}, Status: auctionv1.UserStatus_USER_STATUS_ACTIVE},
	} {
		t.Run(name, func(t *testing.T) {
			pair, err := fixture.auth.IssueTokenPair(user)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+pair.AccessToken)
			request.Header.Set("If-Match", "1")
			response := httptest.NewRecorder()
			fixture.handler.ServeHTTP(response, request)
			code := sessionResultCode(t, response)
			if code != int(appsvc.ResultCodeUserNotFound) && code != int(appsvc.ResultCodePermissionDenied) {
				t.Fatalf("isolation code=%d body=%s", code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), secretTitle) || strings.Contains(response.Body.String(), "candidate-private") {
				t.Fatalf("isolation leaked data: %s", response.Body.String())
			}
		})
	}

	stored, err := fixture.repo.FindSession(context.Background(), "merchant-a", "session-private")
	if err != nil || stored.Session.Version != 1 || stored.Candidates[0].Candidate.Draft.Title != secretTitle {
		t.Fatalf("denied patch mutated aggregate=%+v err=%v", stored, err)
	}
}

func TestAgentSessionHTTPUnknownAndCrossTenantSessionsDoNotLeak(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	fixture.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: "session-other", TenantID: "merchant-b", OperatorID: "operator-b", RoomID: "secret-room", Status: agentconversation.StatusCollectingInput, Version: 1}, Runs: []agentcontrol.Run{{ID: "secret-run", TenantID: "merchant-b", OperatorID: "operator-b", Status: agentcontrol.StatusPlanning}}})

	for _, suffix := range []string{"session-missing", "session-other", "session-missing/events?after_seq=0", "session-other/events?after_seq=0"} {
		response := fixture.request(http.MethodGet, agentSessionsPath+"/"+suffix, "")
		var envelope struct {
			Result struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"result"`
		}
		decodeSessionEnvelope(t, response, &envelope)
		if response.Code != http.StatusOK || envelope.Result.Code != int(appsvc.ResultCodeUserNotFound) || envelope.Result.Message != "not found" {
			t.Fatalf("GET %s status=%d body=%s", suffix, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "merchant-b") || strings.Contains(response.Body.String(), "secret-room") || strings.Contains(response.Body.String(), "secret-run") {
			t.Fatalf("GET %s leaked cross-tenant state: %s", suffix, response.Body.String())
		}
	}
	if fixture.runtime.getCount() != 0 || fixture.runtime.streamCount() != 0 {
		t.Fatalf("runtime called for concealed sessions: get=%d stream=%d", fixture.runtime.getCount(), fixture.runtime.streamCount())
	}
}

func TestAgentSessionHTTPRejectsInvalidAfterSeqBeforeStreaming(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	for _, query := range []string{"after_seq=-1", "after_seq=1&after_seq=2", "after_seq=abc"} {
		response := fixture.request(http.MethodGet, agentSessionsPath+"/session-any/events?"+query, "")
		if response.Code != http.StatusOK || sessionResultCode(t, response) != int(appsvc.ResultCodeInvalidArgument) {
			t.Fatalf("%s status=%d body=%s", query, response.Code, response.Body.String())
		}
	}
	if fixture.runtime.streamCount() != 0 {
		t.Fatalf("invalid after_seq opened %d streams", fixture.runtime.streamCount())
	}
}

func TestAgentSessionHTTPSSETwoRunGlobalReplayAndResume(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	fixture.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: "session-events", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentconversation.StatusCollectingInput, Version: 3}, Runs: []agentcontrol.Run{
		{ID: "run-2", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentcontrol.StatusCompleted, CreatedAtUnixMs: 20},
		{ID: "run-1", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentcontrol.StatusCompleted, CreatedAtUnixMs: 10},
	}})
	fixture.runtime.setStream("run-1", sessionHTTPStreamPlan{events: []agentcontrol.RuntimeEvent{{RunID: "run-1", Seq: 1, Type: "run.created"}, {RunID: "run-1", Seq: 2, Type: "run.completed"}}})
	fixture.runtime.setStream("run-2", sessionHTTPStreamPlan{events: []agentcontrol.RuntimeEvent{{RunID: "run-2", Seq: 1, Type: "run.created"}, {RunID: "run-2", Seq: 2, Type: "run.completed"}}})

	all := fixture.request(http.MethodGet, agentSessionsPath+"/session-events/events?after_seq=0", "")
	if all.Code != http.StatusOK || all.Header().Get("Content-Type") != "text/event-stream" || !equalInt64s(sessionSSEIDs(all.Body.String()), []int64{1, 2, 3, 4}) {
		t.Fatalf("full replay status=%d ids=%v body=%q", all.Code, sessionSSEIDs(all.Body.String()), all.Body.String())
	}
	if strings.Index(all.Body.String(), `"runId":"run-1"`) > strings.Index(all.Body.String(), `"runId":"run-2"`) {
		t.Fatalf("runs were not streamed in creation order: %q", all.Body.String())
	}

	resumed := fixture.request(http.MethodGet, agentSessionsPath+"/session-events/events?after_seq=2", "")
	if resumed.Code != http.StatusOK || !equalInt64s(sessionSSEIDs(resumed.Body.String()), []int64{3, 4}) {
		t.Fatalf("resume ids=%v body=%q", sessionSSEIDs(resumed.Body.String()), resumed.Body.String())
	}
	if strings.Contains(resumed.Body.String(), `"runId":"run-1"`) {
		t.Fatalf("resume replayed previous-run frames: %q", resumed.Body.String())
	}
}

func TestAgentSessionHTTPSSEFailureUsesSafeClosedVocabulary(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	fixture.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: "session-error", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentconversation.StatusCollectingInput, Version: 1}, Runs: []agentcontrol.Run{{ID: "run-error", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentcontrol.StatusPlanning}}})
	fixture.runtime.setStream("run-error", sessionHTTPStreamPlan{finalErr: errors.New("mysql 10.0.0.8 Bearer provider-secret")})

	response := fixture.request(http.MethodGet, agentSessionsPath+"/session-error/events?after_seq=0", "")
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "event: agent.run.error\n") || !strings.Contains(body, `"code":"AGENT_EVENT_STREAM_UNAVAILABLE"`) || !strings.Contains(body, `"message":"Agent event stream is temporarily unavailable."`) || !strings.Contains(body, `"retriable":true`) {
		t.Fatalf("unsafe/invalid SSE error status=%d body=%q", response.Code, body)
	}
	for _, secret := range []string{"mysql", "10.0.0.8", "provider-secret", "Bearer"} {
		if strings.Contains(body, secret) {
			t.Fatalf("SSE error leaked %q: %q", secret, body)
		}
	}
}

func TestAgentSessionHTTPSSEHeartbeatDisconnectClosesEveryRunStream(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	fixture.rebuildHandler(t, 10*time.Millisecond)
	fixture.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: "session-live", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentconversation.StatusCollectingInput, Version: 2}, Runs: []agentcontrol.Run{
		{ID: "run-finished", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentcontrol.StatusCompleted, CreatedAtUnixMs: 10},
		{ID: "run-blocking", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentcontrol.StatusPlanning, CreatedAtUnixMs: 20},
	}})
	fixture.runtime.setStream("run-finished", sessionHTTPStreamPlan{events: []agentcontrol.RuntimeEvent{{RunID: "run-finished", Seq: 1, Type: "run.completed"}}})
	fixture.runtime.setStream("run-blocking", sessionHTTPStreamPlan{block: true})

	returned := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.handler.ServeHTTP(w, r)
		close(returned)
	}))
	t.Cleanup(server.Close)
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, server.URL+agentSessionsPath+"/session-live/events?after_seq=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+fixture.token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("SSE request: %v", err)
	}
	reader := bufio.NewReader(response.Body)
	heartbeat := false
	for i := 0; i < 8; i++ {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("read SSE before heartbeat: %v", readErr)
		}
		if line == ": heartbeat\n" {
			heartbeat = true
			break
		}
	}
	if !heartbeat {
		t.Fatal("idle stream did not emit heartbeat")
	}
	waitSessionHTTPCondition(t, "second run stream to open", func() bool { return len(fixture.runtime.streamsFor("run-blocking")) == 1 })
	cancel()
	_ = response.Body.Close()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("session SSE handler did not return after disconnect")
	}
	for _, runID := range []string{"run-finished", "run-blocking"} {
		streams := fixture.runtime.streamsFor(runID)
		if len(streams) != 1 || !streams[0].isClosed() {
			t.Fatalf("%s streams=%d closed=%v", runID, len(streams), len(streams) == 1 && streams[0].isClosed())
		}
	}
}

func TestAgentSessionHTTPRecoveryActiveRunReflectsTerminality(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	active := agentcontrol.Run{ID: "run-active", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentcontrol.StatusPlanning, CreatedAtUnixMs: 10}
	fixture.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: "session-active", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentconversation.StatusCollectingInput, Version: 1}, Runs: []agentcontrol.Run{active}})

	response := fixture.request(http.MethodGet, agentSessionsPath+"/session-active", "")
	var activeEnvelope struct {
		Result struct {
			Code int `json:"code"`
		} `json:"result"`
		ActiveRun *agentRunResponse `json:"activeRun"`
	}
	decodeSessionEnvelope(t, response, &activeEnvelope)
	if activeEnvelope.Result.Code != 0 || activeEnvelope.ActiveRun == nil || activeEnvelope.ActiveRun.RunID != "run-active" || activeEnvelope.ActiveRun.Status != string(agentcontrol.StatusPlanning) {
		t.Fatalf("active recovery=%s", response.Body.String())
	}

	active.Status = agentcontrol.StatusCompleted
	fixture.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: "session-active", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentconversation.StatusCollectingInput, Version: 1}, Messages: []agentconversation.Message{{SessionID: "session-active", Seq: 1, Role: agentconversation.MessageRoleAssistant, RunID: active.ID, Content: "done"}}, Runs: []agentcontrol.Run{active}})
	terminal := fixture.request(http.MethodGet, agentSessionsPath+"/session-active", "")
	var terminalEnvelope struct {
		Result struct {
			Code int `json:"code"`
		} `json:"result"`
		ActiveRun *agentRunResponse `json:"activeRun"`
	}
	decodeSessionEnvelope(t, terminal, &terminalEnvelope)
	if terminalEnvelope.Result.Code != 0 || terminalEnvelope.ActiveRun != nil {
		t.Fatalf("terminal recovery=%s", terminal.Body.String())
	}
}

type sessionHandlerFixture struct {
	handler http.Handler
	token   string
	repo    *sessionHTTPRepo
	room    *sessionHTTPRoom
	runtime *sessionHTTPRuntime
	auth    *auth.Manager
}

func newAgentSessionHandlerFixture(t *testing.T, tenant string, permissions []string) *sessionHandlerFixture {
	t.Helper()
	manager, err := auth.NewManager(auth.Config{Secret: "session-http", Issuer: "test", AccessTTL: time.Hour, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := manager.IssueTokenPair(&auctionv1.User{Id: "operator-a", MainAccountId: tenant, RoleCodes: []string{userbiz.RoleOperator}, PermissionCodes: permissions, Status: auctionv1.UserStatus_USER_STATUS_ACTIVE})
	if err != nil {
		t.Fatal(err)
	}
	repo := &sessionHTTPRepo{sessions: map[string]agentconversation.SessionAggregate{}}
	room := &sessionHTTPRoom{main: "merchant-a"}
	runtime := newSessionHTTPRuntime(repo)
	service, err := agentconversation.NewService(repo, room, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return &sessionHandlerFixture{handler: NewAgentSessionHTTPHandler(service, manager, AgentHTTPOptions{}), token: pair.AccessToken, repo: repo, room: room, runtime: runtime, auth: manager}
}

func (f *sessionHandlerFixture) rebuildHandler(t *testing.T, heartbeat time.Duration) {
	t.Helper()
	service, err := agentconversation.NewService(f.repo, f.room, f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	f.handler = NewAgentSessionHTTPHandler(service, f.auth, AgentHTTPOptions{HeartbeatInterval: heartbeat})
}
func (f *sessionHandlerFixture) request(method, path, body string) *httptest.ResponseRecorder {
	return f.requestWithHeaders(method, path, body, nil)
}
func (f *sessionHandlerFixture) requestWithHeaders(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+f.token)
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func decodeSessionEnvelope(t *testing.T, w *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
}

func createAgentHTTPSession(t *testing.T, fixture *sessionHandlerFixture) sessionHTTPResponse {
	t.Helper()
	response := fixture.request(http.MethodPost, agentSessionsPath, `{"roomId":"room-a"}`)
	var envelope struct {
		Result struct {
			Code int `json:"code"`
		} `json:"result"`
		Session sessionHTTPResponse `json:"session"`
	}
	decodeSessionEnvelope(t, response, &envelope)
	if response.Code != http.StatusOK || envelope.Result.Code != 0 || envelope.Session.ID == "" {
		t.Fatalf("create session status=%d body=%s", response.Code, response.Body.String())
	}
	return envelope.Session
}

func sessionSSEIDs(body string) []int64 {
	var ids []int64
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "id: ") {
			continue
		}
		if id, err := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func equalInt64s(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func waitSessionHTTPCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}
func sessionResultCode(t *testing.T, w *httptest.ResponseRecorder) int {
	t.Helper()
	var body struct {
		Result struct {
			Code int `json:"code"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Result.Code
}

type sessionHTTPRoom struct{ main string }

func (r *sessionHTTPRoom) FindRoomByID(context.Context, string) (*auction.Room, bool, error) {
	return &auction.Room{ID: "room-a", MainAccountID: r.main}, true, nil
}

type sessionHTTPStreamPlan struct {
	events   []agentcontrol.RuntimeEvent
	finalErr error
	block    bool
}

type sessionHTTPRuntime struct {
	mu          sync.Mutex
	repo        *sessionHTTPRepo
	starts      []string
	gets        []string
	runs        map[string]agentcontrol.Run
	plans       map[string]sessionHTTPStreamPlan
	streams     map[string][]*sessionHTTPEventStream
	streamAfter map[string][]int64
}

func newSessionHTTPRuntime(repo *sessionHTTPRepo) *sessionHTTPRuntime {
	return &sessionHTTPRuntime{repo: repo, runs: map[string]agentcontrol.Run{}, plans: map[string]sessionHTTPStreamPlan{}, streams: map[string][]*sessionHTTPEventStream{}, streamAfter: map[string][]int64{}}
}

func (r *sessionHTTPRuntime) StartPersistedSessionRun(_ context.Context, _ agentcontrol.Actor, id string) (agentcontrol.Run, error) {
	r.mu.Lock()
	r.starts = append(r.starts, id)
	run, ok := r.runs[id]
	r.mu.Unlock()
	if ok {
		r.repo.replaceRun(run)
		return run, nil
	}
	return r.repo.findRun(id)
}

func (r *sessionHTTPRuntime) GetRun(_ context.Context, _ agentcontrol.Actor, id string) (agentcontrol.Run, error) {
	r.mu.Lock()
	r.gets = append(r.gets, id)
	run, ok := r.runs[id]
	r.mu.Unlock()
	if !ok {
		return agentcontrol.Run{}, apperr.ErrNotFound
	}
	r.repo.replaceRun(run)
	return run, nil
}

func (r *sessionHTTPRuntime) StreamRunEvents(ctx context.Context, _ agentcontrol.Actor, id string, after int64) (agentcontrol.EventStream, error) {
	r.mu.Lock()
	plan, ok := r.plans[id]
	if !ok {
		r.mu.Unlock()
		return nil, apperr.ErrNotFound
	}
	stream := &sessionHTTPEventStream{ctx: ctx, events: append([]agentcontrol.RuntimeEvent(nil), plan.events...), finalErr: plan.finalErr, block: plan.block, closed: make(chan struct{})}
	r.streams[id] = append(r.streams[id], stream)
	r.streamAfter[id] = append(r.streamAfter[id], after)
	r.mu.Unlock()
	return stream, nil
}

func (r *sessionHTTPRuntime) setRun(run agentcontrol.Run) {
	r.mu.Lock()
	r.runs[run.ID] = run
	r.mu.Unlock()
}

func (r *sessionHTTPRuntime) setStream(id string, plan sessionHTTPStreamPlan) {
	r.mu.Lock()
	r.plans[id] = plan
	r.mu.Unlock()
}

func (r *sessionHTTPRuntime) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.starts)
}

func (r *sessionHTTPRuntime) getCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.gets)
}

func (r *sessionHTTPRuntime) streamCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, streams := range r.streams {
		total += len(streams)
	}
	return total
}

func (r *sessionHTTPRuntime) streamsFor(id string) []*sessionHTTPEventStream {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*sessionHTTPEventStream(nil), r.streams[id]...)
}

type sessionHTTPEventStream struct {
	ctx      context.Context
	events   []agentcontrol.RuntimeEvent
	index    int
	finalErr error
	block    bool
	closed   chan struct{}
	once     sync.Once
}

func (s *sessionHTTPEventStream) Recv() (agentcontrol.RuntimeEvent, error) {
	if s.index < len(s.events) {
		event := s.events[s.index]
		s.index++
		return event, nil
	}
	if s.finalErr != nil {
		return agentcontrol.RuntimeEvent{}, s.finalErr
	}
	if s.block {
		select {
		case <-s.ctx.Done():
			return agentcontrol.RuntimeEvent{}, s.ctx.Err()
		case <-s.closed:
			return agentcontrol.RuntimeEvent{}, io.EOF
		}
	}
	return agentcontrol.RuntimeEvent{}, io.EOF
}

func (s *sessionHTTPEventStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *sessionHTTPEventStream) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

type sessionHTTPRepo struct {
	mu                 sync.Mutex
	sessions           map[string]agentconversation.SessionAggregate
	lastTenant         string
	snapshots          map[string][]agentconversation.ReviewSnapshot
	reviewAuthorizeErr error
}

func (r *sessionHTTPRepo) CreateSession(_ context.Context, s agentconversation.Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastTenant = s.TenantID
	r.sessions[s.ID] = agentconversation.SessionAggregate{Session: s}
	return nil
}
func (r *sessionHTTPRepo) FindSession(_ context.Context, t, id string) (agentconversation.SessionAggregate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.sessions[id]
	if !ok || v.Session.TenantID != t {
		return agentconversation.SessionAggregate{}, apperr.ErrNotFound
	}
	return cloneSessionHTTPAggregate(v), nil
}
func (r *sessionHTTPRepo) AppendUserMessageAndStartRun(_ context.Context, command agentconversation.StartTurnCommand) (agentconversation.SessionAggregate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	aggregate, ok := r.sessions[command.SessionID]
	if !ok || aggregate.Session.TenantID != command.TenantID {
		return agentconversation.SessionAggregate{}, apperr.ErrNotFound
	}
	if aggregate.Session.OperatorID != command.OperatorID {
		return agentconversation.SessionAggregate{}, apperr.ErrPermissionDenied
	}
	if aggregate.Session.Version != command.ExpectedVersion {
		return agentconversation.SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
	}
	aggregate.Session.Version++
	aggregate.Session.UpdatedAtUnixMs = command.Run.UpdatedAtUnixMs
	command.Run.SessionVersion = aggregate.Session.Version
	command.Message.SessionID = command.SessionID
	command.Message.Seq = int64(len(aggregate.Messages) + 1)
	command.Message.Role = agentconversation.MessageRoleUser
	aggregate.Messages = append(aggregate.Messages, command.Message)
	aggregate.Runs = append(aggregate.Runs, command.Run)
	r.sessions[command.SessionID] = aggregate
	return cloneSessionHTTPAggregate(aggregate), nil
}
func (r *sessionHTTPRepo) MergeTurnResult(_ context.Context, command agentconversation.MergeTurnCommand) (agentconversation.SessionAggregate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	aggregate, ok := r.sessions[command.SessionID]
	if !ok || aggregate.Session.TenantID != command.TenantID {
		return agentconversation.SessionAggregate{}, apperr.ErrNotFound
	}
	if aggregate.Session.OperatorID != command.OperatorID {
		return agentconversation.SessionAggregate{}, apperr.ErrPermissionDenied
	}
	if aggregate.Session.Version != command.ExpectedVersion {
		return agentconversation.SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
	}
	for _, message := range aggregate.Messages {
		if message.Role == agentconversation.MessageRoleAssistant && message.RunID == command.RunID {
			return cloneSessionHTTPAggregate(aggregate), nil
		}
	}
	aggregate.Session.Version++
	aggregate.Session.Status = command.Status
	aggregate.Session.ConversationSummary = command.ConversationSummary
	aggregate.Session.UpdatedAtUnixMs = command.UpdatedAtUnixMs
	command.AssistantMessage.SessionID = command.SessionID
	command.AssistantMessage.Seq = int64(len(aggregate.Messages) + 1)
	command.AssistantMessage.Role = agentconversation.MessageRoleAssistant
	aggregate.Messages = append(aggregate.Messages, command.AssistantMessage)
	aggregate.Candidates = append([]agentconversation.DraftCandidate(nil), command.Candidates...)
	r.sessions[command.SessionID] = aggregate
	return cloneSessionHTTPAggregate(aggregate), nil
}
func (r *sessionHTTPRepo) PatchCandidate(_ context.Context, command agentconversation.PatchCandidateCommand) (agentconversation.SessionAggregate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	aggregate, ok := r.sessions[command.SessionID]
	if !ok || aggregate.Session.TenantID != command.TenantID {
		return agentconversation.SessionAggregate{}, apperr.ErrNotFound
	}
	if aggregate.Session.Version != command.ExpectedSessionVersion {
		return agentconversation.SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
	}
	if aggregate.Session.OperatorID != command.OperatorID {
		return agentconversation.SessionAggregate{}, apperr.ErrPermissionDenied
	}
	found := false
	for i := range aggregate.Candidates {
		if aggregate.Candidates[i].Candidate.ID == command.CandidateID {
			if aggregate.Candidates[i].Version != command.ExpectedCandidateVersion {
				return agentconversation.SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
			}
			command.Candidate.Version = aggregate.Candidates[i].Version + 1
			aggregate.Candidates[i] = command.Candidate
			found = true
		}
	}
	if !found {
		return agentconversation.SessionAggregate{}, apperr.ErrNotFound
	}
	aggregate.Session.Version++
	aggregate.Session.Status = command.Status
	aggregate.Session.ActiveSnapshotVersion = 0
	r.sessions[command.SessionID] = aggregate
	return cloneSessionHTTPAggregate(aggregate), nil
}
func (r *sessionHTTPRepo) BindCandidateImage(_ context.Context, command agentconversation.BindCandidateImageCommand) (agentconversation.SessionAggregate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	aggregate, ok := r.sessions[command.SessionID]
	if !ok || aggregate.Session.TenantID != command.TenantID {
		return agentconversation.SessionAggregate{}, apperr.ErrNotFound
	}
	if aggregate.Session.OperatorID != command.OperatorID {
		return agentconversation.SessionAggregate{}, apperr.ErrPermissionDenied
	}
	if aggregate.Session.Version != command.ExpectedSessionVersion {
		return agentconversation.SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
	}
	for i := range aggregate.Candidates {
		if aggregate.Candidates[i].Candidate.ID != command.CandidateID {
			continue
		}
		if aggregate.Candidates[i].Version != command.ExpectedCandidateVersion {
			return agentconversation.SessionAggregate{}, apperr.ErrAgentSessionVersionConflict
		}
		aggregate.Candidates[i].Candidate.Draft.MainImageAssetID = command.AssetID
		aggregate.Candidates[i].Candidate.Draft.MainImageURL = "https://assets.example/" + command.AssetID + ".jpg"
		aggregate.Candidates[i].Version++
		aggregate.Session.Version++
		aggregate.Session.ActiveSnapshotVersion = 0
		r.sessions[command.SessionID] = aggregate
		return cloneSessionHTTPAggregate(aggregate), nil
	}
	return agentconversation.SessionAggregate{}, apperr.ErrNotFound
}
func (r *sessionHTTPRepo) CancelSession(_ context.Context, tenant, operator, id string, expectedVersion int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	aggregate, ok := r.sessions[id]
	if !ok || aggregate.Session.TenantID != tenant {
		return apperr.ErrNotFound
	}
	if aggregate.Session.OperatorID != operator {
		return apperr.ErrPermissionDenied
	}
	if aggregate.Session.Version != expectedVersion {
		return apperr.ErrAgentSessionVersionConflict
	}
	aggregate.Session.Status = agentconversation.StatusCancelled
	aggregate.Session.Version++
	r.sessions[id] = aggregate
	return nil
}

func (r *sessionHTTPRepo) CreateReviewSnapshot(_ context.Context, command agentconversation.CreateReviewSnapshotCommand) (agentconversation.ReviewSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	aggregate, ok := r.sessions[command.SessionID]
	if !ok || aggregate.Session.TenantID != command.TenantID {
		return agentconversation.ReviewSnapshot{}, apperr.ErrNotFound
	}
	if aggregate.Session.OperatorID != command.OperatorID {
		return agentconversation.ReviewSnapshot{}, apperr.ErrPermissionDenied
	}
	if aggregate.Session.Version != command.ExpectedSessionVersion {
		return agentconversation.ReviewSnapshot{}, apperr.ErrAgentSessionVersionConflict
	}
	if r.snapshots == nil {
		r.snapshots = map[string][]agentconversation.ReviewSnapshot{}
	}
	command.Snapshot.SnapshotVersion = int64(len(r.snapshots[command.SessionID]) + 1)
	r.snapshots[command.SessionID] = append(r.snapshots[command.SessionID], command.Snapshot)
	aggregate.Session.Version++
	aggregate.Session.ActiveSnapshotVersion = command.Snapshot.SnapshotVersion
	r.sessions[command.SessionID] = aggregate
	return command.Snapshot, nil
}

func (r *sessionHTTPRepo) AuthorizeReviewSnapshot(context.Context, agentconversation.CreateReviewSnapshotCommand) error {
	return r.reviewAuthorizeErr
}

func (r *sessionHTTPRepo) FindReviewSnapshot(_ context.Context, tenantID, operatorID, sessionID string, version int64) (agentconversation.ReviewSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	aggregate, ok := r.sessions[sessionID]
	if !ok || aggregate.Session.TenantID != tenantID {
		return agentconversation.ReviewSnapshot{}, apperr.ErrNotFound
	}
	if aggregate.Session.OperatorID != operatorID {
		return agentconversation.ReviewSnapshot{}, apperr.ErrPermissionDenied
	}
	for _, snapshot := range r.snapshots[sessionID] {
		if snapshot.SnapshotVersion == version {
			return snapshot, nil
		}
	}
	return agentconversation.ReviewSnapshot{}, apperr.ErrNotFound
}

func (r *sessionHTTPRepo) put(aggregate agentconversation.SessionAggregate) {
	r.mu.Lock()
	r.sessions[aggregate.Session.ID] = cloneSessionHTTPAggregate(aggregate)
	r.mu.Unlock()
}

func (r *sessionHTTPRepo) findRun(id string) (agentcontrol.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, aggregate := range r.sessions {
		for _, run := range aggregate.Runs {
			if run.ID == id {
				return run, nil
			}
		}
	}
	return agentcontrol.Run{}, apperr.ErrNotFound
}

func (r *sessionHTTPRepo) replaceRun(run agentcontrol.Run) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, aggregate := range r.sessions {
		for i := range aggregate.Runs {
			if aggregate.Runs[i].ID == run.ID {
				aggregate.Runs[i] = run
				r.sessions[id] = aggregate
				return
			}
		}
	}
}

func cloneSessionHTTPAggregate(aggregate agentconversation.SessionAggregate) agentconversation.SessionAggregate {
	aggregate.Messages = append([]agentconversation.Message(nil), aggregate.Messages...)
	aggregate.Candidates = append([]agentconversation.DraftCandidate(nil), aggregate.Candidates...)
	aggregate.Runs = append([]agentcontrol.Run(nil), aggregate.Runs...)
	return aggregate
}

func TestAgentSessionHTTPStrictBodyAndAfterSeq(t *testing.T) {
	for _, body := range []string{`{"roomId":"r","unknown":true}`, `{"roomId":"r"} {}`} {
		request := httptest.NewRequest(http.MethodPost, agentSessionsPath, strings.NewReader(body))
		response := httptest.NewRecorder()
		var target struct {
			RoomID string `json:"roomId"`
		}
		if decodeSessionBody(response, request, &target) {
			t.Fatalf("accepted %q", body)
		}
	}
	for _, path := range []string{"?after_seq=-1", "?after_seq=1&after_seq=2", "?after_seq=x"} {
		request := httptest.NewRequest(http.MethodGet, agentSessionsPath+"/s/events"+path, nil)
		if _, err := parseAfterSeq(request); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
}

func TestAgentSessionHTTPReviewSnapshotLifecycleStrictnessAndAuth(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	candidate := agentconversation.Candidate{ID: "candidate-1", SourceType: agentconversation.SourceNew, StartPriceSpecified: true, Draft: agentconversation.CandidateDraft{Title: "private title", StartPriceAmount: 100, DurationSeconds: 120, MainImageAssetID: "asset-1", MainImageURL: "https://signed.example/secret", MinIncrementAmount: 10, AntiSnipeWindowSeconds: 10, AntiSnipeExtendSeconds: 15, MaxExtendCount: 5}}
	fixture.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: "session-review", TenantID: "merchant-a", OperatorID: "operator-a", RoomID: "room-a", Status: agentconversation.StatusReadyForReview, Version: 8}, Candidates: []agentconversation.DraftCandidate{{Candidate: candidate, Position: 1, Version: 3, FieldSourcesJSON: reviewSnapshotHTTPFieldSources("USER")}}})
	path := agentSessionsPath + "/session-review/review-snapshots"

	noBearer := httptest.NewRecorder()
	fixture.handler.ServeHTTP(noBearer, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"expectedSessionVersion":8,"selectedCandidateIds":["candidate-1"],"orderedCandidateIds":["candidate-1"]}`)))
	if sessionResultCode(t, noBearer) != int(appsvc.ResultCodeUnauthenticated) || strings.Contains(noBearer.Body.String(), "private title") {
		t.Fatalf("unauthorized snapshot leaked content: %s", noBearer.Body.String())
	}
	denied := newAgentSessionHandlerFixture(t, "merchant-a", nil)
	denied.repo = fixture.repo
	denied.runtime = newSessionHTTPRuntime(denied.repo)
	denied.rebuildHandler(t, 0)
	deniedResponse := denied.request(http.MethodPost, path, `{"expectedSessionVersion":8,"selectedCandidateIds":["candidate-1"],"orderedCandidateIds":["candidate-1"]}`)
	if sessionResultCode(t, deniedResponse) != int(appsvc.ResultCodePermissionDenied) || strings.Contains(deniedResponse.Body.String(), "private title") {
		t.Fatalf("permission denial leaked content: %s", deniedResponse.Body.String())
	}
	for name, user := range map[string]*auctionv1.User{
		"cross tenant":   {Id: "operator-b", MainAccountId: "merchant-b", RoleCodes: []string{userbiz.RoleOperator}, PermissionCodes: []string{userbiz.PermissionLotViewAdmin}, Status: auctionv1.UserStatus_USER_STATUS_ACTIVE},
		"cross operator": {Id: "operator-b", MainAccountId: "merchant-a", RoleCodes: []string{userbiz.RoleOperator}, PermissionCodes: []string{userbiz.PermissionLotViewAdmin}, Status: auctionv1.UserStatus_USER_STATUS_ACTIVE},
	} {
		t.Run(name, func(t *testing.T) {
			pair, err := fixture.auth.IssueTokenPair(user)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"expectedSessionVersion":8,"selectedCandidateIds":["candidate-1"],"orderedCandidateIds":["candidate-1"]}`))
			request.Header.Set("Authorization", "Bearer "+pair.AccessToken)
			response := httptest.NewRecorder()
			fixture.handler.ServeHTTP(response, request)
			code := sessionResultCode(t, response)
			if code != int(appsvc.ResultCodeUserNotFound) && code != int(appsvc.ResultCodePermissionDenied) {
				t.Fatalf("isolation response=%s", response.Body.String())
			}
			if strings.Contains(response.Body.String(), "private title") || strings.Contains(response.Body.String(), "candidate-1") {
				t.Fatalf("isolation leaked content: %s", response.Body.String())
			}
		})
	}
	wrongMethod := fixture.request(http.MethodGet, path, "")
	if wrongMethod.Code != http.StatusMethodNotAllowed || wrongMethod.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("method status=%d allow=%q", wrongMethod.Code, wrongMethod.Header().Get("Allow"))
	}
	for _, body := range []string{
		`{"expectedSessionVersion":8,"selectedCandidateIds":[],"orderedCandidateIds":[]}`,
		`{"expectedSessionVersion":8,"selectedCandidateIds":["candidate-1"],"orderedCandidateIds":["candidate-1"],"unknown":true}`,
	} {
		response := fixture.request(http.MethodPost, path, body)
		if sessionResultCode(t, response) != int(appsvc.ResultCodeInvalidArgument) {
			t.Fatalf("strict snapshot body accepted: %s", response.Body.String())
		}
	}
	fixture.repo.reviewAuthorizeErr = fmt.Errorf("%w: storage-secret-do-not-leak", apperr.ErrAgentImageAssetInvalid)
	unsafe := fixture.request(http.MethodPost, path, `{"expectedSessionVersion":8,"selectedCandidateIds":["candidate-1"],"orderedCandidateIds":["candidate-1"]}`)
	if sessionResultCode(t, unsafe) != int(appsvc.ResultCodeInvalidArgument) || strings.Contains(unsafe.Body.String(), "storage-secret") {
		t.Fatalf("unsafe snapshot authorization error: %s", unsafe.Body.String())
	}
	fixture.repo.reviewAuthorizeErr = nil
	response := fixture.request(http.MethodPost, path, `{"expectedSessionVersion":8,"selectedCandidateIds":["candidate-1"],"orderedCandidateIds":["candidate-1"]}`)
	var envelope struct {
		Result struct {
			Code int `json:"code"`
		} `json:"result"`
		SnapshotVersion int64                                 `json:"snapshotVersion"`
		ContentHash     string                                `json:"contentHash"`
		Candidates      []agentconversation.SnapshotCandidate `json:"candidates"`
	}
	decodeSessionEnvelope(t, response, &envelope)
	if envelope.Result.Code != 0 || envelope.SnapshotVersion != 1 || len(envelope.ContentHash) != 64 || len(envelope.Candidates) != 1 {
		t.Fatalf("snapshot response: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "signed.example") || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("snapshot response leaked raw URL: %s", response.Body.String())
	}
	recoveryPath := path + "/1"
	unauthorizedRecovery := httptest.NewRecorder()
	fixture.handler.ServeHTTP(unauthorizedRecovery, httptest.NewRequest(http.MethodGet, recoveryPath, nil))
	if sessionResultCode(t, unauthorizedRecovery) != int(appsvc.ResultCodeUnauthenticated) || strings.Contains(unauthorizedRecovery.Body.String(), "private title") {
		t.Fatalf("unauthorized recovery leaked content: %s", unauthorizedRecovery.Body.String())
	}
	crossPair, err := fixture.auth.IssueTokenPair(&auctionv1.User{Id: "operator-b", MainAccountId: "merchant-a", RoleCodes: []string{userbiz.RoleOperator}, PermissionCodes: []string{userbiz.PermissionLotViewAdmin}, Status: auctionv1.UserStatus_USER_STATUS_ACTIVE})
	if err != nil {
		t.Fatal(err)
	}
	crossRequest := httptest.NewRequest(http.MethodGet, recoveryPath, nil)
	crossRequest.Header.Set("Authorization", "Bearer "+crossPair.AccessToken)
	crossRecovery := httptest.NewRecorder()
	fixture.handler.ServeHTTP(crossRecovery, crossRequest)
	if sessionResultCode(t, crossRecovery) != int(appsvc.ResultCodePermissionDenied) || strings.Contains(crossRecovery.Body.String(), "private title") || strings.Contains(crossRecovery.Body.String(), "candidate-1") {
		t.Fatalf("cross-operator recovery leaked content: %s", crossRecovery.Body.String())
	}
	recovered := fixture.request(http.MethodGet, recoveryPath, "")
	var recoveredEnvelope struct {
		Result struct {
			Code int `json:"code"`
		} `json:"result"`
		SnapshotVersion int64                                 `json:"snapshotVersion"`
		ContentHash     string                                `json:"contentHash"`
		Candidates      []agentconversation.SnapshotCandidate `json:"candidates"`
	}
	decodeSessionEnvelope(t, recovered, &recoveredEnvelope)
	if recoveredEnvelope.Result.Code != 0 || recoveredEnvelope.SnapshotVersion != 1 || recoveredEnvelope.ContentHash != envelope.ContentHash || len(recoveredEnvelope.Candidates) != 1 {
		t.Fatalf("recovered snapshot=%s", recovered.Body.String())
	}
	if wrongRecoveryMethod := fixture.request(http.MethodPost, recoveryPath, "{}"); wrongRecoveryMethod.Code != http.StatusMethodNotAllowed || wrongRecoveryMethod.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("recovery method status=%d allow=%q", wrongRecoveryMethod.Code, wrongRecoveryMethod.Header().Get("Allow"))
	}
	if missing := fixture.request(http.MethodGet, path+"/2", ""); sessionResultCode(t, missing) != int(appsvc.ResultCodeConflict) || strings.Contains(missing.Body.String(), "private title") {
		t.Fatalf("non-active recovery=%s", missing.Body.String())
	}
	aggregate, err := fixture.repo.FindSession(context.Background(), "merchant-a", "session-review")
	if err != nil || aggregate.Session.Version != 9 || aggregate.Session.ActiveSnapshotVersion != 1 {
		t.Fatalf("active snapshot session=%+v err=%v", aggregate.Session, err)
	}
	stale := fixture.request(http.MethodPost, path, `{"expectedSessionVersion":8,"selectedCandidateIds":["candidate-1"],"orderedCandidateIds":["candidate-1"]}`)
	if sessionResultCode(t, stale) != int(appsvc.ResultCodeConflict) {
		t.Fatalf("stale snapshot=%s", stale.Body.String())
	}
}

func reviewSnapshotHTTPFieldSources(source string) string {
	fields := []string{"antiSnipeExtendSeconds", "antiSnipeWindowSeconds", "capPriceAmount", "category", "depositAmount", "description", "durationSeconds", "mainImageAssetId", "maxExtendCount", "minIncrementAmount", "startPriceAmount", "stock", "tags", "title"}
	values := make([]string, 0, len(fields))
	for _, field := range fields {
		values = append(values, `"`+field+`":"`+source+`"`)
	}
	return "{" + strings.Join(values, ",") + "}"
}

func TestAgentAdminHTTPDispatchesSessionAndRunHandlers(t *testing.T) {
	runs := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "run") })
	sessions := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "session") })
	handler := NewAgentAdminHTTPHandler(runs, sessions)
	for path, want := range map[string]string{"/api/admin/agent/sessions/s-1/events?after_seq=2": "session", "/api/admin/agent/runs/r-1": "run"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Body.String() != want {
			t.Fatalf("%s = %q, want %q", path, response.Body.String(), want)
		}
	}
}
