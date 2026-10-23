package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type capturedBarrageBatch struct {
	roomID  string
	payload []byte
}

type barrageSinkSpy struct {
	mu      sync.Mutex
	batches []capturedBarrageBatch
}

type barrageRoomSink struct {
	rooms chan string
}

func (s *barrageRoomSink) deliverPublicBatch(roomID string, _ []byte) {
	s.rooms <- roomID
}

func (s *barrageSinkSpy) deliverPublicBatch(roomID string, payload []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, capturedBarrageBatch{roomID, append([]byte(nil), payload...)})
}

func (s *barrageSinkSpy) first() (capturedBarrageBatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.batches) == 0 {
		return capturedBarrageBatch{}, false
	}
	return s.batches[0], true
}

func newTestBarrageService(t testing.TB, cfg BarrageConfig, sink barrageBatchSink, moderation BarrageModeration) *BarrageService {
	t.Helper()
	service, err := NewBarrageService(cfg, sink, moderation)
	if err != nil {
		t.Fatalf("NewBarrageService: %v", err)
	}
	return service
}

func TestNormalizeBarrageConfigRejectsSubMillisecondWindow(t *testing.T) {
	cfg := DefaultBarrageConfig()
	cfg.Window = 500 * time.Microsecond

	if _, err := NormalizeBarrageConfig(cfg); err == nil {
		t.Fatal("expected an invalid window error")
	}
}

func TestNormalizeBarrageConfigDefaultsZeroValues(t *testing.T) {
	got, err := NormalizeBarrageConfig(BarrageConfig{})
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultBarrageConfig()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized config = %+v, want %+v", got, want)
	}
}

func TestNormalizeBarrageConfigDefaultsBoundedWindowState(t *testing.T) {
	got, err := NormalizeBarrageConfig(BarrageConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxActiveRoomsPerWindow != 32 {
		t.Fatalf("MaxActiveRoomsPerWindow = %d, want 32", got.MaxActiveRoomsPerWindow)
	}
	if got.MaxUniquePerRoomWindow != 5_000 {
		t.Fatalf("MaxUniquePerRoomWindow = %d, want 5000", got.MaxUniquePerRoomWindow)
	}
}

func TestNormalizeBarrageConfigRejectsUnsafeWindowStateBounds(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*BarrageConfig)
	}{
		{"negative active rooms", func(cfg *BarrageConfig) { cfg.MaxActiveRoomsPerWindow = -1 }},
		{"active rooms above absolute cap", func(cfg *BarrageConfig) { cfg.MaxActiveRoomsPerWindow = 4_097 }},
		{"near MaxInt active rooms", func(cfg *BarrageConfig) { cfg.MaxActiveRoomsPerWindow = math.MaxInt }},
		{"unique candidates above absolute cap", func(cfg *BarrageConfig) { cfg.MaxUniquePerRoomWindow = 5_001 }},
		{"combined candidate envelope", func(cfg *BarrageConfig) { cfg.MaxActiveRoomsPerWindow = 51 }},
		{"near MaxInt unique candidates", func(cfg *BarrageConfig) { cfg.MaxUniquePerRoomWindow = math.MaxInt }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultBarrageConfig()
			test.mutate(&cfg)
			if _, err := NormalizeBarrageConfig(cfg); err == nil {
				t.Fatal("expected unsafe window state config to be rejected")
			}
		})
	}
}

func TestNormalizeBarrageConfigAcceptsWindowStateEnvelopeBoundary(t *testing.T) {
	cfg := DefaultBarrageConfig()
	cfg.MaxActiveRoomsPerWindow = 50
	cfg.MaxUniquePerRoomWindow = 5_000

	got, err := NormalizeBarrageConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxActiveRoomsPerWindow != 50 || got.MaxUniquePerRoomWindow != 5_000 {
		t.Fatalf("normalized bounds = rooms:%d unique:%d", got.MaxActiveRoomsPerWindow, got.MaxUniquePerRoomWindow)
	}
}

func TestNormalizeBarrageConfigCopiesSensitiveWords(t *testing.T) {
	words := []string{" Blocked "}
	cfg := DefaultBarrageConfig()
	cfg.SensitiveWords = words

	got, err := NormalizeBarrageConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if words[0] != " Blocked " {
		t.Fatalf("caller word was mutated to %q", words[0])
	}
	words[0] = "allowed"
	if got.SensitiveWords[0] != "blocked" {
		t.Fatalf("normalized word changed to %q after caller mutation", got.SensitiveWords[0])
	}
}

