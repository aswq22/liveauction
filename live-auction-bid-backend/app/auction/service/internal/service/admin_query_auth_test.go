package service

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	kratoshttp "github.com/go-kratos/kratos/v2/transport/http"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	auctionbiz "live-auction-bid/backend/app/auction/service/internal/biz/auction"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
	"live-auction-bid/backend/app/auction/service/internal/pkg/auth"
)

// Storage is the only fake: token issuance, parsing, current-user usecase, service
// authorization, generated HTTP bindings and response envelopes all run for real.
type currentAccountRepository struct {
	userbiz.Repository
	user *v1.User
	err  error
}

func (r *currentAccountRepository) FindUserByID(_ context.Context, id string) (*v1.User, string, error) {
	if r.err != nil {
		return nil, "", r.err
	}
	if r.user == nil || r.user.GetId() != id {
		return nil, "", apperr.ErrUserNotFound
	}
	return proto.Clone(r.user).(*v1.User), "", nil
}

func currentDashboardAccount() *v1.User {
	return &v1.User{Id: "operator-a", Username: "operator", MainAccountId: "merchant-a", Status: v1.UserStatus_USER_STATUS_ACTIVE, RoleCodes: []string{"operator"}, PermissionCodes: []string{userbiz.PermissionRealtimeView, userbiz.PermissionLotViewAdmin, userbiz.PermissionOrderManage}}
}

func withCurrentAccount(s *AuctionService) *AuctionService {
	return s.SetCurrentUserProvider(userbiz.NewUsecase(&currentAccountRepository{user: currentDashboardAccount()}, nil))
}

func TestAdminDashboardRechecksAlreadyIssuedTokenAfterAccountChange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*currentAccountRepository)
		code   int32
	}{
		{"revoke realtime", func(r *currentAccountRepository) {
			r.user.PermissionCodes = []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionOrderManage}
		}, ResultCodeForbidden},
		{"revoke lot", func(r *currentAccountRepository) {
			r.user.PermissionCodes = []string{userbiz.PermissionRealtimeView, userbiz.PermissionOrderManage}
		}, ResultCodeForbidden},
		{"revoke order", func(r *currentAccountRepository) {
			r.user.PermissionCodes = []string{userbiz.PermissionLotViewAdmin, userbiz.PermissionRealtimeView}
		}, ResultCodeForbidden},
		{"disable", func(r *currentAccountRepository) { r.user.Status = v1.UserStatus_USER_STATUS_DISABLED }, ResultCodeAccountDisabled},
		{"move tenant", func(r *currentAccountRepository) { r.user.MainAccountId = "merchant-b" }, ResultCodeForbidden},
		{"remove tenant", func(r *currentAccountRepository) { r.user.MainAccountId = "" }, ResultCodeForbidden},
		{"unavailable", func(r *currentAccountRepository) { r.err = errors.New("private database error") }, ResultCodeInternalError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := &currentAccountRepository{user: currentDashboardAccount()}
			manager, err := auth.NewManager(auth.Config{Secret: "test-only-signing-key"})
			if err != nil {
				t.Fatal(err)
			}
			tokens, err := manager.IssueTokenPair(accounts.user)
			if err != nil {
				t.Fatal(err)
			}
			repo := &dashboardServiceRepository{snapshot: dashboardServiceSnapshot()}
			svc := NewAuctionService(auctionbiz.NewAuctionUsecase(repo, nil, nil, nil)).SetCurrentUserProvider(userbiz.NewUsecase(accounts, manager))
			srv := kratoshttp.NewServer(kratoshttp.Middleware(manager.Middleware()))
			v1.RegisterAuctionServiceHTTPServer(srv, svc)
			query := func() *v1.GetAdminDashboardReply {
				req := httptest.NewRequest("GET", "/api/admin/dashboard?mainAccountId=merchant-b", nil)
				req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
				recorder := httptest.NewRecorder()
				srv.ServeHTTP(recorder, req)
				if strings.Contains(recorder.Body.String(), "private") {
					t.Fatalf("unsafe reply: %s", recorder.Body)
				}
				out := &v1.GetAdminDashboardReply{}
				if err := protojson.Unmarshal(recorder.Body.Bytes(), out); err != nil {
					t.Fatal(err)
				}
				return out
			}
			if got := query(); got.GetResult().GetCode() != ResultCodeOK || repo.query.MainAccountID != "merchant-a" {
				t.Fatalf("initial scoped query = %+v; %+v", got, repo.query)
			}
			tc.change(accounts)
			if got := query(); got.GetResult().GetCode() != tc.code || got.GetSummary() != nil || repo.calls != 1 {
				t.Fatalf("old token accepted after change: code=%d calls=%d", got.GetResult().GetCode(), repo.calls)
			}
		})
	}
}

