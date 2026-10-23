package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"live-auction-bid/backend/app/auction/service/internal/agentapproval"
	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
	"live-auction-bid/backend/app/auction/service/internal/agentconversation"
	"live-auction-bid/backend/app/auction/service/internal/agenttool"
	"live-auction-bid/backend/app/auction/service/internal/aiassistant"
	"live-auction-bid/backend/app/auction/service/internal/biz/auction"
	shopbiz "live-auction-bid/backend/app/auction/service/internal/biz/shop"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"live-auction-bid/backend/app/auction/service/internal/cluster"
	"live-auction-bid/backend/app/auction/service/internal/data"
	"live-auction-bid/backend/app/auction/service/internal/observability"
	"live-auction-bid/backend/app/auction/service/internal/pkg/auth"
	"live-auction-bid/backend/app/auction/service/internal/realtime"
	"live-auction-bid/backend/app/auction/service/internal/searchindex"
	"live-auction-bid/backend/app/auction/service/internal/server"
	appsvc "live-auction-bid/backend/app/auction/service/internal/service"
	"live-auction-bid/backend/app/auction/service/internal/storage"

	"golang.org/x/sync/errgroup"
)

func main() {
	os.Exit(applicationExitCode(run))
}

func applicationExitCode(run func() error) int {
	if err := run(); err != nil {
		log.Printf("auction server stopped with error: %v", err)
		return 1
	}
	return 0
}

