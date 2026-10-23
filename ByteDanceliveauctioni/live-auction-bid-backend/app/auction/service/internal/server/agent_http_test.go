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
	"strings"
	"sync"
	"testing"
	"time"

	auctionv1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
	"live-auction-bid/backend/app/auction/service/internal/pkg/auth"
	appsvc "live-auction-bid/backend/app/auction/service/internal/service"

	httptransport "github.com/go-kratos/kratos/v2/transport/http"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRegisterAgentHTTPRoutesOnlyAgentPrefix(t *testing.T) {
	transport := httptransport.NewServer()
	registerAgentHTTP(transport, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(w, request.URL.Path)
	}))

	agentResponse := httptest.NewRecorder()
	transport.ServeHTTP(agentResponse, httptest.NewRequest(http.MethodGet, "/api/admin/agent/runs/run-1", nil))
	if agentResponse.Body.String() != "/api/admin/agent/runs/run-1" {
		t.Fatalf("registered agent response = %q", agentResponse.Body.String())
	}
	nonAgentResponse := httptest.NewRecorder()
	transport.ServeHTTP(nonAgentResponse, httptest.NewRequest(http.MethodGet, "/api/admin/lots", nil))
	if nonAgentResponse.Body.String() == "/api/admin/lots" {
		t.Fatal("agent handler captured a non-agent route")
	}
}

func TestAgentHTTPCreateDerivesIdentityAndIgnoresClientAuthorityFields(t *testing.T) {
	fixture := newAgentHTTPFixture(t, agentHTTPUser{
		ID: "user-a", MainAccountID: "merchant-a", Permissions: []string{userbiz.PermissionLotViewAdmin},
	})
	body := `{"goal":"清理库存","tenantId":"merchant-b","operatorId":"attacker","traceId":"client-trace","requestId":"client-request"}`

	response := fixture.request(http.MethodPost, "/api/admin/agent/runs", body)
	if response.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", response.Code)
	}
	envelope := decodeAgentEnvelope(t, response.Body.Bytes())
	if envelope.Result.Code != appsvc.ResultCodeOK || envelope.Run.RunID == "" {
		t.Fatalf("POST envelope = %+v", envelope)
	}
	start := fixture.runtime.lastStart()
	if start.Context.TenantID != "merchant-a" || start.Context.OperatorID != "user-a" {
		t.Fatalf("runtime authority = %+v", start.Context)
	}
	if start.Context.TraceID == "client-trace" || start.Context.RequestID == "client-request" {
		t.Fatalf("client correlation fields reached runtime authority context: %+v", start.Context)
	}
}

func TestAgentHTTPCreateRequiresAuthenticationPermissionAndEffectiveMainAccount(t *testing.T) {
	tests := []struct {
		name     string
		user     agentHTTPUser
		noToken  bool
		wantCode int32
	}{
		{name: "authentication", noToken: true, wantCode: appsvc.ResultCodeUnauthenticated},
		{name: "permission", user: agentHTTPUser{ID: "user-a", MainAccountID: "merchant-a"}, wantCode: appsvc.ResultCodePermissionDenied},
		{name: "effective main account", user: agentHTTPUser{ID: "user-a", Permissions: []string{userbiz.PermissionLotViewAdmin}}, wantCode: appsvc.ResultCodePermissionDenied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAgentHTTPFixture(t, test.user)
			if test.noToken {
				fixture.token = ""
			}
			response := fixture.request(http.MethodPost, "/api/admin/agent/runs", `{"goal":"清理库存"}`)
			envelope := decodeAgentEnvelope(t, response.Body.Bytes())
			if envelope.Result.Code != test.wantCode {
				t.Fatalf("result code = %d, want %d; body=%s", envelope.Result.Code, test.wantCode, response.Body.String())
			}
			if fixture.runtime.startCount() != 0 {
				t.Fatal("runtime called before HTTP authorization completed")
			}
		})
	}
}