func TestBarrageServiceCopiesSensitiveWords(t *testing.T) {
	words := []string{" Blocked "}
	cfg := DefaultBarrageConfig()
	cfg.SensitiveWords = words
	service := newTestBarrageService(t, cfg, nil, nil)

	words[0] = "allowed"
	err := service.SubmitTrusted(Barrage{RoomID: "room", UserID: "user", Content: "contains blocked text"})
	if !errors.Is(err, ErrBarrageSensitive) {
		t.Fatalf("SubmitTrusted error = %v, want %v", err, ErrBarrageSensitive)
	}
}

func TestNormalizeBarrageConfigRejectsNegativeValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*BarrageConfig)
	}{
		{"queue capacity", func(cfg *BarrageConfig) { cfg.QueueCapacity = -1 }},
		{"window", func(cfg *BarrageConfig) { cfg.Window = -time.Millisecond }},
		{"room output budget", func(cfg *BarrageConfig) { cfg.MaxPerRoomWindow = -1 }},
		{"admin reserved budget", func(cfg *BarrageConfig) { cfg.AdminReserved = -1 }},
		{"paid reserved budget", func(cfg *BarrageConfig) { cfg.PaidReserved = -1 }},
		{"active room capacity", func(cfg *BarrageConfig) { cfg.MaxActiveRoomsPerWindow = -1 }},
		{"unique capacity", func(cfg *BarrageConfig) { cfg.MaxUniquePerRoomWindow = -1 }},
		{"content capacity", func(cfg *BarrageConfig) { cfg.MaxContentRunes = -1 }},
		{"user burst", func(cfg *BarrageConfig) { cfg.UserBurst = -1 }},
		{"user rate", func(cfg *BarrageConfig) { cfg.UserRatePerSecond = -1 }},
		{"rate entry ttl", func(cfg *BarrageConfig) { cfg.RateEntryTTL = -time.Second }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultBarrageConfig()
			test.mutate(&cfg)
			if _, err := NormalizeBarrageConfig(cfg); err == nil {
				t.Fatal("expected invalid config error")
			}
		})
	}
}

func TestNormalizeBarrageConfigRejectsOverflowingReservedBudgets(t *testing.T) {
	cfg := DefaultBarrageConfig()
	cfg.MaxPerRoomWindow = math.MaxInt
	cfg.AdminReserved = math.MaxInt - 1
	cfg.PaidReserved = math.MaxInt - 1

	if _, err := NormalizeBarrageConfig(cfg); err == nil {
		t.Fatal("expected reserved budgets above the room output budget to be rejected")
	}
}

func TestAggregateHigherPriorityDuplicateReplacesRepresentative(t *testing.T) {
	service := newTestBarrageService(t, DefaultBarrageConfig(), nil, nil)
	rooms := map[string]map[string]*Barrage{}
	service.aggregate(rooms, Barrage{
		ID: "ordinary", RoomID: "room", UserID: "ordinary-user", DisplayName: "Ordinary",
		Content: "same", Priority: BarrageOrdinary, CreatedAtMs: 100,
	})
	service.aggregate(rooms, Barrage{
		ID: "paid", RoomID: "room", UserID: "paid-user", DisplayName: "Paid",
		Content: "same", Priority: BarragePaid, CreatedAtMs: 200,
	})

	got := rooms["room"]["same"]
	if got.ID != "paid" || got.UserID != "paid-user" || got.DisplayName != "Paid" || got.CreatedAtMs != 200 || got.Priority != BarragePaid || got.Count != 2 {
		t.Fatalf("aggregate representative = %+v", got)
	}
}

