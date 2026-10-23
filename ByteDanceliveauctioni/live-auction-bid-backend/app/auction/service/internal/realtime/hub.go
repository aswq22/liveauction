package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	auctionbiz "live-auction-bid/backend/app/auction/service/internal/biz/auction"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"live-auction-bid/backend/app/auction/service/internal/observability"
	"live-auction-bid/backend/app/auction/service/internal/pkg/auth"
	"live-auction-bid/backend/app/auction/service/internal/pkg/clock"
	"live-auction-bid/backend/app/auction/service/internal/pkg/idgen"
	"live-auction-bid/backend/app/auction/service/internal/pkg/requestctx"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	v1 "live-auction-bid/backend/api/auction/service/v1"
)

const (
	writeTimeout         = 3 * time.Second
	pingInterval         = 20 * time.Second
	pongTimeout          = 60 * time.Second
	connectionSendBuffer = 32
	roomCoalesceDelay    = 75 * time.Millisecond
	connectionShardCount = 64
)

var eventJSONMarshal = protojson.MarshalOptions{
	UseEnumNumbers: false,
	UseProtoNames:  false,
}

type SnapshotProvider interface {
	Snapshot(ctx context.Context, roomID string) (*v1.RoomSnapshot, error)
}

type RoomAccessValidator interface {
	ValidateRoomInMainAccount(ctx context.Context, roomID, mainAccountID string) error
}

type Hub struct {
	connectionShards [connectionShardCount]connectionShard
	nextShard        atomic.Uint64
	lifecycleMu      sync.Mutex
	draining         bool
	serveHandlers    sync.WaitGroup
	shutdownOnce     sync.Once
	shutdownDone     chan struct{}
	// lifecycleHooks are immutable deterministic test gates installed before use.
	lifecycleHooks *hubLifecycleHooks
	coalesceMu     sync.Mutex
	coalescedRooms map[string]*coalescedRoomEvents
	snapshot       SnapshotProvider
	roomAccess     RoomAccessValidator
	publicRooms    PublicRoomValidator
	auth           *auth.Manager
	config         Config
	allowedOrigins map[string]struct{}
	tickets        wsTicketCodec
	upgrader       websocket.Upgrader
	barrage        *BarrageService
}

type connectionShard struct {
	mu    sync.RWMutex
	rooms map[string]map[*connection]struct{}
}

type coalescedRoomEvents struct {
	timer  *time.Timer
	order  []v1.AuctionEventType
	events map[v1.AuctionEventType]v1.AuctionEvent
}

type outboundMessage struct {
	event                  v1.AuctionEvent
	anonymousPublicPayload []byte
	rawPublicPayload       []byte
	metricType             string
}

type connection struct {
	hub         *Hub
	roomID      string
	scope       string
	mu          sync.RWMutex
	ctx         context.Context
	authCtx     auth.AuthContext
	client      requestctx.RequestContext
	conn        *websocket.Conn
	send        chan outboundMessage
	barrageSend chan outboundMessage
	done        chan struct{}
	closeOnce   sync.Once
	shardOnce   sync.Once
	shardIndex  uint8
	writeDone   chan struct{}
}

type hubLifecycleHooks struct {
	afterUpgradeBeforeRegister func()
	afterDrainScan             func()
	beforeWritePumpExit        func()
}

type clientMessage struct {
	Type          string `json:"type"`
	AccessToken   string `json:"accessToken"`
	Authorization string `json:"authorization"`
}

func NewHub(snapshot SnapshotProvider, configs ...Config) *Hub {
	cfg := DefaultConfig()
	if len(configs) > 0 {
		cfg = configs[0]
	}
	normalized, err := NormalizeConfig(cfg)
	if err != nil {
		panic(err)
	}
	allowedOrigins := make(map[string]struct{}, len(normalized.AllowedOrigins))
	for _, origin := range normalized.AllowedOrigins {
		allowedOrigins[origin] = struct{}{}
	}
	h := &Hub{
		coalescedRooms: make(map[string]*coalescedRoomEvents),
		shutdownDone:   make(chan struct{}),
		snapshot:       snapshot,
		config:         normalized,
		allowedOrigins: allowedOrigins,
		tickets:        newWSTicketCodec(normalized),
	}
	h.upgrader = websocket.Upgrader{CheckOrigin: h.checkOrigin}
	return h
}

