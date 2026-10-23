package data

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/biz/auction"
)

// 2026-09-06 00:00 Shanghai and noon, respectively.
const dashboardStart int64 = 1788624000000
const dashboardNow int64 = 1788667200000

func dashboardTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "dashboard.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&AuctionRoomModel{}, &AuctionLotModel{}, &AuctionBidModel{}, &AuctionLotParticipantModel{}, &UserOrderModel{}, &UserOrderItemModel{}); err != nil {
		t.Fatal(err)
	}
	return &Store{db: db}
}

func dashboardInsert(t *testing.T, s *Store, value any) {
	t.Helper()
	if err := s.db.Create(value).Error; err != nil {
		t.Fatal(err)
	}
}
func dashboardQuery() auction.DashboardQuery {
	return auction.DashboardQuery{MainAccountID: "merchant-a", Range: auction.DashboardRangeToday, NowUnixMs: dashboardNow}
}
func dashboardLot(id, tenant, room, category string, started, settled int64) AuctionLotModel {
	status := v1.LotStatus_LOT_STATUS_LIVE
	if settled > 0 {
		status = v1.LotStatus_LOT_STATUS_SETTLED
	}
	return AuctionLotModel{ID: id, MainAccountID: tenant, RoomID: room, Title: id, Status: int32(status), QueueStatus: int32(v1.LotQueueStatus_LOT_QUEUE_STATUS_QUEUED), StartedAtUnixMs: started, SettledAtUnixMs: settled, FinalPriceAmount: 6440, CreatedAt: time.UnixMilli(dashboardStart), Payload: fmt.Sprintf(`{"category":%q}`, category)}
}
func dashboardOrder(t *testing.T, s *Store, id, tenant, lot, status, payment string, amount, created, paid, expiry int64) {
	t.Helper()
	dashboardInsert(t, s, &UserOrderModel{ID: id, Source: "auction", SourceOrderID: id, MainAccountID: tenant, UserID: "buyer", Status: status, PaymentStatus: payment, TotalAmount: amount, CreatedAtUnixMs: created, PaidAtUnixMs: paid, ExpiresAtUnixMs: expiry})
	dashboardInsert(t, s, &UserOrderItemModel{ID: "item-" + id, OrderID: id, Source: "auction", LotID: lot, TotalAmount: amount, UnitAmount: amount})
}

