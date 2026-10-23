package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

type signalingBarrageSink struct {
	batches chan []byte
}

func (s *signalingBarrageSink) deliverPublicBatch(_ string, payload []byte) {
	s.batches <- append([]byte(nil), payload...)
}

func TestBarrageMetricAdmissionRecordsEveryRealOutcome(t *testing.T) {
	results := []string{"accepted", "invalid", "rate_limited", "muted", "sensitive", "overloaded"}
	before := make(map[string]float64, len(results))
	for _, result := range results {
		before[result] = gatheredBarrageCounter(t, "auction_barrage_admission_total", "result", result)
	}

	accepted := newTestBarrageService(t, DefaultBarrageConfig(), nil, nil)
	if err := accepted.SubmitTrusted(Barrage{RoomID: "room", UserID: "accepted", Content: "hello"}); err != nil {
		t.Fatalf("accepted submission: %v", err)
	}
	if err := accepted.SubmitTrusted(Barrage{RoomID: "", UserID: "invalid", Content: "hello"}); !errors.Is(err, ErrBarrageInvalid) {
		t.Fatalf("invalid submission error = %v", err)
	}

	sensitiveConfig := DefaultBarrageConfig()
	sensitiveConfig.SensitiveWords = []string{"blocked"}
	sensitive := newTestBarrageService(t, sensitiveConfig, nil, nil)
	if err := sensitive.SubmitTrusted(Barrage{RoomID: "room", UserID: "sensitive", Content: "blocked text"}); !errors.Is(err, ErrBarrageSensitive) {
		t.Fatalf("sensitive submission error = %v", err)
	}

	rateConfig := DefaultBarrageConfig()
	rateConfig.GlobalUserBurst = 1
	rateConfig.GlobalUserRatePerSecond = 0.0001
	rateLimited := newTestBarrageService(t, rateConfig, nil, nil)
	rateLimited.now = func() time.Time { return time.Unix(100, 0) }
	rateMessage := Barrage{RoomID: "room", UserID: "limited", Content: "hello"}
	if err := rateLimited.SubmitPublic(context.Background(), rateMessage); err != nil {
		t.Fatalf("first rate-limited submission: %v", err)
	}
	if err := rateLimited.SubmitPublic(context.Background(), rateMessage); !errors.Is(err, ErrBarrageRateLimited) {
		t.Fatalf("rate-limited submission error = %v", err)
	}

	mutedModeration := &countingBarrageModeration{muted: true}
	muted := newTestBarrageService(t, DefaultBarrageConfig(), nil, mutedModeration)
	if err := muted.SubmitPublic(context.Background(), Barrage{RoomID: "room", UserID: "muted", Content: "hello"}); !errors.Is(err, ErrBarrageMuted) {
		t.Fatalf("muted submission error = %v", err)
	}

	overloadConfig := DefaultBarrageConfig()
	overloadConfig.QueueCapacity = 1
	overloaded := newTestBarrageService(t, overloadConfig, nil, nil)
	if err := overloaded.SubmitTrusted(Barrage{RoomID: "room", UserID: "first", Content: "hello"}); err != nil {
		t.Fatalf("queue-filling submission: %v", err)
	}
	if err := overloaded.SubmitTrusted(Barrage{RoomID: "room", UserID: "second", Content: "hello again"}); !errors.Is(err, ErrBarrageOverloaded) {
		t.Fatalf("overloaded submission error = %v", err)
	}

	wantDelta := map[string]float64{
		"accepted": 3, "invalid": 1, "rate_limited": 1,
		"muted": 1, "sensitive": 1, "overloaded": 1,
	}
	for _, result := range results {
		after := gatheredBarrageCounter(t, "auction_barrage_admission_total", "result", result)
		if delta := after - before[result]; delta != wantDelta[result] {
			t.Errorf("admission result %q delta = %v, want %v", result, delta, wantDelta[result])
		}
	}
}

