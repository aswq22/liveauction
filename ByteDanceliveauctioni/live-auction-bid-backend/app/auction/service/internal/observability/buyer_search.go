package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"time"
)

var buyerSearchLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "auction_buyer_search_duration_seconds", Help: "Buyer search latency by bounded stage and outcome.", Buckets: []float64{.01, .025, .05, .1, .2, .45, .8, 1.5, 3}}, []string{"stage", "outcome"})
var buyerSearchRequests = promauto.NewCounterVec(prometheus.CounterOpts{Name: "auction_buyer_search_requests_total", Help: "Buyer search outcomes; no user or query labels."}, []string{"outcome"})

type BuyerSearchObserver struct{}

func (BuyerSearchObserver) Observe(stage, outcome string, d time.Duration) {
	switch stage {
	case "total", "embedding", "facts", "index":
	default:
		stage = "other"
	}
	switch outcome {
	case "HYBRID", "LEXICAL", "error", "invalid", "capacity", "ok":
	default:
		outcome = "error"
	}
	buyerSearchLatency.WithLabelValues(stage, outcome).Observe(d.Seconds())
	if stage == "total" {
		buyerSearchRequests.WithLabelValues(outcome).Inc()
	}
}
