package realtime

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"live-auction-bid/backend/app/auction/service/internal/observability"
)

var (
	ErrBarrageInvalid     = errors.New("invalid barrage")
	ErrBarrageRateLimited = errors.New("barrage rate limited")
	ErrBarrageMuted       = errors.New("barrage sender muted")
	ErrBarrageSensitive   = errors.New("barrage contains sensitive content")
	ErrBarrageOverloaded  = errors.New("barrage pipeline overloaded")
)

type BarragePriority uint8

const (
	BarrageOrdinary BarragePriority = iota
	BarragePaid
	BarrageAdmin
)

const (
	// Defaults permit 32 busy rooms at 5,000 candidates each. Normalization
	// allows more rooms only when the per-room cap keeps the full window within
	// the fixed 250,000-candidate cost envelope.
	defaultMaxActiveRoomsPerWindow = 32
	maxActiveRoomsPerWindow        = 4_096
	maxUniquePerRoomWindow         = 5_000
	maxCandidatesPerWindow         = 250_000
)

type Barrage struct {
	ID          string          `json:"id"`
	RoomID      string          `json:"roomId"`
	UserID      string          `json:"-"`
	DisplayName string          `json:"displayName,omitempty"`
	Content     string          `json:"content"`
	Priority    BarragePriority `json:"priority"`
	CreatedAtMs int64           `json:"createdAtMs"`
	Count       int             `json:"count,omitempty"`
}

type BarrageConfig struct {
	QueueCapacity           int
	Window                  time.Duration
	MaxActiveRoomsPerWindow int
	MaxPerRoomWindow        int
	AdminReserved           int
	PaidReserved            int
	MaxUniquePerRoomWindow  int
	MaxContentRunes         int
	GlobalUserBurst         float64
	GlobalUserRatePerSecond float64
	GlobalRateEntryCapacity int
	RoomUserBurst           float64
	RoomUserRatePerSecond   float64
	RoomRateEntryCapacity   int
	// UserBurst and UserRatePerSecond are retained as compatibility aliases.
	// When set, they override both limiter scopes during normalization.
	UserBurst         float64
	UserRatePerSecond float64
	RateEntryTTL      time.Duration
	SensitiveWords    []string
}

func DefaultBarrageConfig() BarrageConfig {
	return BarrageConfig{
		QueueCapacity:           100_000,
		Window:                  100 * time.Millisecond,
		MaxActiveRoomsPerWindow: defaultMaxActiveRoomsPerWindow,
		MaxPerRoomWindow:        30,
		AdminReserved:           3,
		PaidReserved:            7,
		MaxUniquePerRoomWindow:  5_000,
		MaxContentRunes:         100,
		GlobalUserBurst:         5,
		GlobalUserRatePerSecond: 2,
		GlobalRateEntryCapacity: defaultRateEntryCapacity,
		RoomUserBurst:           5,
		RoomUserRatePerSecond:   2,
		RoomRateEntryCapacity:   defaultRateEntryCapacity,
		RateEntryTTL:            10 * time.Minute,
		SensitiveWords:          nil,
	}
}

