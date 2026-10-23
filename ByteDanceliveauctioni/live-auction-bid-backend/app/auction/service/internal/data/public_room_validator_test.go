package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/biz/auction"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
)

func TestStorePublicRoomAvailableRequiresActivePublicMerchantOwnerInOneQuery(t *testing.T) {
	tests := []struct {
		name        string
		roomStatus  auction.RoomStatus
		ownerStatus v1.UserStatus
		ownerExists bool
		roleCode    string
		want        bool
	}{
		{name: "active public merchant owner", roomStatus: auction.RoomStatusActive, ownerStatus: v1.UserStatus_USER_STATUS_ACTIVE, ownerExists: true, roleCode: userbiz.RoleMerchantOwner, want: true},
		{name: "disabled room", roomStatus: auction.RoomStatus("DISABLED"), ownerStatus: v1.UserStatus_USER_STATUS_ACTIVE, ownerExists: true, roleCode: userbiz.RoleMerchantOwner},
		{name: "disabled owner", roomStatus: auction.RoomStatusActive, ownerStatus: v1.UserStatus_USER_STATUS_DISABLED, ownerExists: true, roleCode: userbiz.RoleMerchantOwner},
		{name: "missing owner", roomStatus: auction.RoomStatusActive, roleCode: userbiz.RoleMerchantOwner},
		{name: "owner without public role", roomStatus: auction.RoomStatusActive, ownerStatus: v1.UserStatus_USER_STATUS_ACTIVE, ownerExists: true, roleCode: userbiz.RoleBuyer},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newPublicRoomTestStore(t)
			ctx := context.Background()
			if test.ownerExists {
				if err := store.db.Create(&AuctionUserModel{ID: "owner-1", Username: "owner", Nickname: "Owner", PasswordHash: "hash", Status: int32(test.ownerStatus)}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if test.roleCode != "" {
				if err := store.db.Create(&AuctionUserRoleModel{ID: "role-1", UserID: "owner-1", RoleCode: test.roleCode}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := store.db.Create(&AuctionRoomModel{ID: "room-1", MainAccountID: "owner-1", Name: "Room", Platform: "douyin", Status: string(test.roomStatus)}).Error; err != nil {
				t.Fatal(err)
			}

			var queries atomic.Int32
			if err := store.db.Callback().Query().Before("gorm:query").Register("test:count-public-room-query", func(*gorm.DB) {
				queries.Add(1)
			}); err != nil {
				t.Fatal(err)
			}
			got, err := store.PublicRoomAvailable(ctx, "room-1")
			if err != nil {
				t.Fatalf("PublicRoomAvailable() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("PublicRoomAvailable() = %v, want %v", got, test.want)
			}
			if gotQueries := queries.Load(); gotQueries != 1 {
				t.Fatalf("query calls = %d, want 1", gotQueries)
			}
		})
	}
}

func TestPublicRoomValidatorCachesPositiveAndNegativeResults(t *testing.T) {
	query := newFakePublicRoomAvailabilityQuery()
	query.results["active"] = true
	query.results["missing"] = false
	validator := newPublicRoomValidator(query, 10, 5*time.Second)

	for _, roomID := range []string{"active", "active", "missing", "missing"} {
		if _, err := validator.PublicRoomExists(context.Background(), roomID); err != nil {
			t.Fatal(err)
		}
	}
	if calls := query.callCount("active"); calls != 1 {
		t.Fatalf("active query calls = %d, want 1", calls)
	}
	if calls := query.callCount("missing"); calls != 1 {
		t.Fatalf("missing query calls = %d, want 1", calls)
	}
}

func TestPublicRoomValidatorExpiresCachedResults(t *testing.T) {
	query := newFakePublicRoomAvailabilityQuery()
	query.results["room-1"] = true
	validator := newPublicRoomValidator(query, 10, 5*time.Second)
	now := time.Unix(100, 0)
	validator.now = func() time.Time { return now }

	if _, err := validator.PublicRoomExists(context.Background(), "room-1"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Second)
	if _, err := validator.PublicRoomExists(context.Background(), "room-1"); err != nil {
		t.Fatal(err)
	}
	if calls := query.callCount("room-1"); calls != 2 {
		t.Fatalf("query calls after expiry = %d, want 2", calls)
	}
}

func TestPublicRoomValidatorDoesNotCacheErrors(t *testing.T) {
	query := newFakePublicRoomAvailabilityQuery()
	query.errs["room-1"] = errors.New("database unavailable")
	validator := newPublicRoomValidator(query, 10, 5*time.Second)
	validator.queryGate = newPublicRoomQueryGate(1)

	if exists, err := validator.PublicRoomExists(context.Background(), "room-1"); err == nil || exists {
		t.Fatalf("first lookup = (%v, %v), want fail closed", exists, err)
	}
	delete(query.errs, "room-1")
	query.results["room-1"] = true
	if exists, err := validator.PublicRoomExists(context.Background(), "room-1"); err != nil || !exists {
		t.Fatalf("retry lookup = (%v, %v), want (true, nil)", exists, err)
	}
	if calls := query.callCount("room-1"); calls != 2 {
		t.Fatalf("query calls = %d, want 2", calls)
	}
	if inUse := validator.queryGate.inUse.Load(); inUse != 0 {
		t.Fatalf("query permits in use after error retry = %d, want 0", inUse)
	}
}

func TestPublicRoomValidatorCacheNeverExceedsHardCapacity(t *testing.T) {
	query := newFakePublicRoomAvailabilityQuery()
	validator := newPublicRoomValidator(query, 7, time.Minute)

	for i := 0; i < 1_000; i++ {
		if _, err := validator.PublicRoomExists(context.Background(), fmt.Sprintf("room-%d", i)); err != nil {
			t.Fatal(err)
		}
		validator.mu.Lock()
		entries := len(validator.cache)
		validator.mu.Unlock()
		if entries > 7 {
			t.Fatalf("cache entries = %d, capacity 7", entries)
		}
	}
}

func TestPublicRoomValidatorCoalescesConcurrentSameRoomMisses(t *testing.T) {
	query := newFakePublicRoomAvailabilityQuery()
	query.results["room-1"] = true
	query.started = make(chan struct{})
	query.release = make(chan struct{})
	validator := newPublicRoomValidator(query, 100, time.Minute)

	const workers = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			exists, err := validator.PublicRoomExists(context.Background(), "room-1")
			if err != nil || !exists {
				errCh <- fmt.Errorf("lookup = (%v, %v)", exists, err)
			}
		}()
	}
	close(start)
	<-query.started
	close(query.release)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if calls := query.callCount("room-1"); calls != 1 {
		t.Fatalf("query calls = %d, want 1", calls)
	}
}

func TestPublicRoomValidatorCoalescesConcurrentSameRoomErrorsWithoutCaching(t *testing.T) {
	query := newFakePublicRoomAvailabilityQuery()
	query.errs["room-1"] = errors.New("database unavailable")
	query.started = make(chan struct{})
	query.release = make(chan struct{})
	validator := newPublicRoomValidator(query, 100, time.Minute)

	const workers = 64
	waitersJoined := sync.WaitGroup{}
	waitersJoined.Add(workers - 1)
	validator.beforeLookupWait = waitersJoined.Done
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if exists, err := validator.PublicRoomExists(context.Background(), "room-1"); err == nil || exists {
				t.Errorf("lookup = (%v, %v), want fail closed", exists, err)
			}
		}()
	}
	close(start)
	<-query.started
	waitersJoined.Wait()
	close(query.release)
	wg.Wait()
	if calls := query.callCount("room-1"); calls != 1 {
		t.Fatalf("query calls = %d, want 1", calls)
	}

	query.mu.Lock()
	query.started, query.release = nil, nil
	query.mu.Unlock()
	if _, err := validator.PublicRoomExists(context.Background(), "room-1"); err == nil {
		t.Fatal("later lookup error = nil; concurrent error must not remain cached")
	}
	if calls := query.callCount("room-1"); calls != 2 {
		t.Fatalf("query calls after later retry = %d, want 2", calls)
	}
}