func TestBarrageWindowDropsNewRoomsWithoutGrowingInputState(t *testing.T) {
	cfg := DefaultBarrageConfig()
	cfg.MaxActiveRoomsPerWindow = 2
	cfg.MaxUniquePerRoomWindow = 2
	service := newTestBarrageService(t, cfg, nil, nil)
	rooms := map[string]map[string]*Barrage{}
	inputByRoom := map[string]int{}

	for _, message := range []Barrage{
		{ID: "a1", RoomID: "room-a", UserID: "user", Content: "first"},
		{ID: "b1", RoomID: "room-b", UserID: "user", Content: "first"},
		{ID: "c1", RoomID: "room-c", UserID: "user", Content: "dropped"},
		{ID: "a2", RoomID: "room-a", UserID: "user", Content: "second"},
	} {
		service.consumeWindowMessage(rooms, inputByRoom, message)
	}

	if len(rooms) != 2 || rooms["room-a"] == nil || rooms["room-b"] == nil || rooms["room-c"] != nil {
		t.Fatalf("retained rooms = %+v", rooms)
	}
	if len(inputByRoom) != 2 || inputByRoom["room-a"] != 2 || inputByRoom["room-b"] != 1 {
		t.Fatalf("inputByRoom = %+v", inputByRoom)
	}
	if _, exists := inputByRoom["room-c"]; exists {
		t.Fatal("dropped room created an input counter")
	}
}

func TestBarrageWindowCapsPublicOrdinaryCandidates(t *testing.T) {
	cfg := DefaultBarrageConfig()
	cfg.MaxActiveRoomsPerWindow = 1
	cfg.MaxUniquePerRoomWindow = 2
	service := newTestBarrageService(t, cfg, nil, nil)
	for i := 0; i < 3; i++ {
		if err := service.SubmitPublic(context.Background(), Barrage{
			ID: fmt.Sprintf("public-%d", i), RoomID: "room", UserID: fmt.Sprintf("user-%d", i), Content: fmt.Sprintf("message-%d", i),
		}); err != nil {
			t.Fatalf("SubmitPublic %d: %v", i, err)
		}
	}
	rooms := map[string]map[string]*Barrage{}
	inputByRoom := map[string]int{}
	for len(service.queue) > 0 {
		service.consumeWindowMessage(rooms, inputByRoom, <-service.queue)
	}

	if got := len(rooms["room"]); got != 2 {
		t.Fatalf("retained public candidates = %d, want 2", got)
	}
	if inputByRoom["room"] != 3 {
		t.Fatalf("consumed public inputs = %d, want 3", inputByRoom["room"])
	}
}

func TestBarrageWindowCapsPaidAndAdminCandidates(t *testing.T) {
	for _, priority := range []BarragePriority{BarragePaid, BarrageAdmin} {
		t.Run(fmt.Sprint(priority), func(t *testing.T) {
			cfg := DefaultBarrageConfig()
			cfg.MaxActiveRoomsPerWindow = 1
			cfg.MaxUniquePerRoomWindow = 2
			service := newTestBarrageService(t, cfg, nil, nil)
			rooms := map[string]map[string]*Barrage{}
			inputByRoom := map[string]int{}
			for i := 0; i < 3; i++ {
				id := fmt.Sprintf("trusted-%d", i)
				service.consumeWindowMessage(rooms, inputByRoom, Barrage{ID: id, RoomID: "room", UserID: id, Content: id, Priority: priority})
			}
			if got := len(rooms["room"]); got != 2 {
				t.Fatalf("retained priority %d candidates = %d, want 2", priority, got)
			}
			if inputByRoom["room"] != 3 {
				t.Fatalf("consumed priority %d inputs = %d, want 3", priority, inputByRoom["room"])
			}
		})
	}
}

func TestBarrageWindowHigherPriorityReplacementUsesDeterministicVictim(t *testing.T) {
	cfg := DefaultBarrageConfig()
	cfg.MaxActiveRoomsPerWindow = 1
	cfg.MaxUniquePerRoomWindow = 3
	service := newTestBarrageService(t, cfg, nil, nil)
	rooms := map[string]map[string]*Barrage{}
	inputByRoom := map[string]int{}
	for _, message := range []Barrage{
		{ID: "alpha", RoomID: "room", UserID: "ordinary-a", Content: "alpha"},
		{ID: "zeta", RoomID: "room", UserID: "ordinary-z", Content: "zeta"},
		{ID: "paid-old", RoomID: "room", UserID: "paid-old", Content: "paid old", Priority: BarragePaid},
		{ID: "paid-new", RoomID: "room", UserID: "paid-new", Content: "paid new", Priority: BarragePaid},
	} {
		service.consumeWindowMessage(rooms, inputByRoom, message)
	}
	items := rooms["room"]
	if len(items) != 3 || items["zeta"] != nil || items["alpha"] == nil || items["paid old"] == nil || items["paid new"] == nil {
		t.Fatalf("items after paid replacement = %+v", items)
	}

	service.consumeWindowMessage(rooms, inputByRoom, Barrage{ID: "admin", RoomID: "room", UserID: "admin", Content: "admin", Priority: BarrageAdmin})
	if len(items) != 3 || items["alpha"] != nil || items["admin"] == nil {
		t.Fatalf("items after admin replacement = %+v", items)
	}
}

