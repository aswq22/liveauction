package agenttool

import (
	"context"
	"reflect"
	"testing"

	agentv1 "live-auction-bid/backend/api/agent/v1"
	auctionv1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
	"live-auction-bid/backend/app/auction/service/internal/biz/auction"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestQueryInventoryUsesPersistedRunTenant(t *testing.T) {
	fixture := newToolServiceFixture(t, "secret", agentcontrol.Run{ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a"})
	fixture.Auctions.LotResults = []auction.LotList{{Lots: []*auctionv1.Lot{
		{Id: "draft-1", MainAccountId: "merchant-a", Status: auctionv1.LotStatus_LOT_STATUS_DRAFT, QueueStatus: auctionv1.LotQueueStatus_LOT_QUEUE_STATUS_NONE, Rule: &auctionv1.BidRule{DurationSeconds: 3000}, UpdatedAtUnixMs: 20},
	}}}

	reply, err := fixture.Service.QueryInventory(authContext("Bearer secret"), &agentv1.QueryInventoryRequest{
		Context: &agentv1.RequestContext{RunId: "run-1", TenantId: "merchant-b", OperatorId: "user-b"},
	})
	if err != nil {
		t.Fatalf("QueryInventory() error = %v", err)
	}
	if len(reply.GetLots()) != 1 {
		t.Fatalf("QueryInventory() lots = %d, want 1", len(reply.GetLots()))
	}
	if got := reply.GetLots()[0].GetDurationSeconds(); got != 3000 {
		t.Fatalf("QueryInventory() duration = %d, want 3000", got)
	}
	if len(fixture.Auctions.LotQueries) == 0 {
		t.Fatal("QueryInventory() did not query merchant-owned auction lots")
	}
	for _, query := range fixture.Auctions.LotQueries {
		if query.MainAccountID != "merchant-a" {
			t.Fatalf("ListLotsByQuery() tenant = %q, want persisted tenant merchant-a", query.MainAccountID)
		}
	}
}

func TestStartedLotCannotReturnToPlanningInventory(t *testing.T) {
	lot := &auctionv1.Lot{Status: auctionv1.LotStatus_LOT_STATUS_READY, QueueStatus: auctionv1.LotQueueStatus_LOT_QUEUE_STATUS_NONE, StartedAtUnixMs: 123}
	if isAppendableInventoryLot(lot) {
		t.Fatal("a previously started lot must never be eligible for planning")
	}
	lot.StartedAtUnixMs = 0
	if !isAppendableInventoryLot(lot) {
		t.Fatal("an unstarted ready lot should be eligible for planning")
	}
}

func TestEveryToolRPCRequiresBearerToken(t *testing.T) {
	tests := []struct {
		name string
		call func(*Service) error
	}{
		{name: "inventory", call: func(service *Service) error {
			_, err := service.QueryInventory(context.Background(), &agentv1.QueryInventoryRequest{Context: &agentv1.RequestContext{RunId: "run-1"}})
			return err
		}},
		{name: "history", call: func(service *Service) error {
			_, err := service.QueryAuctionHistory(context.Background(), &agentv1.QueryAuctionHistoryRequest{Context: &agentv1.RequestContext{RunId: "run-1"}})
			return err
		}},
		{name: "snapshot", call: func(service *Service) error {
			_, err := service.GetSessionSnapshot(context.Background(), &agentv1.GetSessionSnapshotRequest{Context: &agentv1.RequestContext{RunId: "run-1"}, RoomId: "room-1"})
			return err
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newToolServiceFixture(t, "secret", agentcontrol.Run{ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a"})
			if got := status.Code(test.call(fixture.Service)); got != codes.Unauthenticated {
				t.Fatalf("RPC status = %s, want %s", got, codes.Unauthenticated)
			}
			if fixture.Runs.FindCalls != 0 {
				t.Fatalf("run repository called %d times before authentication, want 0", fixture.Runs.FindCalls)
			}
		})
	}
}

func TestToolAuthorizationRejectsNonExactBearerCredentials(t *testing.T) {
	for _, authorization := range []string{
		"secret",
		"bearer secret",
		"Bearer wrong",
		"Bearer  secret",
		"Bearer secret extra",
	} {
		t.Run(authorization, func(t *testing.T) {
			fixture := newToolServiceFixture(t, "secret", agentcontrol.Run{ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a"})
			_, err := fixture.Service.QueryInventory(authContext(authorization), &agentv1.QueryInventoryRequest{Context: &agentv1.RequestContext{RunId: "run-1"}})
			if got := status.Code(err); got != codes.Unauthenticated {
				t.Fatalf("QueryInventory() status = %s, want %s", got, codes.Unauthenticated)
			}
		})
	}
}

func TestToolRPCRejectsUnknownRun(t *testing.T) {
	fixture := newToolServiceFixture(t, "secret", agentcontrol.Run{})
	fixture.Runs.FindErr = apperr.ErrNotFound

	_, err := fixture.Service.QueryInventory(authContext("Bearer secret"), &agentv1.QueryInventoryRequest{Context: &agentv1.RequestContext{RunId: "missing"}})
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("QueryInventory() status = %s, want %s", got, codes.NotFound)
	}
}

func TestToolRPCRequiresRunID(t *testing.T) {
	fixture := newToolServiceFixture(t, "secret", agentcontrol.Run{})

	_, err := fixture.Service.QueryInventory(authContext("Bearer secret"), &agentv1.QueryInventoryRequest{})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("QueryInventory() status = %s, want %s", got, codes.InvalidArgument)
	}
}

func TestNewServiceRejectsBlankInternalToken(t *testing.T) {
	_, err := NewService("   ", &fakeRunRepository{}, &fakeAuctionReader{})
	if err == nil {
		t.Fatal("NewService() accepted a blank internal token")
	}
}

func TestQueryInventoryReturnsOnlyInventoryStatusesSortedAndLimited(t *testing.T) {
	fixture := newToolServiceFixture(t, "secret", agentcontrol.Run{ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a"})
	fixture.Auctions.LotResults = []auction.LotList{
		{Lots: []*auctionv1.Lot{
			inventoryLot("draft-old", auctionv1.LotStatus_LOT_STATUS_DRAFT, 10),
			queuedInventoryLot("draft-but-queued", auctionv1.LotStatus_LOT_STATUS_DRAFT, 40),
			inventoryLot("live-defensive-filter", auctionv1.LotStatus_LOT_STATUS_LIVE, 1000),
		}},
		{Lots: []*auctionv1.Lot{inventoryLot("ready-new", auctionv1.LotStatus_LOT_STATUS_READY, 30)}},
	}

	reply, err := fixture.Service.QueryInventory(authContext("Bearer secret"), &agentv1.QueryInventoryRequest{
		Context: &agentv1.RequestContext{RunId: "run-1"},
		Limit:   2,
	})
	if err != nil {
		t.Fatalf("QueryInventory() error = %v", err)
	}
	gotIDs := make([]string, 0, len(reply.GetLots()))
	for _, lot := range reply.GetLots() {
		gotIDs = append(gotIDs, lot.GetId())
	}
	if want := []string{"ready-new", "draft-old"}; !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("QueryInventory() lot IDs = %v, want %v", gotIDs, want)
	}
	if got := reply.GetLots()[0].GetStatus(); got != "READY" {
		t.Fatalf("inventory status = %q, want READY", got)
	}
	if len(fixture.Auctions.LotQueries) != 2 {
		t.Fatalf("ListLotsByQuery() calls = %d, want 2 appendable statuses", len(fixture.Auctions.LotQueries))
	}
	wantStatuses := []auctionv1.LotStatus{
		auctionv1.LotStatus_LOT_STATUS_DRAFT,
		auctionv1.LotStatus_LOT_STATUS_READY,
	}
	for i, want := range wantStatuses {
		query := fixture.Auctions.LotQueries[i]
		if query.Status != want || query.PageSize != 100 || !query.OrderByUpdatedAt {
			t.Fatalf("lot query %d = %+v, want status %s, page size 100, and updated-at ordering", i, query, want)
		}
	}
}

func TestQueryAuctionHistoryAggregatesTenantOrdersWithoutBuyerData(t *testing.T) {
	fixture := newToolServiceFixture(t, "secret", agentcontrol.Run{ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a"})
	fixture.Auctions.LotResults = []auction.LotList{{Lots: []*auctionv1.Lot{
		{
			Id: "lot-1", RoomId: "room-1", MainAccountId: "merchant-a",
			Status:     auctionv1.LotStatus_LOT_STATUS_SETTLED,
			FinalPrice: &auctionv1.Money{Amount: 8800, Currency: "CNY"}, SettledAtUnixMs: 1234,
		},
		{Id: "lot-2", RoomId: "room-1", MainAccountId: "merchant-a", Status: auctionv1.LotStatus_LOT_STATUS_CANCELLED},
	}}}
	fixture.Auctions.OrderResult = auction.OrderList{Orders: []auction.OrderSummary{{
		ID: "order-1", MainAccountID: "merchant-a", LotID: "lot-1", BuyerUserID: "buyer-secret",
		BuyerNickname: "hidden", ShippingAddressID: "address-secret", Status: auction.OrderStatusPaid,
		PaymentStatus: auction.PaymentStatusSuccess, Amount: 8800, Currency: "CNY",
	}}}

	reply, err := fixture.Service.QueryAuctionHistory(authContext("Bearer secret"), &agentv1.QueryAuctionHistoryRequest{
		Context: &agentv1.RequestContext{RunId: "run-1", TenantId: "merchant-b"}, Limit: 100,
	})
	if err != nil {
		t.Fatalf("QueryAuctionHistory() error = %v", err)
	}
	if len(reply.GetItems()) != 2 {
		t.Fatalf("QueryAuctionHistory() items = %d, want 2", len(reply.GetItems()))
	}
	item := reply.GetItems()[0]
	if item.GetLotId() != "lot-1" || item.GetFinalPrice().GetAmount() != 8800 || item.GetOrderStatus() != "PAID" || item.GetPaymentStatus() != "SUCCESS" {
		t.Fatalf("settled history item = %+v", item)
	}
	if fixture.Auctions.OrderQuery.MainAccountID != "merchant-a" {
		t.Fatalf("ListOrders() tenant = %q, want merchant-a", fixture.Auctions.OrderQuery.MainAccountID)
	}
	// The protobuf response type deliberately has no buyer/contact fields. This
	// reflection assertion guards that security boundary if the contract evolves.
	fields := item.ProtoReflect().Descriptor().Fields()
	for _, forbidden := range []string{"buyer_user_id", "buyer_nickname", "shipping_address_id", "shipping_address"} {
		if fields.ByName(protoreflect.Name(forbidden)) != nil {
			t.Fatalf("AuctionHistoryItem unexpectedly exposes %s", forbidden)
		}
	}
}

func TestQueryAuctionHistoryLooksUpOrderForEverySelectedLot(t *testing.T) {
	fixture := newToolServiceFixture(t, "secret", agentcontrol.Run{ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a"})
	fixture.Auctions.LotResults = []auction.LotList{{Lots: []*auctionv1.Lot{{
		Id: "selected-lot", RoomId: "room-1", MainAccountId: "merchant-a",
		Status: auctionv1.LotStatus_LOT_STATUS_SETTLED,
	}}}}
	for i := 0; i < 100; i++ {
		fixture.Auctions.OrderResult.Orders = append(fixture.Auctions.OrderResult.Orders, auction.OrderSummary{
			ID: "newer-order", MainAccountID: "merchant-a", LotID: "unrelated-lot",
		})
	}
	fixture.Auctions.OrdersByLot = map[string]auction.OrderList{
		"selected-lot": {Orders: []auction.OrderSummary{{
			ID: "selected-order", MainAccountID: "merchant-a", LotID: "selected-lot",
			Status: auction.OrderStatusPaid, PaymentStatus: auction.PaymentStatusSuccess,
		}}},
	}

	reply, err := fixture.Service.QueryAuctionHistory(authContext("Bearer secret"), &agentv1.QueryAuctionHistoryRequest{
		Context: &agentv1.RequestContext{RunId: "run-1"}, Limit: 100,
	})
	if err != nil {
		t.Fatalf("QueryAuctionHistory() error = %v", err)
	}
	if got := reply.GetItems()[0].GetPaymentStatus(); got != "SUCCESS" {
		t.Fatalf("selected lot payment status = %q, want SUCCESS", got)
	}
	if len(fixture.Auctions.OrderQueries) != 1 || fixture.Auctions.OrderQueries[0].LotID != "selected-lot" {
		t.Fatalf("ListOrders() queries = %+v, want exact selected lot lookup", fixture.Auctions.OrderQueries)
	}
}

func TestGetSessionSnapshotValidatesRoomOwnershipAndFiltersLots(t *testing.T) {
	fixture := newToolServiceFixture(t, "secret", agentcontrol.Run{ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a"})
	fixture.Auctions.LotResults = []auction.LotList{{Lots: []*auctionv1.Lot{
		inventoryLotInRoom("queued-2", "room-1", auctionv1.LotStatus_LOT_STATUS_QUEUED, 20, 2),
		inventoryLotInRoom("current", "room-1", auctionv1.LotStatus_LOT_STATUS_LIVE, 30, 0),
		inventoryLotInRoom("draft-hidden", "room-1", auctionv1.LotStatus_LOT_STATUS_DRAFT, 40, 0),
		inventoryLotInRoom("queued-1", "room-1", auctionv1.LotStatus_LOT_STATUS_QUEUED, 10, 1),
	}}}

	reply, err := fixture.Service.GetSessionSnapshot(authContext("Bearer secret"), &agentv1.GetSessionSnapshotRequest{
		Context: &agentv1.RequestContext{RunId: "run-1", TenantId: "merchant-b"}, RoomId: "room-1",
	})
	if err != nil {
		t.Fatalf("GetSessionSnapshot() error = %v", err)
	}
	if fixture.Auctions.ValidatedRoomID != "room-1" || fixture.Auctions.ValidatedTenantID != "merchant-a" {
		t.Fatalf("ValidateRoomInMainAccount() got room=%q tenant=%q", fixture.Auctions.ValidatedRoomID, fixture.Auctions.ValidatedTenantID)
	}
	snapshot := reply.GetSnapshot()
	if snapshot.GetCurrentLot().GetId() != "current" {
		t.Fatalf("current lot = %q, want current", snapshot.GetCurrentLot().GetId())
	}
	gotQueued := []string{snapshot.GetQueuedLots()[0].GetId(), snapshot.GetQueuedLots()[1].GetId()}
	if want := []string{"queued-1", "queued-2"}; !reflect.DeepEqual(gotQueued, want) {
		t.Fatalf("queued lots = %v, want %v", gotQueued, want)
	}
	if snapshot.GetUpdatedAtUnixMs() != 30 {
		t.Fatalf("snapshot updated time = %d, want 30", snapshot.GetUpdatedAtUnixMs())
	}
}

func TestGetSessionSnapshotQueriesRelevantStatusesInsteadOfMixedCurrentPage(t *testing.T) {
	fixture := newToolServiceFixture(t, "secret", agentcontrol.Run{ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a"})
	crowded := make([]*auctionv1.Lot, 0, 100)
	for i := 0; i < 100; i++ {
		crowded = append(crowded, inventoryLotInRoom("draft", "room-1", auctionv1.LotStatus_LOT_STATUS_DRAFT, int64(1000-i), 0))
	}
	fixture.Auctions.LotResults = []auction.LotList{{Lots: crowded}}
	fixture.Auctions.LotsByStatus = map[auctionv1.LotStatus]auction.LotList{
		auctionv1.LotStatus_LOT_STATUS_LIVE: {Lots: []*auctionv1.Lot{
			inventoryLotInRoom("live-current", "room-1", auctionv1.LotStatus_LOT_STATUS_LIVE, 200, 0),
		}},
		auctionv1.LotStatus_LOT_STATUS_EXTENDED: {},
		auctionv1.LotStatus_LOT_STATUS_QUEUED: {Lots: []*auctionv1.Lot{
			inventoryLotInRoom("queued-next", "room-1", auctionv1.LotStatus_LOT_STATUS_QUEUED, 190, 1),
		}},
	}

	reply, err := fixture.Service.GetSessionSnapshot(authContext("Bearer secret"), &agentv1.GetSessionSnapshotRequest{
		Context: &agentv1.RequestContext{RunId: "run-1"}, RoomId: "room-1",
	})
	if err != nil {
		t.Fatalf("GetSessionSnapshot() error = %v", err)
	}
	if got := reply.GetSnapshot().GetCurrentLot().GetId(); got != "live-current" {
		t.Fatalf("current lot = %q, want live-current", got)
	}
	if got := reply.GetSnapshot().GetQueuedLots()[0].GetId(); got != "queued-next" {
		t.Fatalf("queued lot = %q, want queued-next", got)
	}
	if len(fixture.Auctions.LotQueries) != 3 {
		t.Fatalf("ListLotsByQuery() calls = %d, want 3 exact statuses", len(fixture.Auctions.LotQueries))
	}
	wantStatuses := []auctionv1.LotStatus{
		auctionv1.LotStatus_LOT_STATUS_LIVE,
		auctionv1.LotStatus_LOT_STATUS_EXTENDED,
		auctionv1.LotStatus_LOT_STATUS_QUEUED,
	}
	for i, want := range wantStatuses {
		if got := fixture.Auctions.LotQueries[i].Status; got != want {
			t.Fatalf("lot query %d status = %s, want %s", i, got, want)
		}
	}
}

func TestGetSessionSnapshotRejectsRoomOutsidePersistedTenant(t *testing.T) {
	fixture := newToolServiceFixture(t, "secret", agentcontrol.Run{ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a"})
	fixture.Auctions.ValidateRoomErr = apperr.ErrPermissionDenied

	_, err := fixture.Service.GetSessionSnapshot(authContext("Bearer secret"), &agentv1.GetSessionSnapshotRequest{
		Context: &agentv1.RequestContext{RunId: "run-1", TenantId: "merchant-b"}, RoomId: "room-b",
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("GetSessionSnapshot() status = %s, want %s", got, codes.PermissionDenied)
	}
	if len(fixture.Auctions.LotQueries) != 0 {
		t.Fatalf("queried lots before room authorization: %+v", fixture.Auctions.LotQueries)
	}
}

func TestGetSessionSnapshotRequiresRoomID(t *testing.T) {
	fixture := newToolServiceFixture(t, "secret", agentcontrol.Run{ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a"})

	_, err := fixture.Service.GetSessionSnapshot(authContext("Bearer secret"), &agentv1.GetSessionSnapshotRequest{
		Context: &agentv1.RequestContext{RunId: "run-1"},
	})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("GetSessionSnapshot() status = %s, want %s", got, codes.InvalidArgument)
	}
}

type toolServiceFixture struct {
	Service  *Service
	Runs     *fakeRunRepository
	Auctions *fakeAuctionReader
}

func newToolServiceFixture(t *testing.T, token string, run agentcontrol.Run) toolServiceFixture {
	t.Helper()
	runs := &fakeRunRepository{Run: run}
	auctions := &fakeAuctionReader{}
	service, err := NewService(token, runs, auctions)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return toolServiceFixture{Service: service, Runs: runs, Auctions: auctions}
}

func authContext(value string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", value))
}

func inventoryLot(id string, status auctionv1.LotStatus, updatedAt int64) *auctionv1.Lot {
	return inventoryLotInRoom(id, "room-1", status, updatedAt, 0)
}

func inventoryLotInRoom(id, roomID string, status auctionv1.LotStatus, updatedAt int64, queuePosition int32) *auctionv1.Lot {
	return &auctionv1.Lot{
		Id: id, RoomId: roomID, MainAccountId: "merchant-a", Title: "title-" + id,
		Status: status, QueueStatus: auctionv1.LotQueueStatus_LOT_QUEUE_STATUS_NONE, QueuePosition: queuePosition, Stock: 1,
		Rule:            &auctionv1.BidRule{StartPrice: &auctionv1.Money{Amount: 100, Currency: "CNY"}},
		CurrentPrice:    &auctionv1.Money{Amount: 120, Currency: "CNY"},
		CreatedAtUnixMs: updatedAt - 1, UpdatedAtUnixMs: updatedAt,
	}
}

func queuedInventoryLot(id string, status auctionv1.LotStatus, updatedAt int64) *auctionv1.Lot {
	lot := inventoryLot(id, status, updatedAt)
	lot.QueueStatus = auctionv1.LotQueueStatus_LOT_QUEUE_STATUS_QUEUED
	lot.QueuePosition = 1
	return lot
}

type fakeRunRepository struct {
	Run       agentcontrol.Run
	FindErr   error
	FindCalls int
}

func (r *fakeRunRepository) Create(context.Context, agentcontrol.Run) error { return nil }
func (r *fakeRunRepository) Find(_ context.Context, _ string) (agentcontrol.Run, error) {
	r.FindCalls++
	return r.Run, r.FindErr
}
func (r *fakeRunRepository) UpdateStatus(context.Context, string, int64, agentcontrol.RunStatus, string, string, string) error {
	return nil
}
func (r *fakeRunRepository) AppendEvent(context.Context, agentcontrol.Event) error { return nil }
func (r *fakeRunRepository) ListEvents(context.Context, string, int64, int) ([]agentcontrol.Event, error) {
	return nil, nil
}

type fakeAuctionReader struct {
	LotQueries        []auction.LotQuery
	LotResults        []auction.LotList
	LotErr            error
	OrderQuery        auction.OrderQuery
	OrderQueries      []auction.OrderQuery
	OrderResult       auction.OrderList
	OrdersByLot       map[string]auction.OrderList
	OrderErr          error
	LotsByStatus      map[auctionv1.LotStatus]auction.LotList
	ValidatedRoomID   string
	ValidatedTenantID string
	ValidateRoomErr   error
}

func (r *fakeAuctionReader) ListLotsByQuery(_ context.Context, query auction.LotQuery) (auction.LotList, error) {
	r.LotQueries = append(r.LotQueries, query)
	if r.LotErr != nil {
		return auction.LotList{}, r.LotErr
	}
	if query.Status != auctionv1.LotStatus_LOT_STATUS_UNSPECIFIED && r.LotsByStatus != nil {
		return r.LotsByStatus[query.Status], nil
	}
	index := len(r.LotQueries) - 1
	if index >= len(r.LotResults) {
		return auction.LotList{}, nil
	}
	return r.LotResults[index], nil
}

func (r *fakeAuctionReader) ListOrders(_ context.Context, query auction.OrderQuery) (auction.OrderList, error) {
	r.OrderQuery = query
	r.OrderQueries = append(r.OrderQueries, query)
	if query.LotID != "" && r.OrdersByLot != nil {
		return r.OrdersByLot[query.LotID], r.OrderErr
	}
	return r.OrderResult, r.OrderErr
}

func (r *fakeAuctionReader) ValidateRoomInMainAccount(_ context.Context, roomID, mainAccountID string) error {
	r.ValidatedRoomID = roomID
	r.ValidatedTenantID = mainAccountID
	return r.ValidateRoomErr
}