func TestBarrageMetricWindowRecordsInputUniqueSelectedAndDuration(t *testing.T) {
	beforeInputCount, beforeInputSum := gatheredBarrageHistogram(t, "auction_barrage_window_input")
	beforeUniqueCount, beforeUniqueSum := gatheredBarrageHistogram(t, "auction_barrage_window_unique")
	beforeSelectedCount, beforeSelectedSum := gatheredBarrageHistogram(t, "auction_barrage_window_selected")
	beforeDurationCount, _ := gatheredBarrageHistogram(t, "auction_barrage_window_duration_ms")

	config := DefaultBarrageConfig()
	config.MaxPerRoomWindow = 1
	service := newTestBarrageService(t, config, &barrageSinkSpy{}, nil)
	service.flush(map[string]map[string]*Barrage{
		"room": {
			"first":  {ID: "first", RoomID: "room", UserID: "one", Content: "first", Count: 2},
			"second": {ID: "second", RoomID: "room", UserID: "two", Content: "second", Count: 3},
		},
	}, map[string]int{"room": 5}, time.UnixMilli(100))

	afterInputCount, afterInputSum := gatheredBarrageHistogram(t, "auction_barrage_window_input")
	afterUniqueCount, afterUniqueSum := gatheredBarrageHistogram(t, "auction_barrage_window_unique")
	afterSelectedCount, afterSelectedSum := gatheredBarrageHistogram(t, "auction_barrage_window_selected")
	afterDurationCount, _ := gatheredBarrageHistogram(t, "auction_barrage_window_duration_ms")
	if afterInputCount-beforeInputCount != 1 || afterInputSum-beforeInputSum != 5 {
		t.Errorf("window input delta = count %d sum %v, want count 1 sum 5", afterInputCount-beforeInputCount, afterInputSum-beforeInputSum)
	}
	if afterUniqueCount-beforeUniqueCount != 1 || afterUniqueSum-beforeUniqueSum != 2 {
		t.Errorf("window unique delta = count %d sum %v, want count 1 sum 2", afterUniqueCount-beforeUniqueCount, afterUniqueSum-beforeUniqueSum)
	}
	if afterSelectedCount-beforeSelectedCount != 1 || afterSelectedSum-beforeSelectedSum != 1 {
		t.Errorf("window selected delta = count %d sum %v, want count 1 sum 1", afterSelectedCount-beforeSelectedCount, afterSelectedSum-beforeSelectedSum)
	}
	if afterDurationCount-beforeDurationCount != 1 {
		t.Errorf("window duration count delta = %d, want 1", afterDurationCount-beforeDurationCount)
	}
}

func TestBarrageMetricRunCountsConsumedInputsDroppedByUniqueCap(t *testing.T) {
	beforeInputCount, beforeInputSum := gatheredBarrageHistogram(t, "auction_barrage_window_input")
	beforeUniqueCount, beforeUniqueSum := gatheredBarrageHistogram(t, "auction_barrage_window_unique")
	beforeSelectedCount, beforeSelectedSum := gatheredBarrageHistogram(t, "auction_barrage_window_selected")

	config := DefaultBarrageConfig()
	config.Window = 25 * time.Millisecond
	config.MaxUniquePerRoomWindow = 1
	config.MaxPerRoomWindow = 1
	sink := &signalingBarrageSink{batches: make(chan []byte, 1)}
	service := newTestBarrageService(t, config, sink, nil)
	for _, message := range []Barrage{
		{ID: "first", RoomID: "room", UserID: "one", Content: "first"},
		{ID: "second", RoomID: "room", UserID: "two", Content: "second"},
	} {
		if err := service.SubmitTrusted(message); err != nil {
			t.Fatalf("SubmitTrusted: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		service.Run(ctx)
		close(runDone)
	}()
	defer func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Error("BarrageService.Run did not stop after cancellation")
		}
	}()

	var payload []byte
	select {
	case payload = <-sink.batches:
	case <-time.After(time.Second):
		t.Fatal("BarrageService.Run did not flush the queued window")
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("BarrageService.Run did not stop after the observed flush")
	}

	var batch struct {
		Messages []Barrage `json:"messages"`
	}
	if err := json.Unmarshal(payload, &batch); err != nil {
		t.Fatalf("decode delivered batch: %v", err)
	}
	if len(batch.Messages) != 1 {
		t.Fatalf("delivered messages = %d, want 1 after unique cap", len(batch.Messages))
	}

	afterInputCount, afterInputSum := gatheredBarrageHistogram(t, "auction_barrage_window_input")
	afterUniqueCount, afterUniqueSum := gatheredBarrageHistogram(t, "auction_barrage_window_unique")
	afterSelectedCount, afterSelectedSum := gatheredBarrageHistogram(t, "auction_barrage_window_selected")
	if afterInputCount-beforeInputCount != 1 || afterInputSum-beforeInputSum != 2 {
		t.Errorf("window input delta = count %d sum %v, want count 1 sum 2 consumed messages", afterInputCount-beforeInputCount, afterInputSum-beforeInputSum)
	}
	if afterUniqueCount-beforeUniqueCount != 1 || afterUniqueSum-beforeUniqueSum != 1 {
		t.Errorf("window unique delta = count %d sum %v, want count 1 sum 1", afterUniqueCount-beforeUniqueCount, afterUniqueSum-beforeUniqueSum)
	}
	if afterSelectedCount-beforeSelectedCount != 1 || afterSelectedSum-beforeSelectedSum != 1 {
		t.Errorf("window selected delta = count %d sum %v, want count 1 sum 1", afterSelectedCount-beforeSelectedCount, afterSelectedSum-beforeSelectedSum)
	}
}