type adminLotRepository struct {
	auctionbiz.LotRepository
	auctionbiz.RoomRepository
	lot   *v1.Lot
	calls int
}

func (r *adminLotRepository) FindByID(_ context.Context, id string) (*v1.Lot, error) {
	r.calls++
	if id != r.lot.GetId() {
		return nil, apperr.ErrNotFound
	}
	return proto.Clone(r.lot).(*v1.Lot), nil
}
func (r *adminLotRepository) FindRoomByID(_ context.Context, id string) (*auctionbiz.Room, bool, error) {
	return &auctionbiz.Room{ID: id, MainAccountID: r.lot.GetMainAccountId(), Status: auctionbiz.RoomStatusActive}, true, nil
}

func TestAdminLotDetailHTTPRequiresCurrentPermissionAndSameTenant(t *testing.T) {
	for _, tc := range []struct {
		name, tenant      string
		permission, login bool
		code              int32
	}{
		{"authorized exact id", "merchant-a", true, true, ResultCodeOK},
		{"anonymous", "merchant-a", true, false, ResultCodeLoginRequired},
		{"revoked permission", "merchant-a", false, true, ResultCodeForbidden},
		{"foreign lot", "merchant-b", true, true, ResultCodeForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := &currentAccountRepository{user: currentDashboardAccount()}
			manager, err := auth.NewManager(auth.Config{Secret: "test-only-signing-key"})
			if err != nil {
				t.Fatal(err)
			}
			tokens, err := manager.IssueTokenPair(accounts.user)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.permission {
				accounts.user.PermissionCodes = []string{userbiz.PermissionRealtimeView}
			}
			repo := &adminLotRepository{lot: &v1.Lot{Id: "lot-exact", RoomId: "room-a", MainAccountId: tc.tenant, Title: "private foreign title", CurrentPrice: &v1.Money{Amount: 12800, Currency: "CNY"}}}
			svc := NewAuctionService(auctionbiz.NewAuctionUsecase(repo, nil, nil, nil)).SetCurrentUserProvider(userbiz.NewUsecase(accounts, manager))
			srv := kratoshttp.NewServer(kratoshttp.Middleware(manager.Middleware()))
			v1.RegisterAuctionServiceHTTPServer(srv, svc)
			req := httptest.NewRequest("GET", "/api/admin/lots/lot-exact?mainAccountId=merchant-b", nil)
			if tc.login {
				req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
			}
			recorder := httptest.NewRecorder()
			srv.ServeHTTP(recorder, req)
			if recorder.Code != 200 {
				t.Fatalf("admin detail route unavailable: status=%d body=%s", recorder.Code, recorder.Body)
			}
			out := &v1.GetLotReply{}
			if err := protojson.Unmarshal(recorder.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
			if out.GetResult().GetCode() != tc.code {
				t.Fatalf("code=%d want=%d", out.GetResult().GetCode(), tc.code)
			}
			if tc.code == ResultCodeOK {
				if out.GetLot().GetId() != "lot-exact" || out.GetLot().GetCurrentPrice().GetAmount() != 12800 {
					t.Fatalf("wrong exact result: %+v", out)
				}
			} else if out.GetLot() != nil || strings.Contains(recorder.Body.String(), "private") {
				t.Fatalf("forbidden lot leaked: %s", recorder.Body)
			}
			if (!tc.login || !tc.permission) && repo.calls != 0 {
				t.Fatalf("unauthorized lookup reached repository: %d", repo.calls)
			}
		})
	}
}
