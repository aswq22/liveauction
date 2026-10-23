package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSearchPostgresOutageDoesNotBlockCoreStartup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	idx, closeIndex, e := openProductionSearchPostgres(ctx, "postgres://unused:unused@127.0.0.1:1/search_test?sslmode=disable", nil)
	if e != nil || idx == nil {
		t.Fatal("search outage blocks core startup", e)
	}
	defer closeIndex()
	if _, e = idx.Active(context.Background()); e == nil {
		t.Fatal("unavailable dependency reported healthy")
	}
}

func TestSearchComposeOptInAndCursorConfiguration(t *testing.T) {
	for _, p := range []string{"deploy/docker-compose.yml", "deploy/prod/docker-compose.yml"} {
		b, e := os.ReadFile(filepath.Join("../../../../..", p))
		if e != nil {
			t.Fatal(e)
		}
		for _, key := range []string{"AUCTION_BUYER_SEARCH_ENABLED:", "AUCTION_SEARCH_CURSOR_KEY:", "AUCTION_SEARCH_MAX_CONCURRENT:"} {
			if !strings.Contains(string(b), key) {
				t.Errorf("%s missing %s", p, key)
			}
		}
	}
}
