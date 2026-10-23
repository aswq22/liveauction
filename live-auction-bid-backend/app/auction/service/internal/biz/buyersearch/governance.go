package buyersearch

import (
	"context"
	"math"
	"time"
)

type embeddingFlight struct {
	done   chan struct{}
	cancel context.CancelFunc
	refs   int
	vector []float64
	err    error
}

func (s *Service) embeddingWaiters(key string) int {
	s.embedMu.Lock()
	defer s.embedMu.Unlock()
	if f := s.flights[key]; f != nil {
		return f.refs
	}
	return 0
}
func validEmbedding(v []float64, n int) bool {
	if len(v) != n {
		return false
	}
	var norm float64
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
		norm += x * x
	}
	return norm > 0 && !math.IsInf(norm, 0)
}
func (s *Service) sharedEmbedding(ctx context.Context, key, text string, g Generation, embedder Embedder) ([]float64, error) {
	s.embedMu.Lock()
	if time.Now().Before(s.embedOpenUntil) {
		s.embedMu.Unlock()
		return nil, ErrUnavailable
	}
	f := s.flights[key]
	if f == nil {
		ec, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.config.EmbeddingTimeout)
		f = &embeddingFlight{done: make(chan struct{}), cancel: cancel}
		s.flights[key] = f
		go s.runEmbedding(ec, key, text, g, f, embedder)
	}
	f.refs++
	s.embedMu.Unlock()
	defer func() {
		s.embedMu.Lock()
		f.refs--
		if f.refs == 0 {
			f.cancel()
			if s.flights[key] == f {
				delete(s.flights, key)
			}
		}
		s.embedMu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		return append([]float64(nil), f.vector...), f.err
	}
}
func (s *Service) runEmbedding(ctx context.Context, key, text string, g Generation, f *embeddingFlight, embedder Embedder) {
	defer f.cancel()
	var v []float64
	var err error
	select {
	case s.embedSlots <- struct{}{}:
		vs, e := embedder.Embed(ctx, []string{text})
		<-s.embedSlots
		if e != nil || len(vs) != 1 || !validEmbedding(vs[0], g.Dimensions) {
			err = ErrUnavailable
		} else {
			v = vs[0]
		}
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err == nil && s.cache != nil {
		cc, stop := context.WithTimeout(ctx, 40*time.Millisecond)
		_ = s.cache.PutVector(cc, key, v, 24*time.Hour)
		stop()
	}
	s.embedMu.Lock()
	f.vector = v
	f.err = err
	if ctx.Err() != context.Canceled {
		if err != nil {
			s.embedFailures++
			if s.embedFailures >= 5 {
				s.embedOpenUntil = time.Now().Add(30 * time.Second)
			}
		} else {
			s.embedFailures = 0
			s.embedOpenUntil = time.Time{}
		}
	}
	if s.flights[key] == f {
		delete(s.flights, key)
	}
	close(f.done)
	s.embedMu.Unlock()
}
