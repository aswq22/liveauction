package data

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/redis/go-redis/v9"
	bs "live-auction-bid/backend/app/auction/service/internal/biz/buyersearch"
	"time"
)

type BuyerSearchCache struct{ client *redis.Client }

func (s *Store) BuyerSearchCache() *BuyerSearchCache { return &BuyerSearchCache{client: s.redis} }
func (c *BuyerSearchCache) get(ctx context.Context, key string, target any) (bool, error) {
	if c.client == nil {
		return false, errors.New("redis unavailable")
	}
	b, e := c.client.Get(ctx, "buyer-search:v1:"+key).Bytes()
	if errors.Is(e, redis.Nil) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if len(b) > 256*1024 {
		return false, errors.New("search cache entry too large")
	}
	if e = json.Unmarshal(b, target); e != nil {
		return false, e
	}
	return true, nil
}
func (c *BuyerSearchCache) put(ctx context.Context, key string, value any, ttl, maxTTL time.Duration) error {
	if c.client == nil {
		return errors.New("redis unavailable")
	}
	if ttl <= 0 || ttl > maxTTL {
		return errors.New("invalid search cache ttl")
	}
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	if len(b) > 256*1024 {
		return errors.New("search cache entry too large")
	}
	return c.client.Set(ctx, "buyer-search:v1:"+key, b, ttl).Err()
}
func (c *BuyerSearchCache) Vector(ctx context.Context, key string) ([]float64, bool, error) {
	var v []float64
	ok, e := c.get(ctx, "vector:"+key, &v)
	return v, ok, e
}
func (c *BuyerSearchCache) PutVector(ctx context.Context, key string, v []float64, ttl time.Duration) error {
	return c.put(ctx, "vector:"+key, v, ttl, 24*time.Hour)
}
func (c *BuyerSearchCache) Candidates(ctx context.Context, key string) ([]bs.Candidate, bool, error) {
	var v []bs.Candidate
	ok, e := c.get(ctx, "candidates:"+key, &v)
	return v, ok, e
}
func (c *BuyerSearchCache) PutCandidates(ctx context.Context, key string, v []bs.Candidate, ttl time.Duration) error {
	return c.put(ctx, "candidates:"+key, v, ttl, 10*time.Second)
}
func (c *BuyerSearchCache) Snapshot(ctx context.Context, key string) (bs.Snapshot, bool, error) {
	var v bs.Snapshot
	ok, e := c.get(ctx, "snapshot:"+key, &v)
	return v, ok, e
}
func (c *BuyerSearchCache) PutSnapshot(ctx context.Context, key string, v bs.Snapshot, ttl time.Duration) error {
	if len(v.IDs) > 200 {
		return errors.New("snapshot too large")
	}
	return c.put(ctx, "snapshot:"+key, v, ttl, 2*time.Minute)
}
