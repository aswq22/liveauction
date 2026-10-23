package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"live-auction-bid/backend/app/auction/service/internal/agentconversation"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

func TestAgentCandidateImageHTTPSafelyMapsInvalidAsset(t *testing.T) {
	err := agentSessionHTTPError(apperr.ErrAgentImageAssetInvalid)
	if !errors.Is(err, apperr.ErrInvalidArgument) || errors.Is(err, apperr.ErrAgentImageAssetInvalid) {
		t.Fatalf("mapped error=%v, want generic invalid argument without asset sentinel", err)
	}
}

func TestAgentCandidateImageHTTPBindsWithBothPermissionsAndStrictBody(t *testing.T) {
	fixture := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionUploadImage})
	fixture.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: "session-image", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentconversation.StatusCollectingInput, Version: 4}, Candidates: []agentconversation.DraftCandidate{{Candidate: agentconversation.Candidate{ID: "candidate-image", SourceType: agentconversation.SourceNew}, Version: 2}}})
	response := fixture.request(http.MethodPost, agentSessionsPath+"/session-image/candidates/candidate-image/images", `{"expectedSessionVersion":4,"expectedCandidateVersion":2,"assetId":"asset-image"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("bind status=%d body=%s", response.Code, response.Body.String())
	}
	got := fixture.repo.snapshot("session-image")
	if got.Session.Version != 5 || got.Candidates[0].Version != 3 || got.Candidates[0].Candidate.Draft.MainImageAssetID != "asset-image" {
		t.Fatalf("bound aggregate=%#v", got)
	}

	invalid := fixture.request(http.MethodPost, agentSessionsPath+"/session-image/candidates/candidate-image/images", `{"expectedSessionVersion":5,"expectedCandidateVersion":3,"assetId":"asset-2","unknown":true}`)
	if resultCode(t, invalid) != 400001 {
		t.Fatalf("unknown-field status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	method := fixture.request(http.MethodGet, agentSessionsPath+"/session-image/candidates/candidate-image/images", "")
	if method.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status=%d body=%s", method.Code, method.Body.String())
	}
}

func TestAgentCandidateImageHTTPRequiresUploadPermissionAndDoesNotLeakTenant(t *testing.T) {
	withoutUpload := newAgentSessionHandlerFixture(t, "merchant-a", []string{userbiz.PermissionLotViewAdmin})
	withoutUpload.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: "session-image", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentconversation.StatusCollectingInput, Version: 1}, Candidates: []agentconversation.DraftCandidate{{Candidate: agentconversation.Candidate{ID: "candidate-image"}, Version: 1}}})
	denied := withoutUpload.request(http.MethodPost, agentSessionsPath+"/session-image/candidates/candidate-image/images", `{"expectedSessionVersion":1,"expectedCandidateVersion":1,"assetId":"asset-image"}`)
	if resultCode(t, denied) != 403001 {
		t.Fatalf("missing upload permission status=%d body=%s", denied.Code, denied.Body.String())
	}

	crossTenant := newAgentSessionHandlerFixture(t, "merchant-b", []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionUploadImage})
	crossTenant.repo.put(agentconversation.SessionAggregate{Session: agentconversation.Session{ID: "session-image", TenantID: "merchant-a", OperatorID: "operator-a", Status: agentconversation.StatusCollectingInput, Version: 1}, Candidates: []agentconversation.DraftCandidate{{Candidate: agentconversation.Candidate{ID: "candidate-image"}, Version: 1}}})
	response := crossTenant.request(http.MethodPost, agentSessionsPath+"/session-image/candidates/candidate-image/images", `{"expectedSessionVersion":1,"expectedCandidateVersion":1,"assetId":"asset-image"}`)
	if code := resultCode(t, response); code != 404001 && code != 403001 {
		t.Fatalf("cross-tenant status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "session-image") || strings.Contains(response.Body.String(), "candidate-image") || strings.Contains(response.Body.String(), "asset-image") {
		t.Fatalf("cross-tenant body leaked identifiers: %s", response.Body.String())
	}
}

func resultCode(t *testing.T, response *httptest.ResponseRecorder) int {
	t.Helper()
	var envelope struct {
		Result struct {
			Code int `json:"code"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode result: %v body=%s", err, response.Body.String())
	}
	return envelope.Result.Code
}

func (r *sessionHTTPRepo) snapshot(id string) agentconversation.SessionAggregate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneSessionHTTPAggregate(r.sessions[id])
}
