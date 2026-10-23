package auction

import (
	"context"
	"errors"
	"testing"
	"time"

	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

func TestNormalizeDashboardQueryTodayUsesShanghaiMidnight(t *testing.T) {
	now := time.Date(2026, 9, 6, 14, 30, 0, 0, time.FixedZone("input", 8*60*60))

	got, err := NormalizeDashboardQuery(DashboardQuery{Range: DashboardRangeToday, NowUnixMs: now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	wantStart := time.Date(2026, 9, 6, 0, 0, 0, 0, time.FixedZone("CST", 8*60*60)).UnixMilli()
	if got.Timezone != "Asia/Shanghai" || got.StartUnixMs != wantStart || got.EndUnixMs != now.UnixMilli() {
		t.Fatalf("normalized range = %#v", got)
	}
}

func TestNormalizeDashboardQuerySevenDaysIncludesTodayAndSixPriorShanghaiDays(t *testing.T) {
	now := time.Date(2026, 3, 1, 1, 15, 0, 0, time.FixedZone("input", 8*60*60))

	got, err := NormalizeDashboardQuery(DashboardQuery{Range: DashboardRange7D, NowUnixMs: now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	wantStart := time.Date(2026, 2, 23, 0, 0, 0, 0, time.FixedZone("CST", 8*60*60)).UnixMilli()
	if got.StartUnixMs != wantStart || got.EndUnixMs != now.UnixMilli() {
		t.Fatalf("normalized seven day range = %#v, want start=%d end=%d", got, wantStart, now.UnixMilli())
	}
}

func TestNormalizeDashboardQueryThirtyDaysIncludesTodayAndTwentyNinePriorShanghaiDays(t *testing.T) {
	now := time.Date(2026, 3, 1, 1, 15, 0, 0, time.FixedZone("input", 8*60*60))

	got, err := NormalizeDashboardQuery(DashboardQuery{Range: DashboardRange30D, NowUnixMs: now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	wantStart := time.Date(2026, 1, 31, 0, 0, 0, 0, time.FixedZone("CST", 8*60*60)).UnixMilli()
	if got.StartUnixMs != wantStart || got.EndUnixMs != now.UnixMilli() {
		t.Fatalf("normalized thirty day range = %#v, want start=%d end=%d", got, wantStart, now.UnixMilli())
	}
}

func TestNormalizeDashboardQueryAtShanghaiMidnightStartsAtNow(t *testing.T) {
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.FixedZone("input", 8*60*60))

	got, err := NormalizeDashboardQuery(DashboardQuery{Range: DashboardRangeToday, NowUnixMs: now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if got.StartUnixMs != now.UnixMilli() || got.EndUnixMs != now.UnixMilli() {
		t.Fatalf("midnight range = %#v, want empty [now, now)", got)
	}
}

func TestNormalizeDashboardQueryDefaultsRangeAndSections(t *testing.T) {
	now := time.Date(2026, 9, 6, 14, 30, 0, 0, time.FixedZone("input", 8*60*60))

	got, err := NormalizeDashboardQuery(DashboardQuery{NowUnixMs: now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	want := []DashboardSection{
		DashboardSectionSummary,
		DashboardSectionFunnel,
		DashboardSectionTrend,
		DashboardSectionTopLots,
		DashboardSectionCategories,
		DashboardSectionOrderRisks,
		DashboardSectionLotPerformance,
	}
	if got.Range != DashboardRangeToday || len(got.Sections) != len(want) {
		t.Fatalf("default query = %#v", got)
	}
	for i, section := range want {
		if got.Sections[i] != section {
			t.Fatalf("default section %d = %q, want %q", i, got.Sections[i], section)
		}
	}
}

func TestNormalizeDashboardQueryDeduplicatesSectionsWithoutReordering(t *testing.T) {
	now := time.Date(2026, 9, 6, 14, 30, 0, 0, time.FixedZone("input", 8*60*60))
	got, err := NormalizeDashboardQuery(DashboardQuery{
		NowUnixMs: now.UnixMilli(),
		Sections: []DashboardSection{
			DashboardSectionTrend,
			DashboardSectionSummary,
			DashboardSectionTrend,
			DashboardSectionSummary,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sections) != 2 || got.Sections[0] != DashboardSectionTrend || got.Sections[1] != DashboardSectionSummary {
		t.Fatalf("deduplicated sections = %#v", got.Sections)
	}
}

func TestNormalizeDashboardQueryLeavesLiveStartForRepository(t *testing.T) {
	now := time.Date(2026, 9, 6, 14, 30, 0, 0, time.FixedZone("input", 8*60*60))

	got, err := NormalizeDashboardQuery(DashboardQuery{Range: DashboardRangeLive, NowUnixMs: now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if got.StartUnixMs != 0 || got.EndUnixMs != now.UnixMilli() {
		t.Fatalf("live range = %#v, want unresolved start and now end", got)
	}
}

func TestNormalizeDashboardQueryRejectsUnknownRangeAndSection(t *testing.T) {
	tests := []DashboardQuery{
		{Range: DashboardRange("quarter")},
		{Sections: []DashboardSection{"unknown"}},
	}
	for _, query := range tests {
		_, err := NormalizeDashboardQuery(query)
		if !errors.Is(err, apperr.ErrInvalidArgument) {
			t.Fatalf("NormalizeDashboardQuery(%#v) error = %v, want invalid argument", query, err)
		}
	}
}

func TestDashboardSnapshotCarriesCurrencyAndSnapshotTimestamp(t *testing.T) {
	snapshot := DashboardSnapshot{
		SnapshotAtUnixMs: 1_788_244_200_123,
		Currency:         "CNY",
	}
	if snapshot.SnapshotAtUnixMs != 1_788_244_200_123 || snapshot.Currency != "CNY" {
		t.Fatalf("snapshot metadata = %#v", snapshot)
	}
}

func TestDashboardStatisticsExposeConfirmedAggregationFields(t *testing.T) {
	summary := DashboardSummary{
		GMVAmount:           128_800,
		PendingAmount:       19_900,
		OrderCount:          12,
		CancelledOrderCount: 2,
		ParticipantCount:    8,
		AverageDealAmount:   10_733,
	}
	funnel := DashboardFunnel{
		CreatedLotCount: 10,
		ListedLotCount:  8,
		StartedLotCount: 7,
		BidLotCount:     6,
		SettledLotCount: 5,
		PaidLotCount:    4,
	}
	category := DashboardCategoryPerformance{StartedLotCount: 7, SettledLotCount: 5}
	risks := DashboardOrderRisks{PaymentFailedCount: 3}

	if summary.GMVAmount != 128_800 || summary.PendingAmount != 19_900 || summary.OrderCount != 12 || summary.CancelledOrderCount != 2 || summary.ParticipantCount != 8 || summary.AverageDealAmount != 10_733 {
		t.Fatalf("summary = %#v", summary)
	}
	if funnel.CreatedLotCount != 10 || funnel.ListedLotCount != 8 || funnel.StartedLotCount != 7 || funnel.BidLotCount != 6 || funnel.SettledLotCount != 5 || funnel.PaidLotCount != 4 {
		t.Fatalf("funnel = %#v", funnel)
	}
	if category.StartedLotCount != 7 || category.SettledLotCount != 5 || risks.PaymentFailedCount != 3 {
		t.Fatalf("category = %#v, risks = %#v", category, risks)
	}
}

type dashboardRepositoryStub struct {
	LotRepository
	got      DashboardQuery
	snapshot DashboardSnapshot
	err      error
}

func (r *dashboardRepositoryStub) GetDashboard(_ context.Context, query DashboardQuery) (DashboardSnapshot, error) {
	r.got = query
	return r.snapshot, r.err
}

func TestAuctionUsecaseGetDashboardAttachesRepositoryAndDelegatesNormalizedQuery(t *testing.T) {
	now := time.Date(2026, 9, 6, 14, 30, 0, 0, time.FixedZone("input", 8*60*60))
	repo := &dashboardRepositoryStub{snapshot: DashboardSnapshot{}}
	uc := NewAuctionUsecase(repo, nil, nil, nil)

	if _, err := uc.GetDashboard(context.Background(), DashboardQuery{
		Range:     DashboardRangeToday,
		Timezone:  "America/New_York",
		NowUnixMs: now.UnixMilli(),
	}); err != nil {
		t.Fatalf("GetDashboard() error = %v", err)
	}
	wantStart := time.Date(2026, 9, 6, 0, 0, 0, 0, time.FixedZone("CST", 8*60*60)).UnixMilli()
	if repo.got.Timezone != "Asia/Shanghai" || repo.got.StartUnixMs != wantStart || repo.got.EndUnixMs != now.UnixMilli() {
		t.Fatalf("repository query = %#v", repo.got)
	}
}

func TestAuctionUsecaseGetDashboardRejectsMissingDashboardRepository(t *testing.T) {
	_, err := (&AuctionUsecase{}).GetDashboard(context.Background(), DashboardQuery{})
	if err == nil || err.Error() != "dashboard repository is required" {
		t.Fatalf("GetDashboard() error = %v, want dashboard repository is required", err)
	}
}

func TestAuctionUsecaseGetDashboardPreservesRepositoryError(t *testing.T) {
	wantErr := errors.New("dashboard storage unavailable")
	repo := &dashboardRepositoryStub{err: wantErr}
	uc := NewAuctionUsecase(repo, nil, nil, nil)

	_, err := uc.GetDashboard(context.Background(), DashboardQuery{NowUnixMs: 1})
	if !errors.Is(err, wantErr) {
		t.Fatalf("GetDashboard() error = %v, want repository sentinel %v", err, wantErr)
	}
}