func (h *Hub) BindSnapshotProvider(snapshot SnapshotProvider) {
	h.snapshot = snapshot
}

func (h *Hub) BindAuthManager(authManager *auth.Manager) {
	h.auth = authManager
}

func (h *Hub) BindRoomAccessValidator(validator RoomAccessValidator) {
	h.roomAccess = validator
}

func (h *Hub) BindPublicRoomValidator(validator PublicRoomValidator) {
	h.publicRooms = validator
}

// BindBarrageService attaches the deliberately lossy public barrage pipeline.
// Auction events and barrage traffic keep separate admission/queueing paths so
// a comment storm cannot consume the capacity reserved for bidding state.
func (h *Hub) BindBarrageService(service *BarrageService) {
	h.barrage = service
}

func (h *Hub) Publish(ctx context.Context, event v1.AuctionEvent) error {
	if isPrivateRejectEvent(event.GetType()) {
		observability.RecordWSEventDropped(event.GetType().String())
		return nil
	}
	if isImmediateEvent(event.GetType()) {
		h.flushCoalescedRoom(event.GetRoomId())
		return h.deliver(ctx, event)
	}
	if isCoalescibleEvent(event.GetType()) && event.GetRoomId() != "" {
		h.enqueueCoalesced(event)
		return nil
	}
	return h.deliver(ctx, event)
}

func (h *Hub) deliver(_ context.Context, event v1.AuctionEvent) error {
	message, err := h.prepareBroadcastMessage(event)
	if err != nil {
		return err
	}
	for i := range h.connectionShards {
		shard := &h.connectionShards[i]
		var failed []authoritativeEnqueueFailure
		shard.mu.RLock()
		for conn := range shard.rooms[event.RoomId] {
			result := tryEnqueueAuthoritative(conn, message)
			if result != enqueueSucceeded {
				failed = append(failed, authoritativeEnqueueFailure{conn: conn, result: result})
			}
		}
		shard.mu.RUnlock()

		for _, failure := range failed {
			observability.RecordWSEventDropped(event.GetType().String())
			if failure.result == enqueueQueueFull {
				h.leave(failure.conn)
				failure.conn.close()
			}
		}
	}
	return nil
}

func (h *Hub) deliverPublicBatch(roomID string, payload []byte) {
	message := outboundMessage{rawPublicPayload: payload, metricType: "BARRAGE_BATCH"}
	for i := range h.connectionShards {
		shard := &h.connectionShards[i]
		dropped := 0
		viewerQueueDropped := 0
		shard.mu.RLock()
		for conn := range shard.rooms[roomID] {
			// Barrages are public and lossy. Admin sockets receive authoritative
			// auction events only and are not allowed to be evicted by chat traffic.
			if conn.scope != ScopePublic {
				continue
			}
			result := tryEnqueueBarrage(conn, message)
			if result != enqueueSucceeded {
				dropped++
			}
			if result == enqueueQueueFull {
				viewerQueueDropped++
			}
		}
		shard.mu.RUnlock()
		for range dropped {
			observability.RecordWSEventDropped(message.metricType)
			// Drop this batch for a slow viewer. Do not disconnect it: the next
			// batch is self-contained and more useful than old chat history.
		}
		for range viewerQueueDropped {
			observability.RecordBarrageViewerDrop()
		}
	}
}

type authoritativeEnqueueResult uint8

const (
	enqueueSucceeded authoritativeEnqueueResult = iota
	enqueueConnectionClosed
	enqueueQueueFull
)

type authoritativeEnqueueFailure struct {
	conn   *connection
	result authoritativeEnqueueResult
}

func enqueueAuthoritative(conn *connection, message outboundMessage) bool {
	result := tryEnqueueAuthoritative(conn, message)
	if result == enqueueQueueFull {
		conn.hub.leave(conn)
		conn.close()
	}
	return result == enqueueSucceeded
}