func NormalizeBarrageConfig(cfg BarrageConfig) (BarrageConfig, error) {
	if cfg.Window > 0 && cfg.Window < time.Millisecond {
		return BarrageConfig{}, errors.New("barrage window must be at least 1ms")
	}
	if cfg.QueueCapacity < 0 {
		return BarrageConfig{}, errors.New("barrage queue capacity must not be negative")
	}
	if cfg.Window < 0 {
		return BarrageConfig{}, errors.New("barrage window must not be negative")
	}
	if cfg.MaxActiveRoomsPerWindow < 0 {
		return BarrageConfig{}, errors.New("barrage active room capacity must not be negative")
	}
	if cfg.MaxPerRoomWindow < 0 {
		return BarrageConfig{}, errors.New("barrage room output budget must not be negative")
	}
	if cfg.AdminReserved < 0 {
		return BarrageConfig{}, errors.New("barrage admin reserved budget must not be negative")
	}
	if cfg.PaidReserved < 0 {
		return BarrageConfig{}, errors.New("barrage paid reserved budget must not be negative")
	}
	if cfg.MaxUniquePerRoomWindow < 0 {
		return BarrageConfig{}, errors.New("barrage unique capacity must not be negative")
	}
	if cfg.MaxContentRunes < 0 {
		return BarrageConfig{}, errors.New("barrage content capacity must not be negative")
	}
	if cfg.GlobalUserBurst < 0 || math.IsNaN(cfg.GlobalUserBurst) || math.IsInf(cfg.GlobalUserBurst, 0) {
		return BarrageConfig{}, errors.New("barrage global user burst must be finite and not negative")
	}
	if cfg.GlobalUserRatePerSecond < 0 || math.IsNaN(cfg.GlobalUserRatePerSecond) || math.IsInf(cfg.GlobalUserRatePerSecond, 0) {
		return BarrageConfig{}, errors.New("barrage global user rate must be finite and not negative")
	}
	if cfg.GlobalRateEntryCapacity < 0 {
		return BarrageConfig{}, errors.New("barrage global rate entry capacity must not be negative")
	}
	if cfg.GlobalRateEntryCapacity > 0 && cfg.GlobalRateEntryCapacity < barrageRateShards {
		return BarrageConfig{}, errors.New("barrage global rate entry capacity must cover every shard")
	}
	if cfg.RoomUserBurst < 0 || math.IsNaN(cfg.RoomUserBurst) || math.IsInf(cfg.RoomUserBurst, 0) {
		return BarrageConfig{}, errors.New("barrage room user burst must be finite and not negative")
	}
	if cfg.RoomUserRatePerSecond < 0 || math.IsNaN(cfg.RoomUserRatePerSecond) || math.IsInf(cfg.RoomUserRatePerSecond, 0) {
		return BarrageConfig{}, errors.New("barrage room user rate must be finite and not negative")
	}
	if cfg.RoomRateEntryCapacity < 0 {
		return BarrageConfig{}, errors.New("barrage room rate entry capacity must not be negative")
	}
	if cfg.RoomRateEntryCapacity > 0 && cfg.RoomRateEntryCapacity < barrageRateShards {
		return BarrageConfig{}, errors.New("barrage room rate entry capacity must cover every shard")
	}
	if cfg.UserBurst < 0 || math.IsNaN(cfg.UserBurst) || math.IsInf(cfg.UserBurst, 0) {
		return BarrageConfig{}, errors.New("barrage user burst must be finite and not negative")
	}
	if cfg.UserRatePerSecond < 0 || math.IsNaN(cfg.UserRatePerSecond) || math.IsInf(cfg.UserRatePerSecond, 0) {
		return BarrageConfig{}, errors.New("barrage user rate must be finite and not negative")
	}
	if cfg.RateEntryTTL < 0 {
		return BarrageConfig{}, errors.New("barrage rate entry ttl must not be negative")
	}

	defaults := DefaultBarrageConfig()
	if cfg.QueueCapacity == 0 {
		cfg.QueueCapacity = defaults.QueueCapacity
	}
	if cfg.Window == 0 {
		cfg.Window = defaults.Window
	}
	if cfg.MaxActiveRoomsPerWindow == 0 {
		cfg.MaxActiveRoomsPerWindow = defaults.MaxActiveRoomsPerWindow
	}
	if cfg.MaxPerRoomWindow == 0 {
		cfg.MaxPerRoomWindow = defaults.MaxPerRoomWindow
	}
	if cfg.AdminReserved == 0 {
		cfg.AdminReserved = defaults.AdminReserved
	}
	if cfg.PaidReserved == 0 {
		cfg.PaidReserved = defaults.PaidReserved
	}
	if cfg.MaxUniquePerRoomWindow == 0 {
		cfg.MaxUniquePerRoomWindow = defaults.MaxUniquePerRoomWindow
	}
	if cfg.MaxActiveRoomsPerWindow > maxActiveRoomsPerWindow {
		return BarrageConfig{}, errors.New("barrage active room capacity exceeds 4096")
	}
	if cfg.MaxUniquePerRoomWindow > maxUniquePerRoomWindow {
		return BarrageConfig{}, errors.New("barrage per-room unique candidate capacity exceeds 5000")
	}
	if cfg.MaxUniquePerRoomWindow > maxCandidatesPerWindow/cfg.MaxActiveRoomsPerWindow {
		return BarrageConfig{}, errors.New("barrage window candidate capacity exceeds 250000")
	}
	if cfg.MaxContentRunes == 0 {
		cfg.MaxContentRunes = defaults.MaxContentRunes
	}
	if cfg.UserBurst > 0 {
		cfg.GlobalUserBurst = cfg.UserBurst
		cfg.RoomUserBurst = cfg.UserBurst
	}
	if cfg.UserRatePerSecond > 0 {
		cfg.GlobalUserRatePerSecond = cfg.UserRatePerSecond
		cfg.RoomUserRatePerSecond = cfg.UserRatePerSecond
	}
	if cfg.GlobalUserBurst == 0 {
		cfg.GlobalUserBurst = defaults.GlobalUserBurst
	}
	if cfg.GlobalUserRatePerSecond == 0 {
		cfg.GlobalUserRatePerSecond = defaults.GlobalUserRatePerSecond
	}
	if cfg.GlobalRateEntryCapacity == 0 {
		cfg.GlobalRateEntryCapacity = defaults.GlobalRateEntryCapacity
	}
	if cfg.RoomUserBurst == 0 {
		cfg.RoomUserBurst = defaults.RoomUserBurst
	}
	if cfg.RoomUserRatePerSecond == 0 {
		cfg.RoomUserRatePerSecond = defaults.RoomUserRatePerSecond
	}
	if cfg.RoomRateEntryCapacity == 0 {
		cfg.RoomRateEntryCapacity = defaults.RoomRateEntryCapacity
	}
	if cfg.RateEntryTTL == 0 {
		cfg.RateEntryTTL = defaults.RateEntryTTL
	}
	if cfg.AdminReserved > math.MaxInt-cfg.PaidReserved {
		return BarrageConfig{}, errors.New("combined barrage reserved budgets overflow")
	}
	if len(cfg.SensitiveWords) > 0 {
		cfg.SensitiveWords = append([]string(nil), cfg.SensitiveWords...)
	}
	for i := range cfg.SensitiveWords {
		cfg.SensitiveWords[i] = strings.ToLower(strings.TrimSpace(cfg.SensitiveWords[i]))
	}
	return cfg, nil
}

