package searchindex

import (
	"context"
	"errors"
	bs "live-auction-bid/backend/app/auction/service/internal/biz/buyersearch"
	"sync"
	"testing"
)

type workerJobs struct {
	doc            bs.Document
	acked, retried bool
}
type revisionTarget struct {
	workerTarget
	mu       sync.Mutex
	revision uint64
}

func (i *revisionTarget) Upsert(_ context.Context, _ bs.Generation, d bs.Document, v []float64) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if d.Revision.Metadata >= i.revision {
		i.revision = d.Revision.Metadata
		i.doc = d
		i.vector = v
	}
	return nil
}

type delayedWorkerEmbed struct{ started, release chan struct{} }

func (e *delayedWorkerEmbed) Embed(ctx context.Context, _ []string) ([][]float64, error) {
	close(e.started)
	select {
	case <-e.release:
		return [][]float64{{1, 0}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestNewLexicalRevisionInvalidatesOldVectorEvenWhenEmbeddingFails(t *testing.T) {
	index := &revisionTarget{}
	delayed := &delayedWorkerEmbed{started: make(chan struct{}), release: make(chan struct{})}
	oldJob := &workerJobs{doc: bs.Document{LotID: "a", Public: true, Text: "旧玉石", Revision: bs.Revision{Semantic: 1, Metadata: 1}}}
	oldDone := make(chan error, 1)
	go func() { oldDone <- NewReliableWorker(oldJob, index, delayed).RunOne(context.Background()) }()
	<-delayed.started
	newJob := &workerJobs{doc: bs.Document{LotID: "a", Public: true, Text: "新相机", Revision: bs.Revision{Semantic: 2, Metadata: 2}}}
	if e := NewReliableWorker(newJob, index, &workerEmbed{err: errors.New("provider unavailable")}).RunOne(context.Background()); e == nil {
		t.Fatal("failure hidden")
	}
	close(delayed.release)
	<-oldDone
	index.mu.Lock()
	defer index.mu.Unlock()
	if index.doc.Text != "新相机" || index.revision != 2 || len(index.vector) != 0 {
		t.Fatal("stale semantic revision survived", index.doc, index.vector)
	}
}

func (j *workerJobs) Claim(context.Context) (SearchJob, bool, error) {
	return SearchJob{LotID: "a", Token: "lease", Revision: 1}, true, nil
}
func (j *workerJobs) Load(context.Context, SearchJob) (bs.Document, error) { return j.doc, nil }
func (j *workerJobs) Ack(context.Context, SearchJob) error                 { j.acked = true; return nil }
func (j *workerJobs) Retry(context.Context, SearchJob, error) error        { j.retried = true; return nil }

type workerTarget struct {
	err    error
	vector []float64
	doc    bs.Document
}

func (i *workerTarget) Active(context.Context) (bs.Generation, error) {
	return bs.Generation{ID: "g", Dimensions: 2}, nil
}
func (i *workerTarget) Vector(context.Context, bs.Generation, []float64, bs.Filters, int) ([]bs.Candidate, error) {
	return nil, nil
}
func (i *workerTarget) Lexical(context.Context, bs.Generation, bs.ParsedQuery, int) ([]bs.Candidate, error) {
	return nil, nil
}
func (i *workerTarget) Upsert(_ context.Context, _ bs.Generation, d bs.Document, v []float64) error {
	i.vector = v
	i.doc = d
	return i.err
}

type workerEmbed struct {
	calls int
	err   error
}

func (e *workerEmbed) Embed(context.Context, []string) ([][]float64, error) {
	e.calls++
	return [][]float64{{1, 0}}, e.err
}
func TestReliableWorkerNeverAcknowledgesFailedUpsert(t *testing.T) {
	j := &workerJobs{doc: bs.Document{LotID: "a", Public: true, Text: "玉", Revision: bs.Revision{Semantic: 1, Metadata: 1}}}
	i := &workerTarget{err: errors.New("pg down")}
	w := NewReliableWorker(j, i, &workerEmbed{})
	if e := w.RunOne(context.Background()); e == nil || j.acked || !j.retried {
		t.Fatal(e, j)
	}
}
func TestReliableWorkerDoesNotEmbedHiddenDocument(t *testing.T) {
	j := &workerJobs{doc: bs.Document{LotID: "a", Deleted: true, Revision: bs.Revision{Semantic: 1, Metadata: 1}}}
	i := &workerTarget{}
	e := &workerEmbed{}
	w := NewReliableWorker(j, i, e)
	if err := w.RunOne(context.Background()); err != nil || !j.acked || e.calls != 0 || len(i.vector) != 0 {
		t.Fatal(err, j, e)
	}
}