func run() error {
	addr := os.Getenv("AUCTION_HTTP_ADDR")
	if addr == "" {
		addr = ":18080"
	}
	mysqlDSN := getenv("AUCTION_MYSQL_DSN", "auction:auction_dev@tcp(127.0.0.1:13306)/live_auction?parseTime=true&charset=utf8mb4&loc=Local")
	redisAddr := getenv("AUCTION_REDIS_ADDR", "127.0.0.1:16379")
	redisPassword := getenvAllowEmpty("AUCTION_REDIS_PASSWORD", "auction_redis")
	if redisPassword == "__NO_PASSWORD__" {
		redisPassword = ""
	}

	store, err := data.NewStore(context.Background(), data.Config{
		MySQLDSN:                mysqlDSN,
		RedisAddr:               redisAddr,
		RedisPassword:           redisPassword,
		RuntimeProjectionShards: getenvInt("AUCTION_RUNTIME_PROJECTION_SHARDS", 16),
		RuntimeProjectionBackpressurePendingLimit: int64(getenvInt("AUCTION_RUNTIME_PROJECTION_BACKPRESSURE_PENDING_LIMIT", 0)),
		RuntimeProjectionBackpressureLag:          getenvDuration("AUCTION_RUNTIME_PROJECTION_BACKPRESSURE_LAG", 0),
		DBMaxOpenConns:                            getenvInt("AUCTION_DB_MAX_OPEN_CONNS", 20),
		DBMaxIdleConns:                            getenvInt("AUCTION_DB_MAX_IDLE_CONNS", 10),
		DBConnMaxLifetime:                         getenvDuration("AUCTION_DB_CONN_MAX_LIFETIME", 30*time.Minute),
		DBConnMaxIdleTime:                         getenvDuration("AUCTION_DB_CONN_MAX_IDLE_TIME", 2*time.Minute),
		RedisPoolSize:                             getenvInt("AUCTION_REDIS_POOL_SIZE", 0),
		RedisMinIdleConns:                         getenvInt("AUCTION_REDIS_MIN_IDLE_CONNS", 0),
		SeedRBACDefaults:                          getenvBool("AUCTION_SEED_RBAC_DEFAULTS", true),
		SeedShopDefaults:                          getenvBool("AUCTION_SEED_SHOP_DEFAULTS", true),
	})
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	buyerRetrieval,closeBuyerRetrieval,err:=newProductionBuyerSearch(ctx,store)
	if err!=nil{return err}
	defer closeBuyerRetrieval()
	buyerSearch, buyerEmbedder, buyerSearchWorker := newBuyerSearchFromEnv(ctx, store)
	if buyerSearch != nil {
		defer buyerSearch.Close()
	}
	if buyerSearchWorker != nil {
		buyerSearchWorker.Start(ctx)
	}
	instanceID := getenv("AUCTION_INSTANCE_ID", defaultInstanceID())
	leaseProvider := data.NewRedisLeaseProvider(store)
	leaseTTL := getenvDuration("AUCTION_WORKER_LEASE_TTL", 15*time.Second)
	leaseRenewInterval := getenvDuration("AUCTION_WORKER_LEASE_RENEW_INTERVAL", 5*time.Second)
	outboxWorker := data.NewEventOutboxWorker(store, 10*time.Second, 100)
	outboxWorker.BindLease(leaseProvider, "auction:lease:event-outbox-worker", instanceID, leaseTTL, leaseRenewInterval)
	agentApprovalOutboxWorker := data.NewAgentApprovalOutboxWorker(store, 2*time.Second, 100).WithObserver(observability.AgentApprovalOutboxObserver{})

	authManager, err := auth.NewManager(auth.Config{
		Secret:     os.Getenv("AUCTION_JWT_SECRET"),
		Issuer:     getenv("AUCTION_JWT_ISSUER", "auction-backend"),
		AccessTTL:  getenvDuration("AUCTION_ACCESS_TOKEN_TTL", auth.DefaultAccessTTL),
		RefreshTTL: getenvDuration("AUCTION_REFRESH_TOKEN_TTL", auth.DefaultRefreshTTL),
	})
	if err != nil {
		return err
	}
	imageStorage, err := newImageStorageFromEnv()
	if err != nil {
		return err
	}
	assetCleanupWorker := data.NewAssetCleanupWorker(store, imageStorage, getenvDuration("AUCTION_TEMP_ASSET_CLEANUP_INTERVAL", time.Hour), 100)
	assetCleanupWorker.Start(ctx)

	userUsecase := userbiz.NewUsecase(store, authManager)
	if err := bootstrapMainAccount(ctx, userUsecase); err != nil {
		return err
	}

	realtimeConfig, err := realtime.ConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	hub := realtime.NewHub(nil, realtimeConfig)
	hub.BindAuthManager(authManager)
	barrageConfig := realtime.DefaultBarrageConfig()
	barrageConfig.QueueCapacity = getenvInt("AUCTION_BARRAGE_QUEUE_CAPACITY", barrageConfig.QueueCapacity)
	barrageConfig.Window = getenvDuration("AUCTION_BARRAGE_WINDOW", barrageConfig.Window)
	barrageConfig.MaxActiveRoomsPerWindow = getenvInt("AUCTION_BARRAGE_MAX_ACTIVE_ROOMS_PER_WINDOW", barrageConfig.MaxActiveRoomsPerWindow)
	barrageConfig.MaxPerRoomWindow = getenvInt("AUCTION_BARRAGE_MAX_PER_ROOM_WINDOW", barrageConfig.MaxPerRoomWindow)
	barrageConfig.AdminReserved = getenvInt("AUCTION_BARRAGE_ADMIN_RESERVED", barrageConfig.AdminReserved)
	barrageConfig.PaidReserved = getenvInt("AUCTION_BARRAGE_PAID_RESERVED", barrageConfig.PaidReserved)
	barrageConfig.MaxUniquePerRoomWindow = getenvInt("AUCTION_BARRAGE_MAX_UNIQUE_PER_ROOM_WINDOW", barrageConfig.MaxUniquePerRoomWindow)
	barrageConfig.MaxContentRunes = getenvInt("AUCTION_BARRAGE_MAX_CONTENT_RUNES", barrageConfig.MaxContentRunes)
	barrageConfig.GlobalUserBurst = getenvFloat("AUCTION_BARRAGE_GLOBAL_USER_BURST", barrageConfig.GlobalUserBurst)
	barrageConfig.GlobalUserRatePerSecond = getenvFloat("AUCTION_BARRAGE_GLOBAL_USER_RATE", barrageConfig.GlobalUserRatePerSecond)
	barrageConfig.GlobalRateEntryCapacity = getenvInt("AUCTION_BARRAGE_GLOBAL_RATE_ENTRY_CAPACITY", barrageConfig.GlobalRateEntryCapacity)
	barrageConfig.RoomUserBurst = getenvFloat("AUCTION_BARRAGE_ROOM_USER_BURST", barrageConfig.RoomUserBurst)
	barrageConfig.RoomUserRatePerSecond = getenvFloat("AUCTION_BARRAGE_ROOM_USER_RATE", barrageConfig.RoomUserRatePerSecond)
	barrageConfig.RoomRateEntryCapacity = getenvInt("AUCTION_BARRAGE_ROOM_RATE_ENTRY_CAPACITY", barrageConfig.RoomRateEntryCapacity)
	barrageConfig.SensitiveWords = strings.Split(os.Getenv("AUCTION_BARRAGE_SENSITIVE_WORDS"), ",")
	barrageService, err := realtime.NewBarrageService(barrageConfig, hub, data.NewBarrageModeration(store))
	if err != nil {
		return err
	}
	hub.BindBarrageService(barrageService)
	hub.BindPublicRoomValidator(data.NewPublicRoomValidator(store))
	go barrageService.Run(ctx)
	eventPublisher, closeRealtimeBus := newRealtimePublisherFromEnv(ctx, hub, instanceID, redisAddr, redisPassword)
	defer closeRealtimeBus()
	store.SetEventStreamer(eventPublisher)
	outboxWorker.Start(ctx)
	agentApprovalOutboxWorker.Start(ctx)
	defer agentApprovalOutboxWorker.Stop()
	paymentProvider, err := auction.NewPaymentProviderFromName(os.Getenv("AUCTION_PAYMENT_PROVIDER"))
	if err != nil {
		return err
	}
	auctionUsecase := auction.NewAuctionUsecase(store, store, store, eventPublisher).
		SetPaymentProvider(paymentProvider).
		SetSyncRuntimeProjection(getenvBool("AUCTION_BID_SYNC_PROJECTION", false)).
		SetBidDBGuardMode(getenv("AUCTION_BID_DB_GUARD_MODE", "runtime-first"))
	agentRunRepository := data.NewAgentRunRepository(store)
	agentGatewayHandler, runtimeClient, agentGatewayService, err := newAgentGatewayFromEnv(agentRunRepository, authManager)
	if err != nil {
		return err
	}
	if runtimeClient != nil {
		defer runtimeClient.Close()
		log.Printf("auction agent Runtime client configured")
	} else {
		log.Printf("auction agent Runtime client unavailable; Agent HTTP routes remain enabled")
	}
	if err := agentGatewayService.Start(ctx); err != nil {
		return err
	}
	defer agentGatewayService.Close()
	agentConversationService, err := agentconversation.NewService(data.NewAgentConversationRepository(store), store, agentGatewayService)
	if err != nil {
		return err
	}
	agentApprovalService := agentapproval.NewService(data.NewAgentConversationRepository(store), store).WithObserver(observability.AgentApprovalObserver{})
	agentGatewayHandler = server.NewAgentAdminHTTPHandler(agentGatewayHandler, server.NewAgentSessionHTTPHandler(agentConversationService, authManager, server.AgentHTTPOptions{}, agentApprovalService))
	var agentToolServer lifecycleServer
	if config, enabled := agentToolConfigFromEnv(); enabled {
		agentToolService, err := agenttool.NewService(config.Token, agentRunRepository, auctionUsecase)
		if err != nil {
			return err
		}
		agentToolServer = server.NewAgentToolGRPCServer(config.Addr, agentToolService)
		log.Printf("auction agent Tool gRPC enabled on %s", config.Addr)
	} else {
		log.Printf("auction agent Tool gRPC disabled: AUCTION_INTERNAL_SERVICE_TOKEN is not configured")
	}
	var runtimeProjectionWorker *data.RuntimeProjectionWorker
	if getenvBool("AUCTION_RUNTIME_PROJECTION_WORKER_ENABLED", true) {
		runtimeProjectionWorker = data.NewRuntimeProjectionWorker(
			store,
			eventPublisher,
			getenvDuration("AUCTION_RUNTIME_PROJECTION_INTERVAL", 2*time.Second),
			getenvInt("AUCTION_RUNTIME_PROJECTION_BATCH_LIMIT", 100),
		)
		runtimeProjectionWorker.BindLease(leaseProvider, instanceID, leaseTTL, leaseRenewInterval)
		runtimeProjectionWorker.Start(ctx)
	}
	hub.BindRoomAccessValidator(auctionUsecase)
	auctionCloseWorker := auction.NewAuctionCloseWorker(auctionUsecase, getenvDuration("AUCTION_CLOSE_WORKER_INTERVAL", 2*time.Second), 100)
	auctionCloseWorker.BindLease(leaseProvider, "auction:lease:auction-close-worker", instanceID, leaseTTL, leaseRenewInterval)
	auctionCloseWorker.Start(ctx)
	clusterStatus := newClusterStatusFromEnv()
	hub.BindSnapshotProvider(auctionUsecase)
	auctionService := appsvc.NewAuctionService(auctionUsecase, hub).
		SetCurrentUserProvider(userUsecase).
		SetAIAssistant(aiassistant.NewFromEnv(os.Getenv)).
		SetBuyerSearch(buyerSearch, buyerEmbedder).
		SetBuyerRetrieval(buyerRetrieval).
		SetVerboseBidLog(getenvBool("AUCTION_VERBOSE_BID_LOG", false))
	userService := appsvc.NewUserService(userUsecase)
	shopUsecase := shopbiz.NewUsecase(store)
	shopService := appsvc.NewShopService(shopUsecase)
	orderService := appsvc.NewOrderService(shopUsecase, auctionUsecase)
	consulRegistration, err := server.RegisterConsulService(context.Background(), server.ConsulConfig{
		Addr:           getenv("AUCTION_CONSUL_ADDR", "127.0.0.1:18500"),
		ServiceName:    getenv("AUCTION_SERVICE_NAME", "auction-backend"),
		ServiceAddress: getenv("AUCTION_SERVICE_ADDR", "127.0.0.1"),
		HTTPAddr:       addr,
	})
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := consulRegistration.Deregister(ctx); err != nil {
			log.Printf("consul deregister failed: %v", err)
		}
	}()

	readiness := server.Readiness{
		Store:          store,
		Outbox:         outboxWorker,
		AuctionClose:   auctionCloseWorker,
		WorkerStatuses: workerStatusProviders(outboxWorker, agentApprovalOutboxWorker, auctionCloseWorker, runtimeProjectionWorker),
		Cluster:        clusterStatus,
		Consul:         consulRegistration,
	}
	if runtimeProjectionWorker != nil {
		readiness.RuntimeProjection = runtimeProjectionWorker
		readiness.ProjectionMetrics = runtimeProjectionWorker
	}
	httpServer := server.NewHTTPServer(addr, auctionService, userService, shopService, orderService, hub, readiness, authManager, authManager.Middleware(), imageStorage, store, agentGatewayHandler)

	servers := []lifecycleServer{withPreStop(httpServer, hub.Shutdown)}
	if agentToolServer != nil {
		servers = append(servers, agentToolServer)
	}
	log.Printf("auction backend listening on %s via kratos http", addr)
	err = runServers(ctx, servers...)
	if err != nil {
		stopSignals()
	}
	return err
}