type BarrageModeration interface {
	IsMuted(ctx context.Context, roomID, userID string) (bool, error)
}

type barrageBatchSink interface {
	deliverPublicBatch(roomID string, payload []byte)
}

type BarrageService struct {
	cfg           BarrageConfig
	queue         chan Barrage
	sink          barrageBatchSink
	moderation    BarrageModeration
	globalLimiter *barrageRateLimiter
	limiter       *barrageRateLimiter
	now           func() time.Time
	stopOnce      sync.Once
}

func NewBarrageService(cfg BarrageConfig, sink barrageBatchSink, moderation BarrageModeration) (*BarrageService, error) {
	cfg, err := NormalizeBarrageConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &BarrageService{
		cfg: cfg, queue: make(chan Barrage, cfg.QueueCapacity), sink: sink,
		moderation:    moderation,
		globalLimiter: newBarrageRateLimiter(cfg.GlobalUserBurst, cfg.GlobalUserRatePerSecond, cfg.RateEntryTTL, cfg.GlobalRateEntryCapacity),
		limiter:       newBarrageRateLimiter(cfg.RoomUserBurst, cfg.RoomUserRatePerSecond, cfg.RateEntryTTL, cfg.RoomRateEntryCapacity),
		now:           time.Now,
	}, nil
}

