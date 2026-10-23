package buyersearch

import (
	"context"
	"errors"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	"testing"
	"time"
)

type testIndex struct{ vector, lexical []Candidate }

func (i testIndex) Active(context.Context) (Generation, error) {
	return Generation{ID: "g", Dimensions: 2, MaxDistance: 0.4}, nil
}
func (i testIndex) Vector(context.Context, Generation, []float64, Filters, int) ([]Candidate, error) {
	return i.vector, nil
}
func (i testIndex) Lexical(context.Context, Generation, ParsedQuery, int) ([]Candidate, error) {
	return i.lexical, nil
}
func (i testIndex) Upsert(context.Context, Generation, Document, []float64) error { return nil }

type testEmbedder struct{ err error }

func (e testEmbedder) Embed(context.Context, []string) ([][]float64, error) {
	return [][]float64{{1, 0}}, e.err
}

type testReader struct {
	facts map[string]Fact
	err   error
}

func (r testReader) ReadPublicFacts(context.Context, []string) (map[string]Fact, error) {
	return r.facts, r.err
}
func testFact(id string, price int64, public bool) Fact {
	return Fact{Lot: &v1.Lot{Id: id, RoomId: "r", Status: v1.LotStatus_LOT_STATUS_LIVE, CurrentPrice: &v1.Money{Amount: price, Currency: "CNY"}}, Public: public, ReadAtUnixMs: time.Now().UnixMilli()}
}
func TestHybridSemanticRecall(t *testing.T) {
	d := 0.2
	s := NewService(testIndex{vector: []Candidate{{LotID: "jade", Distance: &d}}, lexical: []Candidate{{LotID: "model", LexicalScore: 1}}}, testEmbedder{}, testReader{facts: map[string]Fact{"jade": testFact("jade", 100, true), "model": testFact("model", 100, true)}}, nil, Config{})
	p, e := s.Search(context.Background(), Query{Text: "送礼"})
	if e != nil || len(p.Hits) != 2 || p.Mode != "HYBRID" {
		t.Fatal(p, e)
	}
}
func TestUnrelatedQueryEmpty(t *testing.T) {
	d := 0.95
	s := NewService(testIndex{vector: []Candidate{{LotID: "jade", Distance: &d}}}, testEmbedder{}, testReader{facts: map[string]Fact{"jade": testFact("jade", 100, true)}}, nil, Config{})
	p, e := s.Search(context.Background(), Query{Text: "飞机发动机"})
	if e != nil || len(p.Hits) != 0 {
		t.Fatal(p, e)
	}
}
func TestLiveBudgetAndVisibility(t *testing.T) {
	m := int64(300000)
	s := NewService(testIndex{lexical: []Candidate{{LotID: "expensive", LexicalScore: 1}, {LotID: "hidden", LexicalScore: 1}, {LotID: "zero", LexicalScore: 1}}}, testEmbedder{}, testReader{facts: map[string]Fact{"expensive": testFact("expensive", 300001, true), "hidden": testFact("hidden", 100, false), "zero": testFact("zero", 0, true)}}, nil, Config{})
	p, e := s.Search(context.Background(), Query{Text: "玉", MaxPrice: &m})
	if e != nil || len(p.Hits) != 1 || p.Hits[0].Fact.Lot.Id != "zero" {
		t.Fatal(p, e)
	}
}
func TestExplicitLexicalDegradation(t *testing.T) {
	s := NewService(testIndex{lexical: []Candidate{{LotID: "a", LexicalScore: 1}}}, testEmbedder{err: errors.New("timeout")}, testReader{facts: map[string]Fact{"a": testFact("a", 100, true)}}, nil, Config{})
	p, e := s.Search(context.Background(), Query{Text: "玉"})
	if e != nil || p.Mode != "LEXICAL" || p.DegradedReason == "" || len(p.Hits) != 1 {
		t.Fatal(p, e)
	}
}
func TestSearchFactsDependencyError(t *testing.T) {
	s := NewService(testIndex{lexical: []Candidate{{LotID: "a", LexicalScore: 1}}}, testEmbedder{}, testReader{err: errors.New("db down")}, nil, Config{})
	if _, e := s.Search(context.Background(), Query{Text: "玉"}); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
}
func TestCursorBindingAndExpiry(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	p := CursorPayload{SnapshotID: "s", QueryHash: "q", GenerationID: "g", Offset: 20, ExpiresAtUnixMs: time.Now().Add(time.Minute).UnixMilli()}
	c, e := EncodeCursor(p, key)
	if e != nil {
		t.Fatal(e)
	}
	out, e := DecodeCursor(c, key, time.Now())
	if e != nil || out.Offset != 20 {
		t.Fatal(out, e)
	}
	if _, e = DecodeCursor(c+"x", key, time.Now()); e == nil {
		t.Fatal("tamper accepted")
	}
	if _, e = DecodeCursor(c, key, time.Now().Add(2*time.Minute)); !errors.Is(e, ErrCursorExpired) {
		t.Fatal(e)
	}
}