func TestPublicRoomValidatorLeaderCancellationOnlyCancelsLeaderWait(t *testing.T) {
	for _, test := range []struct {
		name      string
		available bool
	}{
		{name: "positive result", available: true},
		{name: "negative result", available: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := newFakePublicRoomAvailabilityQuery()
			query.results["room-1"] = test.available
			query.started = make(chan struct{})
			query.release = make(chan struct{})
			validator := newPublicRoomValidator(query, 100, time.Minute)
			waiterJoined := sync.WaitGroup{}
			waiterJoined.Add(1)
			validator.beforeLookupWait = waiterJoined.Done

			leaderCtx, cancelLeader := context.WithCancel(context.Background())
			leaderResult := make(chan publicRoomLookupTestResult, 1)
			waiterResult := make(chan publicRoomLookupTestResult, 1)
			go func() {
				exists, err := validator.PublicRoomExists(leaderCtx, "room-1")
				leaderResult <- publicRoomLookupTestResult{exists: exists, err: err}
			}()
			<-query.started
			go func() {
				exists, err := validator.PublicRoomExists(context.Background(), "room-1")
				waiterResult <- publicRoomLookupTestResult{exists: exists, err: err}
			}()
			waiterJoined.Wait()

			cancelLeader()
			leaderReturnedBeforeQuery := false
			var leader publicRoomLookupTestResult
			select {
			case leader = <-leaderResult:
				leaderReturnedBeforeQuery = true
			case <-time.After(250 * time.Millisecond):
			}
			close(query.release)
			if !leaderReturnedBeforeQuery {
				leader = <-leaderResult
			}
			waiter := <-waiterResult

			if !leaderReturnedBeforeQuery || !errors.Is(leader.err, context.Canceled) || leader.exists {
				t.Fatalf("leader result before query=%v lookup=(%v, %v), want prompt (false, context.Canceled)", leaderReturnedBeforeQuery, leader.exists, leader.err)
			}
			if waiter.err != nil || waiter.exists != test.available {
				t.Fatalf("waiter lookup=(%v, %v), want (%v, nil)", waiter.exists, waiter.err, test.available)
			}
			if calls := query.callCount("room-1"); calls != 1 {
				t.Fatalf("query calls=%d, want 1", calls)
			}
		})
	}
}