func TestDashboardDoesNotMixMerchantsPaymentCohortsOrDuplicateParticipants(t *testing.T) {
	s := dashboardTestStore(t)
	dashboardInsert(t, s, &[]AuctionRoomModel{{ID: "room-a1", MainAccountID: "merchant-a"}, {ID: "room-a2", MainAccountID: "merchant-a"}, {ID: "room-b", MainAccountID: "merchant-b"}})
	dashboardInsert(t, s, &[]AuctionLotModel{dashboardLot("lot-a1", "merchant-a", "room-a1", "瓷器", dashboardStart, dashboardStart+500), dashboardLot("lot-a2", "merchant-a", "room-a2", "", dashboardStart+1, dashboardStart+600), dashboardLot("lot-a3", "merchant-a", "room-a2", "瓷器", dashboardStart+1, 0), dashboardLot("lot-b", "merchant-b", "room-b", "外部", dashboardStart, dashboardStart+2)})
	dashboardOrder(t, s, "paid-old", "merchant-a", "lot-a1", "paid", "success", 6440, dashboardStart-1, dashboardStart, 0)
	dashboardOrder(t, s, "paid-new", "merchant-a", "lot-a2", "paid", "success", 6440, dashboardStart, dashboardStart+3600000, 0)
	dashboardOrder(t, s, "pending", "merchant-a", "lot-a3", "pending_payment", "init", 3000, dashboardStart, 0, dashboardNow+600000)
	dashboardOrder(t, s, "failed", "merchant-a", "lot-a3", "pending_payment", "failed", 200, dashboardStart, 0, dashboardNow+600001)
	dashboardOrder(t, s, "cancelled", "merchant-a", "lot-a3", "cancelled", "closed", 9000, dashboardStart, 0, 0)
	dashboardOrder(t, s, "expired", "merchant-a", "lot-a3", "expired", "closed", 500, dashboardStart, 0, dashboardNow-1)
	dashboardOrder(t, s, "expires-now", "merchant-a", "lot-a3", "pending_payment", "init", 700, dashboardStart, 0, dashboardNow)
	dashboardOrder(t, s, "created", "merchant-a", "lot-a3", "created", "init", 800, dashboardStart, 0, 0)
	dashboardOrder(t, s, "other", "merchant-b", "lot-b", "paid", "success", 999999, dashboardStart, dashboardStart, 0)
	dashboardOrder(t, s, "other-risk", "merchant-b", "lot-b", "pending_payment", "failed", 999999, dashboardStart, 0, dashboardNow+1)
	dashboardOrder(t, s, "before", "merchant-a", "lot-a1", "paid", "success", 99999, dashboardStart-1, dashboardStart-1, 0)
	dashboardOrder(t, s, "after", "merchant-a", "lot-a1", "paid", "success", 99999, dashboardNow+1, dashboardNow+1, 0)
	dashboardInsert(t, s, &UserOrderModel{ID: "shop", Source: "shop", SourceOrderID: "shop", MainAccountID: "merchant-a", Status: "paid", PaymentStatus: "success", TotalAmount: 999999, CreatedAtUnixMs: dashboardStart, PaidAtUnixMs: dashboardStart})
	dashboardInsert(t, s, &[]AuctionBidModel{{ID: "b1", MainAccountID: "merchant-a", LotID: "lot-a1", UserID: "u1", IdempotencyKey: "1", CreatedAtUnixMs: dashboardStart}, {ID: "b2", MainAccountID: "merchant-a", LotID: "lot-a2", UserID: "u1", IdempotencyKey: "2", CreatedAtUnixMs: dashboardStart}, {ID: "b3", MainAccountID: "merchant-a", LotID: "lot-a2", UserID: "u2", IdempotencyKey: "3", CreatedAtUnixMs: dashboardStart + 3600000}, {ID: "b4", MainAccountID: "merchant-b", LotID: "lot-b", UserID: "foreign", CreatedAtUnixMs: dashboardStart}})
	got, err := s.GetDashboard(context.Background(), dashboardQuery())
	if err != nil {
		t.Fatal(err)
	}
	want := auction.DashboardSummary{GMVAmount: 12880, PaidAmount: 12880, PendingAmount: 3200, OrderCount: 6, CancelledOrderCount: 1, ParticipantCount: 2, AverageDealAmount: 6440, PaymentRate: auction.DashboardRatio{Numerator: 1, Denominator: 6}, DealRate: auction.DashboardRatio{Numerator: 2, Denominator: 3}}
	if got.Summary != want {
		t.Fatalf("summary = %#v; want %#v", got.Summary, want)
	}
	if got.OrderRisks != (auction.DashboardOrderRisks{PendingPaymentCount: 2, ExpiringSoonCount: 1, ExpiredCount: 2, PaymentFailedCount: 1}) {
		t.Fatalf("risks = %#v", got.OrderRisks)
	}
	if got.Currency != "CNY" || got.Range.StartUnixMs != dashboardStart || len(got.PartialErrors) != 0 {
		t.Fatalf("metadata = %#v", got)
	}
	if got.Funnel != (auction.DashboardFunnel{CreatedLotCount: 3, ListedLotCount: 3, StartedLotCount: 3, BidLotCount: 2, SettledLotCount: 2, PaidLotCount: 2}) {
		t.Fatalf("funnel = %#v", got.Funnel)
	}
	if len(got.TopLots) != 2 || got.TopLots[0].LotID != "lot-a1" || got.TopLots[1].LotID != "lot-a2" || got.TopLots[1].ParticipantCount != 2 {
		t.Fatalf("top = %#v", got.TopLots)
	}
	wantCategories := []auction.DashboardCategoryPerformance{{Category: "未分类", PaidAmount: 6440, StartedLotCount: 1, SettledLotCount: 1, DealRate: auction.DashboardRatio{Numerator: 1, Denominator: 1}}, {Category: "瓷器", PaidAmount: 6440, StartedLotCount: 2, SettledLotCount: 1, DealRate: auction.DashboardRatio{Numerator: 1, Denominator: 2}}}
	if !reflect.DeepEqual(got.Categories, wantCategories) {
		t.Fatalf("categories = %#v", got.Categories)
	}
	if len(got.Trend) != 24 || got.Trend[0].PaidAmount != 6440 || got.Trend[0].Bidders != 1 || got.Trend[1].PaidOrders != 1 || got.Trend[1].Bidders != 1 || got.Trend[23].PaidAmount != 0 {
		t.Fatalf("trend = %#v", got.Trend)
	}
	if len(got.LotPerformance) != 3 || got.LotPerformance[1].ConversionRate != (auction.DashboardRatio{Numerator: 1, Denominator: 2}) {
		t.Fatalf("lots = %#v", got.LotPerformance)
	}
}

