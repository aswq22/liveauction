package service

import (
	"context"
	"errors"
	"testing"

	v1 "live-auction-bid/backend/api/auction/service/v1"
	auctionbiz "live-auction-bid/backend/app/auction/service/internal/biz/auction"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"live-auction-bid/backend/app/auction/service/internal/pkg/auth"
)

type dashboardServiceRepository struct {
	auctionbiz.LotRepository
	calls    int
	query    auctionbiz.DashboardQuery
	snapshot auctionbiz.DashboardSnapshot
	err      error
}

func (r *dashboardServiceRepository) GetDashboard(_ context.Context, query auctionbiz.DashboardQuery) (auctionbiz.DashboardSnapshot, error) {
	r.calls++
	r.query = query
	return r.snapshot, r.err
}

func dashboardServiceClaims(permissions ...string) *auth.Claims {
	return &auth.Claims{UserID: "operator-a", MainAccountID: "merchant-a", PermissionCodes: permissions}
}

func TestGetAdminDashboardFailsClosedWithoutCurrentAccountVerification(t *testing.T) {
	repo := &dashboardServiceRepository{}
	service := NewAuctionService(auctionbiz.NewAuctionUsecase(repo, nil, nil, nil))
	ctx := auth.WithClaims(context.Background(), dashboardServiceClaims(userbiz.PermissionRealtimeView, userbiz.PermissionLotViewAdmin, userbiz.PermissionOrderManage))
	reply, err := service.GetAdminDashboard(ctx, &v1.GetAdminDashboardRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if reply.GetResult().GetCode() != ResultCodeInternalError || repo.calls != 0 {
		t.Fatalf("unconfigured verifier must fail closed: code=%d queries=%d", reply.GetResult().GetCode(), repo.calls)
	}
}

func TestGetAdminDashboardRejectsEachMissingPermissionBeforeRepositoryAccess(t *testing.T) {
	required := []string{userbiz.PermissionRealtimeView, userbiz.PermissionLotViewAdmin, userbiz.PermissionOrderManage}
	for _, missing := range required {
		t.Run(missing, func(t *testing.T) {
			repo := &dashboardServiceRepository{}
			service := NewAuctionService(auctionbiz.NewAuctionUsecase(repo, nil, nil, nil))
			permissions := make([]string, 0, len(required)-1)
			for _, permission := range required {
				if permission != missing {
					permissions = append(permissions, permission)
				}
			}
			current := currentDashboardAccount()
			current.PermissionCodes = permissions
			service.SetCurrentUserProvider(userbiz.NewUsecase(&currentAccountRepository{user: current}, nil))

			reply, err := service.GetAdminDashboard(auth.WithClaims(context.Background(), dashboardServiceClaims(permissions...)), &v1.GetAdminDashboardRequest{})
			if err != nil {
				t.Fatalf("GetAdminDashboard() transport error = %v", err)
			}
			if reply.GetResult().GetCode() != ResultCodeForbidden {
				t.Fatalf("GetAdminDashboard() code = %d, want forbidden", reply.GetResult().GetCode())
			}
			if repo.calls != 0 {
				t.Fatalf("repository calls = %d, want 0", repo.calls)
			}
		})
	}
}

func TestGetAdminDashboardMapsClaimScopedSnapshot(t *testing.T) {
	repo := &dashboardServiceRepository{snapshot: dashboardServiceSnapshot()}
	service := withCurrentAccount(NewAuctionService(auctionbiz.NewAuctionUsecase(repo, nil, nil, nil)))
	ctx := auth.WithClaims(context.Background(), dashboardServiceClaims(
		userbiz.PermissionRealtimeView,
		userbiz.PermissionLotViewAdmin,
		userbiz.PermissionOrderManage,
	))

	reply, err := service.GetAdminDashboard(ctx, &v1.GetAdminDashboardRequest{
		Range: "7d", Timezone: "America/New_York", Sections: []string{"summary", "trend", "topLots"},
	})
	if err != nil {
		t.Fatalf("GetAdminDashboard() transport error = %v", err)
	}
	if reply.GetResult().GetCode() != ResultCodeOK {
		t.Fatalf("GetAdminDashboard() code = %d, want ok", reply.GetResult().GetCode())
	}
	if repo.calls != 1 || repo.query.MainAccountID != "merchant-a" {
		t.Fatalf("repository calls/query = %d/%#v, want one merchant-a query", repo.calls, repo.query)
	}
	if repo.query.Range != auctionbiz.DashboardRange7D || repo.query.Timezone != "Asia/Shanghai" || len(repo.query.Sections) != 3 {
		t.Fatalf("repository query = %#v", repo.query)
	}

	if reply.GetCurrency() != "CNY" || reply.GetSnapshotAtUnixMs() != 900 || reply.GetRange().GetRange() != "7d" || reply.GetRange().GetTimezone() != "Asia/Shanghai" || reply.GetRange().GetStartUnixMs() != 100 || reply.GetRange().GetEndUnixMs() != 800 {
		t.Fatalf("top-level mapping = %#v", reply)
	}
	if got := reply.GetSummary(); got.GetGmvAmount() != 101 || got.GetPaidAmount() != 102 || got.GetPendingAmount() != 103 || got.GetOrderCount() != 104 || got.GetCancelledOrderCount() != 105 || got.GetParticipantCount() != 106 || got.GetAverageDealAmount() != 107 || got.GetPaymentRate().GetNumerator() != 108 || got.GetPaymentRate().GetDenominator() != 109 || got.GetPaymentRate().GetRate() != 108.0/109.0 || got.GetDealRate().GetNumerator() != 110 || got.GetDealRate().GetDenominator() != 111 || got.GetDealRate().GetRate() != 110.0/111.0 {
		t.Fatalf("summary mapping = %#v", got)
	}
	if got := reply.GetFunnel(); got.GetCreatedLotCount() != 1 || got.GetListedLotCount() != 2 || got.GetStartedLotCount() != 3 || got.GetBidLotCount() != 4 || got.GetSettledLotCount() != 5 || got.GetPaidLotCount() != 6 {
		t.Fatalf("funnel mapping = %#v", got)
	}
	if got := reply.GetTrend()[0]; got.GetStartUnixMs() != 11 || got.GetEndUnixMs() != 12 || got.GetPaidAmount() != 13 || got.GetPaidOrders() != 14 || got.GetBidders() != 15 {
		t.Fatalf("trend mapping = %#v", got)
	}
	if got := reply.GetTopLots()[0]; got.GetLotId() != "lot-1" || got.GetTitle() != "Top" || got.GetCategory() != "jade" || got.GetPaidAmount() != 21 || got.GetFinalAmount() != 22 || got.GetBidCount() != 23 || got.GetParticipantCount() != 24 {
		t.Fatalf("top lot mapping = %#v", got)
	}
	if got := reply.GetCategories()[0]; got.GetCategory() != "jade" || got.GetPaidAmount() != 31 || got.GetStartedLotCount() != 32 || got.GetSettledLotCount() != 33 || got.GetDealRate().GetNumerator() != 34 || got.GetDealRate().GetDenominator() != 35 || got.GetDealRate().GetRate() != 34.0/35.0 {
		t.Fatalf("category mapping = %#v", got)
	}
	if got := reply.GetOrderRisks(); got.GetPendingPaymentCount() != 41 || got.GetExpiringSoonCount() != 42 || got.GetExpiredCount() != 43 || got.GetPaymentFailedCount() != 44 {
		t.Fatalf("risk mapping = %#v", got)
	}
	if got := reply.GetLotPerformance()[0]; got.GetLotId() != "lot-2" || got.GetTitle() != "Performance" || got.GetCategory() != "tea" || got.GetStatus() != "SETTLED" || got.GetBidCount() != 51 || got.GetParticipantCount() != 52 || got.GetFinalAmount() != 53 || got.GetPaidAmount() != 54 || got.GetConversionRate().GetNumerator() != 55 || got.GetConversionRate().GetDenominator() != 56 || got.GetConversionRate().GetRate() != 55.0/56.0 {
		t.Fatalf("lot performance mapping = %#v", got)
	}
	if got := reply.GetPartialErrors()[0]; got.GetSection() != "trend" || got.GetCode() != "QUERY_TIMEOUT" || got.GetMessage() != "trend source timed out" {
		t.Fatalf("partial error mapping = %#v", got)
	}
}

func TestGetAdminDashboardReturnsBusinessErrorEnvelope(t *testing.T) {
	repo := &dashboardServiceRepository{err: errors.New("dashboard storage unavailable")}
	service := withCurrentAccount(NewAuctionService(auctionbiz.NewAuctionUsecase(repo, nil, nil, nil)))
	ctx := auth.WithClaims(context.Background(), dashboardServiceClaims(userbiz.PermissionRealtimeView, userbiz.PermissionLotViewAdmin, userbiz.PermissionOrderManage))

	reply, err := service.GetAdminDashboard(ctx, &v1.GetAdminDashboardRequest{})
	if err != nil {
		t.Fatalf("GetAdminDashboard() transport error = %v", err)
	}
	if reply.GetResult().GetCode() != ResultCodeInternalError {
		t.Fatalf("GetAdminDashboard() code = %d, want internal error", reply.GetResult().GetCode())
	}
}

func dashboardServiceSnapshot() auctionbiz.DashboardSnapshot {
	return auctionbiz.DashboardSnapshot{
		SnapshotAtUnixMs: 900, Currency: "CNY", Range: auctionbiz.DashboardRangeWindow{Range: auctionbiz.DashboardRange7D, Timezone: "Asia/Shanghai", StartUnixMs: 100, EndUnixMs: 800},
		Summary:        auctionbiz.DashboardSummary{GMVAmount: 101, PaidAmount: 102, PendingAmount: 103, OrderCount: 104, CancelledOrderCount: 105, ParticipantCount: 106, AverageDealAmount: 107, PaymentRate: auctionbiz.DashboardRatio{Numerator: 108, Denominator: 109}, DealRate: auctionbiz.DashboardRatio{Numerator: 110, Denominator: 111}},
		Funnel:         auctionbiz.DashboardFunnel{CreatedLotCount: 1, ListedLotCount: 2, StartedLotCount: 3, BidLotCount: 4, SettledLotCount: 5, PaidLotCount: 6},
		Trend:          []auctionbiz.DashboardTrendPoint{{StartUnixMs: 11, EndUnixMs: 12, PaidAmount: 13, PaidOrders: 14, Bidders: 15}},
		TopLots:        []auctionbiz.DashboardTopLot{{LotID: "lot-1", Title: "Top", Category: "jade", PaidAmount: 21, FinalAmount: 22, BidCount: 23, ParticipantCount: 24}},
		Categories:     []auctionbiz.DashboardCategoryPerformance{{Category: "jade", PaidAmount: 31, StartedLotCount: 32, SettledLotCount: 33, DealRate: auctionbiz.DashboardRatio{Numerator: 34, Denominator: 35}}},
		OrderRisks:     auctionbiz.DashboardOrderRisks{PendingPaymentCount: 41, ExpiringSoonCount: 42, ExpiredCount: 43, PaymentFailedCount: 44},
		LotPerformance: []auctionbiz.DashboardLotPerformance{{LotID: "lot-2", Title: "Performance", Category: "tea", Status: "SETTLED", BidCount: 51, ParticipantCount: 52, FinalAmount: 53, PaidAmount: 54, ConversionRate: auctionbiz.DashboardRatio{Numerator: 55, Denominator: 56}}},
		PartialErrors:  []auctionbiz.DashboardPartialError{{Section: auctionbiz.DashboardSectionTrend, Code: "QUERY_TIMEOUT", Message: "trend source timed out"}},
	}
}