func TestBarrageMetricOverflowCountsEveryDroppedInputOnce(t *testing.T) {
	beforeRoom := gatheredBarrageCounter(t, "auction_barrage_overflow_dropped_total", "reason", "room_limit")
	beforeCandidate := gatheredBarrageCounter(t, "auction_barrage_overflow_dropped_total", "reason", "candidate_limit")
	cfg := DefaultBarrageConfig()
	cfg.MaxActiveRoomsPerWindow = 1
	cfg.MaxUniquePerRoomWindow = 1
	service := newTestBarrageService(t, cfg, nil, nil)
	rooms := map[string]map[string]*Barrage{}
	inputByRoom := map[string]int{}
	for _, message := range []Barrage{
		{ID: "ordinary", RoomID: "room", UserID: "one", Content: "same"},
		{ID: "duplicate", RoomID: "room", UserID: "two", Content: " SAME "},
		{ID: "new-room", RoomID: "dropped-room", UserID: "three", Content: "dropped"},
		{ID: "paid", RoomID: "room", UserID: "four", Content: "paid", Priority: BarragePaid},
		{ID: "ordinary-drop", RoomID: "room", UserID: "five", Content: "ordinary drop"},
	} {
		service.consumeWindowMessage(rooms, inputByRoom, message)
	}

	afterRoom := gatheredBarrageCounter(t, "auction_barrage_overflow_dropped_total", "reason", "room_limit")
	afterCandidate := gatheredBarrageCounter(t, "auction_barrage_overflow_dropped_total", "reason", "candidate_limit")
	if delta := afterRoom - beforeRoom; delta != 1 {
		t.Fatalf("room-limit drop delta = %v, want 1", delta)
	}
	if delta := afterCandidate - beforeCandidate; delta != 3 {
		t.Fatalf("candidate-limit drop delta = %v, want 3 (two evicted duplicates plus one rejected input)", delta)
	}
	if len(inputByRoom) != 1 || inputByRoom["room"] != 4 {
		t.Fatalf("inputByRoom = %+v, want only tracked room with four consumed inputs", inputByRoom)
	}
}

func TestBarrageMetricViewerDropRecordsOnlyFullActiveQueue(t *testing.T) {
	before := gatheredBarrageCounter(t, "auction_barrage_viewer_dropped_total", "", "")
	hub := NewHub(nil)
	full := bufferedTestConnection(hub, "room", ScopePublic, 1)
	full.barrageSend <- outboundMessage{metricType: "occupied"}
	hub.join(full)
	closed := bufferedTestConnection(hub, "room", ScopePublic, 1)
	closed.barrageSend <- outboundMessage{metricType: "occupied"}
	close(closed.done)
	hub.join(closed)

	hub.deliverPublicBatch("room", []byte(`{"type":"BARRAGE_BATCH"}`))

	after := gatheredBarrageCounter(t, "auction_barrage_viewer_dropped_total", "", "")
	if delta := after - before; delta != 1 {
		t.Fatalf("viewer drop delta = %v, want 1 full active queue and no closed-connection drop", delta)
	}
}

func gatheredBarrageCounter(t *testing.T, name, labelName, labelValue string) float64 {
	t.Helper()
	metric := gatheredBarrageMetric(t, name, labelName, labelValue)
	if metric == nil {
		return 0
	}
	return metric.GetCounter().GetValue()
}

func gatheredBarrageHistogram(t *testing.T, name string) (uint64, float64) {
	t.Helper()
	metric := gatheredBarrageMetric(t, name, "", "")
	if metric == nil {
		return 0, 0
	}
	histogram := metric.GetHistogram()
	return histogram.GetSampleCount(), histogram.GetSampleSum()
}

func gatheredBarrageMetric(t *testing.T, name, labelName, labelValue string) *dto.Metric {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather default registry: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if labelName == "" && len(metric.GetLabel()) == 0 {
				return metric
			}
			for _, label := range metric.GetLabel() {
				if label.GetName() == labelName && label.GetValue() == labelValue {
					return metric
				}
			}
		}
		return nil
	}
	return nil
}
