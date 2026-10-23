package realtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	auctionv1 "live-auction-bid/backend/api/auction/service/v1"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"live-auction-bid/backend/app/auction/service/internal/pkg/auth"
)

type countingPublicRoomValidator struct {
	exists  bool
	err     error
	calls   int
	roomIDs []string
}

func (v *countingPublicRoomValidator) PublicRoomExists(_ context.Context, roomID string) (bool, error) {
	v.calls++
	v.roomIDs = append(v.roomIDs, roomID)
	return v.exists, v.err
}

type countingBarrageModeration struct {
	muted bool
	err   error
	calls int
}

func (m *countingBarrageModeration) IsMuted(context.Context, string, string) (bool, error) {
	m.calls++
	return m.muted, m.err
}

type barrageHTTPFixture struct {
	hub        *Hub
	service    *BarrageService
	rooms      *countingPublicRoomValidator
	moderation *countingBarrageModeration
	token      string
}

func newBarrageHTTPFixture(t *testing.T, mutate func(*BarrageConfig)) *barrageHTTPFixture {
	t.Helper()
	manager, err := auth.NewManager(auth.Config{
		Secret: "barrage-http-test-secret", Issuer: "barrage-http-test",
		AccessTTL: time.Hour, RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := manager.IssueTokenPair(&auctionv1.User{
		Id: "viewer-1", MainAccountId: "merchant-1",
		RoleCodes: []string{userbiz.RoleBuyer}, Status: auctionv1.UserStatus_USER_STATUS_ACTIVE,
	})
	if err != nil {
		t.Fatal(err)
	}
	rooms := &countingPublicRoomValidator{exists: true}
	moderation := &countingBarrageModeration{}
	cfg := DefaultBarrageConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	service := newTestBarrageService(t, cfg, nil, moderation)
	service.now = func() time.Time { return time.Unix(100, 0) }
	hub := NewHub(nil)
	hub.BindAuthManager(manager)
	hub.BindPublicRoomValidator(rooms)
	hub.BindBarrageService(service)
	return &barrageHTTPFixture{hub: hub, service: service, rooms: rooms, moderation: moderation, token: pair.AccessToken}
}

func (f *barrageHTTPFixture) request(roomID, body string) *httptest.ResponseRecorder {
	return f.requestBytes(roomID, []byte(body))
}

func (f *barrageHTTPFixture) requestBytes(roomID string, body []byte) *httptest.ResponseRecorder {
	return f.requestReader(roomID, bytes.NewReader(body))
}

func (f *barrageHTTPFixture) requestReader(roomID string, body io.Reader) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/realtime/rooms/"+url.PathEscape(roomID)+"/barrages", body)
	request.Header.Set("Authorization", "Bearer "+f.token)
	response := httptest.NewRecorder()
	f.hub.ServeBarrage(response, request, roomID)
	return response
}

func TestServeBarrageRequiresAuthenticationBeforeRoomLookup(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/realtime/rooms/room-1/barrages", strings.NewReader(`{"content":"hello"}`))
	response := httptest.NewRecorder()

	fixture.hub.ServeBarrage(response, request, "room-1")

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if fixture.rooms.calls != 0 || fixture.moderation.calls != 0 || len(fixture.service.queue) != 0 {
		t.Fatalf("unauthenticated request reached admission: room=%d moderation=%d queue=%d", fixture.rooms.calls, fixture.moderation.calls, len(fixture.service.queue))
	}
}

func TestServeBarrageRejectsTrailingJSONDocument(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, nil)
	response := fixture.request("room-1", `{"content":"hello"}{"content":"again"}`)
	assertBarrageHTTPRejectedBody(t, fixture, response, http.StatusBadRequest)
}

func TestServeBarrageRejectsTrailingGarbage(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, nil)
	response := fixture.request("room-1", `{"content":"hello"}garbage`)
	assertBarrageHTTPRejectedBody(t, fixture, response, http.StatusBadRequest)
}