func TestPublicRoomValidatorTimeoutClearsStripeAndDoesNotCacheError(t *testing.T) {
	query := &timeoutThenSuccessPublicRoomQuery{}
	validator := newPublicRoomValidator(query, 100, time.Minute)
	validator.queryGate = newPublicRoomQueryGate(1)
	validator.lookupTimeout = 10 * time.Millisecond

	if exists, err := validator.PublicRoomExists(context.Background(), "room-1"); !errors.Is(err, context.DeadlineExceeded) || exists {
		t.Fatalf("timed out lookup=(%v, %v), want (false, context.DeadlineExceeded)", exists, err)
	}
	if exists, err := validator.PublicRoomExists(context.Background(), "room-1"); err != nil || !exists {
		t.Fatalf("retry lookup=(%v, %v), want (true, nil)", exists, err)
	}
	if calls := query.calls.Load(); calls != 2 {
		t.Fatalf("query calls=%d, want 2", calls)
	}
	if inUse := validator.queryGate.inUse.Load(); inUse != 0 {
		t.Fatalf("query permits in use after timeout retry = %d, want 0", inUse)
	}
}

func TestPublicRoomValidatorPanicWakesWaitersAndClearsCollidingStripe(t *testing.T) {
	const panicRoom = "panic-room"
	collisionRoom := publicRoomTestCollidingKey(panicRoom)
	query := newFakePublicRoomAvailabilityQuery()
	query.panicValues[panicRoom] = "query panic"
	query.results[collisionRoom] = true
	query.started = make(chan struct{})
	query.release = make(chan struct{})
	validator := newPublicRoomValidator(query, 100, time.Minute)
	validator.queryGate = newPublicRoomQueryGate(1)
	waitersJoined := sync.WaitGroup{}
	waitersJoined.Add(2)
	validator.beforeLookupWait = waitersJoined.Done

	leaderResult := make(chan publicRoomLookupTestResult, 1)
	sameRoomResult := make(chan publicRoomLookupTestResult, 1)
	collisionResult := make(chan publicRoomLookupTestResult, 1)
	go func() {
		exists, err := validator.PublicRoomExists(context.Background(), panicRoom)
		leaderResult <- publicRoomLookupTestResult{exists: exists, err: err}
	}()
	<-query.started
	go func() {
		exists, err := validator.PublicRoomExists(context.Background(), panicRoom)
		sameRoomResult <- publicRoomLookupTestResult{exists: exists, err: err}
	}()
	go func() {
		exists, err := validator.PublicRoomExists(context.Background(), collisionRoom)
		collisionResult <- publicRoomLookupTestResult{exists: exists, err: err}
	}()
	waitersJoined.Wait()
	close(query.release)

	for name, resultCh := range map[string]<-chan publicRoomLookupTestResult{
		"leader": leaderResult, "same-room waiter": sameRoomResult,
	} {
		result := awaitPublicRoomLookupTestResult(t, resultCh)
		if result.exists || result.err == nil || result.err.Error() != "public room lookup failed" {
			t.Fatalf("%s lookup=(%v, %v), want stable fail-closed error", name, result.exists, result.err)
		}
	}
	collision := awaitPublicRoomLookupTestResult(t, collisionResult)
	if collision.err != nil || !collision.exists {
		t.Fatalf("collision lookup=(%v, %v), want its own (true, nil) result", collision.exists, collision.err)
	}

	query.mu.Lock()
	delete(query.panicValues, panicRoom)
	query.results[panicRoom] = true
	query.mu.Unlock()
	if exists, err := validator.PublicRoomExists(context.Background(), panicRoom); err != nil || !exists {
		t.Fatalf("post-panic retry=(%v, %v), want (true, nil)", exists, err)
	}
	if calls := query.callCount(panicRoom); calls != 2 {
		t.Fatalf("panic-room query calls=%d, want 2", calls)
	}
	if calls := query.callCount(collisionRoom); calls != 1 {
		t.Fatalf("collision-room query calls=%d, want 1", calls)
	}
	if inUse := validator.queryGate.inUse.Load(); inUse != 0 {
		t.Fatalf("query permits in use after panic recovery = %d, want 0", inUse)
	}
}