func TestAgentHTTPCreateRejectsInvalidAndOversizedJSONSafely(t *testing.T) {
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	tests := []struct {
		name string
		body string
	}{
		{name: "blank goal", body: `{"goal":"   "}`},
		{name: "goal too long", body: `{"goal":"` + strings.Repeat("a", agentcontrol.MaxGoalBytes+1) + `"}`},
		{name: "malformed", body: `{"goal":`},
		{name: "multiple documents", body: `{"goal":"one"} {"goal":"two"}`},
		{name: "body too large", body: `{"goal":"ok","padding":"` + strings.Repeat("x", 70*1024) + `"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := fixture.request(http.MethodPost, "/api/admin/agent/runs", test.body)
			envelope := decodeAgentEnvelope(t, response.Body.Bytes())
			if envelope.Result.Code != appsvc.ResultCodeInvalidArgument {
				t.Fatalf("result code = %d, want invalid argument; body=%s", envelope.Result.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), strings.Repeat("x", 64)) {
				t.Fatal("oversized input was echoed in error response")
			}
		})
	}
	if fixture.runtime.startCount() != 0 {
		t.Fatalf("runtime called %d times for invalid bodies", fixture.runtime.startCount())
	}
}

func TestAgentHTTPCreateRequiresStrictIdempotencyKey(t *testing.T) {
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	for _, key := range []string{"", "contains space", strings.Repeat("a", 129), "slash/not-allowed"} {
		request := httptest.NewRequest(http.MethodPost, "/api/admin/agent/runs", strings.NewReader(`{"goal":"清理库存"}`))
		request.Header.Set("Authorization", "Bearer "+fixture.token)
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response := httptest.NewRecorder()
		fixture.handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("key %q status = %d, want 400; body=%s", key, response.Code, response.Body.String())
		}
	}
	if fixture.runtime.startCount() != 0 {
		t.Fatal("runtime called for invalid idempotency key")
	}
}

func TestAgentHTTPCreateIdempotencyRetryReturnsSameRunAndConflictIsStable(t *testing.T) {
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	first := fixture.requestWithKey(http.MethodPost, "/api/admin/agent/runs", `{"goal":" 清理库存 "}`, "retry-key-1001")
	second := fixture.requestWithKey(http.MethodPost, "/api/admin/agent/runs", `{"goal":"清理库存"}`, "retry-key-1001")
	firstEnvelope := decodeAgentEnvelope(t, first.Body.Bytes())
	secondEnvelope := decodeAgentEnvelope(t, second.Body.Bytes())
	if first.Code != http.StatusOK || second.Code != http.StatusOK || firstEnvelope.Run.RunID == "" || firstEnvelope.Run.RunID != secondEnvelope.Run.RunID {
		t.Fatalf("idempotent responses = %d/%+v and %d/%+v", first.Code, firstEnvelope, second.Code, secondEnvelope)
	}
	if fixture.runtime.startCount() != 1 {
		t.Fatalf("Runtime StartRun calls = %d, want 1", fixture.runtime.startCount())
	}

	conflict := fixture.requestWithKey(http.MethodPost, "/api/admin/agent/runs", `{"goal":"新品场"}`, "retry-key-1001")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d, want 409; body=%s", conflict.Code, conflict.Body.String())
	}
	if fixture.runtime.startCount() != 1 {
		t.Fatalf("Runtime StartRun calls after conflict = %d, want 1", fixture.runtime.startCount())
	}
}

func TestAgentHTTPRuntimeCapacityReturns429AndIdempotentFailedRun(t *testing.T) {
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	fixture.runtime.startErr = status.Error(codes.ResourceExhausted, "tenant active run limit reached: secret details")
	first := fixture.requestWithKey(http.MethodPost, agentRunsPath, `{"goal":"清理库存"}`, "retry-key-capacity")
	second := fixture.requestWithKey(http.MethodPost, agentRunsPath, `{"goal":"清理库存"}`, "retry-key-capacity")
	firstEnvelope := decodeAgentEnvelope(t, first.Body.Bytes())
	secondEnvelope := decodeAgentEnvelope(t, second.Body.Bytes())
	if first.Code != http.StatusTooManyRequests || second.Code != http.StatusTooManyRequests {
		t.Fatalf("capacity statuses = %d/%d, want 429", first.Code, second.Code)
	}
	if firstEnvelope.Result.Code != appsvc.ResultCodeResourceExhausted || secondEnvelope.Result.Code != appsvc.ResultCodeResourceExhausted {
		t.Fatalf("capacity result codes = %d/%d, want %d", firstEnvelope.Result.Code, secondEnvelope.Result.Code, appsvc.ResultCodeResourceExhausted)
	}
	if firstEnvelope.Run.RunID == "" || firstEnvelope.Run.RunID != secondEnvelope.Run.RunID || firstEnvelope.Run.Status != string(agentcontrol.StatusFailed) {
		t.Fatalf("capacity runs = %+v / %+v", firstEnvelope.Run, secondEnvelope.Run)
	}
	if firstEnvelope.Run.ErrorCode != agentcontrol.ErrorCodeRuntimeCapacity || strings.Contains(first.Body.String(), "secret details") {
		t.Fatalf("capacity response unsafe = %s", first.Body.String())
	}
	if fixture.runtime.startCount() != 1 {
		t.Fatalf("Runtime StartRun calls = %d, want 1", fixture.runtime.startCount())
	}
}

func TestAgentHTTPAmbiguousRuntimeStartReturnsRecoverableExistingRun(t *testing.T) {
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	fixture.runtime.startErr = status.Error(codes.Unavailable, "response lost after durable accept")
	first := fixture.requestWithKey(http.MethodPost, agentRunsPath, `{"goal":"清理库存"}`, "retry-key-ambiguous")
	second := fixture.requestWithKey(http.MethodPost, agentRunsPath, `{"goal":"清理库存"}`, "retry-key-ambiguous")
	firstEnvelope := decodeAgentEnvelope(t, first.Body.Bytes())
	secondEnvelope := decodeAgentEnvelope(t, second.Body.Bytes())
	if first.Code != http.StatusOK || second.Code != http.StatusOK || firstEnvelope.Result.Code != appsvc.ResultCodeOK {
		t.Fatalf("ambiguous statuses/results = %d/%+v and %d/%+v", first.Code, firstEnvelope.Result, second.Code, secondEnvelope.Result)
	}
	if firstEnvelope.Run.RunID == "" || secondEnvelope.Run.RunID != firstEnvelope.Run.RunID || firstEnvelope.Run.Status != string(agentcontrol.StatusCreated) || firstEnvelope.Run.ErrorCode != "" {
		t.Fatalf("ambiguous runs = %+v / %+v", firstEnvelope.Run, secondEnvelope.Run)
	}
	if fixture.runtime.startCount() != 1 {
		t.Fatalf("Runtime StartRun calls = %d, want 1", fixture.runtime.startCount())
	}
}

func TestAgentHTTPGetUsesMerchantScopeButNotCreatorOperator(t *testing.T) {
	fixture := newAgentHTTPFixture(t, agentHTTPUser{
		ID: "operator-b", MainAccountID: "merchant-a", Permissions: []string{userbiz.PermissionLotViewAdmin},
	})
	fixture.repo.putRun(agentcontrol.Run{
		ID: "run-1", TenantID: "merchant-a", OperatorID: "creator-a", TraceID: strings.Repeat("1", 32),
		Goal: "清理库存", Status: agentcontrol.StatusCreated, Version: 1,
	})
	fixture.runtime.getRun = agentcontrol.RuntimeRun{
		RunID: "run-1", Status: agentcontrol.StatusCompleted,
		ProposalJSON: validAgentProposalJSON,
	}

	response := fixture.request(http.MethodGet, "/api/admin/agent/runs/run-1", "")
	envelope := decodeAgentEnvelope(t, response.Body.Bytes())
	if envelope.Result.Code != appsvc.ResultCodeOK || envelope.Run.Status != string(agentcontrol.StatusCompleted) {
		t.Fatalf("GET envelope = %+v; body=%s", envelope, response.Body.String())
	}
	if envelope.Run.OperatorID != "" || envelope.Run.TenantID != "" || envelope.Run.TraceID != "" {
		t.Fatalf("GET exposed internal ownership/correlation: %+v", envelope.Run)
	}
	if envelope.Run.Proposal["summary"] != "Secretariat tokenization keeps this plan balanced." || strings.Contains(response.Body.String(), "[redacted]") {
		t.Fatalf("GET proposal was not safely projected: %s", response.Body.String())
	}
	lots, ok := envelope.Run.Proposal["lots"].([]any)
	if !ok || len(lots) != 1 {
		t.Fatalf("GET proposal lots = %#v", envelope.Run.Proposal["lots"])
	}
	lot, ok := lots[0].(map[string]any)
	if !ok || lot["lot_id"] != "lot-1" {
		t.Fatalf("GET proposal lot = %#v", lots[0])
	}
	if fixture.runtime.lastGet().Context.OperatorID != "operator-b" {
		t.Fatalf("GET runtime audit operator = %+v", fixture.runtime.lastGet().Context)
	}
}

const validAgentProposalJSON = `{"summary":"Secretariat tokenization keeps this plan balanced.","strategy":"balanced","estimated_duration_minutes":12,"lots":[{"lot_id":"lot-1","sequence":1,"role":"opening","start_price_amount":0,"increment_amount":100,"duration_seconds":120,"extension_seconds":10,"reason":"A secretariat-themed item supports tokenization education."}],"evidence":[{"tool":"inventory.query","reference_id":"lot-1","summary":"Inventory confirms the merchant-owned lot."}],"risks":["Tokenization terminology may need explanation."]}`

func TestAgentHTTPGetRejectsUnsafeOrMalformedRuntimeProposal(t *testing.T) {
	tests := []struct {
		name     string
		proposal string
		secrets  []string
	}{
		{
			name: "unknown identity provider and Unicode case bypass fields",
			proposal: strings.TrimSuffix(validAgentProposalJSON, "}") +
				`,"tenantId":"merchant-b","OperatorID":"attacker","traceId":"trace-secret","accessKey":"AKIA-EXAMPLE","api-key":"api-secret","Ａｕｔｈｏｒｉｚａｔｉｏｎ":"Bearer unicode-secret"}`,
			secrets: []string{"merchant-b", "attacker", "trace-secret", "AKIA-EXAMPLE", "api-secret", "unicode-secret"},
		},
		{
			name: "credential shaped free text",
			proposal: strings.Replace(validAgentProposalJSON, "Secretariat tokenization keeps this plan balanced.",
				"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJtZXJjaGFudC1hIn0.c2lnbmF0dXJlLXZhbHVl", 1),
			secrets: []string{"eyJhbGciOiJIUzI1NiJ9"},
		},
		{
			name: "Unicode and case credential marker",
			proposal: strings.Replace(validAgentProposalJSON, "Secretariat tokenization keeps this plan balanced.",
				"ＢｅＡｒＥｒ opaquevalue123456", 1),
			secrets: []string{"opaquevalue123456"},
		},
		{name: "unknown strategy enum", proposal: strings.Replace(validAgentProposalJSON, `"strategy":"balanced"`, `"strategy":"provider-secret"`, 1), secrets: []string{"provider-secret"}},
		{name: "negative amount", proposal: strings.Replace(validAgentProposalJSON, `"increment_amount":100`, `"increment_amount":-1`, 1)},
		{name: "missing required start price", proposal: strings.Replace(validAgentProposalJSON, `"start_price_amount":0,`, "", 1)},
		{name: "missing required increment", proposal: strings.Replace(validAgentProposalJSON, `"increment_amount":100,`, "", 1)},
		{name: "duration above phase one bound", proposal: strings.Replace(validAgentProposalJSON, `"duration_seconds":120`, `"duration_seconds":86401`, 1)},
		{name: "missing required evidence", proposal: `{"summary":"plan","strategy":"balanced","estimated_duration_minutes":12,"lots":[{"lot_id":"lot-1","sequence":1,"role":"opening","start_price_amount":0,"increment_amount":0,"duration_seconds":120,"extension_seconds":10,"reason":"grounded"}],"risks":[]}`},
		{name: "oversized text", proposal: strings.Replace(validAgentProposalJSON, "Secretariat tokenization keeps this plan balanced.", strings.Repeat("a", 2001), 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
			fixture.repo.putRun(agentcontrol.Run{
				ID: "run-1", TenantID: "merchant-a", OperatorID: "creator-a", TraceID: strings.Repeat("1", 32), Status: agentcontrol.StatusCreated, Version: 1,
			})
			fixture.runtime.getRun = agentcontrol.RuntimeRun{RunID: "run-1", Status: agentcontrol.StatusCompleted, ProposalJSON: test.proposal}

			response := fixture.request(http.MethodGet, "/api/admin/agent/runs/run-1", "")
			envelope := decodeAgentEnvelope(t, response.Body.Bytes())
			if len(envelope.Run.Proposal) != 0 {
				t.Fatalf("unsafe/malformed proposal reached browser: %#v", envelope.Run.Proposal)
			}
			for _, secret := range test.secrets {
				if strings.Contains(response.Body.String(), secret) {
					t.Fatalf("proposal leaked %q: %s", secret, response.Body.String())
				}
			}
		})
	}
}

func TestAgentHTTPGetRejectsCredentialMaterialInEveryProposalTextField(t *testing.T) {
	tests := []struct {
		name, field, text, marker string
	}{
		{name: "password assignment", field: "summary", text: "password=correcthorsebatterystaple", marker: "correcthorsebatterystaple"},
		{name: "client secret assignment", field: "lot_role", text: "client_secret=clientSecretValue123456", marker: "clientSecretValue123456"},
		{name: "GitHub classic token", field: "lot_reason", text: "ghp_1234567890abcdefghijklmnopqrstuvwxyzABCD", marker: "ghp_1234567890"},
		{name: "GitHub fine grained token", field: "evidence_source", text: "github_pat_11AA0abcdefghijklmnopqrstuvwxyz0123456789", marker: "github_pat_"},
		{name: "Slack token", field: "evidence_detail", text: "xoxb-" + "123456789012-123456789012-abcdefghijklmnopqrstuvwx", marker: "xoxb-"},
		{name: "Google API key", field: "risk", text: "AIzaSyA1234567890bcdefghijklmnopqrstuv", marker: "AIza"},
		{name: "fullwidth mixed case label", field: "summary", text: "ＰａＳｓＷｏＲｄ＝fullwidthSecret123456", marker: "fullwidthSecret"},
		{name: "flexible separator label", field: "lot_role", text: "AcCeSs . ToKeN : separatorToken123456", marker: "separatorToken"},
		{name: "zero width label bypass", field: "lot_reason", text: "pass\u200bword=hiddenValue123456", marker: "hiddenValue"},
		{name: "bidi override", field: "evidence_detail", text: "safe\u202etextHiddenValue123456", marker: "textHiddenValue"},
		{name: "paragraph separator", field: "risk", text: "safe\u2029textHiddenValue123456", marker: "textHiddenValue"},
		{name: "passwd assignment", field: "summary", text: "passwd:passwdValue123456", marker: "passwdValue"},
		{name: "pwd assignment", field: "summary", text: "pwd=>pwdValue123456", marker: "pwdValue"},
		{name: "secret assignment", field: "summary", text: "secret = secretValue123456", marker: "secretValue"},
		{name: "token assignment", field: "summary", text: "token=plainTokenValue123456", marker: "plainTokenValue"},
		{name: "access token assignment", field: "summary", text: "access_token=accessTokenValue123456", marker: "accessTokenValue"},
		{name: "refresh token assignment", field: "summary", text: "refresh-token: refreshTokenValue123456", marker: "refreshTokenValue"},
		{name: "API key assignment", field: "summary", text: "api-key=apiKeyValue123456", marker: "apiKeyValue"},
		{name: "access key assignment", field: "summary", text: "access.key: accessKeyValue123456", marker: "accessKeyValue"},
		{name: "secret key assignment", field: "summary", text: "secret key=secretKeyValue123456", marker: "secretKeyValue"},
		{name: "private key assignment", field: "summary", text: "private-key=privateKeyValue123456", marker: "privateKeyValue"},
		{name: "credential assignment", field: "summary", text: "credential: credentialValue123456", marker: "credentialValue"},
		{name: "authorization header", field: "summary", text: "Authorization: Custom authorizationValue123456", marker: "authorizationValue"},
		{name: "Bearer header", field: "summary", text: "Bearer opaqueBearerValue123456", marker: "opaqueBearerValue"},
		{name: "Basic header", field: "summary", text: "Basic dXNlcjpjb3JyZWN0aG9yc2ViYXR0ZXJ5c3RhcGxl", marker: "dXNlcj"},
		{name: "PEM private key", field: "summary", text: "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\n-----END PRIVATE KEY-----", marker: "BEGIN PRIVATE KEY"},
		{name: "URI userinfo", field: "summary", text: "postgresql://auction:databasePassword123@db.internal/auction", marker: "databasePassword"},
		{name: "DSN assignment", field: "summary", text: "dsn=auction:databasePassword123@tcp(db.internal:3306)/auction", marker: "databasePassword"},
		{name: "OpenAI project token", field: "summary", text: "sk-proj-1234567890abcdefghijklmnopqrstuvwxyz", marker: "sk-proj-"},
		{name: "AWS access key", field: "summary", text: "AKIAIOSFODNN7EXAMPLE", marker: "AKIA"},
		{name: "AWS temporary access key", field: "summary", text: "ASIAIOSFODNN7EXAMPLE", marker: "ASIA"},
		{name: "Stripe live secret", field: "summary", text: "sk_" + "live_1234567890abcdefghijklmnopqrstuv", marker: "sk_live_"},
		{name: "JWT", field: "summary", text: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJtZXJjaGFudC1hIn0.c2lnbmF0dXJlLXZhbHVl", marker: "eyJhbGci"},
		{name: "long hex token", field: "summary", text: "8f14e45fceea167a5a36dedd4bea2543f6f7a9b8c1d2e3f4", marker: "8f14e45f"},
		{name: "long base64 token", field: "summary", text: "QWxhZGRpbjpvcGVuIHNlc2FtZV9jcmVkZW50aWFsXzEyMzQ1Njc4OTA=", marker: "QWxhZGRp"},
		{name: "unknown high entropy token", field: "summary", text: "q9W_7R2pN8vK4xP6cD1sF5hJ0lZ3yB7uE9iO2aG6mT8rQ4wX", marker: "q9W_7R2p"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
			fixture.repo.putRun(agentcontrol.Run{
				ID: "run-1", TenantID: "merchant-a", OperatorID: "creator-a", TraceID: strings.Repeat("1", 32), Status: agentcontrol.StatusCreated, Version: 1,
			})
			fixture.runtime.getRun = agentcontrol.RuntimeRun{
				RunID: "run-1", Status: agentcontrol.StatusCompleted,
				ProposalJSON: proposalWithFreeText(t, validAgentProposalJSON, test.field, test.text),
			}

			response := fixture.request(http.MethodGet, "/api/admin/agent/runs/run-1", "")
			envelope := decodeAgentEnvelope(t, response.Body.Bytes())
			if len(envelope.Run.Proposal) != 0 {
				t.Fatalf("credential-bearing %s reached browser proposal: %#v", test.field, envelope.Run.Proposal)
			}
			if strings.Contains(response.Body.String(), test.marker) {
				t.Fatalf("credential marker %q was echoed: %s", test.marker, response.Body.String())
			}
		})
	}
}

func TestAgentHTTPGetPreservesBenignChineseAndEnglishProposalText(t *testing.T) {
	proposal := validAgentProposalJSON
	benign := []struct{ field, text string }{
		{field: "summary", text: "秘书处（secretariat）将按库存安排拍卖，tokenization 只是商品描述。"},
		{field: "lot_role", text: "secretariat liaison"},
		{field: "lot_reason", text: "令牌化主题商品适合作为开场拍品。"},
		{field: "evidence_source", text: "inventory.tokenization.query"},
		{field: "evidence_detail", text: "The secretariat verified stock; tokenization is product terminology."},
		{field: "risk", text: "客户可能不熟悉令牌化术语；token budget and access token rotation are business topics, and secretariat staffing is limited."},
	}
	for _, item := range benign {
		proposal = proposalWithFreeText(t, proposal, item.field, item.text)
	}
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	fixture.repo.putRun(agentcontrol.Run{
		ID: "run-1", TenantID: "merchant-a", OperatorID: "creator-a", TraceID: strings.Repeat("1", 32), Status: agentcontrol.StatusCreated, Version: 1,
	})
	fixture.runtime.getRun = agentcontrol.RuntimeRun{RunID: "run-1", Status: agentcontrol.StatusCompleted, ProposalJSON: proposal}

	response := fixture.request(http.MethodGet, "/api/admin/agent/runs/run-1", "")
	envelope := decodeAgentEnvelope(t, response.Body.Bytes())
	if len(envelope.Run.Proposal) == 0 {
		t.Fatalf("benign business proposal was rejected: %s", response.Body.String())
	}
	for _, item := range benign {
		if !strings.Contains(response.Body.String(), item.text) {
			t.Fatalf("benign %s text was changed or omitted: %s", item.field, response.Body.String())
		}
	}
}

func proposalWithFreeText(t *testing.T, proposal, field, value string) string {
	t.Helper()
	replacements := map[string]string{
		"summary":         "Secretariat tokenization keeps this plan balanced.",
		"lot_role":        "opening",
		"lot_reason":      "A secretariat-themed item supports tokenization education.",
		"evidence_source": "inventory.query",
		"evidence_detail": "Inventory confirms the merchant-owned lot.",
		"risk":            "Tokenization terminology may need explanation.",
	}
	old, ok := replacements[field]
	if !ok {
		t.Fatalf("unknown proposal free-text field %q", field)
	}
	oldJSON, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	valueJSON, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(proposal, string(oldJSON), string(valueJSON), 1)
	if updated == proposal {
		t.Fatalf("proposal field %q fixture was not replaced", field)
	}
	return updated
}

func TestAgentHTTPGetUsesClosedBrowserErrorCodeVocabulary(t *testing.T) {
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	fixture.repo.putRun(agentcontrol.Run{
		ID: "run-1", TenantID: "merchant-a", OperatorID: "creator-a", TraceID: strings.Repeat("1", 32), Status: agentcontrol.StatusCreated, Version: 1,
	})
	fixture.runtime.getRun = agentcontrol.RuntimeRun{
		RunID: "run-1", Status: agentcontrol.StatusFailed,
		ErrorCode: "PROVIDER_ACCOUNT_QUOTA_DEPLETED", ErrorMessage: "provider account accessKey=AKIA-EXAMPLE",
	}

	response := fixture.request(http.MethodGet, "/api/admin/agent/runs/run-1", "")
	envelope := decodeAgentEnvelope(t, response.Body.Bytes())
	if envelope.Run.ErrorCode != "AGENT_RUNTIME_ERROR" || envelope.Run.ErrorMessage != "Agent run failed. Please try again." {
		t.Fatalf("browser error projection = %+v", envelope.Run)
	}
	if strings.Contains(response.Body.String(), "PROVIDER_ACCOUNT") || strings.Contains(response.Body.String(), "AKIA-EXAMPLE") {
		t.Fatalf("browser error leaked provider detail: %s", response.Body.String())
	}
}

func TestSafeBrowserRunErrorExplainsActionablePlanningFailures(t *testing.T) {
	tests := []struct {
		code    string
		message string
	}{
		{code: "NO_ELIGIBLE_INVENTORY", message: "No eligible inventory lots are available."},
		{code: "UNGROUNDED_PROPOSAL", message: "The model proposal was not grounded in current inventory."},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			code, message := safeBrowserRunError(agentcontrol.Run{
				Status: agentcontrol.StatusFailed, ErrorCode: test.code, ErrorMessage: "provider secret detail",
			})
			if code != test.code || message != test.message {
				t.Fatalf("browser error = (%q, %q)", code, message)
			}
		})
	}
}

func TestAgentHTTPUnknownAndCrossTenantRunsDoNotLeakOrCallRuntime(t *testing.T) {
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	fixture.repo.putRun(agentcontrol.Run{ID: "run-other", TenantID: "merchant-b", Status: agentcontrol.StatusCreated, Version: 1})

	for _, path := range []string{
		"/api/admin/agent/runs/run-missing",
		"/api/admin/agent/runs/run-other",
		"/api/admin/agent/runs/run-missing/events?after_seq=0",
		"/api/admin/agent/runs/run-other/events?after_seq=0",
		"/api/admin/agent/runs/run-missing/events?after_seq=invalid",
		"/api/admin/agent/runs/run-other/events?after_seq=invalid",
	} {
		response := fixture.request(http.MethodGet, path, "")
		envelope := decodeAgentEnvelope(t, response.Body.Bytes())
		if envelope.Result.Code != appsvc.ResultCodePermissionDenied || envelope.Result.Message != "permission denied" {
			t.Fatalf("GET %s envelope = %+v; body=%s", path, envelope, response.Body.String())
		}
		if got := response.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("GET %s content type = %q, authorization happened after SSE headers", path, got)
		}
	}
	if fixture.runtime.getCount() != 0 || fixture.runtime.streamCount() != 0 {
		t.Fatalf("runtime called for concealed runs: get=%d stream=%d", fixture.runtime.getCount(), fixture.runtime.streamCount())
	}
}

func TestAgentHTTPSSEReplaysExactFramesWithoutRuntimePayloadValues(t *testing.T) {
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	fixture.repo.putRun(agentcontrol.Run{
		ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a", TraceID: strings.Repeat("1", 32), Status: agentcontrol.StatusCompleted, Version: 1, ProjectionComplete: true,
	})
	fixture.repo.putEvents("run-1", []agentcontrol.Event{{
		RunID: "run-1", Seq: 12, RuntimeSourceSeq: 7, EventType: "tool.completed",
		Message:     "dial tcp 10.0.0.8:19081: connection refused",
		PayloadJSON: `{"tool":"list_lots","summary":"Bearer provider-secret","tenantId":"merchant-b","OperatorID":"attacker","traceId":"trace-secret","accessKey":"AKIA-EXAMPLE","api-key":"api-secret","Ａｕｔｈｏｒｉｚａｔｉｏｎ":"Bearer unicode-secret","risk":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyIn0.c2lnbmF0dXJl"}`,
	}})

	response := fixture.request(http.MethodGet, "/api/admin/agent/runs/run-1/events?after_seq=11", "")
	if got := response.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("SSE content type = %q", got)
	}
	want := "id: 12\nevent: agent.run.event\ndata: {\"runId\":\"run-1\",\"seq\":12,\"type\":\"tool.completed\",\"message\":\"Tool completed.\",\"payload\":{}}\n\n"
	if got := response.Body.String(); got != want {
		t.Fatalf("SSE frame = %q, want %q", got, want)
	}
	if strings.Contains(response.Body.String(), "provider-secret") || strings.Contains(response.Body.String(), "merchant-b") || strings.Contains(response.Body.String(), "attacker") || strings.Contains(response.Body.String(), "AKIA-EXAMPLE") || strings.Contains(response.Body.String(), "eyJhbGci") {
		t.Fatalf("SSE leaked sensitive content: %s", response.Body.String())
	}
	if fixture.runtime.streamCount() != 0 {
		t.Fatal("browser SSE opened a Python runtime subscriber")
	}
}

