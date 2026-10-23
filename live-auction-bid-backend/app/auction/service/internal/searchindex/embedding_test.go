package searchindex

import (
	"context"
	"fmt"
	bs "live-auction-bid/backend/app/auction/service/internal/biz/buyersearch"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbeddingResponseValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		invalid    bool
	}{
		{"valid", `{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`, false},
		{"duplicate", `{"data":[{"index":0,"embedding":[1,0]},{"index":0,"embedding":[0,1]}]}`, true},
		{"zero", `{"data":[{"index":0,"embedding":[0,0]},{"index":1,"embedding":[0,1]}]}`, true},
		{"wrong_dimension", `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[0,1]}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			c := NewEmbeddingClient(EmbeddingConfig{BaseURL: srv.URL, APIKey: "test", Dimensions: 2})
			out, err := c.Embed(context.Background(), []string{"玉", "手镯"})
			if tc.invalid && err == nil {
				t.Fatal("accepted invalid vectors", out)
			}
			if !tc.invalid && (err != nil || out[0][0] != 1) {
				t.Fatal(out, err)
			}
		})
	}
}
func TestEmbeddingResolvesModelFromImmutableGeneration(t *testing.T) {
	client := NewEmbeddingClient(EmbeddingConfig{Model: "old", Dimensions: 2, ProviderVersion: "deployment-1", APIKey: "test"})
	e, err := client.ForGeneration(bs.Generation{Model: "new", Dimensions: 2, ProviderVersion: "deployment-1"})
	if err != nil {
		t.Fatal(err)
	}
	resolved := e.(*EmbeddingClient)
	if resolved.Model() != "new" || client.Model() != "old" {
		t.Fatal("model resolution mutated process default")
	}
	if _, err = client.ForGeneration(bs.Generation{Model: "new", Dimensions: 2, ProviderVersion: "unconfigured-deployment"}); err == nil {
		t.Fatal("unconfigured deployment accepted")
	}
}
func TestEmbeddingRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "3"); w.WriteHeader(429) }))
	defer srv.Close()
	c := NewEmbeddingClient(EmbeddingConfig{BaseURL: srv.URL, APIKey: "test", Dimensions: 2})
	_, err := c.Embed(context.Background(), []string{"玉"})
	p, ok := err.(*ProviderError)
	if !ok || p.Status != 429 || p.RetryAfter.Seconds() != 3 {
		t.Fatal(err)
	}
}
