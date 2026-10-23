package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
)

func TestAgentToolConfigDisablesListenerWithoutToken(t *testing.T) {
	t.Setenv("AUCTION_INTERNAL_SERVICE_TOKEN", "")
	t.Setenv("AUCTION_AGENT_TOOL_GRPC_ADDR", ":29999")

	config, enabled := agentToolConfigFromEnv()
	if enabled {
		t.Fatal("agent Tool listener enabled without internal token")
	}
	if config.Token != "" || config.Addr != "" {
		t.Fatalf("disabled config = %+v, want zero value", config)
	}
}

func TestAgentToolConfigUsesDefaultAddressWhenEnabled(t *testing.T) {
	t.Setenv("AUCTION_INTERNAL_SERVICE_TOKEN", "secret")
	t.Setenv("AUCTION_AGENT_TOOL_GRPC_ADDR", "")

	config, enabled := agentToolConfigFromEnv()
	if !enabled {
		t.Fatal("agent Tool listener disabled with configured token")
	}
	if config.Token != "secret" || config.Addr != ":19082" {
		t.Fatalf("enabled config = %+v, want token and :19082", config)
	}
}

func TestAgentRuntimeConfigRequiresBothAddressAndSharedToken(t *testing.T) {
	for _, test := range []struct {
		name, address, token string
	}{
		{name: "neither"},
		{name: "missing address", token: "secret"},
		{name: "missing token", address: "127.0.0.1:19081"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("AUCTION_AGENT_RUNTIME_ADDR", test.address)
			t.Setenv("AUCTION_INTERNAL_SERVICE_TOKEN", test.token)
			config, enabled := agentRuntimeConfigFromEnv()
			if enabled || config != (agentRuntimeConfig{}) {
				t.Fatalf("config = %+v enabled=%t, want disabled zero value", config, enabled)
			}
		})
	}
}

func TestAgentRuntimeConfigUsesExplicitAddressAndSharedToken(t *testing.T) {
	t.Setenv("AUCTION_AGENT_RUNTIME_ADDR", " auction-agent:19081 ")
	t.Setenv("AUCTION_INTERNAL_SERVICE_TOKEN", " shared-secret ")

	config, enabled := agentRuntimeConfigFromEnv()
	if !enabled {
		t.Fatal("runtime gateway disabled with complete configuration")
	}
	if config.Address != "auction-agent:19081" || config.Token != "shared-secret" {
		t.Fatalf("runtime config = %+v", config)
	}
}

func TestAgentGatewayRoutesRemainRegisteredWithoutRuntimeConfiguration(t *testing.T) {
	t.Setenv("AUCTION_AGENT_RUNTIME_ADDR", "")
	t.Setenv("AUCTION_INTERNAL_SERVICE_TOKEN", "")

	handler, runtimeClient, gatewayService, err := newAgentGatewayFromEnv(&mainAgentGatewayRepository{}, nil)
	if err != nil {
		t.Fatalf("newAgentGatewayFromEnv() error = %v", err)
	}
	if handler == nil {
		t.Fatal("Agent HTTP handler is nil when Runtime configuration is absent")
	}
	if runtimeClient != nil {
		t.Fatal("Runtime client was constructed without address/token configuration")
	}
	if gatewayService == nil {
		t.Fatal("Agent gateway service is nil")
	}
	gatewayService.Close()
}

type mainAgentGatewayRepository struct{}

func (*mainAgentGatewayRepository) Create(context.Context, agentcontrol.Run) error { return nil }
func (*mainAgentGatewayRepository) CreateIdempotent(_ context.Context, run agentcontrol.Run) (agentcontrol.Run, bool, error) {
	return run, true, nil
}
func (*mainAgentGatewayRepository) Find(context.Context, string) (agentcontrol.Run, error) {
	return agentcontrol.Run{}, errors.New("unused")
}
func (*mainAgentGatewayRepository) ListProjectable(context.Context, int) ([]agentcontrol.Run, error) {
	return nil, nil
}
func (*mainAgentGatewayRepository) ProjectRuntimeEvent(context.Context, agentcontrol.RuntimeEvent, *agentcontrol.RuntimeRun) (agentcontrol.Run, bool, error) {
	return agentcontrol.Run{}, false, errors.New("unused")
}
func (*mainAgentGatewayRepository) ProjectRuntimeSnapshot(_ context.Context, _ string, run agentcontrol.RuntimeRun) (agentcontrol.Run, error) {
	return agentcontrol.Run{ID: run.RunID, Status: run.Status}, nil
}
func (*mainAgentGatewayRepository) UpdateStatus(context.Context, string, int64, agentcontrol.RunStatus, string, string, string) error {
	return errors.New("unused")
}
func (*mainAgentGatewayRepository) AppendEvent(context.Context, agentcontrol.Event) error {
	return errors.New("unused")
}
func (*mainAgentGatewayRepository) ListEvents(context.Context, string, int64, int) ([]agentcontrol.Event, error) {
	return nil, errors.New("unused")
}
func (*mainAgentGatewayRepository) FailRuntimeStart(context.Context, string, int64, string, string, agentcontrol.Event) error {
	return errors.New("unused")
}