func TestDashboardAggregatesPast500RowsWhileBoundingStableTopAndDetails(t *testing.T) {
	s := dashboardTestStore(t)
	for i := 0; i < 600; i++ {
		id := fmt.Sprintf("lot-%03d", i)
		lot := dashboardLot(id, "merchant-a", "room-a", "", dashboardStart, dashboardStart+1)
		dashboardInsert(t, s, &lot)
		dashboardOrder(t, s, id, "merchant-a", id, "paid", "success", 10, dashboardStart, dashboardStart, 0)
	}
	got, err := s.GetDashboard(context.Background(), dashboardQuery())
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.GMVAmount != 6000 || got.Summary.OrderCount != 600 || got.Summary.PaymentRate != (auction.DashboardRatio{Numerator: 600, Denominator: 600}) {
		t.Fatalf("summary = %#v", got.Summary)
	}
	if len(got.TopLots) != 10 || got.TopLots[0].LotID != "lot-000" || got.TopLots[9].LotID != "lot-009" || len(got.LotPerformance) != 100 {
		t.Fatalf("top/detail lengths = %d/%d", len(got.TopLots), len(got.LotPerformance))
	}
	if len(got.Categories) != 1 || got.Categories[0].PaidAmount != 6000 || got.Categories[0].StartedLotCount != 600 {
		t.Fatalf("categories = %#v", got.Categories)
	}
}

