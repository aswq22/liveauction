package data

import (
	"context"
	"errors"
	"sync"
	"time"

	"live-auction-bid/backend/app/auction/service/internal/biz/auction"
)

type AgentApprovalOutboxWorker struct {
	repository               *AgentApprovalOutboxRepository
	interval                 time.Duration
	limit                    int
	mu                       sync.Mutex
	running                  bool
	cancel                   context.CancelFunc
	done                     chan struct{}
	lastAttempt, lastSuccess time.Time
	lastError                string
}

func NewAgentApprovalOutboxWorker(store *Store, interval time.Duration, limit int) *AgentApprovalOutboxWorker {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if limit <= 0 {
		limit = 100
	}
	if store == nil {
		return &AgentApprovalOutboxWorker{repository: NewAgentApprovalOutboxRepository(nil, nil), interval: interval, limit: limit}
	}
	return &AgentApprovalOutboxWorker{repository: NewAgentApprovalOutboxRepository(store.db, store), interval: interval, limit: limit}
}
func (w *AgentApprovalOutboxWorker) WithObserver(o AgentApprovalOutboxObserver) *AgentApprovalOutboxWorker {
	if w != nil {
		w.repository.WithObserver(o)
	}
	return w
}
func (w *AgentApprovalOutboxWorker) Start(ctx context.Context) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	w.running = true
	w.cancel = cancel
	w.done = done
	w.mu.Unlock()
	go func() {
		defer func() {
			w.mu.Lock()
			w.running = false
			w.cancel = nil
			close(done)
			w.mu.Unlock()
		}()
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			w.run(workerCtx)
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (w *AgentApprovalOutboxWorker) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (w *AgentApprovalOutboxWorker) Wait() {
	if w == nil {
		return
	}
	w.mu.Lock()
	done := w.done
	w.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (w *AgentApprovalOutboxWorker) run(ctx context.Context) {
	now := time.Now()
	err := w.repository.PublishPending(ctx, w.limit)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastAttempt = now
	if err != nil {
		if errors.Is(err, ErrAgentApprovalOutboxRetryable) {
			w.lastError = "AGENT_APPROVAL_OUTBOX_RETRYABLE"
		} else {
			w.lastError = "AGENT_APPROVAL_OUTBOX_INTERNAL"
		}
		return
	}
	w.lastSuccess = now
	w.lastError = ""
}
func (w *AgentApprovalOutboxWorker) Ping(context.Context) error {
	if w == nil || w.repository == nil || w.repository.db == nil || w.repository.publisher == nil {
		return errors.New("agent approval outbox worker is not initialized")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.running {
		return errors.New("agent approval outbox worker is not started")
	}
	return nil
}
func (w *AgentApprovalOutboxWorker) WorkerStatus(context.Context) auction.WorkerStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return auction.WorkerStatus{Name: "agent_approval_outbox_worker", Mode: auction.WorkerModeLeader, Started: w.running, LastAttemptAt: formatWorkerTime(w.lastAttempt), LastSuccessAt: formatWorkerTime(w.lastSuccess), LastError: w.lastError}
}