func TestPublicRoomValidatorFailFastBudgetPreservesAuthoritativeDBCapacity(t *testing.T) {
	if publicRoomQueryBudget > 4 {
		t.Fatalf("public room query budget = %d, want at most 4", publicRoomQueryBudget)
	}
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(publicRoomQueryBudget + 1)
	sqlDB.SetMaxIdleConns(publicRoomQueryBudget + 1)
	query := &blockingSQLPublicRoomQuery{
		db: sqlDB, started: make(chan struct{}, publicRoomQueryBudget), release: make(chan struct{}),
	}
	validator := newPublicRoomValidator(query, 1_000, time.Minute)
	validator.lookupTimeout = 5 * time.Second
	roomIDs := publicRoomTestDistinctStripeKeys(publicRoomQueryBudget + 32)
	results := make(chan publicRoomLookupTestResult, len(roomIDs))
	start := make(chan struct{})
	for _, roomID := range roomIDs {
		roomID := roomID
		go func() {
			<-start
			exists, err := validator.PublicRoomExists(context.Background(), roomID)
			results <- publicRoomLookupTestResult{exists: exists, err: err}
		}()
	}
	close(start)
	for i := 0; i < publicRoomQueryBudget; i++ {
		select {
		case <-query.started:
		case <-time.After(time.Second):
			t.Fatal("query budget did not fill")
		}
	}

	for i := 0; i < len(roomIDs)-publicRoomQueryBudget; i++ {
		result := awaitPublicRoomLookupTestResult(t, results)
		if result.exists || !errors.Is(result.err, errPublicRoomQuerySaturated) {
			t.Fatalf("excess lookup=(%v, %v), want stable fail-fast saturation", result.exists, result.err)
		}
	}
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), time.Second)
	defer cancelProbe()
	var probe int
	if err := sqlDB.QueryRowContext(probeCtx, "SELECT 1").Scan(&probe); err != nil || probe != 1 {
		t.Fatalf("authoritative DB probe=(%d, %v), want (1, nil)", probe, err)
	}
	close(query.release)
	for i := 0; i < publicRoomQueryBudget; i++ {
		result := awaitPublicRoomLookupTestResult(t, results)
		if result.err != nil || !result.exists {
			t.Fatalf("admitted lookup=(%v, %v), want (true, nil)", result.exists, result.err)
		}
	}
	if maxActive := query.maxActive.Load(); maxActive > int32(publicRoomQueryBudget) {
		t.Fatalf("max concurrent DB lookups = %d, budget %d", maxActive, publicRoomQueryBudget)
	}
	if inUse := validator.queryGate.inUse.Load(); inUse != 0 {
		t.Fatalf("query permits in use after stress = %d, want 0", inUse)
	}
}