type agentToolConfig struct {
	Addr  string
	Token string
}

type agentRuntimeConfig struct {
	Address string
	Token   string
}

func newAgentGatewayFromEnv(repository agentcontrol.GatewayRepository, authManager *auth.Manager) (http.Handler, agentcontrol.RuntimeClient, *agentcontrol.Service, error) {
	var runtimeClient agentcontrol.RuntimeClient
	if config, enabled := agentRuntimeConfigFromEnv(); enabled {
		client, err := agentcontrol.NewGRPCRuntimeClient(agentcontrol.RuntimeClientConfig{
			Address:       config.Address,
			Token:         config.Token,
			UnaryTimeout:  getenvDuration("AUCTION_AGENT_RUNTIME_TIMEOUT", 5*time.Second),
			StreamTimeout: getenvDuration("AUCTION_AGENT_RUNTIME_STREAM_TIMEOUT", 30*time.Minute),
		})
		if err == nil {
			runtimeClient = client
		}
	}
	service, err := agentcontrol.NewService(repository, runtimeClient)
	if err != nil {
		if runtimeClient != nil {
			_ = runtimeClient.Close()
		}
		return nil, nil, nil, err
	}
	return server.NewAgentHTTPHandler(service, authManager, server.AgentHTTPOptions{}), runtimeClient, service, nil
}