// SubmitPublic is the gateway admission path. Priority is intentionally forced
// to ordinary: paid/admin priority may only be assigned by trusted server code.
func (s *BarrageService) SubmitPublic(ctx context.Context, message Barrage) (err error) {
	defer func() { observability.RecordBarrageAdmission(barrageAdmissionResult(err)) }()
	message.Priority = BarrageOrdinary
	if err := s.validateContent(message); err != nil {
		return err
	}
	now := s.now()
	if !s.globalLimiter.Allow(message.UserID, now) {
		return ErrBarrageRateLimited
	}
	if !s.limiter.Allow(message.RoomID+"\x00"+message.UserID, now) {
		return ErrBarrageRateLimited
	}
	if s.moderation != nil {
		muted, err := s.moderation.IsMuted(ctx, message.RoomID, message.UserID)
		if err != nil {
			return ErrBarrageOverloaded
		}
		if muted {
			return ErrBarrageMuted
		}
	}
	if err := s.validateSensitiveContent(message.Content); err != nil {
		return err
	}
	message.CreatedAtMs = now.UnixMilli()
	return s.enqueue(message)
}

// SubmitTrusted admits paid and administrative messages from internal services.
func (s *BarrageService) SubmitTrusted(message Barrage) (err error) {
	defer func() { observability.RecordBarrageAdmission(barrageAdmissionResult(err)) }()
	if err := s.validateContent(message); err != nil {
		return err
	}
	if err := s.validateSensitiveContent(message.Content); err != nil {
		return err
	}
	message.CreatedAtMs = s.now().UnixMilli()
	return s.enqueue(message)
}

func barrageAdmissionResult(err error) string {
	switch {
	case err == nil:
		return "accepted"
	case errors.Is(err, ErrBarrageInvalid):
		return "invalid"
	case errors.Is(err, ErrBarrageRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrBarrageMuted):
		return "muted"
	case errors.Is(err, ErrBarrageSensitive):
		return "sensitive"
	case errors.Is(err, ErrBarrageOverloaded):
		return "overloaded"
	default:
		return "other"
	}
}

func (s *BarrageService) validateContent(message Barrage) error {
	message.RoomID = strings.TrimSpace(message.RoomID)
	message.UserID = strings.TrimSpace(message.UserID)
	content := strings.TrimSpace(message.Content)
	if message.RoomID == "" || message.UserID == "" || content == "" || !utf8.ValidString(content) || utf8.RuneCountInString(content) > s.cfg.MaxContentRunes {
		return ErrBarrageInvalid
	}
	return nil
}

func (s *BarrageService) validateSensitiveContent(content string) error {
	lower := strings.ToLower(strings.TrimSpace(content))
	for _, word := range s.cfg.SensitiveWords {
		if word != "" && strings.Contains(lower, word) {
			return ErrBarrageSensitive
		}
	}
	return nil
}

func (s *BarrageService) enqueue(message Barrage) error {
	message.RoomID = strings.TrimSpace(message.RoomID)
	message.UserID = strings.TrimSpace(message.UserID)
	message.DisplayName = strings.TrimSpace(message.DisplayName)
	message.Content = strings.TrimSpace(message.Content)
	select {
	case s.queue <- message:
		return nil
	default:
		// Bounded memory is more important than accepting another ordinary
		// comment. Trusted callers may retry high-priority messages.
		return ErrBarrageOverloaded
	}
}

func (s *BarrageService) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.Window)
	defer ticker.Stop()
	rooms := make(map[string]map[string]*Barrage)
	inputByRoom := make(map[string]int)
	for {
		select {
		case <-ctx.Done():
			s.flush(rooms, inputByRoom, s.now())
			return
		case message := <-s.queue:
			s.consumeWindowMessage(rooms, inputByRoom, message)
		case now := <-ticker.C:
			s.flush(rooms, inputByRoom, now)
			rooms = make(map[string]map[string]*Barrage)
			inputByRoom = make(map[string]int)
		}
	}
}