func TestServeBarrageRejectsBodyOverLimitAfterValidFirstDocument(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, nil)
	valid := []byte(`{"content":"hello"}`)
	body := append(valid, bytes.Repeat([]byte(" "), maxBarrageRequestBytes+1-len(valid))...)
	response := fixture.requestBytes("room-1", body)
	assertBarrageHTTPRejectedBody(t, fixture, response, http.StatusRequestEntityTooLarge)
}

func TestServeBarragePrefersTooLargeWhenOversizedReadAlsoErrors(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, nil)
	body := &dataThenErrorReader{
		data: bytes.Repeat([]byte(" "), maxBarrageRequestBytes+1),
		err:  errors.New("truncated request body"),
	}
	response := fixture.requestReader("room-1", body)
	assertBarrageHTTPRejectedBody(t, fixture, response, http.StatusRequestEntityTooLarge)
}

func TestServeBarrageRejectsInvalidUTF8(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, nil)
	body := append([]byte(`{"content":"`), 0xff)
	body = append(body, []byte(`"}`)...)
	response := fixture.requestBytes("room-1", body)
	assertBarrageHTTPRejectedBody(t, fixture, response, http.StatusBadRequest)
}

func TestServeBarrageAcceptsExactLimitValidBody(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, nil)
	valid := []byte(`{"content":"hello"}`)
	body := append(valid, bytes.Repeat([]byte(" "), maxBarrageRequestBytes-len(valid))...)

	response := fixture.requestBytes("room-1", body)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%q", response.Code, http.StatusAccepted, response.Body.String())
	}
	if len(fixture.service.queue) != 1 {
		t.Fatalf("submitted barrages = %d, want 1", len(fixture.service.queue))
	}
}

func TestServeBarrageReturnsUnavailableOnModerationError(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, nil)
	fixture.moderation.err = errors.New("redis unavailable")

	response := fixture.request("room-1", `{"content":"hello"}`)

	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("status=%d retry-after=%q", response.Code, response.Header().Get("Retry-After"))
	}
	if len(fixture.service.queue) != 0 {
		t.Fatalf("submitted barrages = %d, want 0", len(fixture.service.queue))
	}
}

func TestServeBarrageReturnsUnavailableWhenQueueIsFull(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, func(cfg *BarrageConfig) {
		cfg.QueueCapacity = 1
		cfg.GlobalUserBurst = 10
		cfg.RoomUserBurst = 10
	})
	fixture.service.queue <- Barrage{ID: "existing", RoomID: "room-1", UserID: "viewer-other", Content: "existing"}

	response := fixture.request("room-1", `{"content":"hello"}`)

	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("status=%d retry-after=%q", response.Code, response.Header().Get("Retry-After"))
	}
	if len(fixture.service.queue) != 1 {
		t.Fatalf("queue length = %d, want existing item only", len(fixture.service.queue))
	}
}

func assertBarrageHTTPRejectedBody(t *testing.T, fixture *barrageHTTPFixture, response *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%q", response.Code, wantStatus, response.Body.String())
	}
	if fixture.rooms.calls != 0 {
		t.Fatalf("room lookup calls = %d, want 0", fixture.rooms.calls)
	}
	if fixture.moderation.calls != 0 || len(fixture.service.queue) != 0 {
		t.Fatalf("rejected body reached submission: moderation=%d queue=%d", fixture.moderation.calls, len(fixture.service.queue))
	}
}

type dataThenErrorReader struct {
	data []byte
	err  error
}

func (r *dataThenErrorReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func TestServeBarrageRejectsInvalidRoomSyntaxBeforeLookupAndSubmission(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, nil)

	response := fixture.requestReader("invalid/room", panicOnRead{})

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if fixture.rooms.calls != 0 || fixture.moderation.calls != 0 || len(fixture.service.queue) != 0 {
		t.Fatalf("invalid room reached admission: room calls=%d moderation calls=%d queue=%d", fixture.rooms.calls, fixture.moderation.calls, len(fixture.service.queue))
	}
}