func agentRuntimeConfigFromEnv() (agentRuntimeConfig, bool) {
	address := strings.TrimSpace(os.Getenv("AUCTION_AGENT_RUNTIME_ADDR"))
	token := strings.TrimSpace(os.Getenv("AUCTION_INTERNAL_SERVICE_TOKEN"))
	if address == "" || token == "" {
		return agentRuntimeConfig{}, false
	}
	return agentRuntimeConfig{Address: address, Token: token}, true
}

func agentToolConfigFromEnv() (agentToolConfig, bool) {
	token := strings.TrimSpace(os.Getenv("AUCTION_INTERNAL_SERVICE_TOKEN"))
	if token == "" {
		return agentToolConfig{}, false
	}
	return agentToolConfig{
		Addr:  getenv("AUCTION_AGENT_TOOL_GRPC_ADDR", ":19082"),
		Token: token,
	}, true
}

type lifecycleServer interface {
	Start(context.Context) error
	Stop(context.Context) error
}

type preStopServer struct {
	lifecycleServer
	beforeStop func(context.Context) error
}

func withPreStop(server lifecycleServer, beforeStop func(context.Context) error) lifecycleServer {
	return &preStopServer{lifecycleServer: server, beforeStop: beforeStop}
}

func (s *preStopServer) Stop(ctx context.Context) error {
	beforeStopErr := s.beforeStop(ctx)
	transportStopErr := s.lifecycleServer.Stop(ctx)
	return errors.Join(beforeStopErr, transportStopErr)
}

