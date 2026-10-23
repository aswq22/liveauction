package data

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type fakeBarrageExists struct {
	mu         sync.Mutex
	values     map[string][]int64
	err        error
	wait       bool
	calls      int
	callsByKey map[string]int
	contexts   chan context.Context
}

func (f *fakeBarrageExists) Exists(ctx context.Context, keys ...string) *redis.IntCmd {
	if f.contexts != nil {
		f.contexts <- ctx
	}
	if f.wait {
		<-ctx.Done()
		return redis.NewIntResult(0, ctx.Err())
	}
	key := ""
	if len(keys) > 0 {
		key = keys[0]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.callsByKey == nil {
		f.callsByKey = make(map[string]int)
	}
	index := f.callsByKey[key]
	f.callsByKey[key] = index + 1
	if f.err != nil {
		return redis.NewIntResult(0, f.err)
	}
	values := f.values[key]
	if len(values) == 0 {
		return redis.NewIntResult(0, nil)
	}
	if index >= len(values) {
		index = len(values) - 1
	}
	return redis.NewIntResult(values[index], nil)
}

func TestBarrageModerationReadsMutedAndUnmutedRedisKeys(t *testing.T) {
	tests := []struct {
		name  string
		count int64
		want  bool
	}{
		{name: "muted", count: 1, want: true},
		{name: "unmuted", count: 0, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key := "auction:barrage:mute:room-1:user-1"
			redisFake := &fakeBarrageExists{values: map[string][]int64{key: {test.count}}}
			moderation := newBarrageModeration(redisFake, 10, 5*time.Second, 20*time.Millisecond)

			got, err := moderation.IsMuted(context.Background(), "room-1", "user-1")
			if err != nil || got != test.want {
				t.Fatalf("IsMuted() = %v, %v; want %v, nil", got, err, test.want)
			}
			if redisFake.callsByKey[key] != 1 {
				t.Fatalf("Redis calls for %q = %d, want 1", key, redisFake.callsByKey[key])
			}
		})
	}
}

func TestNewBarrageModerationWithUninitializedStoreFailsClosed(t *testing.T) {
	moderation := NewBarrageModeration(&Store{})
	if _, err := moderation.IsMuted(context.Background(), "room", "user"); !errors.Is(err, errBarrageModerationUnavailable) {
		t.Fatalf("IsMuted() error = %v, want %v", err, errBarrageModerationUnavailable)
	}
}

func TestBarrageModerationCachesLookupUntilTTLExpires(t *testing.T) {
	key := "auction:barrage:mute:room:user"
	redisFake := &fakeBarrageExists{values: map[string][]int64{key: {1, 0}}}
	moderation := newBarrageModeration(redisFake, 10, 5*time.Second, 20*time.Millisecond)
	now := time.Unix(100, 0)
	moderation.now = func() time.Time { return now }

	first, err := moderation.IsMuted(context.Background(), "room", "user")
	if err != nil || !first {
		t.Fatalf("first IsMuted() = %v, %v", first, err)
	}
	now = now.Add(4 * time.Second)
	second, err := moderation.IsMuted(context.Background(), "room", "user")
	if err != nil || !second {
		t.Fatalf("cached IsMuted() = %v, %v", second, err)
	}
	if redisFake.calls != 1 {
		t.Fatalf("Redis calls before expiry = %d, want 1", redisFake.calls)
	}
	now = now.Add(2 * time.Second)
	afterExpiry, err := moderation.IsMuted(context.Background(), "room", "user")
	if err != nil || afterExpiry {
		t.Fatalf("expired IsMuted() = %v, %v; want false, nil", afterExpiry, err)
	}
	if redisFake.calls != 2 {
		t.Fatalf("Redis calls after expiry = %d, want 2", redisFake.calls)
	}
}

func TestBarrageModerationEvictsOldestSampleAtCapacity(t *testing.T) {
	values := map[string][]int64{
		"auction:barrage:mute:room:u1": {1, 0},
		"auction:barrage:mute:room:u2": {0},
		"auction:barrage:mute:room:u3": {0},
	}
	redisFake := &fakeBarrageExists{values: values}
	moderation := newBarrageModeration(redisFake, 2, time.Minute, 20*time.Millisecond)
	now := time.Unix(100, 0)
	moderation.now = func() time.Time { return now }

	for _, userID := range []string{"u1", "u2", "u3"} {
		if _, err := moderation.IsMuted(context.Background(), "room", userID); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	muted, err := moderation.IsMuted(context.Background(), "room", "u1")
	if err != nil || muted {
		t.Fatalf("evicted oldest IsMuted() = %v, %v; want false, nil", muted, err)
	}
	if redisFake.callsByKey["auction:barrage:mute:room:u1"] != 2 {
		t.Fatalf("oldest key Redis calls = %d, want 2", redisFake.callsByKey["auction:barrage:mute:room:u1"])
	}
	if got := barrageModerationCacheSize(moderation); got > 2 {
		t.Fatalf("cache size = %d, capacity 2", got)
	}
}

func TestBarrageModerationTimesOutAndPropagatesRedisErrors(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		moderation := newBarrageModeration(&fakeBarrageExists{wait: true}, 10, time.Second, 2*time.Millisecond)
		if _, err := moderation.IsMuted(context.Background(), "room", "user"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("IsMuted() error = %v, want deadline exceeded", err)
		}
	})
	t.Run("redis error", func(t *testing.T) {
		marker := errors.New("redis unavailable")
		moderation := newBarrageModeration(&fakeBarrageExists{err: marker}, 10, time.Second, 20*time.Millisecond)
		if _, err := moderation.IsMuted(context.Background(), "room", "user"); !errors.Is(err, marker) {
			t.Fatalf("IsMuted() error = %v, want %v", err, marker)
		}
	})
}

func TestBarrageModerationCancelsChildContextAfterFastLookup(t *testing.T) {
	contexts := make(chan context.Context, 1)
	moderation := newBarrageModeration(&fakeBarrageExists{contexts: contexts}, 10, time.Second, time.Second)
	if _, err := moderation.IsMuted(context.Background(), "room", "user"); err != nil {
		t.Fatal(err)
	}
	child := <-contexts
	select {
	case <-child.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("mute lookup child context was not canceled")
	}
}

func TestBarrageModerationCacheStaysBoundedUnderConcurrentMisses(t *testing.T) {
	redisFake := &fakeBarrageExists{}
	moderation := newBarrageModeration(redisFake, 32, time.Minute, time.Second)
	var workers sync.WaitGroup
	for i := 0; i < 256; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			_, _ = moderation.IsMuted(context.Background(), "room", fmt.Sprintf("user-%d", i))
		}(i)
	}
	workers.Wait()
	if got := barrageModerationCacheSize(moderation); got > 32 {
		t.Fatalf("cache size = %d, capacity 32", got)
	}
}

func barrageModerationCacheSize(moderation *barrageModeration) int {
	moderation.mu.Lock()
	defer moderation.mu.Unlock()
	return len(moderation.cache)
}