func TestBarrageWindowDuplicatePromotesAtCandidateCap(t *testing.T) {
	cfg := DefaultBarrageConfig()
	cfg.MaxActiveRoomsPerWindow = 1
	cfg.MaxUniquePerRoomWindow = 1
	service := newTestBarrageService(t, cfg, nil, nil)
	rooms := map[string]map[string]*Barrage{}
	inputByRoom := map[string]int{}
	service.consumeWindowMessage(rooms, inputByRoom, Barrage{ID: "ordinary", RoomID: "room", UserID: "ordinary", Content: "same"})
	service.consumeWindowMessage(rooms, inputByRoom, Barrage{ID: "admin", RoomID: "room", UserID: "admin", Content: " SAME ", Priority: BarrageAdmin})

	got := rooms["room"]["same"]
	if len(rooms["room"]) != 1 || got == nil || got.ID != "admin" || got.UserID != "admin" || got.Priority != BarrageAdmin || got.Count != 2 {
		t.Fatalf("promoted duplicate = %+v; items = %d", got, len(rooms["room"]))
	}
	if inputByRoom["room"] != 2 {
		t.Fatalf("consumed inputs = %d, want 2", inputByRoom["room"])
	}
}

func TestBarrageWindowCountersSaturateWithoutOverflow(t *testing.T) {
	cfg := DefaultBarrageConfig()
	cfg.MaxActiveRoomsPerWindow = 1
	cfg.MaxUniquePerRoomWindow = 1
	service := newTestBarrageService(t, cfg, nil, nil)
	rooms := map[string]map[string]*Barrage{
		"room": {"same": {ID: "existing", RoomID: "room", UserID: "user", Content: "same", Count: math.MaxInt}},
	}
	inputByRoom := map[string]int{"room": math.MaxInt}

	service.consumeWindowMessage(rooms, inputByRoom, Barrage{ID: "duplicate", RoomID: "room", UserID: "user", Content: "same"})
	if rooms["room"]["same"].Count != math.MaxInt || inputByRoom["room"] != math.MaxInt {
		t.Fatalf("saturated counts = aggregate:%d input:%d", rooms["room"]["same"].Count, inputByRoom["room"])
	}
}

func TestBarrageWindowStateStaysBoundedUnderRefilledTrustedQueue(t *testing.T) {
	cfg := DefaultBarrageConfig()
	cfg.QueueCapacity = 1
	cfg.MaxActiveRoomsPerWindow = 4
	cfg.MaxUniquePerRoomWindow = 8
	service := newTestBarrageService(t, cfg, nil, nil)
	rooms := map[string]map[string]*Barrage{}
	inputByRoom := map[string]int{}
	for i := 0; i < 20_000; i++ {
		message := Barrage{
			ID: fmt.Sprintf("id-%d", i), RoomID: fmt.Sprintf("room-%02d", i%100), UserID: "trusted",
			Content: fmt.Sprintf("message-%d", i), Priority: BarragePriority(i % 3),
		}
		if err := service.SubmitTrusted(message); err != nil {
			t.Fatalf("SubmitTrusted %d: %v", i, err)
		}
		service.consumeWindowMessage(rooms, inputByRoom, <-service.queue)
	}
	if len(rooms) != 4 || len(inputByRoom) != 4 {
		t.Fatalf("window maps = rooms:%d inputs:%d, want 4/4", len(rooms), len(inputByRoom))
	}
	totalCandidates := 0
	for roomID, items := range rooms {
		if len(items) > 8 {
			t.Fatalf("room %q retained %d candidates, want at most 8", roomID, len(items))
		}
		totalCandidates += len(items)
	}
	if totalCandidates > 32 {
		t.Fatalf("retained candidates = %d, want at most 32", totalCandidates)
	}
}