func tryEnqueueAuthoritative(conn *connection, message outboundMessage) authoritativeEnqueueResult {
	select {
	case conn.send <- message:
		return enqueueSucceeded
	case <-conn.done:
		return enqueueConnectionClosed
	default:
		return enqueueQueueFull
	}
}

func tryEnqueueBarrage(conn *connection, message outboundMessage) authoritativeEnqueueResult {
	select {
	case conn.barrageSend <- message:
		return enqueueSucceeded
	case <-conn.done:
		return enqueueConnectionClosed
	default:
		return enqueueQueueFull
	}
}

func (h *Hub) prepareBroadcastMessage(event v1.AuctionEvent) (outboundMessage, error) {
	message := outboundMessage{event: event}
	publicEvent := auctionbiz.EventForViewer(event, auctionbiz.LotResultViewer{})
	payload, err := eventJSONMarshal.Marshal(&publicEvent)
	if err != nil {
		return outboundMessage{}, err
	}
	message.anonymousPublicPayload = payload
	return message, nil
}

func (h *Hub) enqueueCoalesced(event v1.AuctionEvent) {
	roomID := event.GetRoomId()
	eventType := event.GetType()
	h.coalesceMu.Lock()
	pending := h.coalescedRooms[roomID]
	if pending == nil {
		pending = &coalescedRoomEvents{events: make(map[v1.AuctionEventType]v1.AuctionEvent)}
		h.coalescedRooms[roomID] = pending
		pending.timer = time.AfterFunc(roomCoalesceDelay, func() {
			h.flushCoalescedRoom(roomID)
		})
	}
	if _, exists := pending.events[eventType]; exists {
		observability.RecordWSEventCoalesced(eventType.String())
	} else {
		pending.order = append(pending.order, eventType)
	}
	pending.events[eventType] = event
	h.coalesceMu.Unlock()
}

func (h *Hub) flushCoalescedRoom(roomID string) {
	if roomID == "" {
		return
	}
	h.coalesceMu.Lock()
	pending := h.coalescedRooms[roomID]
	if pending == nil {
		h.coalesceMu.Unlock()
		return
	}
	delete(h.coalescedRooms, roomID)
	if pending.timer != nil {
		pending.timer.Stop()
	}
	events := make([]v1.AuctionEvent, 0, len(pending.events))
	for _, eventType := range pending.order {
		if event, ok := pending.events[eventType]; ok {
			events = append(events, event)
		}
	}
	h.coalesceMu.Unlock()
	for _, event := range events {
		_ = h.deliver(context.Background(), event)
	}
}

func isPrivateRejectEvent(eventType v1.AuctionEventType) bool {
	return eventType == v1.AuctionEventType_AUCTION_EVENT_TYPE_BID_REJECTED
}

func isCoalescibleEvent(eventType v1.AuctionEventType) bool {
	switch eventType {
	case v1.AuctionEventType_AUCTION_EVENT_TYPE_BID_ACCEPTED,
		v1.AuctionEventType_AUCTION_EVENT_TYPE_RANKING_UPDATED,
		v1.AuctionEventType_AUCTION_EVENT_TYPE_LOT_UPDATED:
		return true
	default:
		return false
	}
}

func isImmediateEvent(eventType v1.AuctionEventType) bool {
	switch eventType {
	case v1.AuctionEventType_AUCTION_EVENT_TYPE_LOT_SETTLED,
		v1.AuctionEventType_AUCTION_EVENT_TYPE_LOT_CANCELLED,
		v1.AuctionEventType_AUCTION_EVENT_TYPE_AUCTION_CLOSED,
		v1.AuctionEventType_AUCTION_EVENT_TYPE_ORDER_CREATED:
		return true
	default:
		return false
	}
}

