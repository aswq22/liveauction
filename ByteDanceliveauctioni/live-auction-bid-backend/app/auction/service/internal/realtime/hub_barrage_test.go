package realtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"live-auction-bid/backend/app/auction/service/internal/pkg/auth"
)

func TestDeliverRoutesAuctionEventToAuthoritativeQueue(t *testing.T) {
	hub, conn := hubWithBufferedTestConnection("room-1")
	if err := hub.deliver(context.Background(), v1.AuctionEvent{RoomId: "room-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-conn.send:
	default:
		t.Fatal("authoritative queue was empty")
	}
	select {
	case <-conn.barrageSend:
		t.Fatal("auction event reached barrage queue")
	default:
	}
}

func TestDeliverPublicBatchDropsOnlyWhenBarrageQueueIsFull(t *testing.T) {
	hub, conn := hubWithBufferedTestConnection("room-1")
	conn.barrageSend <- outboundMessage{metricType: "occupied"}
	conn.barrageSend <- outboundMessage{metricType: "occupied"}
	hub.deliverPublicBatch("room-1", []byte(`{"type":"BARRAGE_BATCH"}`))
	if len(conn.send) != 0 {
		t.Fatal("barrage touched authoritative queue")
	}
	select {
	case <-conn.done:
		t.Fatal("slow barrage consumer was disconnected")
	default:
	}
}

func TestDeliverPublicBatchExcludesAdminConnections(t *testing.T) {
	hub := NewHub(nil)
	public := bufferedTestConnection(hub, "room-1", ScopePublic, 1)
	admin := bufferedTestConnection(hub, "room-1", ScopeAdmin, 1)
	hub.join(public)
	hub.join(admin)

	hub.deliverPublicBatch("room-1", []byte(`{"type":"BARRAGE_BATCH"}`))

	select {
	case <-public.barrageSend:
	default:
		t.Fatal("public connection did not receive barrage batch")
	}
	select {
	case <-admin.barrageSend:
		t.Fatal("admin connection received public barrage batch")
	default:
	}

	if err := hub.deliver(context.Background(), v1.AuctionEvent{RoomId: "room-1"}); err != nil {
		t.Fatal(err)
	}
	for name, conn := range map[string]*connection{"public": public, "admin": admin} {
		select {
		case <-conn.send:
		default:
			t.Fatalf("%s connection did not receive authoritative event", name)
		}
	}
}

func TestRoomPresenceTracksJoinAndLeave(t *testing.T) {
	hub := NewHub(nil)
	public := bufferedTestConnection(hub, "room-1", ScopePublic, 1)
	admin := bufferedTestConnection(hub, "room-1", ScopeAdmin, 1)
	admin.authCtx = auth.AuthContext{
		TokenStatus: auth.TokenStatusValid,
		Claims: &auth.Claims{
			PermissionCodes: []string{userbiz.PermissionRealtimeView},
		},
	}
	hub.join(public)
	hub.join(admin)

	presence := hub.RoomPresence("room-1")
	if presence.GetTotalConnections() != 2 || presence.GetViewerConnections() != 1 || presence.GetOperatorConnections() != 1 {
		t.Fatalf("presence after joins = %+v", presence)
	}

	hub.leave(admin)
	presence = hub.RoomPresence("room-1")
	if presence.GetTotalConnections() != 1 || presence.GetViewerConnections() != 1 || presence.GetOperatorConnections() != 0 {
		t.Fatalf("presence after admin leave = %+v", presence)
	}

	hub.leave(public)
	presence = hub.RoomPresence("room-1")
	if presence.GetTotalConnections() != 0 || presence.GetViewerConnections() != 0 || presence.GetOperatorConnections() != 0 {
		t.Fatalf("presence after all leave = %+v", presence)
	}
}

func TestRoomPresenceIsExactAcrossShardMembershipChanges(t *testing.T) {
	hub := NewHub(nil)
	first := bufferedTestConnection(hub, "room-1", ScopePublic, 1)
	hub.join(first) // shard 0
	dummy := bufferedTestConnection(hub, "room-1", ScopePublic, 1)
	hub.join(dummy) // shard 1
	hub.leave(dummy)
	later := bufferedTestConnection(hub, "room-1", ScopePublic, 1) // shard 2 on first join

	blockedShard := &hub.connectionShards[1]
	blockedShard.mu.Lock()
	blockedShardLocked := true
	defer func() {
		if blockedShardLocked {
			blockedShard.mu.Unlock()
		}
	}()
	first.mu.Lock()
	firstLocked := true
	defer func() {
		if firstLocked {
			first.mu.Unlock()
		}
	}()

	presenceResult := make(chan *v1.RoomPresence, 1)
	go func() {
		presenceResult <- hub.RoomPresence("room-1")
	}()
	waitForShardReadLock(t, &hub.connectionShards[0])

	leaveStarted := make(chan struct{})
	leaveDone := make(chan struct{})
	go func() {
		close(leaveStarted)
		hub.leave(first)
		close(leaveDone)
	}()
	<-leaveStarted
	runtime.Gosched()
	first.mu.Unlock()
	firstLocked = false

	select {
	case <-leaveDone:
		// A shard-at-a-time presence read has already counted first and is now
		// blocked on shard 1. Add later only after first is authoritatively gone.
		hub.join(later)
	case <-time.After(25 * time.Millisecond):
		// An exact snapshot holds shard 0 while waiting for shard 1, so leave
		// cannot linearize until the snapshot completes.
	}
	blockedShard.mu.Unlock()
	blockedShardLocked = false

	select {
	case presence := <-presenceResult:
		if got := presence.GetTotalConnections(); got != 1 {
			t.Fatalf("presence across non-overlapping memberships = %d, want 1", got)
		}
	case <-time.After(time.Second):
		t.Fatal("RoomPresence did not complete")
	}
	select {
	case <-leaveDone:
	case <-time.After(time.Second):
		t.Fatal("leave did not complete after presence released membership locks")
	}
}

func TestLeaveBeforeFirstJoinUsesCanonicalShard(t *testing.T) {
	hub := NewHub(nil)
	dummy := bufferedTestConnection(hub, "room-1", ScopePublic, 1)
	hub.join(dummy) // consume shard 0
	hub.leave(dummy)
	conn := bufferedTestConnection(hub, "room-1", ScopePublic, 1)

	canonicalShard := &hub.connectionShards[1]
	canonicalShard.mu.Lock()
	locked := true
	defer func() {
		if locked {
			canonicalShard.mu.Unlock()
		}
	}()
	leaveStarted := make(chan struct{})
	leaveDone := make(chan struct{})
	go func() {
		close(leaveStarted)
		hub.leave(conn)
		close(leaveDone)
	}()
	<-leaveStarted

	select {
	case <-leaveDone:
		t.Fatal("leave bypassed the connection's canonical shard before first join")
	case <-time.After(25 * time.Millisecond):
	}
	canonicalShard.mu.Unlock()
	locked = false
	select {
	case <-leaveDone:
	case <-time.After(time.Second):
		t.Fatal("leave did not complete after canonical shard was released")
	}
}

func TestDeliverEvictsAuthoritativeSlowClientWithoutDeadlock(t *testing.T) {
	hub := NewHub(nil)
	conn := bufferedTestConnection(hub, "room-1", ScopePublic, 1)
	conn.conn = testWebsocketConnection(t)
	for range cap(conn.send) {
		conn.send <- outboundMessage{metricType: "occupied"}
	}
	hub.join(conn)

	delivered := make(chan error, 1)
	go func() {
		delivered <- hub.deliver(context.Background(), v1.AuctionEvent{RoomId: "room-1"})
	}()

	select {
	case err := <-delivered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("authoritative delivery deadlocked while evicting slow client")
	}
	select {
	case <-conn.done:
	default:
		t.Fatal("authoritative slow client was not closed")
	}
	if got := hub.RoomPresence("room-1").GetTotalConnections(); got != 0 {
		t.Fatalf("presence after slow-client eviction = %d, want 0", got)
	}
}

func TestConcurrentRoomMembershipAndBroadcast(t *testing.T) {
	const connectionCount = 128
	hub := NewHub(nil)
	connections := make([]*connection, 0, connectionCount)
	for i := 0; i < connectionCount; i++ {
		conn := bufferedTestConnection(hub, "room-1", ScopePublic, 2)
		connections = append(connections, conn)
		hub.join(conn)
	}

	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		defer workers.Done()
		for i := 0; i < 500; i++ {
			hub.deliverPublicBatch("room-1", []byte(`{"type":"BARRAGE_BATCH"}`))
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 500; i++ {
			_ = hub.RoomPresence("room-1")
		}
	}()
	go func() {
		defer workers.Done()
		for round := 0; round < 20; round++ {
			for _, conn := range connections {
				hub.leave(conn)
				hub.join(conn)
			}
		}
	}()
	workers.Wait()

	if got := hub.RoomPresence("room-1").GetTotalConnections(); got != connectionCount {
		t.Fatalf("presence after concurrent membership changes = %d, want %d", got, connectionCount)
	}
}

func TestConcurrentDuplicateJoinKeepsSingleMembership(t *testing.T) {
	hub := NewHub(nil)
	conn := bufferedTestConnection(hub, "room-1", ScopePublic, 1)

	var joins sync.WaitGroup
	for range 128 {
		joins.Add(1)
		go func() {
			defer joins.Done()
			hub.join(conn)
		}()
	}
	joins.Wait()

	if got := hub.RoomPresence("room-1").GetTotalConnections(); got != 1 {
		t.Fatalf("presence after duplicate concurrent joins = %d, want 1", got)
	}
}

func TestDeliverPublicBatchDoesNotAllocateRoomSnapshot(t *testing.T) {
	const connectionCount = 4096
	hub := NewHub(nil)
	for i := 0; i < connectionCount; i++ {
		hub.join(bufferedTestConnection(hub, "room-1", ScopePublic, 16))
	}
	payload := []byte(`{"type":"BARRAGE_BATCH"}`)

	if allocs := testing.AllocsPerRun(10, func() {
		hub.deliverPublicBatch("room-1", payload)
	}); allocs != 0 {
		t.Fatalf("deliverPublicBatch allocations = %.0f, want 0 without a room snapshot", allocs)
	}
}

func BenchmarkBarrageBroadcast100K(b *testing.B) {
	const (
		connectionCount = 100_000
		drainerCount    = 64
	)
	hub := NewHub(nil)
	groups := make([][]*connection, drainerCount)
	for i := 0; i < connectionCount; i++ {
		conn := bufferedTestConnection(hub, "room-1", ScopePublic, 1)
		groups[i%drainerCount] = append(groups[i%drainerCount], conn)
		hub.join(conn)
	}

	type drainer struct {
		trigger chan struct{}
		done    chan struct{}
	}
	drainers := make([]drainer, 0, drainerCount)
	for _, group := range groups {
		group := group
		worker := drainer{trigger: make(chan struct{}), done: make(chan struct{})}
		drainers = append(drainers, worker)
		go func(group []*connection, worker drainer) {
			for range worker.trigger {
				for _, conn := range group {
					<-conn.barrageSend
				}
				worker.done <- struct{}{}
			}
		}(group, worker)
	}
	b.Cleanup(func() {
		for _, worker := range drainers {
			close(worker.trigger)
		}
	})
	payload := []byte(`{"type":"BARRAGE_BATCH"}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hub.deliverPublicBatch("room-1", payload)
		b.StopTimer()
		for _, worker := range drainers {
			worker.trigger <- struct{}{}
		}
		for _, worker := range drainers {
			<-worker.done
		}
		b.StartTimer()
	}
}

func hubWithBufferedTestConnection(roomID string) (*Hub, *connection) {
	hub := NewHub(nil)
	conn := bufferedTestConnection(hub, roomID, ScopePublic, 2)
	hub.join(conn)
	return hub, conn
}

func bufferedTestConnection(hub *Hub, roomID, scope string, barrageCapacity int) *connection {
	return &connection{
		hub:         hub,
		roomID:      roomID,
		scope:       scope,
		send:        make(chan outboundMessage, connectionSendBuffer),
		barrageSend: make(chan outboundMessage, barrageCapacity),
		done:        make(chan struct{}),
	}
}

func testWebsocketConnection(t *testing.T) *websocket.Conn {
	t.Helper()
	upgrader := websocket.Upgrader{}
	serverConn := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConn <- conn
	}))
	t.Cleanup(server.Close)

	client, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial test websocket: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	select {
	case conn := <-serverConn:
		return conn
	case <-time.After(time.Second):
		t.Fatal("test websocket server did not accept connection")
		return nil
	}
}

func waitForShardReadLock(t *testing.T, shard *connectionShard) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !shard.mu.TryLock() {
			return
		}
		shard.mu.Unlock()
		runtime.Gosched()
	}
	t.Fatal("RoomPresence did not acquire the expected shard read lock")
}
