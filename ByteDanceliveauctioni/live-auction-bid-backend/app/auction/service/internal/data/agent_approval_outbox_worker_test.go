package data

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type approvalOutboxPublisherSpy struct {
	mu      sync.Mutex
	events  []AgentApprovalOutboxEvent
	fail    int
	failErr error
}

func (p *approvalOutboxPublisherSpy) PublishAgentApproval(_ context.Context, e AgentApprovalOutboxEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
	if p.fail > 0 {
		p.fail--
		if p.failErr != nil {
			return p.failErr
		}
		return errors.New("transport down")
	}
	return nil
}

type approvalOutboxObserverSpy struct {
	mu             sync.Mutex
	spans, visible int
	results        []string
	latencies      []time.Duration
}

func (o *approvalOutboxObserverSpy) BeginOutboxPublish(ctx context.Context, _ string) (context.Context, func(string)) {
	o.mu.Lock()
	o.spans++
	o.mu.Unlock()
	return ctx, func(r string) { o.mu.Lock(); defer o.mu.Unlock(); o.results = append(o.results, r) }
}
func (o *approvalOutboxObserverSpy) RecordOutboxVisibleLatency(d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.visible++
	o.latencies = append(o.latencies, d)
}

func TestAgentApprovalOutboxPublishSuccessAndFailureRetry(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	f := seedAgentApprovalFixture(t, db, "publish-retry", 1, false)
	if _, err := NewAgentApprovalRepository(db).Apply(context.Background(), f.command); err != nil {
		t.Fatal(err)
	}
	pub := &approvalOutboxPublisherSpy{fail: 1}
	obs := &approvalOutboxObserverSpy{}
	repo := NewAgentApprovalOutboxRepository(db, pub).WithObserver(obs)
	var seeded AgentApprovalOutboxModel
	if err := db.First(&seeded).Error; err != nil {
		t.Fatal(err)
	}
	current := seeded.VisibleAtUnixMs + 1234
	repo.now = func() time.Time { return time.UnixMilli(current) }
	if err := repo.PublishPending(context.Background(), 10); err == nil {
		t.Fatal("first publish succeeded")
	}
	var row AgentApprovalOutboxModel
	db.First(&row)
	if row.PublishedAtUnixMs != 0 {
		t.Fatalf("failure marked published: %+v", row)
	}
	if err := repo.PublishPending(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if len(pub.events) != 1 {
		t.Fatalf("backoff hot-loop deliveries=%d", len(pub.events))
	}
	current += 2000
	if err := repo.PublishPending(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	db.First(&row)
	if row.PublishedAtUnixMs == 0 {
		t.Fatalf("success not marked: %+v", row)
	}
	if len(pub.events) != 2 || pub.events[0].ID != pub.events[1].ID || obs.visible != 1 || obs.spans != 2 {
		t.Fatalf("pub=%+v obs=%+v", pub, obs)
	}
	if len(obs.latencies) != 1 || obs.latencies[0] != 3234*time.Millisecond {
		t.Fatalf("latencies=%v", obs.latencies)
	}
}

func TestAgentApprovalOutboxPoisonIsolationClaimExpiryAndSafeStatus(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	now := time.Now().UnixMilli()
	poison := AgentApprovalOutboxModel{ID: "outbox-poison", ExecutionID: "exec-poison", SessionID: "session-poison", RoomID: "room", PayloadJSON: `{"prompt":"secret"}`, VisibleAtUnixMs: now}
	validPayload := `{"sessionId":"session-valid","executionId":"exec-valid","lotIds":["lot-1"],"roomId":"room","queuePositions":[1],"traceId":""}`
	valid := AgentApprovalOutboxModel{ID: "outbox-valid", ExecutionID: "exec-valid", SessionID: "session-valid", RoomID: "room", PayloadJSON: validPayload, VisibleAtUnixMs: now + 1, ClaimToken: "crashed", ClaimUntilUnixMs: now - 1}
	if err := db.Create(&poison).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&valid).Error; err != nil {
		t.Fatal(err)
	}
	pub := &approvalOutboxPublisherSpy{}
	repo := NewAgentApprovalOutboxRepository(db, pub)
	if err := repo.PublishPending(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var poisonAfter, validAfter AgentApprovalOutboxModel
	db.First(&poisonAfter, "id = ?", poison.ID)
	db.First(&validAfter, "id = ?", valid.ID)
	if poisonAfter.DeadLetteredAtUnixMs == 0 || poisonAfter.LastPublishError != "INVALID_PAYLOAD" || validAfter.PublishedAtUnixMs == 0 || len(pub.events) != 1 {
		t.Fatalf("poison=%+v valid=%+v events=%d", poisonAfter, validAfter, len(pub.events))
	}

	// Worker status never exposes raw publisher/provider/endpoint detail and Stop waits.
	f := seedAgentApprovalFixture(t, db, "worker-safe", 30, false)
	if _, err := NewAgentApprovalRepository(db).Apply(context.Background(), f.command); err != nil {
		t.Fatal(err)
	}
	secretPub := &approvalOutboxPublisherSpy{fail: 1, failErr: errors.New("redis endpoint provider sk-secret")}
	worker := &AgentApprovalOutboxWorker{repository: NewAgentApprovalOutboxRepository(db, secretPub), interval: time.Millisecond, limit: 10}
	ctx, cancel := context.WithCancel(context.Background())
	worker.Start(ctx)
	time.Sleep(20 * time.Millisecond)
	worker.Start(ctx)
	cancel()
	worker.Stop()
	status := worker.WorkerStatus(context.Background())
	if status.Started || strings.Contains(strings.ToLower(status.LastError), "redis") || strings.Contains(strings.ToLower(status.LastError), "secret") || strings.Contains(strings.ToLower(status.LastError), "provider") {
		t.Fatalf("unsafe status=%+v", status)
	}
}

func TestAgentApprovalOutboxConcurrentWorkersClaimOnce(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	f := seedAgentApprovalFixture(t, db, "publish-race", 1, false)
	result, err := NewAgentApprovalRepository(db).Apply(context.Background(), f.command)
	if err != nil {
		t.Fatal(err)
	}
	// Compatible Apply replay must not append a second durable outbox row.
	if _, err := NewAgentApprovalRepository(db).Apply(context.Background(), f.command); err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Model(&AgentApprovalOutboxModel{}).Where("execution_id = ?", result.ID).Count(&count)
	if count != 1 {
		t.Fatalf("rows=%d", count)
	}
	pub := &approvalOutboxPublisherSpy{}
	a := NewAgentApprovalOutboxRepository(db, pub)
	b := NewAgentApprovalOutboxRepository(db, pub)
	var wg sync.WaitGroup
	wg.Add(2)
	for _, repo := range []*AgentApprovalOutboxRepository{a, b} {
		go func(r *AgentApprovalOutboxRepository) {
			defer wg.Done()
			_ = r.PublishPending(context.Background(), 10)
		}(repo)
	}
	wg.Wait()
	if len(pub.events) != 1 {
		t.Fatalf("concurrent deliveries=%d", len(pub.events))
	}
}