func (h *Hub) ServeRoom(w http.ResponseWriter, r *http.Request, roomID string) {
	if !h.beginServeRoom() {
		http.Error(w, "websocket hub is shutting down", http.StatusServiceUnavailable)
		return
	}
	defer h.serveHandlers.Done()
	ctx, authCtx, scope, ok := h.authenticateUpgrade(w, r, roomID)
	if !ok {
		return
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if h.lifecycleHooks != nil && h.lifecycleHooks.afterUpgradeBeforeRegister != nil {
		h.lifecycleHooks.afterUpgradeBeforeRegister()
	}

	client := &connection{
		hub:         h,
		roomID:      roomID,
		scope:       scope,
		ctx:         ctx,
		authCtx:     authCtx,
		client:      requestctx.Snapshot(ctx),
		conn:        conn,
		send:        make(chan outboundMessage, connectionSendBuffer),
		barrageSend: make(chan outboundMessage, 2),
		done:        make(chan struct{}),
		writeDone:   make(chan struct{}),
	}
	if !h.registerConnection(client) {
		client.close()
		return
	}
	slog.DebugContext(ctx, "websocket room connection opened", "room_id", roomID, "scope", scope)
	defer func() {
		client.close()
		<-client.writeDone
		observability.DecWSConnection(scope)
		slog.DebugContext(ctx, "websocket room connection closed", "room_id", roomID, "scope", scope)
		h.leave(client)
	}()

	h.enqueueSnapshot(client.ctx, client)
	go client.writePump()
	client.readPump()
}

func (h *Hub) checkOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return h.config.AllowMissingOrigin
	}
	normalized, ok := normalizeOrigin(origin)
	if !ok {
		return false
	}
	if _, ok := h.allowedOrigins[normalized]; ok {
		return true
	}
	if !isProdEnv(h.config.Environment) && isLocalhostOrigin(normalized) {
		return true
	}
	return false
}

func (h *Hub) authenticateUpgrade(w http.ResponseWriter, r *http.Request, roomID string) (context.Context, auth.AuthContext, string, bool) {
	scope, ok := normalizeScope(r.URL.Query().Get("scope"))
	if !ok {
		http.Error(w, "invalid websocket scope", http.StatusBadRequest)
		return r.Context(), auth.AuthContext{}, "", false
	}
	ticket := strings.TrimSpace(r.URL.Query().Get("ticket"))
	if scope == ScopePublic {
		if ticket == "" {
			return r.Context(), auth.AuthContext{TokenStatus: auth.TokenStatusNone}, scope, true
		}
		ctx, authCtx, err := h.authContextFromTicket(r.Context(), ticket, roomID, scope)
		if err != nil {
			http.Error(w, "invalid websocket ticket", http.StatusUnauthorized)
			return r.Context(), auth.AuthContext{}, "", false
		}
		return ctx, authCtx, scope, true
	}
	if ticket == "" {
		http.Error(w, "websocket ticket is required", http.StatusUnauthorized)
		return r.Context(), auth.AuthContext{}, "", false
	}
	ctx, authCtx, err := h.authContextFromTicket(r.Context(), ticket, roomID, scope)
	if err != nil {
		http.Error(w, "invalid websocket ticket", http.StatusUnauthorized)
		return r.Context(), auth.AuthContext{}, "", false
	}
	if !canOpenAdminScope(authCtx.Claims) {
		http.Error(w, "websocket admin scope is forbidden", http.StatusForbidden)
		return r.Context(), auth.AuthContext{}, "", false
	}
	mainAccountID := auth.EffectiveMainAccountID(authCtx.Claims)
	if mainAccountID == "" {
		http.Error(w, "main account id is required", http.StatusForbidden)
		return r.Context(), auth.AuthContext{}, "", false
	}
	if h.roomAccess == nil {
		http.Error(w, "room access validator is not configured", http.StatusServiceUnavailable)
		return r.Context(), auth.AuthContext{}, "", false
	}
	if err := h.roomAccess.ValidateRoomInMainAccount(ctx, roomID, mainAccountID); err != nil {
		http.Error(w, "room access denied", http.StatusForbidden)
		return r.Context(), auth.AuthContext{}, "", false
	}
	return ctx, authCtx, scope, true
}

