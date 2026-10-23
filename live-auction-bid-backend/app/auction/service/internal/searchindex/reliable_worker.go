package searchindex

import (
	"context"
	"errors"
	bs "live-auction-bid/backend/app/auction/service/internal/biz/buyersearch"
	"log/slog"
	"time"
)

type SearchJob struct {
	LotID, Token string
	Revision     uint64
	Attempts     int
}
type SearchJobs interface {
	Claim(context.Context) (SearchJob, bool, error)
	Load(context.Context, SearchJob) (bs.Document, error)
	Ack(context.Context, SearchJob) error
	Retry(context.Context, SearchJob, error) error
}
type ReliableWorker struct {
	jobs     SearchJobs
	index    bs.Index
	embedder bs.Embedder
}

func NewReliableWorker(j SearchJobs, i bs.Index, e bs.Embedder) *ReliableWorker {
	return &ReliableWorker{jobs: j, index: i, embedder: e}
}

type generationWriter interface {
	WritableGenerations(context.Context) ([]bs.Generation, error)
}
type embeddingInspector interface {
	HasEmbedding(context.Context, bs.Generation, bs.Document) (bool, error)
}

// A durable, fenced acknowledgement follows successful writes to every target.
func (w *ReliableWorker) RunOne(ctx context.Context) error {
	job, ok, e := w.jobs.Claim(ctx)
	if e != nil || !ok {
		return e
	}
	ctx, stop := context.WithTimeout(ctx, 8*time.Second)
	defer stop()
	e = w.apply(ctx, job)
	if e != nil {
		rc, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if re := w.jobs.Retry(rc, job, e); re != nil {
			return errors.Join(e, re)
		}
		return e
	}
	return w.jobs.Ack(ctx, job)
}
func (w *ReliableWorker) apply(ctx context.Context, j SearchJob) error {
	d, e := w.jobs.Load(ctx, j)
	if e != nil {
		return e
	}
	var gs []bs.Generation
	if multi, ok := w.index.(generationWriter); ok {
		gs, e = multi.WritableGenerations(ctx)
	} else {
		var g bs.Generation
		g, e = w.index.Active(ctx)
		gs = []bs.Generation{g}
	}
	if e != nil {
		return e
	}
	if len(gs) == 0 {
		return errors.New("no writable search generation")
	}
	for _, g := range gs {
		embedder, resolveError := bs.ResolveEmbedder(w.embedder, g)
		if resolveError != nil && d.Public && !d.Deleted {
			return resolveError
		}
		d.Hash = bs.SemanticHash(d, g)
		// Publish the revision fence and fresh lexical content before the paid
		// provider call. A newer semantic revision clears its old vector; an
		// unchanged revision preserves its existing vector in the SQL adapter.
		if e = w.index.Upsert(ctx, g, d, nil); e != nil {
			return e
		}
		var v []float64
		if d.Public && !d.Deleted {
			have := false
			if inspector, ok := w.index.(embeddingInspector); ok {
				have, e = inspector.HasEmbedding(ctx, g, d)
				if e != nil {
					return e
				}
			}
			if !have {
				if w.embedder == nil {
					return errors.New("index embedding unavailable")
				}
				vs, err := embedder.Embed(ctx, []string{d.Text})
				if err != nil {
					return err
				}
				if len(vs) != 1 || !validVector(vs[0], g.Dimensions) {
					return errors.New("invalid worker embedding")
				}
				v = vs[0]
			}
		}
		if e = w.index.Upsert(ctx, g, d, v); e != nil {
			return e
		}
	}
	return nil
}
func (w *ReliableWorker) Start(ctx context.Context) {
	for n := 0; n < 4; n++ {
		go func() {
			timer := time.NewTimer(0)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
					if e := w.RunOne(ctx); e != nil {
						slog.Warn("buyer search job failed", "error_type", errorClass(e))
					}
					timer.Reset(250 * time.Millisecond)
				}
			}
		}()
	}
}
func errorClass(e error) string {
	var p *ProviderError
	if errors.As(e, &p) {
		return "embedding_provider"
	}
	if errors.Is(e, context.DeadlineExceeded) {
		return "timeout"
	}
	return "dependency"
}