const (
	barrageOverflowRoomLimit      = "room_limit"
	barrageOverflowCandidateLimit = "candidate_limit"
)

type barrageAggregateOutcome struct {
	roomTracked   bool
	droppedReason string
	droppedCount  int
}

func (s *BarrageService) consumeWindowMessage(rooms map[string]map[string]*Barrage, inputByRoom map[string]int, message Barrage) barrageAggregateOutcome {
	outcome := s.aggregate(rooms, message)
	if outcome.roomTracked && inputByRoom[message.RoomID] < math.MaxInt {
		inputByRoom[message.RoomID]++
	}
	if outcome.droppedCount > 0 {
		observability.RecordBarrageOverflowDrops(outcome.droppedReason, outcome.droppedCount)
	}
	return outcome
}

func (s *BarrageService) aggregate(rooms map[string]map[string]*Barrage, message Barrage) barrageAggregateOutcome {
	items, roomTracked := rooms[message.RoomID]
	if !roomTracked {
		if len(rooms) >= s.cfg.MaxActiveRoomsPerWindow {
			return barrageAggregateOutcome{droppedReason: barrageOverflowRoomLimit, droppedCount: 1}
		}
		items = make(map[string]*Barrage, min(s.cfg.MaxUniquePerRoomWindow, 64))
		rooms[message.RoomID] = items
		roomTracked = true
	}
	key := strings.ToLower(strings.Join(strings.Fields(message.Content), " "))
	if existing := items[key]; existing != nil {
		if existing.Count < math.MaxInt {
			existing.Count++
		}
		if message.Priority > existing.Priority {
			count := existing.Count
			*existing = message
			existing.Count = count
		}
		return barrageAggregateOutcome{roomTracked: true}
	}
	if len(items) >= s.cfg.MaxUniquePerRoomWindow {
		if message.Priority == BarrageOrdinary {
			return barrageAggregateOutcome{roomTracked: true, droppedReason: barrageOverflowCandidateLimit, droppedCount: 1}
		}
		victimKey, victim := deterministicBarrageVictim(items, message.Priority)
		if victim == nil {
			return barrageAggregateOutcome{roomTracked: true, droppedReason: barrageOverflowCandidateLimit, droppedCount: 1}
		}
		delete(items, victimKey)
		message.Count = 1
		items[key] = &message
		return barrageAggregateOutcome{roomTracked: true, droppedReason: barrageOverflowCandidateLimit, droppedCount: max(1, victim.Count)}
	}
	message.Count = 1
	items[key] = &message
	return barrageAggregateOutcome{roomTracked: roomTracked}
}

func deterministicBarrageVictim(items map[string]*Barrage, incomingPriority BarragePriority) (string, *Barrage) {
	var victimKey string
	var victim *Barrage
	for key, candidate := range items {
		if candidate.Priority >= incomingPriority {
			continue
		}
		// A bounded O(per-room-cap) scan is intentional. Prefer the lowest
		// priority, then the lexicographically greatest normalized content key,
		// so map iteration order cannot affect degradation.
		if victim == nil || candidate.Priority < victim.Priority || (candidate.Priority == victim.Priority && key > victimKey) {
			victimKey, victim = key, candidate
		}
	}
	return victimKey, victim
}