// runServers starts all transports concurrently. A signal or any transport
// failure cancels the shared lifecycle and gracefully stops every transport.
func runServers(ctx context.Context, servers ...lifecycleServer) error {
	group, groupCtx := errgroup.WithContext(ctx)
	for _, transport := range servers {
		transport := transport
		group.Go(func() error {
			err := transport.Start(groupCtx)
			if groupCtx.Err() != nil {
				return nil
			}
			if err != nil {
				return err
			}
			return errors.New("server stopped unexpectedly")
		})
	}
	group.Go(func() error {
		<-groupCtx.Done()
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var stopErrors []error
		for _, transport := range servers {
			if err := transport.Stop(stopCtx); err != nil {
				stopErrors = append(stopErrors, err)
			}
		}
		return errors.Join(stopErrors...)
	})
	return group.Wait()
}

func newRealtimePublisherFromEnv(ctx context.Context, hub *realtime.Hub, instanceID, redisAddr, redisPassword string) (*realtime.Publisher, func()) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("AUCTION_REALTIME_BUS")))
	if mode == "" || mode == "local" || mode == "memory" {
		return realtime.NewPublisher(hub), func() {}
	}
	switch mode {
	case "redis", "pubsub", "redis_pubsub":
		bus, err := realtime.NewRedisPubSubBus(realtime.RedisBusConfig{
			Addr:     getenv("AUCTION_REALTIME_BUS_REDIS_ADDR", redisAddr),
			Password: getenv("AUCTION_REALTIME_BUS_REDIS_PASSWORD", redisPassword),
			Channel:  getenv("AUCTION_REALTIME_BUS_CHANNEL", "auction:realtime:events"),
			Origin:   instanceID,
		})
		if err != nil {
			log.Fatalf("create redis realtime bus failed: %v", err)
		}
		if err := bus.Start(ctx, hub); err != nil {
			_ = bus.Close()
			log.Fatalf("start redis realtime bus failed: %v", err)
		}
		log.Printf("redis pubsub realtime bus enabled: channel=%s origin=%s", getenv("AUCTION_REALTIME_BUS_CHANNEL", "auction:realtime:events"), instanceID)
		return realtime.NewPublisher(hub, bus), func() {
			_ = bus.Close()
		}
	case "stream", "redis_stream":
		bus, err := realtime.NewRedisStreamBus(realtime.RedisStreamBusConfig{
			Addr:         getenv("AUCTION_REALTIME_BUS_REDIS_ADDR", redisAddr),
			Password:     getenv("AUCTION_REALTIME_BUS_REDIS_PASSWORD", redisPassword),
			Stream:       getenv("AUCTION_REALTIME_BUS_STREAM", "auction:realtime:events:stream"),
			Group:        getenv("AUCTION_REALTIME_BUS_GROUP", "auction-realtime-"+instanceID),
			Consumer:     getenv("AUCTION_REALTIME_BUS_CONSUMER", instanceID),
			Origin:       instanceID,
			Block:        getenvDuration("AUCTION_REALTIME_BUS_STREAM_BLOCK", 2*time.Second),
			Count:        int64(getenvInt("AUCTION_REALTIME_BUS_STREAM_COUNT", 100)),
			MaxLenApprox: int64(getenvInt("AUCTION_REALTIME_BUS_STREAM_MAXLEN", 100000)),
		})
		if err != nil {
			log.Fatalf("create redis stream realtime bus failed: %v", err)
		}
		if err := bus.Start(ctx, hub); err != nil {
			_ = bus.Close()
			log.Fatalf("start redis stream realtime bus failed: %v", err)
		}
		log.Printf("redis stream realtime bus enabled: stream=%s group=%s consumer=%s origin=%s", getenv("AUCTION_REALTIME_BUS_STREAM", "auction:realtime:events:stream"), getenv("AUCTION_REALTIME_BUS_GROUP", "auction-realtime-"+instanceID), getenv("AUCTION_REALTIME_BUS_CONSUMER", instanceID), instanceID)
		return realtime.NewPublisher(hub, bus), func() {
			_ = bus.Close()
		}
	case "nats":
		bus, err := realtime.NewNATSBus(realtime.NATSBusConfig{
			URL:     getenv("AUCTION_REALTIME_BUS_NATS_URL", "nats://127.0.0.1:4222"),
			Subject: getenv("AUCTION_REALTIME_BUS_NATS_SUBJECT", "auction.realtime.events"),
			Queue:   getenv("AUCTION_REALTIME_BUS_NATS_QUEUE", ""),
			Name:    getenv("AUCTION_REALTIME_BUS_NATS_NAME", "live-auction-"+instanceID),
			Origin:  instanceID,
		})
		if err != nil {
			log.Fatalf("create nats realtime bus failed: %v", err)
		}
		if err := bus.Start(ctx, hub); err != nil {
			_ = bus.Close()
			log.Fatalf("start nats realtime bus failed: %v", err)
		}
		log.Printf("nats realtime bus enabled: url=%s subject=%s queue=%s origin=%s", getenv("AUCTION_REALTIME_BUS_NATS_URL", "nats://127.0.0.1:4222"), getenv("AUCTION_REALTIME_BUS_NATS_SUBJECT", "auction.realtime.events"), getenv("AUCTION_REALTIME_BUS_NATS_QUEUE", ""), instanceID)
		return realtime.NewPublisher(hub, bus), func() {
			_ = bus.Close()
		}
	case "rabbitmq", "rabbit", "amqp":
		bus, err := realtime.NewRabbitMQBus(realtime.RabbitMQBusConfig{
			URL:        getenv("AUCTION_REALTIME_BUS_RABBITMQ_URL", "amqp://guest:guest@127.0.0.1:5672/"),
			Exchange:   getenv("AUCTION_REALTIME_BUS_RABBITMQ_EXCHANGE", "auction.events"),
			RoutingKey: getenv("AUCTION_REALTIME_BUS_RABBITMQ_ROUTING_KEY", "auction.realtime.event"),
			Queue:      getenv("AUCTION_REALTIME_BUS_RABBITMQ_QUEUE", "auction.realtime.websocket"),
			Consumer:   getenv("AUCTION_REALTIME_BUS_RABBITMQ_CONSUMER", instanceID),
			Origin:     instanceID,
			Prefetch:   getenvInt("AUCTION_REALTIME_BUS_RABBITMQ_PREFETCH", 100),
		})
		if err != nil {
			log.Fatalf("create rabbitmq realtime bus failed: %v", err)
		}
		if err := bus.Start(ctx, hub); err != nil {
			_ = bus.Close()
			log.Fatalf("start rabbitmq realtime bus failed: %v", err)
		}
		log.Printf("rabbitmq realtime bus enabled: url=%s exchange=%s routing_key=%s queue=%s consumer=%s origin=%s",
			getenv("AUCTION_REALTIME_BUS_RABBITMQ_URL", "amqp://guest:guest@127.0.0.1:5672/"),
			getenv("AUCTION_REALTIME_BUS_RABBITMQ_EXCHANGE", "auction.events"),
			getenv("AUCTION_REALTIME_BUS_RABBITMQ_ROUTING_KEY", "auction.realtime.event"),
			getenv("AUCTION_REALTIME_BUS_RABBITMQ_QUEUE", "auction.realtime.websocket"),
			getenv("AUCTION_REALTIME_BUS_RABBITMQ_CONSUMER", instanceID),
			instanceID,
		)
		return realtime.NewPublisher(hub, bus), func() {
			_ = bus.Close()
		}
	default:
		log.Fatalf("unsupported AUCTION_REALTIME_BUS: %s", mode)
	}
	return realtime.NewPublisher(hub), func() {}
}

