package realtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
)

type allowRoomAccess struct{}

func (allowRoomAccess) ValidateRoomInMainAccount(context.Context, string, string) error { return nil }

func TestHubShutdownOwnsHandlerPausedAfterUpgradeBeforeRegistration(t *testing.T) {
	baseline := wsConnectionMetricSnapshot(t)
	hub := newShutdownTestHub(t)
	upgraded := make(chan struct{})
	releaseRegistration := make(chan struct{})
	drainScanned := make(chan struct{})
	hub.lifecycleHooks = &hubLifecycleHooks{
		afterUpgradeBeforeRegister: func() {
			close(upgraded)
			<-releaseRegistration
		},
		afterDrainScan: func() { close(drainScanned) },
	}
	handlerDone := make(chan string, 1)
	server := newShutdownTestServer(hub, handlerDone)
	t.Cleanup(server.Close)

	dialResult := make(chan shutdownDialResult, 1)
	go func() {
		client, response, err := websocket.DefaultDialer.Dial(shutdownWebsocketURL(server.URL, "pre-register", ScopePublic, ""), nil)
		dialResult <- shutdownDialResult{client: client, response: response, err: err}
	}()
	select {
	case <-upgraded:
	case <-time.After(time.Second):
		t.Fatal("ServeRoom did not pause after WebSocket upgrade")
	}

	shutdownResult := startHubShutdown(hub)
	select {
	case <-drainScanned:
	case <-time.After(time.Second):
		t.Fatal("Hub shutdown did not finish its registered-connection scan")
	}
	assertSharedShutdownStillWaiting(t, hub)

	close(releaseRegistration)
	if err := <-shutdownResult; err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := hub.Shutdown(context.Background()); err != nil {
		t.Fatalf("later Shutdown() after timed-out waiter error = %v", err)
	}
	result := <-dialResult
	if result.response != nil {
		_ = result.response.Body.Close()
	}
	if result.err == nil {
		_ = result.client.SetReadDeadline(time.Now().Add(time.Second))
		if _, _, err := result.client.ReadMessage(); err == nil {
			t.Fatal("post-upgrade connection survived rejected registration")
		}
		_ = result.client.Close()
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown returned before the pre-registration ServeRoom handler exited")
	}
	if got := hub.RoomPresence("pre-register").GetTotalConnections(); got != 0 {
		t.Fatalf("pre-registration room presence = %d, want 0", got)
	}
	assertWSConnectionMetricRestored(t, baseline)
}

func TestHubShutdownWaitsForAdmittedHandlerAfterMembershipRemoval(t *testing.T) {
	baseline := wsConnectionMetricSnapshot(t)
	snapshot := &gatedSnapshotProvider{entered: make(chan struct{}), release: make(chan struct{})}
	hub := NewHub(snapshot)
	drainScanned := make(chan struct{})
	hub.lifecycleHooks = &hubLifecycleHooks{afterDrainScan: func() { close(drainScanned) }}
	handlerDone := make(chan string, 1)
	server := newShutdownTestServer(hub, handlerDone)
	t.Cleanup(server.Close)
	client := dialShutdownTestConnection(t, server.URL, "self-removed", ScopePublic, "")

	select {
	case <-snapshot.entered:
	case <-time.After(time.Second):
		t.Fatal("ServeRoom did not block in snapshot delivery")
	}
	waitForWSConnectionGauge(t, ScopePublic, baseline[ScopePublic]+1)
	registered := singleRoomConnection(t, hub, "self-removed")
	hub.leave(registered)
	_ = client.Close()
	if got := hub.RoomPresence("self-removed").GetTotalConnections(); got != 0 {
		t.Fatalf("self-removed room presence = %d, want 0", got)
	}

	shutdownResult := startHubShutdown(hub)
	select {
	case <-drainScanned:
	case <-time.After(time.Second):
		t.Fatal("Hub shutdown did not finish scan after membership removal")
	}
	assertSharedShutdownStillWaiting(t, hub)

	close(snapshot.release)
	if err := <-shutdownResult; err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown returned before self-removed ServeRoom handler exited")
	}
	assertWSConnectionMetricRestored(t, baseline)
}