func (s *BarrageService) flush(rooms map[string]map[string]*Barrage, inputByRoom map[string]int, now time.Time) {
	if s.sink == nil {
		return
	}
	for roomID, grouped := range rooms {
		startedAt := time.Now()
		selected := selectBarragesWithReserved(
			grouped,
			s.cfg.MaxPerRoomWindow,
			now.UnixMilli()/s.cfg.Window.Milliseconds(),
			s.cfg.AdminReserved,
			s.cfg.PaidReserved,
		)
		if len(selected) > 0 {
			payload, err := json.Marshal(struct {
				Type     string    `json:"type"`
				RoomID   string    `json:"roomId"`
				WindowMs int64     `json:"windowMs"`
				Messages []Barrage `json:"messages"`
			}{"BARRAGE_BATCH", roomID, s.cfg.Window.Milliseconds(), selected})
			if err == nil {
				s.sink.deliverPublicBatch(roomID, payload)
			}
		}
		observability.ObserveBarrageWindow(inputByRoom[roomID], len(grouped), len(selected), time.Since(startedAt))
	}
}

type rankedBarrage struct {
	message Barrage
	score   uint64
}

type rankedBarrageHeap []rankedBarrage

func (h rankedBarrageHeap) Len() int { return len(h) }

func (h rankedBarrageHeap) Less(i, j int) bool {
	return rankedBarrageBetter(h[j], h[i])
}

func (h rankedBarrageHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *rankedBarrageHeap) Push(value any) {
	*h = append(*h, value.(rankedBarrage))
}

func (h *rankedBarrageHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	old[last] = rankedBarrage{}
	*h = old[:last]
	return value
}

func rankedBarrageBetter(left, right rankedBarrage) bool {
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

func barrageScore(message *Barrage, window int64) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(message.ID + "\x00" + message.UserID + "\x00" + message.Content))
	score := h.Sum64() ^ uint64(window)
	// Very short/repetitive comments remain eligible but lose sampling
	// weight. Aggregation count is exposed to the client instead of sending
	// the same text repeatedly.
	if utf8.RuneCountInString(message.Content) < 3 {
		score >>= 2
	}
	return score
}

func keepTopBarrage(h *rankedBarrageHeap, capacity int, candidate rankedBarrage) {
	if capacity <= 0 {
		return
	}
	if h.Len() < capacity {
		heap.Push(h, candidate)
		return
	}
	if rankedBarrageBetter(candidate, (*h)[0]) {
		(*h)[0] = candidate
		heap.Fix(h, 0)
	}
}

func trimRankedBarrageHeap(h *rankedBarrageHeap, limit int) {
	for h.Len() > limit {
		heap.Pop(h)
	}
}

func selectBarrages(grouped map[string]*Barrage, limit int, window int64) []Barrage {
	defaults := DefaultBarrageConfig()
	return selectBarragesWithReserved(grouped, limit, window, defaults.AdminReserved, defaults.PaidReserved)
}

func selectBarragesWithReserved(grouped map[string]*Barrage, limit int, window int64, adminReserved, paidReserved int) []Barrage {
	if limit <= 0 {
		return nil
	}
	adminBudget := min(limit, max(0, adminReserved))
	paidMaxBudget := min(limit, max(0, paidReserved)+adminBudget)
	candidateCount := len(grouped)
	tiers := [3]rankedBarrageHeap{
		make(rankedBarrageHeap, 0, min(limit, candidateCount)),
		make(rankedBarrageHeap, 0, min(paidMaxBudget, candidateCount)),
		make(rankedBarrageHeap, 0, min(adminBudget, candidateCount)),
	}
	for _, message := range grouped {
		tier := BarrageOrdinary
		switch message.Priority {
		case BarragePaid:
			tier = BarragePaid
		case BarrageAdmin:
			tier = BarrageAdmin
		}
		capacity := limit
		switch tier {
		case BarragePaid:
			capacity = paidMaxBudget
		case BarrageAdmin:
			capacity = adminBudget
		}
		keepTopBarrage(&tiers[tier], capacity, rankedBarrage{message: *message, score: barrageScore(message, window)})
	}

	adminCount := min(tiers[BarrageAdmin].Len(), adminBudget)
	paidBudget := min(limit-adminCount, max(0, paidReserved)+(adminBudget-adminCount))
	paidCount := min(tiers[BarragePaid].Len(), paidBudget)
	ordinaryCount := min(tiers[BarrageOrdinary].Len(), limit-adminCount-paidCount)
	trimRankedBarrageHeap(&tiers[BarrageAdmin], adminCount)
	trimRankedBarrageHeap(&tiers[BarragePaid], paidCount)
	trimRankedBarrageHeap(&tiers[BarrageOrdinary], ordinaryCount)

	items := make([]rankedBarrage, 0, adminCount+paidCount+ordinaryCount)
	for tier := BarrageAdmin; ; tier-- {
		items = append(items, tiers[tier]...)
		if tier == BarrageOrdinary {
			break
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].message.Priority != items[j].message.Priority {
			return items[i].message.Priority > items[j].message.Priority
		}
		return rankedBarrageBetter(items[i], items[j])
	})
	out := make([]Barrage, len(items))
	for i := range items {
		out[i] = items[i].message
	}
	return out
}

