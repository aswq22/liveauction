package observability

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestBarrageMetricsRegisterStableFamiliesAndBoundedAdmissionLabels(t *testing.T) {
	for _, result := range []string{"accepted", "rate_limited", "overloaded", "arbitrary-room-user-value"} {
		RecordBarrageAdmission(result)
	}
	ObserveBarrageWindow(7, 4, 2, 3*time.Millisecond)
	RecordBarrageViewerDrop()
	RecordBarrageOverflowDrops("room_limit", 1)
	RecordBarrageOverflowDrops("candidate_limit", 2)
	RecordBarrageOverflowDrops("arbitrary-room-user-value", 3)

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather default registry: %v", err)
	}
	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, family := range families {
		byName[family.GetName()] = family
	}

	wantNames := []string{
		"auction_barrage_admission_total",
		"auction_barrage_window_input",
		"auction_barrage_window_unique",
		"auction_barrage_window_selected",
		"auction_barrage_window_duration_ms",
		"auction_barrage_viewer_dropped_total",
		"auction_barrage_overflow_dropped_total",
	}
	for _, name := range wantNames {
		family := byName[name]
		if family == nil {
			t.Errorf("metric family %q was not registered", name)
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "room_id" || label.GetName() == "user_id" {
					t.Errorf("metric family %q has unbounded label %q", name, label.GetName())
				}
			}
		}
	}

	overflow := byName["auction_barrage_overflow_dropped_total"]
	if overflow != nil {
		wantReasons := map[string]bool{"room_limit": false, "candidate_limit": false, "other": false}
		for _, metric := range overflow.GetMetric() {
			labels := metric.GetLabel()
			if len(labels) != 1 || labels[0].GetName() != "reason" {
				t.Fatalf("overflow labels = %+v, want only reason", labels)
			}
			reason := labels[0].GetValue()
			if reason == "arbitrary-room-user-value" {
				t.Fatal("arbitrary overflow reason created an unbounded series")
			}
			if _, ok := wantReasons[reason]; ok {
				wantReasons[reason] = true
			}
		}
		for reason, found := range wantReasons {
			if !found {
				t.Errorf("overflow reason label %q was not gathered", reason)
			}
		}
	}

	admission := byName["auction_barrage_admission_total"]
	if admission == nil {
		return
	}
	wantResults := map[string]bool{"accepted": false, "rate_limited": false, "overloaded": false, "other": false}
	for _, metric := range admission.GetMetric() {
		labels := metric.GetLabel()
		if len(labels) != 1 || labels[0].GetName() != "result" {
			t.Fatalf("admission labels = %+v, want only result", labels)
		}
		result := labels[0].GetValue()
		if result == "arbitrary-room-user-value" {
			t.Fatal("arbitrary admission result created an unbounded series")
		}
		if _, ok := wantResults[result]; ok {
			wantResults[result] = true
		}
	}
	for result, found := range wantResults {
		if !found {
			t.Errorf("admission result label %q was not gathered", result)
		}
	}
}