func TestHubShutdownWaitsForWritePumpCompletion(t *testing.T) {
	baseline := wsConnectionMetricSnapshot(t)
	hub := newShutdownTestHub(t)
	writerExiting := make(chan struct{})
	releaseWriter := make(chan struct{})
	hub.lifecycleHooks = &hubLifecycleHooks{beforeWritePumpExit: func() {
		close(writerExiting)
		<-releaseWriter
	}}
	handlerDone := make(chan string, 1)
	server := newShutdownTestServer(hub, handlerDone)
	t.Cleanup(server.Close)
	client := dialShutdownTestConnection(t, server.URL, "blocked-writer", ScopePublic, "")
	waitForWSConnectionGauge(t, ScopePublic, baseline[ScopePublic]+1)
	readResult := make(chan error, 1)
	go func() { _, _, err := client.ReadMessage(); readResult <- err }()

	shutdownResult := startHubShutdown(hub)
	select {
	case <-writerExiting:
	case <-time.After(time.Second):
		t.Fatal("writePump did not unblock and reach its exit gate")
	}
	select {
	case err := <-readResult:
		if err == nil {
			t.Fatal("client read remained open after drain closed the socket")
		}
	case <-time.After(time.Second):
		t.Fatal("client read did not unblock after drain reached writer exit")
	}
	assertSharedShutdownStillWaiting(t, hub)

	close(releaseWriter)
	if err := <-shutdownResult; err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown returned before ServeRoom completed writePump ownership")
	}
	_ = client.Close()
	if got := hub.RoomPresence("blocked-writer").GetTotalConnections(); got != 0 {
		t.Fatalf("blocked-writer room presence = %d, want 0", got)
	}
	assertWSConnectionMetricRestored(t, baseline)
}

func TestHubShutdownDrainsLivePublicAndAdminConnectionsExactlyOnce(t *testing.T) {
	baseline := wsConnectionMetricSnapshot(t)
	hub := newShutdownTestHub(t)
	handlerDone := make(chan string, 2)
	server := newShutdownTestServer(hub, handlerDone)
	t.Cleanup(server.Close)

	public := dialShutdownTestConnection(t, server.URL, "shutdown-public", ScopePublic, "")
	adminTicket := issueShutdownAdminTicket(t, hub, "shutdown-admin")
	admin := dialShutdownTestConnection(t, server.URL, "shutdown-admin", ScopeAdmin, adminTicket)

	waitForWSConnectionGauge(t, ScopePublic, baseline[ScopePublic]+1)
	waitForWSConnectionGauge(t, ScopeAdmin, baseline[ScopeAdmin]+1)

	readResults := make(chan error, 2)
	go func() { _, _, err := public.ReadMessage(); readResults <- err }()
	go func() { _, _, err := admin.ReadMessage(); readResults <- err }()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	shutdownResults := make(chan error, 2)
	go func() { shutdownResults <- hub.Shutdown(shutdownCtx) }()
	go func() { shutdownResults <- hub.Shutdown(shutdownCtx) }()
	for range 2 {
		if err := <-shutdownResults; err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}

	for _, client := range []*websocket.Conn{public, admin} {
		if err := <-readResults; err == nil {
			t.Error("client read remained open after Hub shutdown")
		}
		_ = client.Close()
		_ = client.Close()
	}
	for range 2 {
		select {
		case <-handlerDone:
		case <-time.After(time.Second):
			t.Fatal("ServeRoom handler did not exit during Hub shutdown")
		}
	}
	for _, roomID := range []string{"shutdown-public", "shutdown-admin"} {
		if got := hub.RoomPresence(roomID).GetTotalConnections(); got != 0 {
			t.Errorf("room %q presence after shutdown = %d, want 0", roomID, got)
		}
	}
	assertWSConnectionMetricRestored(t, baseline)

	afterShutdown, response, err := websocket.DefaultDialer.Dial(shutdownWebsocketURL(server.URL, "after-shutdown", ScopePublic, ""), nil)
	if err == nil {
		_ = afterShutdown.Close()
		t.Fatal("connection opened after Hub shutdown")
	}
	if response != nil {
		_ = response.Body.Close()
	}
}