func (*mainAgentGatewayRepository) CompleteProjection(context.Context, string, int64) error {
	return errors.New("unused")
}

var _ agentcontrol.GatewayRepository = (*mainAgentGatewayRepository)(nil)

func TestRunServersStopsAllServersWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	first := newLifecycleTestServer(nil)
	second := newLifecycleTestServer(nil)
	done := make(chan error, 1)
	go func() { done <- runServers(ctx, first, second) }()

	waitForStart(t, first)
	waitForStart(t, second)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServers() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runServers() did not stop after cancellation")
	}
	if !first.wasStopped() || !second.wasStopped() {
		t.Fatalf("stopped states = %v, %v; want both true", first.wasStopped(), second.wasStopped())
	}
}

func TestRunServersPropagatesStartupErrorAndStopsPeer(t *testing.T) {
	wantErr := errors.New("listen failed")
	failing := newLifecycleTestServer(wantErr)
	peer := newLifecycleTestServer(nil)

	err := runServers(context.Background(), peer, failing)
	if !errors.Is(err, wantErr) {
		t.Fatalf("runServers() error = %v, want %v", err, wantErr)
	}
	if !peer.wasStopped() {
		t.Fatal("peer server was not stopped after sibling startup error")
	}
}

func TestApplicationExitCodePropagatesServerStartupError(t *testing.T) {
	wantErr := errors.New("grpc listen failed")
	peer := newLifecycleTestServer(nil)
	failing := newLifecycleTestServer(wantErr)

	got := applicationExitCode(func() error {
		return runServers(context.Background(), peer, failing)
	})
	if got != 1 {
		t.Fatalf("applicationExitCode() = %d, want 1", got)
	}
	if !peer.wasStopped() {
		t.Fatal("peer server was not stopped before nonzero exit decision")
	}
}

func TestPreStopServerRunsHubDrainBeforeTransportStop(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	transport := newLifecycleTestServer(nil)
	wrapped := withPreStop(transport, func(context.Context) error {
		mu.Lock()
		calls = append(calls, "hub-drain")
		mu.Unlock()
		return nil
	})
	transport.onStop = func() {
		mu.Lock()
		calls = append(calls, "transport-stop")
		mu.Unlock()
	}

	if err := wrapped.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := strings.Join(calls, ","), "hub-drain,transport-stop"; got != want {
		t.Fatalf("shutdown calls = %q, want %q", got, want)
	}
}

func TestPreStopServerAttemptsTransportStopAndJoinsBothErrors(t *testing.T) {
	drainErr := errors.New("hub drain failed")
	transportErr := errors.New("transport stop failed")
	transport := newLifecycleTestServer(nil)
	transport.stopErr = transportErr
	wrapped := withPreStop(transport, func(context.Context) error { return drainErr })

	err := wrapped.Stop(context.Background())
	if !errors.Is(err, drainErr) || !errors.Is(err, transportErr) {
		t.Fatalf("Stop() error = %v, want joined drain and transport errors", err)
	}
	if !transport.wasStopped() {
		t.Fatal("transport Stop was not attempted after Hub drain error")
	}
}

func TestPreStopServerAttemptsTransportStopAfterDrainDeadline(t *testing.T) {
	transport := newLifecycleTestServer(nil)
	wrapped := withPreStop(transport, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	err := wrapped.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v, want deadline exceeded", err)
	}
	if !transport.wasStopped() {
		t.Fatal("transport Stop was not attempted after Hub drain deadline")
	}
}

type lifecycleTestServer struct {
	startErr error
	started  chan struct{}
	stop     chan struct{}
	once     sync.Once
	mu       sync.Mutex
	stopped  bool
	onStop   func()
	stopErr  error
}

func newLifecycleTestServer(startErr error) *lifecycleTestServer {
	return &lifecycleTestServer{startErr: startErr, started: make(chan struct{}), stop: make(chan struct{})}
}

func (s *lifecycleTestServer) Start(context.Context) error {
	s.once.Do(func() { close(s.started) })
	if s.startErr != nil {
		return s.startErr
	}
	<-s.stop
	return nil
}

func (s *lifecycleTestServer) Stop(context.Context) error {
	if s.onStop != nil {
		s.onStop()
	}
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	return s.stopErr
}

func (s *lifecycleTestServer) wasStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func waitForStart(t *testing.T, server *lifecycleTestServer) {
	t.Helper()
	select {
	case <-server.started:
	case <-time.After(time.Second):
		t.Fatal("server did not start")
	}
}