func (h *Hub) authContextFromAuthorization(ctx context.Context, authorization string) (context.Context, auth.AuthContext) {
	if h.auth == nil {
		return ctx, auth.AuthContext{TokenStatus: auth.TokenStatusNone}
	}
	authCtx := h.auth.AuthContextFromBearer(authorization)
	if authCtx.TokenStatus == auth.TokenStatusValid {
		ctx = auth.WithClaims(ctx, authCtx.Claims)
	}
	return ctx, authCtx
}

func (h *Hub) authContextFromTicket(ctx context.Context, ticket, roomID, scope string) (context.Context, auth.AuthContext, error) {
	claims, err := h.tickets.parse(ticket, roomID, scope)
	if err != nil {
		return ctx, auth.AuthContext{}, err
	}
	authCtx := authContextFromTicketClaims(ticket, claims)
	return auth.WithClaims(ctx, authCtx.Claims), authCtx, nil
}

func websocketAuthorization(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("Authorization")); value != "" {
		return value
	}
	if value := strings.TrimSpace(r.Header.Get("authorization")); value != "" {
		return value
	}
	return ""
}

func (h *Hub) beginServeRoom() bool {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	if h.draining {
		return false
	}
	h.serveHandlers.Add(1)
	return true
}

func (h *Hub) join(conn *connection) bool {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	if h.draining {
		return false
	}
	h.joinConnection(conn)
	return true
}

func (h *Hub) registerConnection(conn *connection) bool {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	if h.draining {
		return false
	}
	h.joinConnection(conn)
	observability.IncWSConnection(conn.scope)
	return true
}

func (h *Hub) joinConnection(conn *connection) {
	shard := h.connectionShard(conn)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	if shard.rooms == nil {
		shard.rooms = make(map[string]map[*connection]struct{})
	}
	if shard.rooms[conn.roomID] == nil {
		shard.rooms[conn.roomID] = make(map[*connection]struct{})
	}
	shard.rooms[conn.roomID][conn] = struct{}{}
}

// Shutdown stops admission before draining every upgraded WebSocket. The
// transport cannot close hijacked connections, so Hub owns this lifecycle.
// Concurrent and repeated calls wait for the same drain operation.
func (h *Hub) Shutdown(ctx context.Context) error {
	h.lifecycleMu.Lock()
	h.draining = true
	h.lifecycleMu.Unlock()

	h.shutdownOnce.Do(func() {
		go h.drainConnections()
	})
	select {
	case <-h.shutdownDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hub) drainConnections() {
	connections := make(map[*connection]struct{})
	for i := range h.connectionShards {
		shard := &h.connectionShards[i]
		shard.mu.RLock()
		for _, roomConnections := range shard.rooms {
			for conn := range roomConnections {
				connections[conn] = struct{}{}
			}
		}
		shard.mu.RUnlock()
	}
	if h.lifecycleHooks != nil && h.lifecycleHooks.afterDrainScan != nil {
		h.lifecycleHooks.afterDrainScan()
	}

	// Never close a socket while holding a shard lock: both pumps may leave the
	// room as they unblock, and leave takes the shard write lock.
	for conn := range connections {
		h.leave(conn)
		conn.close()
	}
	h.serveHandlers.Wait()
	close(h.shutdownDone)
}

func (h *Hub) leave(conn *connection) {
	shard := h.connectionShard(conn)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	delete(shard.rooms[conn.roomID], conn)
	if len(shard.rooms[conn.roomID]) == 0 {
		delete(shard.rooms, conn.roomID)
	}
}

func (h *Hub) connectionShard(conn *connection) *connectionShard {
	conn.shardOnce.Do(func() {
		conn.shardIndex = uint8((h.nextShard.Add(1) - 1) % connectionShardCount)
	})
	return &h.connectionShards[conn.shardIndex]
}

func (h *Hub) RoomPresence(roomID string) *v1.RoomPresence {
	presence := &v1.RoomPresence{
		RoomId:           roomID,
		ServerTimeUnixMs: clock.NowMs(),
	}
	// Presence is an exact membership snapshot. Acquire every shard in the
	// canonical ascending order; all other membership paths take only one shard.
	for i := range h.connectionShards {
		h.connectionShards[i].mu.RLock()
	}
	for i := range h.connectionShards {
		shard := &h.connectionShards[i]
		for conn := range shard.rooms[roomID] {
			presence.TotalConnections++
			if conn.canReceivePrivateEvents() {
				presence.OperatorConnections++
				continue
			}
			presence.ViewerConnections++
		}
	}
	for i := len(h.connectionShards) - 1; i >= 0; i-- {
		h.connectionShards[i].mu.RUnlock()
	}
	return presence
}

func (h *Hub) enqueueSnapshot(ctx context.Context, conn *connection) {
	if h.snapshot == nil {
		return
	}
	snapshot, err := h.snapshot.Snapshot(ctx, conn.roomID)
	if err != nil {
		return
	}
	event := v1.AuctionEvent{
		Id:               idgen.New("evt"),
		Type:             v1.AuctionEventType_AUCTION_EVENT_TYPE_ROOM_SNAPSHOT,
		RoomId:           conn.roomID,
		OccurredAtUnixMs: clock.NowMs(),
		Snapshot:         snapshot,
	}
	if mainAccountID := snapshotMainAccountID(snapshot); mainAccountID != "" {
		event.MainAccountId = mainAccountID
	}
	select {
	case conn.send <- outboundMessage{event: event}:
	case <-conn.done:
	default:
		h.leave(conn)
		conn.close()
	}
}

func (c *connection) readPump() {
	_ = c.conn.SetReadDeadline(time.Now().Add(pongTimeout))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})
	for {
		_, payload, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		c.handleClientMessage(payload)
	}
}