type panicOnRead struct{}

func (panicOnRead) Read([]byte) (int, error) {
	panic("request body must not be read")
}

func TestServeBarrageRejectsSpacePaddedRoomIDBeforeLookupAndSubmission(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, nil)

	response := fixture.request(" room-1 ", `{"content":"hello"}`)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if fixture.rooms.calls != 0 || fixture.moderation.calls != 0 || len(fixture.service.queue) != 0 {
		t.Fatalf("space-padded room reached admission: room calls=%d moderation calls=%d queue=%d", fixture.rooms.calls, fixture.moderation.calls, len(fixture.service.queue))
	}
}

func TestServeBarrageRejectsMalformedBodyBeforeMissingRoomLookup(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, nil)
	fixture.rooms.exists = false

	response := fixture.request("missing-room", `{not-json`)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if fixture.rooms.calls != 0 || fixture.moderation.calls != 0 || len(fixture.service.queue) != 0 {
		t.Fatalf("malformed body reached admission: room calls=%d moderation calls=%d queue=%d", fixture.rooms.calls, fixture.moderation.calls, len(fixture.service.queue))
	}
}

func TestServeBarrageReturnsNotFoundForValidBodyBeforeSubmission(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, nil)
	fixture.rooms.exists = false

	response := fixture.request("missing-room", `{"content":"hello"}`)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
	if fixture.rooms.calls != 1 || fixture.moderation.calls != 0 || len(fixture.service.queue) != 0 {
		t.Fatalf("missing room reached submission: room calls=%d moderation calls=%d queue=%d", fixture.rooms.calls, fixture.moderation.calls, len(fixture.service.queue))
	}
}

func TestServeBarrageReturnsUnavailableOnRoomLookupErrorBeforeSubmission(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, nil)
	fixture.rooms.err = errors.New("room repository unavailable")

	response := fixture.request("room-1", `{"content":"hello"}`)

	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("status=%d retry-after=%q", response.Code, response.Header().Get("Retry-After"))
	}
	if fixture.moderation.calls != 0 || len(fixture.service.queue) != 0 {
		t.Fatalf("lookup failure reached submission: moderation calls=%d queue=%d", fixture.moderation.calls, len(fixture.service.queue))
	}
}

func TestServeBarrageMutedPrecedesSensitiveFiltering(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, func(cfg *BarrageConfig) {
		cfg.SensitiveWords = []string{"blocked"}
	})
	fixture.moderation.muted = true

	response := fixture.request("room-1", `{"content":"blocked text"}`)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	if fixture.rooms.calls != 1 || fixture.moderation.calls != 1 || len(fixture.service.queue) != 0 {
		t.Fatalf("muted admission calls: room=%d moderation=%d queue=%d", fixture.rooms.calls, fixture.moderation.calls, len(fixture.service.queue))
	}
}

func TestGlobalBarrageRateLimitCannotBeBypassedByAlternatingRooms(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, func(cfg *BarrageConfig) {
		cfg.GlobalUserBurst = 5
		cfg.GlobalUserRatePerSecond = 0.0001
		cfg.RoomUserBurst = 5
		cfg.RoomUserRatePerSecond = 0.0001
	})

	for i := 0; i < 6; i++ {
		response := fixture.request("room-"+string(rune('a'+i)), `{"content":"hello"}`)
		want := http.StatusAccepted
		if i == 5 {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("request %d status = %d, want %d", i+1, response.Code, want)
		}
	}
	if fixture.rooms.calls != 6 {
		t.Fatalf("room lookup calls = %d, want 6", fixture.rooms.calls)
	}
	if fixture.moderation.calls != 5 {
		t.Fatalf("moderation calls = %d, want 5; global limiter must run first", fixture.moderation.calls)
	}
	if len(fixture.service.queue) != 5 {
		t.Fatalf("submitted barrages = %d, want 5", len(fixture.service.queue))
	}
}