type runtimeClusterStatus struct {
	mode     string
	registry *cluster.StaticRegistry
}

func newClusterStatusFromEnv() runtimeClusterStatus {
	mode := strings.ToLower(strings.TrimSpace(getenv("AUCTION_CLUSTER_MODE", "single")))
	if mode == "" || mode == "single" || mode == "standalone" {
		return runtimeClusterStatus{mode: "single"}
	}
	registry, err := cluster.ParseStaticRegistryJSON(os.Getenv("AUCTION_CLUSTER_REGISTRY_JSON"))
	if err != nil {
		log.Fatalf("parse AUCTION_CLUSTER_REGISTRY_JSON failed: %v", err)
	}
	snapshot := registry.Snapshot()
	log.Printf("cluster mode enabled: mode=%s shards=%d assignments=%d", mode, len(snapshot.Shards), len(snapshot.Assignments))
	return runtimeClusterStatus{mode: mode, registry: registry}
}

func (s runtimeClusterStatus) ClusterSnapshot(context.Context) map[string]any {
	if s.registry == nil {
		return map[string]any{"ok": true, "mode": "single"}
	}
	return map[string]any{"ok": true, "mode": s.mode, "registry": s.registry.Snapshot()}
}

func newBuyerSearchFromEnv(ctx context.Context, source searchindex.DocumentSource) (*searchindex.PGVectorIndex, *searchindex.EmbeddingClient, *searchindex.SyncWorker) {
	if getenvBool("AUCTION_BUYER_SEARCH_ENABLED",false){return nil,nil,nil}
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("AUCTION_SEARCH_PROVIDER")))
	dsn := strings.TrimSpace(os.Getenv("AUCTION_SEARCH_PG_DSN"))
	if provider == "" && dsn == "" {
		log.Printf("buyer search index disabled: pgvector dsn is not configured")
		return nil, nil, nil
	}
	if provider != "" && provider != "pgvector" {
		log.Printf("buyer search index disabled: unsupported provider %s", provider)
		return nil, nil, nil
	}
	embedder := searchindex.NewEmbeddingClientFromEnv(os.Getenv)
	if !embedder.Configured() {
		log.Printf("buyer search index disabled: embedding client is not configured")
		return nil, nil, nil
	}
	indexCfg := searchindex.PGVectorConfig{
		DSN:                 dsn,
		EmbeddingModel:      embedder.Model(),
		EmbeddingDimensions: embedder.Dimensions(),
		MaxOpenConns:        getenvInt("AUCTION_SEARCH_PG_MAX_OPEN_CONNS", 5),
		MaxIdleConns:        getenvInt("AUCTION_SEARCH_PG_MAX_IDLE_CONNS", 2),
		ConnMaxLifetime:     getenvDuration("AUCTION_SEARCH_PG_CONN_MAX_LIFETIME", 30*time.Minute),
		ConnMaxIdleTime:     getenvDuration("AUCTION_SEARCH_PG_CONN_MAX_IDLE_TIME", 2*time.Minute),
	}
	index, err := newPGVectorIndexWithStartupWait(ctx, indexCfg)
	if err != nil {
		log.Printf("buyer search index disabled: %v", err)
		return nil, nil, nil
	}
	worker := searchindex.NewSyncWorker(source, index, embedder, searchindex.SyncWorkerConfig{
		Interval:        getenvDuration("AUCTION_SEARCH_SYNC_INTERVAL", 5*time.Minute),
		BatchLimit:      getenvInt("AUCTION_SEARCH_SYNC_BATCH_LIMIT", 500),
		HiddenRetention: getenvDuration("AUCTION_SEARCH_HIDDEN_RETENTION", searchindex.DefaultHiddenRetention),
	})
	log.Printf("buyer search index enabled: model=%s dimensions=%d", embedder.Model(), embedder.Dimensions())
	return index, embedder, worker
}

