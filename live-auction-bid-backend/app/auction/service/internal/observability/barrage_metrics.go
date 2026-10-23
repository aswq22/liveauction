package observability

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	barrageAdmission = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auction_barrage_admission_total",
		Help: "Total barrage admission attempts by stable result.",
	}, []string{"result"})
	barrageWindowInput = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "auction_barrage_window_input",
		Help:    "Number of barrage inputs consumed for each tracked room window; new-room overflow is counted only by the overflow drop counter.",
		Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10_000, 25_000, 50_000, 100_000},
	})
	barrageWindowUnique = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "auction_barrage_window_unique",
		Help:    "Number of unique barrage messages in each processed room window.",
		Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000},
	})
	barrageWindowSelected = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "auction_barrage_window_selected",
		Help:    "Number of barrage messages selected from each processed room window.",
		Buckets: []float64{1, 2, 3, 5, 10, 20, 30, 50, 100},
	})
	barrageWindowDurationMs = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "auction_barrage_window_duration_ms",
		Help:    "Barrage room-window processing duration in milliseconds.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 25, 50, 100, 250},
	})
	barrageViewerDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "auction_barrage_viewer_dropped_total",
		Help: "Total per-viewer barrage batches dropped because the viewer queue was full.",
	})
	barrageOverflowDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auction_barrage_overflow_dropped_total",
		Help: "Total barrage inputs dropped by bounded window-state overflow reason.",
	}, []string{"reason"})
)

func RecordBarrageAdmission(result string) {
	switch result {
	case "accepted", "invalid", "rate_limited", "muted", "sensitive", "overloaded":
	default:
		result = "other"
	}
	barrageAdmission.WithLabelValues(result).Inc()
}

func ObserveBarrageWindow(input, unique, selected int, duration time.Duration) {
	barrageWindowInput.Observe(float64(max(0, input)))
	barrageWindowUnique.Observe(float64(max(0, unique)))
	barrageWindowSelected.Observe(float64(max(0, selected)))
	if duration < 0 {
		duration = 0
	}
	barrageWindowDurationMs.Observe(float64(duration) / float64(time.Millisecond))
}

func RecordBarrageViewerDrop() {
	barrageViewerDropped.Inc()
}

func RecordBarrageOverflowDrops(reason string, count int) {
	if count <= 0 {
		return
	}
	switch reason {
	case "room_limit", "candidate_limit":
	default:
		reason = "other"
	}
	barrageOverflowDropped.WithLabelValues(reason).Add(float64(count))
}
