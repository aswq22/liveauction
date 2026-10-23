package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestWSConnectionMetricKeepsOnlyBoundedScopeSeries(t *testing.T) {
	baseline := wsConnectionMetricSnapshot(t)

	for _, sample := range []struct {
		scope string
	}{
		{scope: "public"},
		{scope: "public"},
		{scope: "admin"},
		{scope: "unexpected-scope"},
	} {
		IncWSConnection(sample.scope)
	}
	assertWSConnectionMetricDelta(t, baseline, map[string]float64{
		"public": 2,
		"admin":  1,
		"other":  1,
	})

	DecWSConnection("public")
	DecWSConnection("public")
	DecWSConnection("admin")
	DecWSConnection("unexpected-scope")
	assertWSConnectionMetricRestored(t, baseline)
}

func assertWSConnectionMetricDelta(t *testing.T, baseline, delta map[string]float64) {
	t.Helper()
	family := gatheredMetricFamily(t, "auction_ws_connections")
	if family == nil {
		t.Fatal("auction_ws_connections was not gathered")
	}
	assertBoundedWSConnectionLabels(t, family)
	values := wsConnectionMetricValues(family)
	for scope, wantDelta := range delta {
		if got := values[scope] - baseline[scope]; got != wantDelta {
			t.Errorf("websocket scope %q delta = %v, want %v", scope, got, wantDelta)
		}
	}
}

func assertWSConnectionMetricRestored(t *testing.T, baseline map[string]float64) {
	t.Helper()
	family := gatheredMetricFamily(t, "auction_ws_connections")
	if family == nil {
		t.Fatal("auction_ws_connections was not gathered")
	}
	assertBoundedWSConnectionLabels(t, family)
	values := wsConnectionMetricValues(family)
	for _, scope := range []string{"public", "admin", "other"} {
		if got := values[scope]; got != baseline[scope] {
			t.Errorf("websocket scope %q gauge = %v, want restored baseline %v", scope, got, baseline[scope])
		}
	}
}

func assertBoundedWSConnectionLabels(t *testing.T, family *dto.MetricFamily) {
	t.Helper()
	allowedScopes := map[string]struct{}{"public": {}, "admin": {}, "other": {}}
	for _, metric := range family.GetMetric() {
		labels := metric.GetLabel()
		if len(labels) != 1 || labels[0].GetName() != "scope" {
			t.Errorf("websocket labels = %+v, want only scope", labels)
			continue
		}
		scope := labels[0].GetValue()
		if _, ok := allowedScopes[scope]; !ok {
			t.Errorf("unbounded websocket scope %q was gathered", scope)
		}
	}
}

func wsConnectionMetricSnapshot(t *testing.T) map[string]float64 {
	t.Helper()
	family := gatheredMetricFamily(t, "auction_ws_connections")
	if family == nil {
		return map[string]float64{}
	}
	assertBoundedWSConnectionLabels(t, family)
	return wsConnectionMetricValues(family)
}

func wsConnectionMetricValues(family *dto.MetricFamily) map[string]float64 {
	values := make(map[string]float64, len(family.GetMetric()))
	for _, metric := range family.GetMetric() {
		for _, label := range metric.GetLabel() {
			if label.GetName() == "scope" {
				values[label.GetValue()] += metric.GetGauge().GetValue()
			}
		}
	}
	return values
}

func gatheredMetricFamily(t *testing.T, name string) *dto.MetricFamily {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather default registry: %v", err)
	}
	for _, family := range families {
		if family.GetName() == name {
			return family
		}
	}
	return nil
}
