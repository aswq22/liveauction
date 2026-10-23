package buyersearch

import (
	"context"
	"testing"
	"time"
)

type testCache struct {
	snapshots  map[string]Snapshot
	candidates []Candidate
}

func (c *testCache) Vector(context.Context, string) ([]float64, bool, error)           { return nil, false, nil }
func (c *testCache) PutVector(context.Context, string, []float64, time.Duration) error { return nil }
func (c *testCache) Candidates(context.Context, string) ([]Candidate, bool, error) {
	return c.candidates, c.candidates != nil, nil
}
func (c *testCache) PutCandidates(_ context.Context, _ string, v []Candidate, _ time.Duration) error {
	c.candidates = v
	return nil
}
func (c *testCache) Snapshot(_ context.Context, key string) (Snapshot, bool, error) {
	v, ok := c.snapshots[key]
	return v, ok, nil
}
func (c *testCache) PutSnapshot(_ context.Context, key string, v Snapshot, _ time.Duration) error {
	if c.snapshots == nil {
		c.snapshots = map[string]Snapshot{}
	}
	c.snapshots[key] = v
	return nil
}
func TestCachedCandidatesRevalidatePriceAndVisibility(t *testing.T) {
	max := int64(100)
	cache := &testCache{candidates: []Candidate{{LotID: "price"}, {LotID: "hidden"}, {LotID: "good"}}}
	reader := testReader{facts: map[string]Fact{"price": testFact("price", 101, true), "hidden": testFact("hidden", 1, false), "good": testFact("good", 100, true)}}
	s := NewService(testIndex{}, testEmbedder{}, reader, cache, Config{})
	p, e := s.Search(context.Background(), Query{Text: "玉", MaxPrice: &max})
	if e != nil || len(p.Hits) != 1 || p.Hits[0].Fact.Lot.Id != "good" {
		t.Fatal(p, e)
	}
}
func TestCursorRevalidationAndQueryBinding(t *testing.T) {
	cache := &testCache{candidates: []Candidate{{LotID: "a"}, {LotID: "b"}, {LotID: "c"}}}
	reader := testReader{facts: map[string]Fact{"a": testFact("a", 100, true), "b": testFact("b", 100, true), "c": testFact("c", 100, true)}}
	s := NewService(testIndex{}, testEmbedder{}, reader, cache, Config{CursorKey: []byte("01234567890123456789012345678901")})
	q := Query{Text: "玉", PageSize: 1}
	p, e := s.Search(context.Background(), q)
	if e != nil || p.NextCursor == "" {
		t.Fatal(p, e)
	}
	f := reader.facts["b"]
	f.Public = false
	reader.facts["b"] = f
	q.Cursor = p.NextCursor
	next, e := s.Search(context.Background(), q)
	if e != nil || len(next.Hits) != 1 || next.Hits[0].Fact.Lot.Id != "c" {
		t.Fatal(next, e)
	}
	q.Text = "瓷器"
	if _, e = s.Search(context.Background(), q); e != ErrInvalidCursor {
		t.Fatal("changed query accepted", e)
	}
}
func TestNoRelevantQueryIsExcludedFromQualityDenominator(t *testing.T) {
	r, n, e := Quality(nil, map[string]int{}, 50, 10)
	if e != nil || r != 0 || n != 0 {
		t.Fatal("empty queries must not inflate relevance metrics", r, n, e)
	}
}