func TestRoomBarrageRateLimitRunsBeforeModeration(t *testing.T) {
	fixture := newBarrageHTTPFixture(t, func(cfg *BarrageConfig) {
		cfg.GlobalUserBurst = 10
		cfg.GlobalUserRatePerSecond = 0.0001
		cfg.RoomUserBurst = 1
		cfg.RoomUserRatePerSecond = 0.0001
	})

	first := fixture.request("room-1", `{"content":"hello"}`)
	second := fixture.request("room-1", `{"content":"hello again"}`)

	if first.Code != http.StatusAccepted || second.Code != http.StatusTooManyRequests {
		t.Fatalf("statuses = %d, %d; want %d, %d", first.Code, second.Code, http.StatusAccepted, http.StatusTooManyRequests)
	}
	if fixture.moderation.calls != 1 {
		t.Fatalf("moderation calls = %d, want 1; room limiter must run first", fixture.moderation.calls)
	}
}

func TestBarrageAdmissionConfigRejectsNonFiniteLimiterValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*BarrageConfig)
	}{
		{name: "global burst NaN", mutate: func(cfg *BarrageConfig) { cfg.GlobalUserBurst = math.NaN() }},
		{name: "global rate infinity", mutate: func(cfg *BarrageConfig) { cfg.GlobalUserRatePerSecond = math.Inf(1) }},
		{name: "room burst infinity", mutate: func(cfg *BarrageConfig) { cfg.RoomUserBurst = math.Inf(1) }},
		{name: "room rate NaN", mutate: func(cfg *BarrageConfig) { cfg.RoomUserRatePerSecond = math.NaN() }},
		{name: "legacy burst NaN", mutate: func(cfg *BarrageConfig) { cfg.UserBurst = math.NaN() }},
		{name: "legacy rate infinity", mutate: func(cfg *BarrageConfig) { cfg.UserRatePerSecond = math.Inf(1) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultBarrageConfig()
			test.mutate(&cfg)
			if _, err := NormalizeBarrageConfig(cfg); err == nil {
				t.Fatal("expected non-finite limiter value to be rejected")
			}
		})
	}
}

func TestBarrageRateEntryCapacityDefaultsAndRejectsNegativeValues(t *testing.T) {
	defaults := DefaultBarrageConfig()
	if defaults.GlobalRateEntryCapacity < 100_000 {
		t.Fatalf("global rate-entry capacity = %d, want at least 100000", defaults.GlobalRateEntryCapacity)
	}
	if defaults.RoomRateEntryCapacity < 100_000 {
		t.Fatalf("room rate-entry capacity = %d, want at least 100000", defaults.RoomRateEntryCapacity)
	}

	for _, test := range []struct {
		name   string
		mutate func(*BarrageConfig)
	}{
		{name: "global", mutate: func(cfg *BarrageConfig) { cfg.GlobalRateEntryCapacity = -1 }},
		{name: "room", mutate: func(cfg *BarrageConfig) { cfg.RoomRateEntryCapacity = -1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultBarrageConfig()
			test.mutate(&cfg)
			if _, err := NormalizeBarrageConfig(cfg); err == nil {
				t.Fatal("expected negative rate-entry capacity to be rejected")
			}
		})
	}
}

func TestBarrageRateEntryCapacityRejectsPositiveValuesBelowShardCount(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*BarrageConfig)
	}{
		{name: "global", mutate: func(cfg *BarrageConfig) { cfg.GlobalRateEntryCapacity = barrageRateShards - 1 }},
		{name: "room", mutate: func(cfg *BarrageConfig) { cfg.RoomRateEntryCapacity = barrageRateShards - 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultBarrageConfig()
			test.mutate(&cfg)
			if _, err := NormalizeBarrageConfig(cfg); err == nil {
				t.Fatal("expected capacity below shard count to be rejected")
			}
		})
	}
}