func TestHubShutdownLeavesNoConnectionAliveWhenDialRacesDrain(t *testing.T) {
	for iteration := range 20 {
		baseline := wsConnectionMetricSnapshot(t)
		hub := newShutdownTestHub(t)
		handlerDone := make(chan string, 1)
		server := newShutdownTestServer(hub, handlerDone)

		start := make(chan struct{})
		var client *websocket.Conn
		var dialErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			client, _, dialErr = websocket.DefaultDialer.Dial(shutdownWebsocketURL(server.URL, fmt.Sprintf("race-%d", iteration), ScopePublic, ""), nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := hub.Shutdown(ctx); err != nil {
				t.Errorf("iteration %d Shutdown() error = %v", iteration, err)
			}
		}()
		close(start)
		wg.Wait()

		if dialErr == nil {
			_ = client.SetReadDeadline(time.Now().Add(time.Second))
			if _, _, err := client.ReadMessage(); err == nil {
				t.Errorf("iteration %d raced connection survived shutdown", iteration)
			}
			_ = client.Close()
			select {
			case <-handlerDone:
			case <-time.After(time.Second):
				t.Fatalf("iteration %d ServeRoom handler remained active", iteration)
			}
		}
		if got := hub.RoomPresence(fmt.Sprintf("race-%d", iteration)).GetTotalConnections(); got != 0 {
			t.Errorf("iteration %d presence = %d, want 0", iteration, got)
		}
		assertWSConnectionMetricRestored(t, baseline)
		server.Close()
	}
}

func newShutdownTestHub(t *testing.T) *Hub {
	t.Helper()
	cfg := DefaultConfig()
	cfg.TicketSecret = "shutdown-test-secret"
	hub := NewHub(nil, cfg)
	hub.BindRoomAccessValidator(allowRoomAccess{})
	return hub
}

func newShutdownTestServer(hub *Hub, done chan<- string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		roomID := r.URL.Query().Get("room")
		defer func() { done <- roomID }()
		hub.ServeRoom(w, r, roomID)
	}))
}

func dialShutdownTestConnection(t *testing.T, serverURL, roomID, scope, ticket string) *websocket.Conn {
	t.Helper()
	client, response, err := websocket.DefaultDialer.Dial(shutdownWebsocketURL(serverURL, roomID, scope, ticket), nil)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial %s websocket: %v", scope, err)
	}
	return client
}

func shutdownWebsocketURL(serverURL, roomID, scope, ticket string) string {
	values := url.Values{"room": {roomID}, "scope": {scope}}
	if ticket != "" {
		values.Set("ticket", ticket)
	}
	return "ws" + strings.TrimPrefix(serverURL, "http") + "?" + values.Encode()
}

func issueShutdownAdminTicket(t *testing.T, hub *Hub, roomID string) string {
	t.Helper()
	ticket, _, err := hub.tickets.issue(wsTicketClaims{
		RoomID:          roomID,
		Scope:           ScopeAdmin,
		UserID:          "shutdown-admin-user",
		MainAccountID:   "shutdown-main-account",
		RoleCodes:       []string{userbiz.RoleOperator},
		PermissionCodes: []string{userbiz.PermissionRealtimeView},
	})
	if err != nil {
		t.Fatalf("issue admin websocket ticket: %v", err)
	}
	return ticket
}

type shutdownDialResult struct {
	client   *websocket.Conn
	response *http.Response
	err      error
}

type gatedSnapshotProvider struct {
	entered chan struct{}
	release chan struct{}
}

func (p *gatedSnapshotProvider) Snapshot(context.Context, string) (*v1.RoomSnapshot, error) {
	close(p.entered)
	<-p.release
	return nil, nil
}

func startHubShutdown(hub *Hub) <-chan error {
	result := make(chan error, 1)
	go func() { result <- hub.Shutdown(context.Background()) }()
	return result
}

func assertSharedShutdownStillWaiting(t *testing.T, hub *Hub) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := hub.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent Shutdown() error = %v, want deadline while admitted handler is gated", err)
	}
}

func singleRoomConnection(t *testing.T, hub *Hub, roomID string) *connection {
	t.Helper()
	var found *connection
	for i := range hub.connectionShards {
		shard := &hub.connectionShards[i]
		shard.mu.RLock()
		for conn := range shard.rooms[roomID] {
			if found != nil {
				shard.mu.RUnlock()
				t.Fatalf("room %q has more than one connection", roomID)
			}
			found = conn
		}
		shard.mu.RUnlock()
	}
	if found == nil {
		t.Fatalf("room %q has no registered connection", roomID)
	}
	return found
}