func TestAgentHTTPGetTerminalSnapshotRestartThenSSEReplaysDurableFrames(t *testing.T) {
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	fixture.repo.putRun(agentcontrol.Run{
		ID: "run-terminal-restart", TenantID: "merchant-a", OperatorID: "user-a", TraceID: strings.Repeat("1", 32),
		Goal: "清理库存", Status: agentcontrol.StatusCreated, Version: 1,
	})
	fixture.runtime.getRun = agentcontrol.RuntimeRun{
		RunID: "run-terminal-restart", Status: agentcontrol.StatusCompleted,
		ProposalJSON: validAgentProposalJSON, UpdatedAtUnixMs: 303,
	}

	firstService, err := agentcontrol.NewService(fixture.repo, fixture.runtime)
	if err != nil {
		t.Fatal(err)
	}
	fixture.handler = NewAgentHTTPHandler(firstService, fixture.auth, AgentHTTPOptions{})
	snapshot := fixture.request(http.MethodGet, "/api/admin/agent/runs/run-terminal-restart", "")
	if envelope := decodeAgentEnvelope(t, snapshot.Body.Bytes()); envelope.Run.Status != string(agentcontrol.StatusCompleted) {
		t.Fatalf("terminal GET snapshot = %+v", envelope.Run)
	}
	persisted, err := fixture.repo.Find(context.Background(), "run-terminal-restart")
	if err != nil || persisted.ProjectionComplete {
		t.Fatalf("pre-restart projection = %+v, %v", persisted, err)
	}
	firstService.Close()

	fixture.repo.projectableRuns = true
	fixture.runtime.streamFactory = func(ctx context.Context) agentcontrol.EventStream {
		return &agentHTTPEventStream{ctx: ctx, events: []agentcontrol.RuntimeEvent{
			{RunID: "run-terminal-restart", Seq: 1, Type: "run.created", Status: agentcontrol.StatusCreated, PayloadJSON: `{}`, CreatedAtUnixMs: 301},
			{RunID: "run-terminal-restart", Seq: 2, Type: "tool.completed", Status: agentcontrol.StatusCollectingContext, PayloadJSON: `{}`, CreatedAtUnixMs: 302},
			{RunID: "run-terminal-restart", Seq: 3, Type: "run.completed", Status: agentcontrol.StatusCompleted, PayloadJSON: `{}`, CreatedAtUnixMs: 303},
		}}
	}
	restarted, err := agentcontrol.NewService(fixture.repo, fixture.runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatalf("restarted Start() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		persisted, err = fixture.repo.Find(context.Background(), "run-terminal-restart")
		if err == nil && persisted.ProjectionComplete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal projection did not complete: %+v, %v", persisted, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	fixture.handler = NewAgentHTTPHandler(restarted, fixture.auth, AgentHTTPOptions{})

	replay := fixture.request(http.MethodGet, "/api/admin/agent/runs/run-terminal-restart/events?after_seq=0", "")
	wantFrames := []string{
		"id: 1\nevent: agent.run.event\n",
		`"type":"run.created"`,
		"id: 2\nevent: agent.run.event\n",
		`"type":"tool.completed"`,
		"id: 3\nevent: agent.run.event\n",
		`"type":"run.completed"`,
	}
	for _, want := range wantFrames {
		if !strings.Contains(replay.Body.String(), want) {
			t.Fatalf("durable SSE replay missing %q: %q", want, replay.Body.String())
		}
	}
}

func TestAgentHTTPSSEServesLocalUnavailableFailureWithoutPython(t *testing.T) {
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	service, err := agentcontrol.NewService(fixture.repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture.handler = NewAgentHTTPHandler(service, fixture.auth, AgentHTTPOptions{})
	created := fixture.request(http.MethodPost, "/api/admin/agent/runs", `{"goal":"清理库存"}`)
	createEnvelope := decodeAgentEnvelope(t, created.Body.Bytes())
	if createEnvelope.Run.Status != string(agentcontrol.StatusFailed) || createEnvelope.Run.ErrorCode != agentcontrol.ErrorCodeRuntimeUnavailable {
		t.Fatalf("create unavailable envelope = %+v", createEnvelope)
	}
	if strings.Contains(created.Body.String(), "10.0.0.8") || strings.Contains(created.Body.String(), "internal-secret") {
		t.Fatalf("create unavailable leaked provider details: %s", created.Body.String())
	}

	events := fixture.request(http.MethodGet, "/api/admin/agent/runs/"+createEnvelope.Run.RunID+"/events?after_seq=0", "")
	if !strings.Contains(events.Body.String(), "id: 1\nevent: agent.run.event\n") || !strings.Contains(events.Body.String(), agentcontrol.ErrorCodeRuntimeUnavailable) {
		t.Fatalf("local terminal SSE = %q", events.Body.String())
	}
	if fixture.runtime.streamCount() != 0 {
		t.Fatal("Python stream called for local runtime-unavailable failure")
	}

	replayed := fixture.request(http.MethodGet, "/api/admin/agent/runs/"+createEnvelope.Run.RunID+"/events?after_seq=1", "")
	if replayed.Body.Len() != 0 {
		t.Fatalf("after_seq=1 replayed duplicate local event: %q", replayed.Body.String())
	}
}

func TestAgentHTTPSSEHeartbeatAndBrowserDisconnectCancelRuntimeStream(t *testing.T) {
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	fixture.heartbeat = 10 * time.Millisecond
	fixture.rebuildHandler(t)
	fixture.repo.putRun(agentcontrol.Run{
		ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a", TraceID: strings.Repeat("1", 32), Status: agentcontrol.StatusPlanning, Version: 1,
	})
	httpServer := httptest.NewServer(fixture.handler)
	t.Cleanup(httpServer.Close)
	requestCtx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, httpServer.URL+"/api/admin/agent/runs/run-1/events?after_seq=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+fixture.token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("SSE request error = %v", err)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read heartbeat error = %v", err)
	}
	if line != ": heartbeat\n" {
		t.Fatalf("heartbeat line = %q", line)
	}
	blank, err := reader.ReadString('\n')
	if err != nil || blank != "\n" {
		t.Fatalf("heartbeat terminator = %q, error=%v", blank, err)
	}
	cancel()
	_ = response.Body.Close()
	if fixture.runtime.streamCount() != 0 {
		t.Fatal("browser heartbeat stream opened Python runtime")
	}
}

func TestAgentHTTPSSEStreamFailureAfterHeadersIsSafeSSEError(t *testing.T) {
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	fixture.repo.putRun(agentcontrol.Run{
		ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a", TraceID: strings.Repeat("1", 32), Status: agentcontrol.StatusPlanning, Version: 1,
	})
	fixture.repo.listErr = errors.New("mysql 10.0.0.8 Bearer provider-secret")

	response := fixture.request(http.MethodGet, "/api/admin/agent/runs/run-1/events?after_seq=0", "")
	if !strings.Contains(response.Body.String(), "event: agent.run.error\n") || strings.Contains(response.Body.String(), `"result"`) {
		t.Fatalf("post-header failure was not a safe SSE error: %q", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "10.0.0.8") || strings.Contains(response.Body.String(), "provider-secret") {
		t.Fatalf("SSE error leaked runtime details: %q", response.Body.String())
	}
}

func TestAgentHTTPSSEErrorFramesClassifyRetriableFailures(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retriable bool
	}{
		{name: "unavailable", err: status.Error(codes.Unavailable, "secret"), retriable: true},
		{name: "deadline", err: status.Error(codes.DeadlineExceeded, "secret"), retriable: true},
		{name: "resource exhausted", err: status.Error(codes.ResourceExhausted, "secret"), retriable: true},
		{name: "permission", err: status.Error(codes.PermissionDenied, "secret"), retriable: false},
		{name: "not found", err: status.Error(codes.NotFound, "secret"), retriable: false},
		{name: "invalid", err: status.Error(codes.InvalidArgument, "secret"), retriable: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeSafeSSEError(response, response, test.err)
			frame := response.Body.String()
			if !strings.Contains(frame, fmt.Sprintf(`"retriable":%t`, test.retriable)) || !strings.Contains(frame, `"code":"`) || !strings.Contains(frame, `"message":"`) {
				t.Fatalf("SSE error frame = %q", frame)
			}
			if strings.Contains(frame, "secret") {
				t.Fatalf("SSE error leaked cause: %q", frame)
			}
		})
	}
}