func TestBarrageRateLimiterScopesNeverExceedConfiguredHardCapacity(t *testing.T) {
	cfg := DefaultBarrageConfig()
	cfg.QueueCapacity = 1
	cfg.GlobalRateEntryCapacity = 97
	cfg.RoomRateEntryCapacity = 101
	service := newTestBarrageService(t, cfg, nil, nil)
	service.now = func() time.Time { return time.Unix(100, 0) }

	for i := 0; i < 2_000; i++ {
		_ = service.SubmitPublic(context.Background(), Barrage{
			RoomID: fmt.Sprintf("room-%d", i), UserID: fmt.Sprintf("user-%d", i), Content: "hello",
		})
	}
	assertBarrageRateLimiterBound(t, "global", service.globalLimiter, cfg.GlobalRateEntryCapacity)
	assertBarrageRateLimiterBound(t, "room", service.limiter, cfg.RoomRateEntryCapacity)
}

func TestBarrageRateLimiterScopesStayBoundedUnderConcurrentFreshKeys(t *testing.T) {
	cfg := DefaultBarrageConfig()
	cfg.QueueCapacity = 1
	cfg.GlobalRateEntryCapacity = 193
	cfg.RoomRateEntryCapacity = 197
	service := newTestBarrageService(t, cfg, nil, nil)
	service.now = func() time.Time { return time.Unix(100, 0) }

	var workers sync.WaitGroup
	for worker := 0; worker < 64; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for i := 0; i < 64; i++ {
				identity := worker*64 + i
				_ = service.SubmitPublic(context.Background(), Barrage{
					RoomID: fmt.Sprintf("room-%d", identity), UserID: fmt.Sprintf("user-%d", identity), Content: "hello",
				})
			}
		}(worker)
	}
	workers.Wait()
	assertBarrageRateLimiterBound(t, "global", service.globalLimiter, cfg.GlobalRateEntryCapacity)
	assertBarrageRateLimiterBound(t, "room", service.limiter, cfg.RoomRateEntryCapacity)
}

func TestBarrageRateLimiterExpiresSampledStaleEntryWithoutFullScan(t *testing.T) {
	limiter := newBarrageRateLimiter(1, 1, time.Second, 128)
	oldKey := "old-key"
	shardIndex := barrageRateTestShard(oldKey)
	newKey := ""
	for i := 0; ; i++ {
		candidate := fmt.Sprintf("new-key-%d", i)
		if barrageRateTestShard(candidate) == shardIndex {
			newKey = candidate
			break
		}
	}
	start := time.Unix(100, 0)
	limiter.Allow(oldKey, start)
	limiter.Allow(newKey, start.Add(2*time.Second))

	shard := &limiter.shards[shardIndex]
	shard.mu.Lock()
	_, stalePresent := shard.entries[oldKey]
	entries := len(shard.entries)
	shard.mu.Unlock()
	if stalePresent || entries != 1 {
		t.Fatalf("stale present=%v entries=%d, want false and 1", stalePresent, entries)
	}
}

func assertBarrageRateLimiterBound(t *testing.T, scope string, limiter *barrageRateLimiter, capacity int) {
	t.Helper()
	total := 0
	allocated := 0
	for i := range limiter.shards {
		shard := &limiter.shards[i]
		shard.mu.Lock()
		if len(shard.entries) > shard.capacity {
			t.Fatalf("%s shard %d entries = %d, shard capacity %d", scope, i, len(shard.entries), shard.capacity)
		}
		total += len(shard.entries)
		allocated += shard.capacity
		shard.mu.Unlock()
	}
	if allocated != capacity {
		t.Fatalf("%s limiter allocated capacity = %d, configured %d", scope, allocated, capacity)
	}
	if total > capacity {
		t.Fatalf("%s limiter entries = %d, hard capacity %d", scope, total, capacity)
	}
}

func barrageRateTestShard(key string) uint32 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(key))
	return hash.Sum32() % barrageRateShards
}