func TestBarrageRunResetsRoomCapacityAcrossTicksAndStopsOnCancel(t *testing.T) {
	cfg := DefaultBarrageConfig()
	cfg.Window = 10 * time.Millisecond
	cfg.MaxActiveRoomsPerWindow = 1
	cfg.MaxUniquePerRoomWindow = 1
	sink := &barrageRoomSink{rooms: make(chan string, 4)}
	service := newTestBarrageService(t, cfg, sink, nil)
	if err := service.SubmitTrusted(Barrage{ID: "a", RoomID: "room-a", UserID: "user", Content: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := service.SubmitTrusted(Barrage{ID: "b-dropped", RoomID: "room-b", UserID: "user", Content: "b dropped"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		service.Run(ctx)
		close(done)
	}()

	for _, wantRoom := range []string{"room-a", "room-b", "room-c"} {
		select {
		case got := <-sink.rooms:
			if got != wantRoom {
				t.Fatalf("delivered room = %q, want %q", got, wantRoom)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q", wantRoom)
		}
		next := "room-b"
		if wantRoom == "room-b" {
			next = "room-c"
		}
		if wantRoom != "room-c" {
			if err := service.SubmitTrusted(Barrage{ID: next, RoomID: next, UserID: "user", Content: next}); err != nil {
				t.Fatal(err)
			}
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("BarrageService.Run did not stop after cancellation")
	}
}

func TestBarrageServiceAggregatesSamplesAndPreservesPriority(t *testing.T) {
	sink := &barrageSinkSpy{}
	cfg := DefaultBarrageConfig()
	cfg.Window = 10 * time.Millisecond
	cfg.MaxPerRoomWindow = 3
	service := newTestBarrageService(t, cfg, sink, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go service.Run(ctx)

	messages := []Barrage{
		{ID: "a", RoomID: "room-1", UserID: "u1", Content: "支持主播"},
		{ID: "b", RoomID: "room-1", UserID: "u2", Content: " 支持主播 "},
		{ID: "c", RoomID: "room-1", UserID: "u3", Content: "付费置顶", Priority: BarragePaid},
		{ID: "d", RoomID: "room-1", UserID: "admin", Content: "管理员通知", Priority: BarrageAdmin},
		{ID: "e", RoomID: "room-1", UserID: "u4", Content: "普通候选"},
	}
	for _, message := range messages {
		if err := service.SubmitTrusted(message); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	deadline := time.Now().Add(time.Second)
	var captured capturedBarrageBatch
	for time.Now().Before(deadline) {
		if value, ok := sink.first(); ok {
			captured = value
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(captured.payload) == 0 {
		t.Fatal("expected a flushed barrage batch")
	}
	var batch struct {
		Type     string    `json:"type"`
		Messages []Barrage `json:"messages"`
	}
	if err := json.Unmarshal(captured.payload, &batch); err != nil {
		t.Fatal(err)
	}
	if batch.Type != "BARRAGE_BATCH" || len(batch.Messages) != 3 {
		t.Fatalf("unexpected batch: %+v", batch)
	}
	if batch.Messages[0].Priority != BarrageAdmin || batch.Messages[1].Priority != BarragePaid {
		t.Fatalf("priority messages not retained first: %+v", batch.Messages)
	}
	for _, message := range batch.Messages {
		if message.Content == "支持主播" && message.Count != 2 {
			t.Fatalf("duplicate count = %d", message.Count)
		}
	}
}

func TestBarrageServicePublicAdmissionIsBounded(t *testing.T) {
	cfg := DefaultBarrageConfig()
	cfg.QueueCapacity = 1
	cfg.UserBurst = 1
	cfg.UserRatePerSecond = 0.0001
	service := newTestBarrageService(t, cfg, nil, nil)
	service.now = func() time.Time { return time.Unix(100, 0) }
	message := Barrage{RoomID: "room", UserID: "user", Content: "hello", Priority: BarrageAdmin}
	if err := service.SubmitPublic(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if err := service.SubmitPublic(context.Background(), message); !errors.Is(err, ErrBarrageRateLimited) {
		t.Fatalf("second message error = %v", err)
	}
	if got := (<-service.queue).Priority; got != BarrageOrdinary {
		t.Fatalf("public priority = %v", got)
	}

	for i := 0; i < cap(service.queue); i++ {
		service.queue <- Barrage{ID: fmt.Sprint(i)}
	}
	service.limiter = newBarrageRateLimiter(cfg.UserBurst, cfg.UserRatePerSecond, cfg.RateEntryTTL)
	if err := service.SubmitPublic(context.Background(), Barrage{RoomID: "room", UserID: "other", Content: "hello"}); !errors.Is(err, ErrBarrageOverloaded) {
		t.Fatalf("full queue error = %v", err)
	}
}

type referenceRankedBarrage struct {
	message Barrage
	score   uint64
}

func referenceBarrageScore(message *Barrage, window int64) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(message.ID + "\x00" + message.UserID + "\x00" + message.Content))
	score := h.Sum64() ^ uint64(window)
	if utf8.RuneCountInString(message.Content) < 3 {
		score >>= 2
	}
	return score
}

func referenceRankedBetter(left, right referenceRankedBarrage) bool {
	if left.score != right.score {
		return left.score > right.score
	}
	if left.message.ID != right.message.ID {
		return left.message.ID < right.message.ID
	}
	if left.message.UserID != right.message.UserID {
		return left.message.UserID < right.message.UserID
	}
	return left.message.Content < right.message.Content
}

func referenceSelectBarrages(grouped map[string]*Barrage, limit int, window int64, adminReserved, paidReserved int) []Barrage {
	tiers := [3][]referenceRankedBarrage{}
	for _, message := range grouped {
		priority := int(message.Priority)
		if priority < int(BarrageOrdinary) || priority > int(BarrageAdmin) {
			priority = int(BarrageOrdinary)
		}
		tiers[priority] = append(tiers[priority], referenceRankedBarrage{*message, referenceBarrageScore(message, window)})
	}
	for i := range tiers {
		sort.Slice(tiers[i], func(left, right int) bool {
			return referenceRankedBetter(tiers[i][left], tiers[i][right])
		})
	}

	adminBudget := min(limit, adminReserved)
	adminCount := min(len(tiers[BarrageAdmin]), adminBudget)
	paidBudget := min(limit-adminCount, paidReserved+(adminBudget-adminCount))
	paidCount := min(len(tiers[BarragePaid]), paidBudget)
	ordinaryCount := min(len(tiers[BarrageOrdinary]), limit-adminCount-paidCount)

	selected := make([]referenceRankedBarrage, 0, adminCount+paidCount+ordinaryCount)
	selected = append(selected, tiers[BarrageAdmin][:adminCount]...)
	selected = append(selected, tiers[BarragePaid][:paidCount]...)
	selected = append(selected, tiers[BarrageOrdinary][:ordinaryCount]...)
	sort.Slice(selected, func(left, right int) bool {
		if selected[left].message.Priority != selected[right].message.Priority {
			return selected[left].message.Priority > selected[right].message.Priority
		}
		return referenceRankedBetter(selected[left], selected[right])
	})
	out := make([]Barrage, len(selected))
	for i := range selected {
		out[i] = selected[i].message
	}
	return out
}

func TestSelectBarragesMatchesFullSortReference(t *testing.T) {
	grouped := make(map[string]*Barrage, 5_000)
	for i := 0; i < 5_000; i++ {
		priority := BarrageOrdinary
		if i%11 == 0 {
			priority = BarragePaid
		}
		if i%97 == 0 {
			priority = BarrageAdmin
		}
		content := fmt.Sprintf("message-%04d", i)
		grouped[content] = &Barrage{
			ID: fmt.Sprintf("id-%04d", i), UserID: fmt.Sprintf("user-%03d", i%313),
			Content: content, Priority: priority, Count: 1,
		}
	}
	const window = int64(37)
	defaults := DefaultBarrageConfig()
	want := referenceSelectBarrages(grouped, 30, window, defaults.AdminReserved, defaults.PaidReserved)
	got := selectBarrages(grouped, 30, window)
	if len(got) != 30 {
		t.Fatalf("selected %d barrages, want 30", len(got))
	}
	if !reflect.DeepEqual(got, want) {
		gotIDs := make([]string, len(got))
		wantIDs := make([]string, len(want))
		for i := range got {
			gotIDs[i] = got[i].ID
			wantIDs[i] = want[i].ID
		}
		t.Fatalf("selected IDs = %v, want %v", gotIDs, wantIDs)
	}
}

func TestSelectBarragesReservedCapacityFlowsDown(t *testing.T) {
	tests := []struct {
		name                      string
		admin, paid, ordinary     int
		wantAdmin, wantPaid, want int
	}{
		{"reserved budgets cap priority tiers", 5, 10, 40, 3, 7, 20},
		{"unused admin flows to paid", 0, 10, 40, 0, 10, 20},
		{"unused paid flows to ordinary", 0, 2, 40, 0, 2, 28},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			grouped := make(map[string]*Barrage, test.admin+test.paid+test.ordinary)
			add := func(prefix string, count int, priority BarragePriority) {
				for i := 0; i < count; i++ {
					id := fmt.Sprintf("%s-%02d", prefix, i)
					grouped[id] = &Barrage{ID: id, UserID: id, Content: id, Priority: priority}
				}
			}
			add("admin", test.admin, BarrageAdmin)
			add("paid", test.paid, BarragePaid)
			add("ordinary", test.ordinary, BarrageOrdinary)

			got := selectBarrages(grouped, 30, 1)
			counts := map[BarragePriority]int{}
			for _, message := range got {
				counts[message.Priority]++
			}
			if counts[BarrageAdmin] != test.wantAdmin || counts[BarragePaid] != test.wantPaid || counts[BarrageOrdinary] != test.want {
				t.Fatalf("priority counts = admin:%d paid:%d ordinary:%d", counts[BarrageAdmin], counts[BarragePaid], counts[BarrageOrdinary])
			}
		})
	}
}

func TestSelectBarragesLargeLimitDoesNotPreallocateLimit(t *testing.T) {
	grouped := map[string]*Barrage{
		"only": &Barrage{ID: "only", UserID: "user", Content: "only"},
	}

	got := selectBarragesWithReserved(grouped, math.MaxInt, 1, 3, 7)
	if len(got) != 1 || got[0].ID != "only" {
		t.Fatalf("selected barrages = %+v", got)
	}
}

func TestBarrageServiceUsesConfiguredReservedBudgets(t *testing.T) {
	cfg := DefaultBarrageConfig()
	cfg.MaxPerRoomWindow = 4
	cfg.AdminReserved = 1
	cfg.PaidReserved = 1
	sink := &barrageSinkSpy{}
	service := newTestBarrageService(t, cfg, sink, nil)
	grouped := make(map[string]*Barrage)
	for priority := BarrageOrdinary; priority <= BarrageAdmin; priority++ {
		for i := 0; i < 3; i++ {
			id := fmt.Sprintf("%d-%d", priority, i)
			grouped[id] = &Barrage{ID: id, RoomID: "room", UserID: id, Content: id, Priority: priority}
		}
	}
	service.flush(map[string]map[string]*Barrage{"room": grouped}, map[string]int{"room": len(grouped)}, time.UnixMilli(100))
	captured, ok := sink.first()
	if !ok {
		t.Fatal("expected flushed batch")
	}
	var batch struct {
		Messages []Barrage `json:"messages"`
	}
	if err := json.Unmarshal(captured.payload, &batch); err != nil {
		t.Fatal(err)
	}
	counts := map[BarragePriority]int{}
	for _, message := range batch.Messages {
		counts[message.Priority]++
	}
	if counts[BarrageAdmin] != 1 || counts[BarragePaid] != 1 || counts[BarrageOrdinary] != 2 {
		t.Fatalf("priority counts = admin:%d paid:%d ordinary:%d", counts[BarrageAdmin], counts[BarragePaid], counts[BarrageOrdinary])
	}
}

func BenchmarkSelectBarragesFiveThousand(b *testing.B) {
	grouped := make(map[string]*Barrage, 5_000)
	for i := 0; i < 5_000; i++ {
		content := fmt.Sprintf("message-%d", i)
		grouped[content] = &Barrage{ID: fmt.Sprint(i), UserID: fmt.Sprintf("u-%d", i), Content: content, Count: 1}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = selectBarrages(grouped, 30, int64(i))
	}
}

func BenchmarkBarrageWindowCandidateOverflowAtCap(b *testing.B) {
	cfg := DefaultBarrageConfig()
	cfg.MaxActiveRoomsPerWindow = 1
	cfg.MaxUniquePerRoomWindow = 5_000
	service := newTestBarrageService(b, cfg, nil, nil)
	rooms := map[string]map[string]*Barrage{"room": make(map[string]*Barrage, cfg.MaxUniquePerRoomWindow)}
	for i := 0; i < cfg.MaxUniquePerRoomWindow; i++ {
		content := fmt.Sprintf("message-%04d", i)
		rooms["room"][content] = &Barrage{ID: content, RoomID: "room", UserID: "user", Content: content, Count: 1}
	}
	overflow := Barrage{ID: "overflow", RoomID: "room", UserID: "user", Content: "overflow"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		service.aggregate(rooms, overflow)
	}
}