func TestAgentHTTPSSERejectsNonContiguousSequence(t *testing.T) {
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	fixture.repo.putRun(agentcontrol.Run{
		ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a", TraceID: strings.Repeat("1", 32), Status: agentcontrol.StatusCompleted, Version: 1, ProjectionComplete: true,
	})
	fixture.repo.putEvents("run-1", []agentcontrol.Event{{RunID: "run-1", Seq: 2, EventType: "run.planning"}})

	response := fixture.request(http.MethodGet, "/api/admin/agent/runs/run-1/events?after_seq=0", "")
	if strings.Contains(response.Body.String(), "id: 2\n") || !strings.Contains(response.Body.String(), "event: agent.run.error\n") {
		t.Fatalf("non-contiguous event was not rejected safely: %q", response.Body.String())
	}
}

func TestAgentHTTPValidatesAfterSeqAndExactMethods(t *testing.T) {
	fixture := newAgentHTTPFixture(t, authorizedAgentHTTPUser())
	fixture.repo.putRun(agentcontrol.Run{
		ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a", TraceID: strings.Repeat("1", 32), Status: agentcontrol.StatusPlanning, Version: 1,
	})
	for _, query := range []string{"-1", "abc", "1&after_seq=2"} {
		response := fixture.request(http.MethodGet, "/api/admin/agent/runs/run-1/events?after_seq="+query, "")
		envelope := decodeAgentEnvelope(t, response.Body.Bytes())
		if envelope.Result.Code != appsvc.ResultCodeInvalidArgument {
			t.Fatalf("after_seq=%q result = %+v", query, envelope.Result)
		}
	}
	if fixture.runtime.streamCount() != 0 {
		t.Fatal("runtime stream called for invalid after_seq")
	}
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/api/admin/agent/runs"},
		{http.MethodPost, "/api/admin/agent/runs/run-1"},
		{http.MethodPost, "/api/admin/agent/runs/run-1/events"},
	} {
		response := fixture.request(request.method, request.path, "")
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s status = %d, want 405", request.method, request.path, response.Code)
		}
	}
}