func newPGVectorIndexWithStartupWait(ctx context.Context, cfg searchindex.PGVectorConfig) (*searchindex.PGVectorIndex, error) {
	wait := getenvDuration("AUCTION_SEARCH_STARTUP_WAIT", 30*time.Second)
	retryEvery := 2 * time.Second
	deadline := time.Now().Add(wait)
	var lastErr error
	for attempt := 1; ; attempt++ {
		index, err := searchindex.NewPGVectorIndex(ctx, cfg)
		if err == nil {
			return index, nil
		}
		lastErr = err
		if wait <= 0 || time.Now().After(deadline) {
			return nil, lastErr
		}
		log.Printf("buyer search index waiting for pgvector: attempt=%d error=%v", attempt, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryEvery):
		}
	}
}

func defaultInstanceID() string {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "local"
	}
	return fmt.Sprintf("%s-%d", hostname, os.Getpid())
}

func workerStatusProviders(providers ...auction.WorkerStatusProvider) []auction.WorkerStatusProvider {
	out := make([]auction.WorkerStatusProvider, 0, len(providers))
	for _, provider := range providers {
		if provider != nil {
			out = append(out, provider)
		}
	}
	return out
}

func getenv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func getenvAllowEmpty(key, fallback string) string {
	value, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	return value
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		log.Fatalf("invalid %s duration: %v", key, err)
	}
	return duration
}

func getenvInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		log.Fatalf("invalid %s integer: %v", key, err)
	}
	return parsed
}

func getenvFloat(key string, fallback float64) float64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		log.Fatalf("invalid %s number: %v", key, err)
	}
	return parsed
}

func bootstrapMainAccount(ctx context.Context, users *userbiz.Usecase) error {
	username := os.Getenv("AUCTION_BOOTSTRAP_MAIN_ACCOUNT_USERNAME")
	password := os.Getenv("AUCTION_BOOTSTRAP_MAIN_ACCOUNT_PASSWORD")
	nickname := os.Getenv("AUCTION_BOOTSTRAP_MAIN_ACCOUNT_NICKNAME")
	if username == "" && password == "" && nickname == "" {
		return nil
	}
	if username == "" || password == "" || nickname == "" {
		return errors.New("bootstrap main account username, password and nickname must be configured together")
	}
	return users.BootstrapMainAccount(ctx, username, password, nickname)
}

func newImageStorageFromEnv() (storage.StorageProvider, error) {
	provider := strings.TrimSpace(os.Getenv("AUCTION_STORAGE_PROVIDER"))
	if provider == "" {
		provider = "local"
	}
	switch provider {
	case "local":
		return storage.NewLocalStorage(storage.LocalConfig{
			RootDir:       strings.TrimSpace(os.Getenv("AUCTION_LOCAL_STORAGE_DIR")),
			Bucket:        strings.TrimSpace(os.Getenv("AUCTION_LOCAL_STORAGE_BUCKET")),
			PublicBaseURL: strings.TrimSpace(os.Getenv("AUCTION_LOCAL_STORAGE_PUBLIC_BASE_URL")),
		})
	case "tos":
	default:
		return nil, errors.New("unsupported auction storage provider: " + provider)
	}
	return storage.NewTOSStorage(storage.TOSConfig{
		Endpoint:      strings.TrimSpace(os.Getenv("AUCTION_TOS_ENDPOINT")),
		Region:        strings.TrimSpace(os.Getenv("AUCTION_TOS_REGION")),
		Bucket:        strings.TrimSpace(os.Getenv("AUCTION_TOS_BUCKET")),
		AccessKey:     strings.TrimSpace(os.Getenv("AUCTION_TOS_ACCESS_KEY")),
		SecretKey:     strings.TrimSpace(os.Getenv("AUCTION_TOS_SECRET_KEY")),
		PublicBaseURL: strings.TrimSpace(os.Getenv("AUCTION_TOS_PUBLIC_BASE_URL")),
		UseSSL:        getenvBool("AUCTION_TOS_USE_SSL", true),
	})
}

func getenvBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "TRUE", "True", "yes", "YES", "on", "ON":
		return true
	case "0", "false", "FALSE", "False", "no", "NO", "off", "OFF":
		return false
	default:
		log.Fatalf("invalid %s bool: %s", key, value)
		return fallback
	}
}