type publicRoomLookupTestResult struct {
	exists bool
	err    error
}

func awaitPublicRoomLookupTestResult(t *testing.T, resultCh <-chan publicRoomLookupTestResult) publicRoomLookupTestResult {
	t.Helper()
	select {
	case result := <-resultCh:
		return result
	case <-time.After(time.Second):
		t.Fatal("lookup did not finish")
		return publicRoomLookupTestResult{}
	}
}

func publicRoomTestCollidingKey(roomID string) string {
	wantStripe := publicRoomLookupStripe(roomID)
	for i := 0; ; i++ {
		candidate := fmt.Sprintf("collision-room-%d", i)
		if candidate != roomID && publicRoomLookupStripe(candidate) == wantStripe {
			return candidate
		}
	}
}

func publicRoomTestDistinctStripeKeys(count int) []string {
	keys := make([]string, 0, count)
	used := make(map[uint32]struct{}, count)
	for i := 0; len(keys) < count; i++ {
		candidate := fmt.Sprintf("distinct-room-%d", i)
		stripe := publicRoomLookupStripe(candidate)
		if _, exists := used[stripe]; exists {
			continue
		}
		used[stripe] = struct{}{}
		keys = append(keys, candidate)
	}
	return keys
}

type blockingSQLPublicRoomQuery struct {
	db        *sql.DB
	started   chan struct{}
	release   chan struct{}
	active    atomic.Int32
	maxActive atomic.Int32
}

func (q *blockingSQLPublicRoomQuery) PublicRoomAvailable(ctx context.Context, _ string) (bool, error) {
	conn, err := q.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	active := q.active.Add(1)
	defer q.active.Add(-1)
	for {
		maxActive := q.maxActive.Load()
		if active <= maxActive || q.maxActive.CompareAndSwap(maxActive, active) {
			break
		}
	}
	q.started <- struct{}{}
	select {
	case <-q.release:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

type timeoutThenSuccessPublicRoomQuery struct {
	calls atomic.Int32
}

func (q *timeoutThenSuccessPublicRoomQuery) PublicRoomAvailable(ctx context.Context, _ string) (bool, error) {
	if q.calls.Add(1) == 1 {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return true, nil
}

func newPublicRoomTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&AuctionRoomModel{}, &AuctionUserModel{}, &AuctionUserRoleModel{}); err != nil {
		t.Fatal(err)
	}
	return &Store{db: db}
}

type fakePublicRoomAvailabilityQuery struct {
	mu          sync.Mutex
	results     map[string]bool
	errs        map[string]error
	calls       map[string]int
	started     chan struct{}
	release     chan struct{}
	panicValues map[string]any
	startOnce   sync.Once
}

func newFakePublicRoomAvailabilityQuery() *fakePublicRoomAvailabilityQuery {
	return &fakePublicRoomAvailabilityQuery{
		results: make(map[string]bool), errs: make(map[string]error), calls: make(map[string]int), panicValues: make(map[string]any),
	}
}

func (q *fakePublicRoomAvailabilityQuery) PublicRoomAvailable(_ context.Context, roomID string) (bool, error) {
	q.mu.Lock()
	q.calls[roomID]++
	result, err := q.results[roomID], q.errs[roomID]
	panicValue, shouldPanic := q.panicValues[roomID]
	started, release := q.started, q.release
	q.mu.Unlock()
	if started != nil {
		q.startOnce.Do(func() { close(started) })
	}
	if release != nil {
		<-release
	}
	if shouldPanic {
		panic(panicValue)
	}
	return result, err
}

func (q *fakePublicRoomAvailabilityQuery) callCount(roomID string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.calls[roomID]
}
