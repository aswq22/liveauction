package buyersearch

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockedEmbedder struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}
type identityEmbedder struct {
	testEmbedder
	calls int
}

func (e *identityEmbedder) Model() string   { return "old-model" }
func (e *identityEmbedder) Dimensions() int { return 2 }
func (e *identityEmbedder) Embed(ctx context.Context, v []string) ([][]float64, error) {
	e.calls++
	return e.testEmbedder.Embed(ctx, v)
}

type switchedIndex struct{ testIndex }

func (i switchedIndex) Active(context.Context) (Generation, error) {
	return Generation{ID: "new-generation", Model: "new-model", Dimensions: 2, MaxDistance: .4}, nil
}

type cachedIdentityVector struct {
	testCache
	reads int
}

func (c *cachedIdentityVector) Vector(context.Context, string) ([]float64, bool, error) {
	c.reads++
	return []float64{1, 0}, true, nil
}
func TestEqualDimensionModelSwitchRejectsOldEmbedderAndCache(t *testing.T) {
	e := &identityEmbedder{}
	cache := &cachedIdentityVector{}
	s := NewService(switchedIndex{testIndex: testIndex{lexical: []Candidate{{LotID: "a", LexicalScore: 1}}}}, e, testReader{facts: map[string]Fact{"a": testFact("a", 1, true)}}, cache, Config{})
	p, err := s.Search(context.Background(), Query{Text: "玉"})
	if err != nil || p.Mode != "LEXICAL" || e.calls != 0 || cache.reads != 0 {
		t.Fatal("incompatible vector space used", p, err, e.calls, cache.reads)
	}
}

func (e *blockedEmbedder) Embed(ctx context.Context, _ []string) ([][]float64, error) {
	if e.calls.Add(1) == 1 {
		close(e.started)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.release:
		return [][]float64{{1, 0}}, nil
	}
}
func TestQueryEmbeddingCoalescesAndSurvivesOneCancelledWaiter(t *testing.T) {
	e := &blockedEmbedder{started: make(chan struct{}), release: make(chan struct{})}
	s := NewService(testIndex{}, e, testReader{}, nil, Config{})
	p, _ := Parse(Query{Text: "玉"})
	g, _ := testIndex{}.Active(context.Background())
	c, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = s.embedding(c, p, g) }()
	<-e.started
	second := make(chan error, 1)
	go func() { defer wg.Done(); _, err := s.embedding(context.Background(), p, g); second <- err }()
	// Synchronize on the waiter count rather than scheduler timing.
	deadline := time.Now().Add(time.Second)
	for s.embeddingWaiters(SemanticHash(Document{Text: p.SemanticText}, g)) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("second waiter did not join")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	close(e.release)
	wg.Wait()
	if err := <-second; err != nil || e.calls.Load() != 1 {
		t.Fatal(err, e.calls.Load())
	}
}
