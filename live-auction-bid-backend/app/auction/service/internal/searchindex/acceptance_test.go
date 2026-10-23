package searchindex

import (
	"testing"
	"time"
)

func TestAcceptanceRejectsUnverifiedMetrics(t *testing.T) {
	r := AcceptanceReport{GenerationID: "g", CreatedAt: time.Now(), HumanJudgedQueries: 200, PublicLots: 100000, Recall50: .91, NDCG10: .81, ANNRecall50: .96, SearchQPS: 100, P95Ms: 790, P99Ms: 1400, FreshnessP95Ms: 29000}
	if r.Validate(time.Now()) != nil {
		t.Fatal("valid report rejected")
	}
	r.LeakedLots = 1
	if r.Validate(time.Now()) == nil {
		t.Fatal("leakage allowed")
	}
	r.LeakedLots = 0
	r.HumanJudgedQueries = 199
	if r.Validate(time.Now()) == nil {
		t.Fatal("unreviewed test size allowed")
	}
}
