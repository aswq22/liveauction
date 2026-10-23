package buyersearch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Config struct {
	Deadline, EmbeddingTimeout, IndexTimeout, FactsTimeout time.Duration
	CursorKey                                              []byte
	MaxConcurrent                                          int
	Observer                                               interface {
		Observe(string, string, time.Duration)
	}
}
type Service struct {
	index          Index
	embedder       Embedder
	reader         Reader
	cache          Cache
	config         Config
	slots          chan struct{}
	embedSlots     chan struct{}
	embedMu        sync.Mutex
	flights        map[string]*embeddingFlight
	embedFailures  int
	embedOpenUntil time.Time
}

func NewService(index Index, embedder Embedder, reader Reader, cache Cache, c Config) *Service {
	if c.Deadline <= 0 {
		c.Deadline = 1500 * time.Millisecond
	}
	if c.EmbeddingTimeout <= 0 {
		c.EmbeddingTimeout = 450 * time.Millisecond
	}
	if c.IndexTimeout <= 0 {
		c.IndexTimeout = 200 * time.Millisecond
	}
	if c.FactsTimeout <= 0 {
		c.FactsTimeout = 200 * time.Millisecond
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 128
	}
	c.CursorKey = append([]byte(nil), c.CursorKey...)
	return &Service{index: index, embedder: embedder, reader: reader, cache: cache, config: c, slots: make(chan struct{}, c.MaxConcurrent), embedSlots: make(chan struct{}, 32), flights: map[string]*embeddingFlight{}}
}
func queryHash(q ParsedQuery, g Generation) string {
	b, _ := json.Marshal(struct {
		Query      ParsedQuery
		Generation string
	}{q, g.ID})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (s *Service) Search(ctx context.Context, q Query) (result Page, err error) {
	started := time.Now()
	defer func() {
		if s.config.Observer != nil {
			outcome := result.Mode
			if err != nil {
				outcome = "error"
			}
			s.config.Observer.Observe("total", outcome, time.Since(started))
		}
	}()
	p, e := Parse(q)
	if e != nil {
		return Page{}, e
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return Page{}, ErrCapacity
	}
	ctx, cancel := context.WithTimeout(ctx, s.config.Deadline)
	defer cancel()
	if s.index == nil || s.reader == nil {
		return Page{}, ErrUnavailable
	}
	ac, done := context.WithTimeout(ctx, s.config.IndexTimeout)
	g, e := s.index.Active(ac)
	done()
	if e != nil {
		return Page{}, fmt.Errorf("%w: active generation", ErrUnavailable)
	}
	hash := queryHash(p, g)
	if q.Cursor != "" {
		return s.next(ctx, p, g, hash, q.Cursor)
	}
	_, identityError := ResolveEmbedder(s.embedder, g)
	if s.cache != nil && identityError == nil {
		cc, stop := context.WithTimeout(ctx, 40*time.Millisecond)
		cached, ok, ce := s.cache.Candidates(cc, hash)
		stop()
		if ce == nil && ok && len(cached) <= 400 {
			ids := candidateIDs(cached)
			facts, fe := s.facts(ctx, ids)
			if fe != nil {
				return Page{}, fe
			}
			return s.finish(ctx, p, g, hash, ids, facts, "HYBRID", "")
		}
	}
	mode, reason := "HYBRID", ""
	vec, e := s.embedding(ctx, p, g)
	if e != nil {
		mode, reason = "LEXICAL", "EMBEDDING_UNAVAILABLE"
		if errors.Is(e, ErrEmbeddingIdentity) {
			reason = "EMBEDDING_GENERATION_MISMATCH"
		}
	}
	var ranked []Candidate
	for _, limit := range []int{100, 200} {
		lexicalCh := make(chan struct {
			c []Candidate
			e error
		}, 1)
		go func() {
			ic, stop := context.WithTimeout(ctx, s.config.IndexTimeout)
			defer stop()
			c, e := s.index.Lexical(ic, g, p, limit)
			lexicalCh <- struct {
				c []Candidate
				e error
			}{c, e}
		}()
		var semantic []Candidate
		if mode == "HYBRID" {
			ic, stop := context.WithTimeout(ctx, s.config.IndexTimeout)
			semantic, e = s.index.Vector(ic, g, vec, p.Filters, limit)
			stop()
			if e != nil {
				mode, reason = "LEXICAL", "VECTOR_INDEX_UNAVAILABLE"
				semantic = nil
			}
		}
		var lr struct {
			c []Candidate
			e error
		}
		select {
		case lr = <-lexicalCh:
		case <-ctx.Done():
			return Page{}, ErrUnavailable
		}
		if lr.e != nil {
			return Page{}, fmt.Errorf("%w: lexical index", ErrUnavailable)
		}
		accepted := make([]Candidate, 0, len(semantic))
		for _, c := range semantic {
			if c.Distance != nil && *c.Distance >= 0 && *c.Distance <= g.MaxDistance {
				accepted = append(accepted, c)
			}
		}
		lexical := make([]Candidate, 0, len(lr.c))
		for _, c := range lr.c {
			if c.LexicalScore > 0 {
				lexical = append(lexical, c)
			}
		}
		ranked = Fuse(accepted, lexical, 400)
		ids := candidateIDs(ranked)
		facts, e := s.facts(ctx, ids)
		if e != nil {
			return Page{}, e
		}
		page, _ := pageFromFacts(p, ids, facts, 0)
		if len(page.Hits) >= p.PageSize || limit == 200 || (len(lr.c) < limit && len(semantic) < limit) {
			if mode == "HYBRID" && s.cache != nil {
				cc, stop := context.WithTimeout(ctx, 40*time.Millisecond)
				_ = s.cache.PutCandidates(cc, hash, ranked, 10*time.Second)
				stop()
			}
			return s.finish(ctx, p, g, hash, ids, facts, mode, reason)
		}
	}
	return Page{}, ErrUnavailable
}
func candidateIDs(cs []Candidate) []string {
	ids := make([]string, 0, len(cs))
	for _, c := range cs {
		if c.LotID != "" {
			ids = append(ids, c.LotID)
		}
	}
	return ids
}
func (s *Service) embedding(ctx context.Context, q ParsedQuery, g Generation) ([]float64, error) {
	embedder, e := ResolveEmbedder(s.embedder, g)
	if e != nil {
		return nil, e
	}
	key := SemanticHash(Document{Text: q.SemanticText}, g)
	if s.cache != nil {
		if v, ok, e := s.cache.Vector(ctx, key); e == nil && ok && len(v) == g.Dimensions {
			return v, nil
		}
	}
	return s.sharedEmbedding(ctx, key, q.SemanticText, g, embedder)
}
func (s *Service) finish(ctx context.Context, p ParsedQuery, g Generation, hash string, ids []string, facts map[string]Fact, mode, reason string) (Page, error) {
	// Freeze only currently eligible IDs, up to 200; later pages revalidate them.
	all := p
	all.PageSize = 400
	eligible, _ := pageFromFacts(all, ids, facts, 0)
	ids = make([]string, 0, len(eligible.Hits))
	for _, h := range eligible.Hits {
		ids = append(ids, h.Fact.Lot.Id)
	}
	if len(ids) > 200 {
		ids = ids[:200]
	}
	page, offset := pageFromFacts(p, ids, facts, 0)
	page.Mode = mode
	page.DegradedReason = reason
	if offset < len(ids) && s.cache != nil && len(s.config.CursorKey) >= 32 {
		b := make([]byte, 16)
		if _, e := rand.Read(b); e == nil {
			sid := hex.EncodeToString(b)
			expiry := time.Now().Add(2 * time.Minute).UnixMilli()
			snap := Snapshot{QueryHash: hash, GenerationID: g.ID, IDs: ids, Mode: mode, DegradedReason: reason, ExpiresAtUnixMs: expiry}
			cc, stop := context.WithTimeout(ctx, 40*time.Millisecond)
			e = s.cache.PutSnapshot(cc, sid, snap, 2*time.Minute)
			stop()
			if e == nil {
				page.NextCursor, _ = EncodeCursor(CursorPayload{SnapshotID: sid, QueryHash: hash, GenerationID: g.ID, Offset: offset, ExpiresAtUnixMs: expiry}, s.config.CursorKey)
			} else {
				page.Warnings = append(page.Warnings, "分页暂不可用，请稍后重新搜索")
			}
		}
	}
	return page, nil
}
func (s *Service) facts(ctx context.Context, ids []string) (map[string]Fact, error) {
	started := time.Now()
	if len(ids) == 0 {
		return map[string]Fact{}, nil
	}
	fc, stop := context.WithTimeout(ctx, s.config.FactsTimeout)
	defer stop()
	f, e := s.reader.ReadPublicFacts(fc, ids)
	if s.config.Observer != nil {
		outcome := "ok"
		if e != nil {
			outcome = "error"
		}
		s.config.Observer.Observe("facts", outcome, time.Since(started))
	}
	if e != nil {
		return nil, fmt.Errorf("%w: authoritative facts", ErrUnavailable)
	}
	return f, nil
}
func pageFromFacts(p ParsedQuery, ids []string, facts map[string]Fact, start int) (Page, int) {
	page := Page{Applied: p.Filters, Warnings: append([]string(nil), p.Warnings...), Hits: []Hit{}}
	offset := start
	for offset < len(ids) && len(page.Hits) < p.PageSize {
		id := ids[offset]
		offset++
		f, ok := facts[id]
		if !ok || !f.Public || f.Lot == nil || f.Lot.Id != id || !PublicStatus(f.Lot.Status) {
			continue
		}
		lot := f.Lot
		if p.Filters.RoomID != "" && lot.RoomId != p.Filters.RoomID {
			continue
		}
		allowed := false
		for _, status := range p.Filters.Statuses {
			if lot.Status == status {
				allowed = true
				break
			}
		}
		if !allowed {
			continue
		}
		price := lot.CurrentPrice
		if price == nil || price.Currency != "CNY" || price.Amount < 0 {
			continue
		}
		if p.Filters.MinPrice != nil && price.Amount < *p.Filters.MinPrice {
			continue
		}
		if p.Filters.MaxPrice != nil && price.Amount > *p.Filters.MaxPrice {
			continue
		}
		page.Hits = append(page.Hits, Hit{Fact: f, MatchReason: "与搜索描述相关"})
	}
	return page, offset
}
func (s *Service) next(ctx context.Context, p ParsedQuery, g Generation, hash, cursor string) (Page, error) {
	c, e := DecodeCursor(cursor, s.config.CursorKey, time.Now())
	if e != nil {
		return Page{}, e
	}
	if c.QueryHash != hash || c.GenerationID != g.ID {
		return Page{}, ErrInvalidCursor
	}
	if s.cache == nil {
		return Page{}, ErrCursorExpired
	}
	snap, ok, e := s.cache.Snapshot(ctx, c.SnapshotID)
	if e != nil {
		return Page{}, ErrUnavailable
	}
	if !ok || snap.ExpiresAtUnixMs <= time.Now().UnixMilli() {
		return Page{}, ErrCursorExpired
	}
	if snap.QueryHash != hash || snap.GenerationID != g.ID || snap.ExpiresAtUnixMs != c.ExpiresAtUnixMs || len(snap.IDs) > 200 || c.Offset > len(snap.IDs) {
		return Page{}, ErrInvalidCursor
	}
	facts, e := s.facts(ctx, snap.IDs[c.Offset:])
	if e != nil {
		return Page{}, e
	}
	page, offset := pageFromFacts(p, snap.IDs, facts, c.Offset)
	page.Mode = snap.Mode
	page.DegradedReason = snap.DegradedReason
	if offset < len(snap.IDs) {
		c.Offset = offset
		page.NextCursor, _ = EncodeCursor(c, s.config.CursorKey)
	}
	return page, nil
}
