package realtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestWSConnectionMetricServeRoomBoundsAndBalancesScopeSeries(t *testing.T) {
	baseline := wsConnectionMetricSnapshot(t)
	hub := NewHub(nil)
	handlerDone := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { handlerDone <- struct{}{} }()
		hub.ServeRoom(w, r, r.URL.Query().Get("room"))
	}))
	t.Cleanup(server.Close)

	clients := make([]*websocket.Conn, 0, 2)
	for _, roomID := range []string{"callsite-room-a", "callsite-room-b"} {
		url := "ws" + strings.TrimPrefix(server.URL, "http") + "?scope=public&room=" + roomID
		client, response, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			if response != nil {
				_ = response.Body.Close()
			}
			t.Fatalf("dial room %q websocket: %v", roomID, err)
		}
		clients = append(clients, client)
	}
	defer func() {
		for _, client := range clients {
			_ = client.Close()
		}
	}()

	waitForWSConnectionGauge(t, "public", baseline["public"]+2)
	assertBoundedWSConnectionFamily(t)

	for _, client := range clients {
		_ = client.Close()
		_ = client.Close()
	}
	for range clients {
		select {
		case <-handlerDone:
		case <-time.After(time.Second):
			t.Fatal("ServeRoom did not finish after websocket close")
		}
	}
	waitForWSConnectionGauge(t, "public", baseline["public"])
	assertWSConnectionMetricRestored(t, baseline)
}

func waitForWSConnectionGauge(t *testing.T, scope string, want float64) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if wsConnectionScopeValue(t, scope) == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("websocket scope %q gauge did not reach %v", scope, want)
		case <-ticker.C:
		}
	}
}

func assertBoundedWSConnectionFamily(t *testing.T) {
	t.Helper()
	family := gatheredWSConnectionFamily(t)
	if family == nil {
		t.Fatal("auction_ws_connections was not gathered")
	}
	allowedScopes := map[string]struct{}{"public": {}, "admin": {}, "other": {}}
	for _, metric := range family.GetMetric() {
		labels := metric.GetLabel()
		if len(labels) != 1 || labels[0].GetName() != "scope" {
			t.Errorf("websocket labels = %+v, want only scope", labels)
			continue
		}
		if _, ok := allowedScopes[labels[0].GetValue()]; !ok {
			t.Errorf("unbounded websocket scope %q was gathered", labels[0].GetValue())
		}
	}
}

func assertWSConnectionMetricRestored(t *testing.T, baseline map[string]float64) {
	t.Helper()
	if gatheredWSConnectionFamily(t) == nil {
		for scope, value := range baseline {
			if value != 0 {
				t.Errorf("websocket scope %q series disappeared from nonzero baseline %v", scope, value)
			}
		}
		return
	}
	assertBoundedWSConnectionFamily(t)
	values := wsConnectionMetricSnapshot(t)
	for _, scope := range []string{"public", "admin", "other"} {
		if got := values[scope]; got != baseline[scope] {
			t.Errorf("websocket scope %q gauge = %v, want restored baseline %v", scope, got, baseline[scope])
		}
	}
}

func wsConnectionMetricSnapshot(t *testing.T) map[string]float64 {
	t.Helper()
	values := make(map[string]float64)
	family := gatheredWSConnectionFamily(t)
	if family == nil {
		return values
	}
	for _, metric := range family.GetMetric() {
		for _, label := range metric.GetLabel() {
			if label.GetName() == "scope" {
				values[label.GetValue()] += metric.GetGauge().GetValue()
			}
		}
	}
	return values
}

func wsConnectionScopeValue(t *testing.T, scope string) float64 {
	t.Helper()
	family := gatheredWSConnectionFamily(t)
	if family == nil {
		return 0
	}
	total := 0.0
	for _, metric := range family.GetMetric() {
		for _, label := range metric.GetLabel() {
			if label.GetName() == "scope" && label.GetValue() == scope {
				total += metric.GetGauge().GetValue()
			}
		}
	}
	return total
}

func gatheredWSConnectionFamily(t *testing.T) *dto.MetricFamily {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather default registry: %v", err)
	}
	for _, family := range families {
		if family.GetName() == "auction_ws_connections" {
			return family
		}
	}
	return nil
}