type agentHTTPFixture struct {
	repo      *agentHTTPRepository
	runtime   *agentHTTPRuntime
	auth      *auth.Manager
	handler   http.Handler
	token     string
	heartbeat time.Duration
}

type agentHTTPUser struct {
	ID            string
	MainAccountID string
	Permissions   []string
}

func authorizedAgentHTTPUser() agentHTTPUser {
	return agentHTTPUser{ID: "user-a", MainAccountID: "merchant-a", Permissions: []string{userbiz.PermissionLotViewAdmin}}
}

func newAgentHTTPFixture(t *testing.T, user agentHTTPUser) *agentHTTPFixture {
	t.Helper()
	manager, err := auth.NewManager(auth.Config{Secret: "test-jwt-secret", Issuer: "test", AccessTTL: time.Hour, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &agentHTTPFixture{
		repo: newAgentHTTPRepository(), runtime: &agentHTTPRuntime{}, auth: manager, heartbeat: 15 * time.Second,
	}
	if user.ID != "" {
		pair, err := manager.IssueTokenPair(&auctionv1.User{
			Id: user.ID, MainAccountId: user.MainAccountID, RoleCodes: []string{userbiz.RoleOperator},
			PermissionCodes: user.Permissions, Status: auctionv1.UserStatus_USER_STATUS_ACTIVE,
		})
		if err != nil {
			t.Fatal(err)
		}
		fixture.token = pair.AccessToken
	}
	fixture.rebuildHandler(t)
	return fixture
}

func (f *agentHTTPFixture) rebuildHandler(t *testing.T) {
	t.Helper()
	service, err := agentcontrol.NewService(f.repo, f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	f.handler = NewAgentHTTPHandler(service, f.auth, AgentHTTPOptions{HeartbeatInterval: f.heartbeat})
}

func (f *agentHTTPFixture) request(method, path, body string) *httptest.ResponseRecorder {
	return f.requestWithKey(method, path, body, "test-idempotency-key")
}

func (f *agentHTTPFixture) requestWithKey(method, path, body, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if f.token != "" {
		request.Header.Set("Authorization", "Bearer "+f.token)
	}
	if method == http.MethodPost && path == agentRunsPath && key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

type agentEnvelope struct {
	Result struct {
		Code    int32  `json:"code"`
		Message string `json:"message"`
	} `json:"result"`
	Run struct {
		RunID        string         `json:"runId"`
		TenantID     string         `json:"tenantId"`
		OperatorID   string         `json:"operatorId"`
		TraceID      string         `json:"traceId"`
		Status       string         `json:"status"`
		Proposal     map[string]any `json:"proposal"`
		ErrorCode    string         `json:"errorCode"`
		ErrorMessage string         `json:"errorMessage"`
	} `json:"run"`
}

func decodeAgentEnvelope(t *testing.T, body []byte) agentEnvelope {
	t.Helper()
	var envelope agentEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode envelope %q: %v", body, err)
	}
	return envelope
}

type agentHTTPRepository struct {
	mu              sync.Mutex
	runs            map[string]agentcontrol.Run
	events          map[string][]agentcontrol.Event
	listErr         error
	projectableRuns bool
}

func newAgentHTTPRepository() *agentHTTPRepository {
	return &agentHTTPRepository{runs: map[string]agentcontrol.Run{}, events: map[string][]agentcontrol.Event{}}
}

func (r *agentHTTPRepository) putRun(run agentcontrol.Run) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[run.ID] = run
}

func (r *agentHTTPRepository) putEvents(runID string, events []agentcontrol.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events[runID] = append([]agentcontrol.Event(nil), events...)
}

func (r *agentHTTPRepository) Create(_ context.Context, run agentcontrol.Run) error {
	r.putRun(run)
	return nil
}

func (r *agentHTTPRepository) CreateIdempotent(_ context.Context, run agentcontrol.Run) (agentcontrol.Run, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.runs {
		if existing.TenantID == run.TenantID && existing.OperatorID == run.OperatorID && existing.IdempotencyKey == run.IdempotencyKey {
			if existing.Goal != run.Goal {
				return agentcontrol.Run{}, false, apperr.ErrConflict
			}
			return existing, false, nil
		}
	}
	r.runs[run.ID] = run
	return run, true, nil
}

func (r *agentHTTPRepository) Find(_ context.Context, runID string) (agentcontrol.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[runID]
	if !ok {
		return agentcontrol.Run{}, apperr.ErrNotFound
	}
	return run, nil
}

func (r *agentHTTPRepository) ListProjectable(_ context.Context, limit int) ([]agentcontrol.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.projectableRuns {
		return nil, nil
	}
	projectable := make([]agentcontrol.Run, 0, len(r.runs))
	for _, run := range r.runs {
		if run.ProjectionComplete {
			continue
		}
		projectable = append(projectable, run)
		if limit > 0 && len(projectable) == limit {
			break
		}
	}
	return projectable, nil
}

func (r *agentHTTPRepository) ProjectRuntimeSnapshot(_ context.Context, runID string, runtime agentcontrol.RuntimeRun) (agentcontrol.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[runID]
	if !ok {
		return agentcontrol.Run{}, apperr.ErrNotFound
	}
	run = agentcontrol.MergeRuntimeProjection(run, runtime)
	r.runs[runID] = run
	return run, nil
}

func (r *agentHTTPRepository) ProjectRuntimeEvent(_ context.Context, event agentcontrol.RuntimeEvent, terminal *agentcontrol.RuntimeRun) (agentcontrol.Run, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[event.RunID]
	if !ok {
		return agentcontrol.Run{}, false, apperr.ErrNotFound
	}
	if event.Seq <= run.LastRuntimeSourceSeq {
		return run, false, nil
	}
	run.Status, run.LastRuntimeSourceSeq = event.Status, event.Seq
	if terminal != nil {
		run = agentcontrol.MergeRuntimeProjection(run, *terminal)
		run.ProjectionComplete = true
	}
	r.events[event.RunID] = append(r.events[event.RunID], agentcontrol.Event{RunID: event.RunID, Seq: int64(len(r.events[event.RunID]) + 1), RuntimeSourceSeq: event.Seq, EventType: event.Type, Message: event.Message, PayloadJSON: event.PayloadJSON})
	r.runs[event.RunID] = run
	return run, true, nil
}

func (r *agentHTTPRepository) UpdateStatus(_ context.Context, runID string, expectedVersion int64, status agentcontrol.RunStatus, proposalJSON, errorCode, errorMessage string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[runID]
	if !ok {
		return apperr.ErrNotFound
	}
	if run.Version != expectedVersion {
		return apperr.ErrConflict
	}
	run.Status, run.ProposalJSON, run.ErrorCode, run.ErrorMessage = status, proposalJSON, errorCode, errorMessage
	run.Version++
	r.runs[runID] = run
	return nil
}

func (r *agentHTTPRepository) AppendEvent(_ context.Context, event agentcontrol.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events[event.RunID] = append(r.events[event.RunID], event)
	return nil
}

func (r *agentHTTPRepository) ListEvents(_ context.Context, runID string, afterSeq int64, limit int) ([]agentcontrol.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	var events []agentcontrol.Event
	for _, event := range r.events[runID] {
		if event.Seq <= afterSeq {
			continue
		}
		events = append(events, event)
		if limit > 0 && len(events) == limit {
			break
		}
	}
	return events, nil
}

func (r *agentHTTPRepository) FailRuntimeStart(_ context.Context, runID string, expectedVersion int64, errorCode, errorMessage string, event agentcontrol.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[runID]
	if !ok {
		return apperr.ErrNotFound
	}
	if run.Version != expectedVersion {
		return apperr.ErrConflict
	}
	run.Status, run.ErrorCode, run.ErrorMessage = agentcontrol.StatusFailed, errorCode, errorMessage
	run.ProjectionComplete = true
	run.Version++
	r.runs[runID] = run
	r.events[runID] = append(r.events[runID], event)
	return nil
}

func (r *agentHTTPRepository) CompleteProjection(_ context.Context, runID string, expectedVersion int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.runs[runID]
	if run.Version != expectedVersion {
		return apperr.ErrConflict
	}
	run.ProjectionComplete = true
	run.Version++
	r.runs[runID] = run
	return nil
}

type agentHTTPRuntime struct {
	mu            sync.Mutex
	starts        []agentcontrol.RuntimeStartRequest
	gets          []agentcontrol.RuntimeGetRequest
	streams       []agentcontrol.RuntimeStreamRequest
	startErr      error
	getRun        agentcontrol.RuntimeRun
	getErr        error
	streamFactory func(context.Context) agentcontrol.EventStream
	streamErr     error
}

func (r *agentHTTPRuntime) StartRun(_ context.Context, request agentcontrol.RuntimeStartRequest) (agentcontrol.RuntimeRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, request)
	return agentcontrol.RuntimeRun{RunID: request.RunID, Goal: request.Goal, Status: agentcontrol.StatusCreated}, r.startErr
}

func (r *agentHTTPRuntime) GetRun(_ context.Context, request agentcontrol.RuntimeGetRequest) (agentcontrol.RuntimeRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gets = append(r.gets, request)
	return r.getRun, r.getErr
}

func (r *agentHTTPRuntime) StreamRunEvents(ctx context.Context, request agentcontrol.RuntimeStreamRequest) (agentcontrol.EventStream, error) {
	r.mu.Lock()
	r.streams = append(r.streams, request)
	factory, err := r.streamFactory, r.streamErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if factory == nil {
		return &agentHTTPEventStream{ctx: ctx}, nil
	}
	return factory(ctx), nil
}

func (*agentHTTPRuntime) Close() error { return nil }

func (r *agentHTTPRuntime) startCount() int  { r.mu.Lock(); defer r.mu.Unlock(); return len(r.starts) }
func (r *agentHTTPRuntime) getCount() int    { r.mu.Lock(); defer r.mu.Unlock(); return len(r.gets) }
func (r *agentHTTPRuntime) streamCount() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.streams) }
func (r *agentHTTPRuntime) lastStart() agentcontrol.RuntimeStartRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts[len(r.starts)-1]
}
func (r *agentHTTPRuntime) lastGet() agentcontrol.RuntimeGetRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gets[len(r.gets)-1]
}
func (r *agentHTTPRuntime) lastStream() agentcontrol.RuntimeStreamRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streams[len(r.streams)-1]
}

type agentHTTPEventStream struct {
	ctx      context.Context
	events   []agentcontrol.RuntimeEvent
	index    int
	finalErr error
	closed   chan struct{}
	block    bool
	once     sync.Once
}

func (s *agentHTTPEventStream) Recv() (agentcontrol.RuntimeEvent, error) {
	if s.index < len(s.events) {
		event := s.events[s.index]
		s.index++
		return event, nil
	}
	if s.finalErr != nil {
		return agentcontrol.RuntimeEvent{}, s.finalErr
	}
	if s.block {
		<-s.ctx.Done()
		return agentcontrol.RuntimeEvent{}, s.ctx.Err()
	}
	return agentcontrol.RuntimeEvent{}, io.EOF
}

func (s *agentHTTPEventStream) Close() error {
	if s.closed != nil {
		s.once.Do(func() { close(s.closed) })
	}
	return nil
}
