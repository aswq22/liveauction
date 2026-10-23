package data

import (
	"context"
	"errors"
	"hash/fnv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"live-auction-bid/backend/app/auction/service/internal/realtime"
)

const (
	publicRoomCacheCapacity       = 100_000
	publicRoomCacheTTL            = 5 * time.Second
	publicRoomCacheEvictionSample = 16
	publicRoomLookupStripes       = 256
	// publicRoomQueryBudget limits barrage admission to four of the Store's
	// default 20 DB connections, preserving most of the pool for authoritative work.
	publicRoomQueryBudget = 4
	// publicRoomLookupTimeout bounds the single public-availability SQL query.
	publicRoomLookupTimeout = 100 * time.Millisecond
)

var (
	errPublicRoomValidationUnavailable = errors.New("public room validation is unavailable")
	errPublicRoomLookupFailed          = errors.New("public room lookup failed")
	errPublicRoomQuerySaturated        = errors.New("public room lookup capacity exhausted")
)

type publicRoomAvailabilityQuery interface {
	PublicRoomAvailable(ctx context.Context, roomID string) (bool, error)
}

type publicRoomCacheEntry struct {
	available bool
	cachedAt  time.Time
	expiresAt time.Time
}

type publicRoomValidator struct {
	query         publicRoomAvailabilityQuery
	capacity      int
	ttl           time.Duration
	lookupTimeout time.Duration
	queryGate     *publicRoomQueryGate
	now           func() time.Time

	mu    sync.Mutex
	cache map[string]publicRoomCacheEntry

	lookupStripes [publicRoomLookupStripes]publicRoomLookupStripeState
	// beforeLookupWait is a test-only synchronization observer. It is nil in production.
	beforeLookupWait func()
}

type publicRoomLookupStripeState struct {
	mu   sync.Mutex
	call *publicRoomLookupCall
}

type publicRoomLookupCall struct {
	roomID    string
	done      chan struct{}
	available bool
	err       error
}

type publicRoomQueryGate struct {
	limit int32
	inUse atomic.Int32
}

func newPublicRoomQueryGate(limit int) *publicRoomQueryGate {
	if limit < 1 {
		limit = 1
	}
	return &publicRoomQueryGate{limit: int32(limit)}
}

func (g *publicRoomQueryGate) tryAcquire() bool {
	for {
		inUse := g.inUse.Load()
		if inUse >= g.limit {
			return false
		}
		if g.inUse.CompareAndSwap(inUse, inUse+1) {
			return true
		}
	}
}

func (g *publicRoomQueryGate) release() {
	g.inUse.Add(-1)
}

func NewPublicRoomValidator(store *Store) realtime.PublicRoomValidator {
	var query publicRoomAvailabilityQuery
	if store != nil && store.db != nil {
		query = store
	}
	return newPublicRoomValidator(query, publicRoomCacheCapacity, publicRoomCacheTTL)
}

func newPublicRoomValidator(query publicRoomAvailabilityQuery, capacity int, ttl time.Duration) *publicRoomValidator {
	return &publicRoomValidator{
		query: query, capacity: capacity, ttl: ttl, lookupTimeout: publicRoomLookupTimeout,
		queryGate: newPublicRoomQueryGate(publicRoomQueryBudget), now: time.Now,
		cache: make(map[string]publicRoomCacheEntry, max(0, capacity)),
	}
}

func (v *publicRoomValidator) PublicRoomExists(ctx context.Context, roomID string) (bool, error) {
	if v == nil || v.query == nil {
		return false, errPublicRoomValidationUnavailable
	}
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return false, errors.New("room id is required")
	}
	stripe := &v.lookupStripes[publicRoomLookupStripe(roomID)]
	for {
		if available, ok := v.cached(roomID, v.now()); ok {
			return available, nil
		}
		stripe.mu.Lock()
		if call := stripe.call; call != nil {
			stripe.mu.Unlock()
			if v.beforeLookupWait != nil {
				v.beforeLookupWait()
			}
			select {
			case <-call.done:
				if call.roomID == roomID {
					return call.available, call.err
				}
				continue
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		if available, ok := v.cached(roomID, v.now()); ok {
			stripe.mu.Unlock()
			return available, nil
		}
		call := &publicRoomLookupCall{roomID: roomID, done: make(chan struct{})}
		stripe.call = call
		stripe.mu.Unlock()
		go v.runLookup(context.WithoutCancel(ctx), stripe, call)
		select {
		case <-call.done:
			if call.err != nil {
				return false, call.err
			}
			return call.available, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

func (v *publicRoomValidator) runLookup(baseCtx context.Context, stripe *publicRoomLookupStripeState, call *publicRoomLookupCall) {
	lookupCtx, cancel := context.WithTimeout(baseCtx, v.lookupTimeout)
	defer cancel()
	defer func() {
		if recover() != nil {
			call.available = false
			call.err = errPublicRoomLookupFailed
		}
		stripe.mu.Lock()
		if stripe.call == call {
			stripe.call = nil
		}
		close(call.done)
		stripe.mu.Unlock()
	}()
	if !v.queryGate.tryAcquire() {
		call.available = false
		call.err = errPublicRoomQuerySaturated
		return
	}
	defer v.queryGate.release()
	call.available, call.err = v.query.PublicRoomAvailable(lookupCtx, call.roomID)
	if call.err == nil {
		v.store(call.roomID, call.available, v.now())
	}
}

func (v *publicRoomValidator) cached(roomID string, now time.Time) (bool, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	entry, ok := v.cache[roomID]
	if !ok {
		return false, false
	}
	if !now.Before(entry.expiresAt) {
		delete(v.cache, roomID)
		return false, false
	}
	return entry.available, true
}

func (v *publicRoomValidator) store(roomID string, available bool, now time.Time) {
	if v.capacity <= 0 || v.ttl <= 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, exists := v.cache[roomID]; !exists && len(v.cache) >= v.capacity {
		v.evictOldestSampleLocked()
	}
	v.cache[roomID] = publicRoomCacheEntry{available: available, cachedAt: now, expiresAt: now.Add(v.ttl)}
}

func (v *publicRoomValidator) evictOldestSampleLocked() {
	var oldestKey string
	var oldestTime time.Time
	sampled := 0
	for key, entry := range v.cache {
		if sampled == 0 || entry.cachedAt.Before(oldestTime) {
			oldestKey, oldestTime = key, entry.cachedAt
		}
		sampled++
		if sampled >= publicRoomCacheEvictionSample {
			break
		}
	}
	if sampled > 0 {
		delete(v.cache, oldestKey)
	}
}

func publicRoomLookupStripe(roomID string) uint32 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(roomID))
	return hash.Sum32() % publicRoomLookupStripes
}