func (c *connection) handleClientMessage(payload []byte) {
	var msg clientMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return
	}
	if !strings.EqualFold(msg.Type, "AUTH") {
		return
	}
	if c.scope != ScopePublic {
		return
	}
	if c.hub.auth == nil {
		return
	}
	token := strings.TrimSpace(msg.AccessToken)
	authorization := strings.TrimSpace(msg.Authorization)
	if authorization == "" && token != "" {
		authorization = "Bearer " + token
	}
	if authorization == "" {
		return
	}
	ctx, authCtx := c.hub.authContextFromAuthorization(c.context(), authorization)
	if authCtx.TokenStatus != auth.TokenStatusValid {
		_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "invalid auth"), time.Now().Add(writeTimeout))
		c.close()
		return
	}
	c.mu.Lock()
	c.authCtx = authCtx
	c.ctx = ctx
	c.client = requestctx.Snapshot(ctx)
	c.mu.Unlock()
	c.hub.enqueueSnapshot(c.context(), c)
}

func (c *connection) context() context.Context {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ctx
}

func (c *connection) canReceivePrivateEvents() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.scope != ScopeAdmin {
		return false
	}
	if c.authCtx.TokenStatus != auth.TokenStatusValid || c.authCtx.Claims == nil {
		return false
	}
	return auth.HasAnyPermission(c.authCtx.Claims, userbiz.PermissionRealtimeView, userbiz.PermissionAuctionControl, userbiz.PermissionLotViewAdmin)
}

func (c *connection) writePump() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	defer func() {
		c.hub.leave(c)
		c.close()
		if c.hub.lifecycleHooks != nil && c.hub.lifecycleHooks.beforeWritePumpExit != nil {
			c.hub.lifecycleHooks.beforeWritePumpExit()
		}
		close(c.writeDone)
	}()

	for {
		select {
		case message := <-c.send:
			if !c.writeOutbound(message) {
				return
			}
			continue
		default:
		}

		select {
		case message := <-c.send:
			if !c.writeOutbound(message) {
				return
			}
		case message := <-c.barrageSend:
			if !c.writeOutbound(message) {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *connection) writeOutbound(message outboundMessage) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	payload, eventType, err := c.payloadForDelivery(message)
	if err != nil {
		return false
	}
	if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return false
	}
	metricType := eventType.String()
	if message.metricType != "" {
		metricType = message.metricType
	}
	observability.RecordWSEventSent(metricType)
	return true
}

