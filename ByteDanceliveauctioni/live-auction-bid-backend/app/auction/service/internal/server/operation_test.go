package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	httptransport "github.com/go-kratos/kratos/v2/transport/http"
	"live-auction-bid/backend/app/auction/service/internal/biz/auction"
)

type unsafeWorkerStatusProvider struct{}

func (unsafeWorkerStatusProvider) WorkerStatus(context.Context) auction.WorkerStatus {
	return auction.WorkerStatus{
		Name:      "approval",
		Started:   true,
		LastError: "redis://user:sk-secret@provider.internal",
		Shards: []auction.WorkerShardState{{
			ShardID:   1,
			LastError: "endpoint provider password=secret",
		}},
	}
}

func TestWorkerzRedactsRawWorkerErrors(t *testing.T) {
	transport := httptransport.NewServer()
	registerOperationHTTP(transport, Readiness{WorkerStatuses: []auction.WorkerStatusProvider{unsafeWorkerStatusProvider{}}})
	response := httptest.NewRecorder()
	transport.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/workerz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, forbidden := range []string{"redis", "sk-secret", "provider.internal", "password=secret"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(forbidden)) {
			t.Fatalf("worker status leaked %q: %s", forbidden, body)
		}
	}
	if strings.Count(body, "WORKER_DEGRADED") != 2 {
		t.Fatalf("stable errors missing: %s", body)
	}
}
