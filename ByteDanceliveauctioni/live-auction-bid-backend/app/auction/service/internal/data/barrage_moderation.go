package data

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"live-auction-bid/backend/app/auction/service/internal/realtime"
)

const (
	barrageMuteCacheCapacity  = 100_000
	barrageMuteCacheTTL       = 5 * time.Second
	barrageMuteLookupTimeout  = 20 * time.Millisecond
	barrageMuteEvictionSample = 16
)

var errBarrageModerationUnavailable = errors.New("barrage moderation is unavailable")

type barrageRedisExists interface {
	Exists(ctx context.Context, keys ...string) *redis.IntCmd
}

type barrageMuteCacheEntry struct {
	muted     bool
	cachedAt  time.Time
	expiresAt time.Time
}

type barrageModeration struct {
	redis    barrageRedisExists
	capacity int
	ttl      time.Duration
	timeout  time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]barrageMuteCacheEntry
}

func NewBarrageModeration(store *Store) realtime.BarrageModeration {
	var redisClient barrageRedisExists
	if store != nil && store.redis != nil {
		redisClient = store.redis
	}
	return newBarrageModeration(redisClient, barrageMuteCacheCapacity, barrageMuteCacheTTL, barrageMuteLookupTimeout)
}

func newBarrageModeration(redisClient barrageRedisExists, capacity int, ttl, timeout time.Duration) *barrageModeration {
	return &barrageModeration{
		redis: redisClient, capacity: capacity, ttl: ttl, timeout: timeout,
		now: time.Now, cache: make(map[string]barrageMuteCacheEntry, max(0, capacity)),
	}
}

func (m *barrageModeration) IsMuted(ctx context.Context, roomID, userID string) (bool, error) {
	if m == nil || m.redis == nil {
		return false, errBarrageModerationUnavailable
	}
	cacheKey := roomID + "\x00" + userID
	now := m.now()
	if muted, ok := m.cached(cacheKey, now); ok {
		return muted, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	count, err := m.redis.Exists(lookupCtx, "auction:barrage:mute:"+roomID+":"+userID).Result()
	if err != nil {
		return false, err
	}
	m.store(cacheKey, count > 0, m.now())
	return count > 0, nil
}

func (m *barrageModeration) cached(key string, now time.Time) (bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.cache[key]
	if !ok {
		return false, false
	}
	if !now.Before(entry.expiresAt) {
		delete(m.cache, key)
		return false, false
	}
	return entry.muted, true
}

func (m *barrageModeration) store(key string, muted bool, now time.Time) {
	if m.capacity <= 0 || m.ttl <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.cache[key]; !exists && len(m.cache) >= m.capacity {
		m.evictOldestSampleLocked()
	}
	m.cache[key] = barrageMuteCacheEntry{muted: muted, cachedAt: now, expiresAt: now.Add(m.ttl)}
}

func (m *barrageModeration) evictOldestSampleLocked() {
	var oldestKey string
	var oldestTime time.Time
	sampled := 0
	for key, entry := range m.cache {
		if sampled == 0 || entry.cachedAt.Before(oldestTime) {
			oldestKey, oldestTime = key, entry.cachedAt
		}
		sampled++
		if sampled >= barrageMuteEvictionSample {
			break
		}
	}
	if sampled > 0 {
		delete(m.cache, oldestKey)
	}
}
