package searchindex

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"
)

type AcceptanceReport struct {
	GenerationID                                                           string    `json:"generationId"`
	CreatedAt                                                              time.Time `json:"createdAt"`
	HumanJudgedQueries, PublicLots, LeakedLots                             int
	Recall50, NDCG10, ANNRecall50, SearchQPS, P95Ms, P99Ms, FreshnessP95Ms float64
}

func (r AcceptanceReport) Validate(now time.Time) error {
	for _, x := range []float64{r.Recall50, r.NDCG10, r.ANNRecall50, r.SearchQPS, r.P95Ms, r.P99Ms, r.FreshnessP95Ms} {
		if math.IsNaN(x) || math.IsInf(x, 0) || x <= 0 {
			return errors.New("invalid acceptance measurement")
		}
	}
	if r.GenerationID == "" || r.CreatedAt.IsZero() || r.CreatedAt.After(now.Add(time.Minute)) || now.Sub(r.CreatedAt) > 24*time.Hour {
		return errors.New("acceptance report missing or stale")
	}
	if r.HumanJudgedQueries < 200 || r.PublicLots < 100000 || r.LeakedLots != 0 || r.Recall50 < .9 || r.Recall50 > 1 || r.NDCG10 < .8 || r.NDCG10 > 1 || r.ANNRecall50 < .95 || r.ANNRecall50 > 1 || r.SearchQPS < 100 || r.P95Ms > 800 || r.P99Ms > 1500 || r.FreshnessP95Ms > 30000 {
		return errors.New("buyer search acceptance gate not met")
	}
	return nil
}

// The report is supplied and reviewed by an operator; this method enforces
// numerical gates but cannot attest that human judgments actually occurred.
func (i *HybridIndex) ValidateGeneration(ctx context.Context, r AcceptanceReport) error {
	if e := r.Validate(time.Now()); e != nil {
		return e
	}
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	result, e := i.db.ExecContext(ctx, `UPDATE buyer_search_generations SET ready=TRUE,acceptance_report=$2::jsonb WHERE id=$1`, r.GenerationID, string(b))
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return errors.New("generation missing")
	}
	return nil
}