func (c *connection) payloadForDelivery(message outboundMessage) ([]byte, v1.AuctionEventType, error) {
	if len(message.rawPublicPayload) > 0 {
		return message.rawPublicPayload, v1.AuctionEventType_AUCTION_EVENT_TYPE_UNSPECIFIED, nil
	}
	if len(message.anonymousPublicPayload) > 0 && c.canUseAnonymousPublicPayload() {
		return message.anonymousPublicPayload, message.event.GetType(), nil
	}
	event := c.eventForDelivery(message.event)
	payload, err := eventJSONMarshal.Marshal(&event)
	if err != nil {
		return nil, event.GetType(), err
	}
	return payload, event.GetType(), nil
}

func (c *connection) canUseAnonymousPublicPayload() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.scope == ScopePublic && (c.authCtx.TokenStatus != auth.TokenStatusValid || c.authCtx.Claims == nil)
}

func (c *connection) eventForDelivery(event v1.AuctionEvent) v1.AuctionEvent {
	viewer := c.deliveryViewer()
	if event.GetType() == v1.AuctionEventType_AUCTION_EVENT_TYPE_ROOM_SNAPSHOT && event.GetSnapshot() != nil {
		mainAccountID := event.GetMainAccountId()
		if mainAccountID == "" {
			mainAccountID = snapshotMainAccountID(event.GetSnapshot())
		}
		if viewer.CanViewMainAccountPrivate(mainAccountID) {
			if event.GetMainAccountId() == "" && mainAccountID != "" {
				cloned := proto.Clone(&event).(*v1.AuctionEvent)
				cloned.MainAccountId = mainAccountID
				return *cloned
			}
			return event
		}
		cloned := proto.Clone(&event).(*v1.AuctionEvent)
		cloned.Snapshot = auctionbiz.SnapshotForViewer(event.GetSnapshot(), viewer)
		cloned.MainAccountId = ""
		return *cloned
	}
	return auctionbiz.EventForViewer(event, viewer)
}

func (c *connection) deliveryViewer() auctionbiz.LotResultViewer {
	viewer := c.lotResultViewer()
	if c.scope == ScopeAdmin {
		return viewer
	}
	public := auctionbiz.LotResultViewer{UserID: viewer.UserID}
	for _, permission := range viewer.PermissionCodes {
		if userbiz.NormalizePermissionCode(permission) == userbiz.PermissionOrderViewOwn {
			public.PermissionCodes = append(public.PermissionCodes, userbiz.PermissionOrderViewOwn)
			break
		}
	}
	return public
}

func (c *connection) lotResultViewer() auctionbiz.LotResultViewer {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.authCtx.TokenStatus != auth.TokenStatusValid || c.authCtx.Claims == nil {
		return auctionbiz.LotResultViewer{}
	}
	return auctionbiz.LotResultViewer{
		UserID:          c.authCtx.Claims.UserID,
		MainAccountID:   auth.EffectiveMainAccountID(c.authCtx.Claims),
		RoleCodes:       append([]string(nil), c.authCtx.Claims.RoleCodes...),
		PermissionCodes: append([]string(nil), c.authCtx.Claims.PermissionCodes...),
	}
}

func snapshotMainAccountID(snapshot *v1.RoomSnapshot) string {
	if snapshot == nil || snapshot.GetCurrentLot() == nil {
		return ""
	}
	return strings.TrimSpace(snapshot.GetCurrentLot().GetMainAccountId())
}

func canOpenAdminScope(claims *auth.Claims) bool {
	if claims == nil || !auth.HasPermission(claims, userbiz.PermissionRealtimeView) {
		return false
	}
	return auth.HasRoleCode(claims, userbiz.RoleMerchantOwner) || auth.HasRoleCode(claims, userbiz.RoleAnchor) || auth.HasRoleCode(claims, userbiz.RoleOperator)
}

func (c *connection) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		if c.conn != nil {
			_ = c.conn.Close()
		}
	})
}