type rateEntry struct {
	tokens        float64
	updated, seen time.Time
}

const (
	barrageRateShards         = 64
	barrageRateEvictionSample = 16
	defaultRateEntryCapacity  = 100_000
)

type barrageRateShard struct {
	mu       sync.Mutex
	entries  map[string]rateEntry
	capacity int
}

type barrageRateLimiter struct {
	shards      [barrageRateShards]barrageRateShard
	burst, rate float64
	ttl         time.Duration
}

func newBarrageRateLimiter(burst, rate float64, ttl time.Duration, capacities ...int) *barrageRateLimiter {
	limiter := &barrageRateLimiter{burst: burst, rate: rate, ttl: ttl}
	capacity := defaultRateEntryCapacity
	if len(capacities) > 0 {
		capacity = max(0, capacities[0])
	}
	perShard := capacity / barrageRateShards
	remainder := capacity % barrageRateShards
	for i := range limiter.shards {
		shardCapacity := perShard
		if i < remainder {
			shardCapacity++
		}
		limiter.shards[i].capacity = shardCapacity
		limiter.shards[i].entries = make(map[string]rateEntry, min(shardCapacity, 64))
	}
	return limiter
}

func (l *barrageRateLimiter) Allow(key string, now time.Time) bool {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	shard := &l.shards[h.Sum32()%barrageRateShards]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	entry, ok := shard.entries[key]
	if !ok {
		entry = rateEntry{tokens: l.burst, updated: now}
		if shard.capacity > 0 && len(shard.entries) > 0 {
			cutoff := time.Time{}
			if l.ttl > 0 {
				cutoff = now.Add(-l.ttl)
			}
			// Saturation is deliberately approximate: evicting one old sampled
			// identity may grant it a fresh burst later, but admission remains
			// lossy while memory and lock work stay strictly bounded.
			shard.evictSampledEntry(cutoff, len(shard.entries) >= shard.capacity)
		}
	}
	entry.tokens += now.Sub(entry.updated).Seconds() * l.rate
	if entry.tokens > l.burst {
		entry.tokens = l.burst
	}
	entry.updated, entry.seen = now, now
	allowed := entry.tokens >= 1
	if allowed {
		entry.tokens--
	}
	if shard.capacity > 0 {
		shard.entries[key] = entry
	}
	return allowed
}

func (s *barrageRateShard) evictSampledEntry(expiredBefore time.Time, required bool) {
	var oldestKey string
	var oldestSeen time.Time
	sampled := 0
	for key, entry := range s.entries {
		if sampled == 0 || entry.seen.Before(oldestSeen) {
			oldestKey, oldestSeen = key, entry.seen
		}
		sampled++
		if sampled >= barrageRateEvictionSample {
			break
		}
	}
	if sampled > 0 && (required || (!expiredBefore.IsZero() && oldestSeen.Before(expiredBefore))) {
		delete(s.entries, oldestKey)
	}
}