func TestDashboardCategoryLimitReturnsExplicitPartialFailureWithoutSubset(t *testing.T) {
	s := dashboardTestStore(t)
	rows := make([]AuctionLotModel, 1000)
	for i := range rows {
		id := fmt.Sprintf("category-%04d", i)
		rows[i] = dashboardLot(id, "merchant-a", "room-a", id, dashboardStart, dashboardStart+1)
	}
	if err := s.db.CreateInBatches(rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	q := dashboardQuery()
	q.Sections = []auction.DashboardSection{auction.DashboardSectionCategories}
	got, err := s.GetDashboard(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Categories) != 1000 || len(got.PartialErrors) != 0 {
		t.Fatalf("valid maximum categories=%d errors=%v", len(got.Categories), got.PartialErrors)
	}
	overflow := dashboardLot("overflow", "merchant-a", "room-a", "overflow", dashboardStart, 0)
	dashboardInsert(t, s, &overflow)
	got, err = s.GetDashboard(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Categories) != 0 || len(got.PartialErrors) != 1 || got.PartialErrors[0].Section != auction.DashboardSectionCategories || got.PartialErrors[0].Code != "DATA_INCOMPLETE" {
		t.Fatalf("overflow must omit whole category panel with safe error: categories=%d errors=%v", len(got.Categories), got.PartialErrors)
	}
	if got.Summary.DealRate.Denominator != 1001 {
		t.Fatalf("core summary lost rows: %+v", got.Summary)
	}
}

func TestDashboardLiveUsesEarliestActiveRoomOfCurrentMerchant(t *testing.T) {
	s := dashboardTestStore(t)
	dashboardInsert(t, s, &[]AuctionRoomModel{{ID: "a1", MainAccountID: "merchant-a", LiveStartedAtUnixMs: dashboardStart + 200}, {ID: "a2", MainAccountID: "merchant-a", LiveStartedAtUnixMs: dashboardStart - 300}, {ID: "inactive", MainAccountID: "merchant-a", Status: "INACTIVE", LiveStartedAtUnixMs: dashboardStart - 999}, {ID: "zero", MainAccountID: "merchant-a"}, {ID: "foreign", MainAccountID: "merchant-b", LiveStartedAtUnixMs: 1}})
	q := dashboardQuery()
	q.Range = auction.DashboardRangeLive
	got, err := s.GetDashboard(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Range.StartUnixMs != dashboardStart-300 {
		t.Fatalf("live start = %d", got.Range.StartUnixMs)
	}
	if err := s.db.Model(&AuctionRoomModel{}).Where("main_account_id = ?", "merchant-a").Update("status", "INACTIVE").Error; err != nil {
		t.Fatal(err)
	}
	got, err = s.GetDashboard(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Range.StartUnixMs != dashboardStart {
		t.Fatalf("fallback = %d", got.Range.StartUnixMs)
	}
}

func TestDashboardEmptyDenominatorsAndNaturalDayBuckets(t *testing.T) {
	s := dashboardTestStore(t)
	for _, tc := range []struct {
		r     auction.DashboardRange
		n     int
		start int64
	}{{auction.DashboardRangeToday, 24, dashboardStart}, {auction.DashboardRange7D, 7, 1788105600000}, {auction.DashboardRange30D, 30, 1786118400000}} {
		q := dashboardQuery()
		q.Range = tc.r
		got, err := s.GetDashboard(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if got.Summary != (auction.DashboardSummary{}) || len(got.Trend) != tc.n || got.Trend[0].StartUnixMs != tc.start {
			t.Fatalf("range %s: %#v", tc.r, got)
		}
	}
}

func TestDashboardPanelFailureIsPartialButCoreFailureAborts(t *testing.T) {
	s := dashboardTestStore(t)
	if err := s.db.Migrator().DropTable(&UserOrderItemModel{}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDashboard(context.Background(), dashboardQuery())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PartialErrors) == 0 {
		t.Fatal("missing panel errors")
	}
	for _, e := range got.PartialErrors {
		if e.Code != "DATA_INCOMPLETE" || e.Section == auction.DashboardSectionSummary {
			t.Fatalf("partial = %#v", e)
		}
	}
	q := dashboardQuery()
	q.Sections = []auction.DashboardSection{auction.DashboardSectionSummary}
	got, err = s.GetDashboard(context.Background(), q)
	if err != nil || len(got.PartialErrors) != 0 {
		t.Fatalf("unrequested panel affected summary: %#v %v", got, err)
	}
	if err := s.db.Migrator().DropTable(&UserOrderModel{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDashboard(context.Background(), q); err == nil {
		t.Fatal("core failure returned success")
	}
}

func TestStoreAllowsMultipleRoomsPerMainAccountAfterLegacyIndexMigration(t *testing.T) {
	s := dashboardTestStore(t)
	if !s.db.Migrator().HasIndex(&AuctionRoomModel{}, "uidx_room_main_account") {
		if err := s.db.Exec("CREATE UNIQUE INDEX uidx_room_main_account ON auction_rooms(main_account_id)").Error; err != nil {
			t.Fatal(err)
		}
	}
	dashboardInsert(t, s, &AuctionRoomModel{ID: "room-1", MainAccountID: "merchant-a"})
	if err := s.migrateAuctionRooms(context.Background()); err != nil {
		t.Fatal(err)
	}
	dashboardInsert(t, s, &AuctionRoomModel{ID: "room-2", MainAccountID: "merchant-a"})
	if !s.db.Migrator().HasIndex(&AuctionRoomModel{}, "idx_room_main_status") {
		t.Fatal("tenant/status index lost")
	}
	var count int64
	if err := s.db.Model(&AuctionRoomModel{}).Where("main_account_id = ?", "merchant-a").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("rooms = %d", count)
	}
}

func TestStoreAllowsMultipleRoomsPerMainAccountWhenLegacyIndexIsAbsent(t *testing.T) {
	s := dashboardTestStore(t)
	for i := 0; i < 2; i++ {
		if err := s.migrateAuctionRooms(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	dashboardInsert(t, s, &[]AuctionRoomModel{{ID: "a1", MainAccountID: "merchant-a"}, {ID: "a2", MainAccountID: "merchant-a"}})
	if s.db.Migrator().HasIndex(&AuctionRoomModel{}, "uidx_room_main_account") {
		t.Fatal("startup recreated legacy uniqueness")
	}
}

func TestDashboardDealRateExcludesOldStartCohortAndFutureSettlements(t *testing.T) {
	s := dashboardTestStore(t)
	dashboardInsert(t, s, &[]AuctionLotModel{dashboardLot("old", "merchant-a", "a", "", dashboardStart-1, dashboardStart), dashboardLot("future", "merchant-a", "b", "", dashboardStart, dashboardNow+1), dashboardLot("now", "merchant-a", "b", "", dashboardStart, dashboardNow)})
	got, err := s.GetDashboard(context.Background(), dashboardQuery())
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.DealRate != (auction.DashboardRatio{Numerator: 0, Denominator: 2}) || got.Funnel.SettledLotCount != 0 {
		t.Fatalf("summary/funnel = %#v %#v", got.Summary, got.Funnel)
	}
	if len(got.Categories) != 1 || got.Categories[0].DealRate != (auction.DashboardRatio{Numerator: 0, Denominator: 2}) {
		t.Fatalf("categories = %#v", got.Categories)
	}
}

func TestDashboardListedFunnelUsesCurrentQueueStateOfCreationCohort(t *testing.T) {
	s := dashboardTestStore(t)
	draft := dashboardLot("draft", "merchant-a", "room-a", "", 0, 0)
	draft.Status = int32(v1.LotStatus_LOT_STATUS_DRAFT)
	draft.QueueStatus = int32(v1.LotQueueStatus_LOT_QUEUE_STATUS_NONE)
	queued := dashboardLot("queued", "merchant-a", "room-a", "", 0, 0)
	queued.Status = int32(v1.LotStatus_LOT_STATUS_READY)
	old := dashboardLot("old-created", "merchant-a", "room-b", "", dashboardStart, 0)
	old.CreatedAt = time.UnixMilli(dashboardStart - 1)
	dashboardInsert(t, s, &[]AuctionLotModel{draft, queued, old, dashboardLot("started", "merchant-a", "room-b", "", dashboardStart, 0)})
	got, err := s.GetDashboard(context.Background(), dashboardQuery())
	if err != nil {
		t.Fatal(err)
	}
	if got.Funnel.CreatedLotCount != 3 || got.Funnel.ListedLotCount != 2 || got.Funnel.StartedLotCount != 2 {
		t.Fatalf("current-state funnel = %#v", got.Funnel)
	}
}

func TestDashboardPaymentCohortExcludesRefundsAndUnsuccessfulPaidOrdersFromNumerator(t *testing.T) {
	s := dashboardTestStore(t)
	dashboardOrder(t, s, "paid", "merchant-a", "lot", "paid", "success", 100, dashboardStart, dashboardNow, 0)
	dashboardOrder(t, s, "bad-paid", "merchant-a", "lot", "paid", "failed", 200, dashboardStart, dashboardNow, 0)
	dashboardOrder(t, s, "refunded", "merchant-a", "lot", "refunded", "success", 300, dashboardStart, dashboardStart, 0)
	dashboardOrder(t, s, "cancelled", "merchant-a", "lot", "cancelled", "closed", 400, dashboardStart, 0, 0)
	q := dashboardQuery()
	q.Sections = []auction.DashboardSection{auction.DashboardSectionSummary}
	got, err := s.GetDashboard(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.GMVAmount != 0 || got.Summary.PaymentRate != (auction.DashboardRatio{Numerator: 1, Denominator: 2}) || got.Summary.OrderCount != 3 || got.Summary.CancelledOrderCount != 1 {
		t.Fatalf("cohort = %#v", got.Summary)
	}
}

func TestDashboardEverySectionExcludesEndAndIncludesStart(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		end                                  int64
		summary                              auction.DashboardSummary
		funnel                               auction.DashboardFunnel
		risks                                auction.DashboardOrderRisks
		lotIDs                               []string
		categories                           []auction.DashboardCategoryPerformance
		trendPaid, trendOrders, trendBidders int64
	}{
		{name: "nonempty", end: dashboardNow,
			summary: auction.DashboardSummary{GMVAmount: 200, PaidAmount: 200, PendingAmount: 20, OrderCount: 4, ParticipantCount: 2, AverageDealAmount: 100, PaymentRate: auction.DashboardRatio{Numerator: 2, Denominator: 4}, DealRate: auction.DashboardRatio{Numerator: 2, Denominator: 2}},
			funnel:  auction.DashboardFunnel{CreatedLotCount: 2, ListedLotCount: 2, StartedLotCount: 2, BidLotCount: 2, SettledLotCount: 2, PaidLotCount: 2},
			risks:   auction.DashboardOrderRisks{PendingPaymentCount: 2, ExpiringSoonCount: 2, PaymentFailedCount: 2},
			lotIDs:  []string{"edge-1", "edge-2"}, categories: []auction.DashboardCategoryPerformance{{Category: "边界", PaidAmount: 200, StartedLotCount: 2, SettledLotCount: 2, DealRate: auction.DashboardRatio{Numerator: 2, Denominator: 2}}}, trendPaid: 200, trendOrders: 2, trendBidders: 2},
		{name: "start_equals_end", end: dashboardStart, lotIDs: []string{}, categories: []auction.DashboardCategoryPerformance{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dashboardTestStore(t)
			// Independent literal expectations above: only start and end-1 qualify.
			for i, at := range []int64{dashboardStart - 1, dashboardStart, tc.end - 1, tc.end, tc.end + 1} {
				id := fmt.Sprintf("edge-%d", i)
				lot := dashboardLot(id, "merchant-a", "room-a", "边界", at, at)
				lot.CreatedAt = time.UnixMilli(at)
				dashboardInsert(t, s, &lot)
				dashboardOrder(t, s, id, "merchant-a", id, "paid", "success", 100, at, at, 0)
				dashboardOrder(t, s, "pending-"+id, "merchant-a", id, "pending_payment", "failed", 10, at, 0, tc.end+600000)
				dashboardInsert(t, s, &AuctionBidModel{ID: id, MainAccountID: "merchant-a", LotID: id, UserID: id, IdempotencyKey: id, CreatedAtUnixMs: at})
			}
			q := dashboardQuery()
			q.NowUnixMs = tc.end
			got, err := s.GetDashboard(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.PartialErrors) != 0 {
				t.Fatalf("unexpected partial errors: %#v", got.PartialErrors)
			}
			if got.Range.StartUnixMs != dashboardStart || got.Range.EndUnixMs != tc.end {
				t.Fatalf("window = %#v", got.Range)
			}
			if got.Summary != tc.summary {
				t.Errorf("summary = %#v; want %#v", got.Summary, tc.summary)
			}
			if got.Funnel != tc.funnel {
				t.Errorf("funnel = %#v; want %#v", got.Funnel, tc.funnel)
			}
			if got.OrderRisks != tc.risks {
				t.Errorf("risks = %#v; want %#v", got.OrderRisks, tc.risks)
			}
			if !reflect.DeepEqual(got.Categories, tc.categories) {
				t.Errorf("categories = %#v; want %#v", got.Categories, tc.categories)
			}
			topIDs, detailIDs := []string{}, []string{}
			for _, lot := range got.TopLots {
				topIDs = append(topIDs, lot.LotID)
				if lot.PaidAmount != 100 || lot.BidCount != 1 || lot.ParticipantCount != 1 {
					t.Errorf("top lot metrics = %#v", lot)
				}
			}
			for _, lot := range got.LotPerformance {
				detailIDs = append(detailIDs, lot.LotID)
				if lot.PaidAmount != 100 || lot.BidCount != 1 || lot.ParticipantCount != 1 {
					t.Errorf("detail metrics = %#v", lot)
				}
			}
			if !reflect.DeepEqual(topIDs, tc.lotIDs) || !reflect.DeepEqual(detailIDs, tc.lotIDs) {
				t.Errorf("top/detail IDs = %v/%v; want %v", topIDs, detailIDs, tc.lotIDs)
			}
			var paid, orders, bidders int64
			for _, point := range got.Trend {
				paid += point.PaidAmount
				orders += point.PaidOrders
				bidders += point.Bidders
			}
			if len(got.Trend) != 24 || paid != tc.trendPaid || orders != tc.trendOrders || bidders != tc.trendBidders {
				t.Errorf("trend totals = %d/%d/%d; want %d/%d/%d", paid, orders, bidders, tc.trendPaid, tc.trendOrders, tc.trendBidders)
			}
		})
	}
}
