package service

import (
	"context"
	"errors"

	v1 "live-auction-bid/backend/api/auction/service/v1"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
	"live-auction-bid/backend/app/auction/service/internal/pkg/auth"
)

// CurrentUserProvider reads the authenticated user's current account state.
type CurrentUserProvider interface {
	GetMe(context.Context) (*v1.User, error)
}

func (s *AuctionService) SetCurrentUserProvider(provider CurrentUserProvider) *AuctionService {
	s.currentUser = provider
	return s
}

func (s *AuctionService) requireCurrentQueryMainAccount(ctx context.Context, permissions ...string) (string, error) {
	claims, err := auth.RequireUser(ctx)
	if err != nil {
		return "", err
	}
	if s.currentUser == nil {
		return "", errors.New("current account verifier is required")
	}
	user, err := s.currentUser.GetMe(ctx)
	if err != nil {
		return "", err
	}
	if user == nil || user.GetId() != claims.UserID {
		return "", apperr.ErrPermissionDenied
	}
	if user.GetStatus() != v1.UserStatus_USER_STATUS_ACTIVE {
		return "", apperr.ErrAccountDisabled
	}
	current := &auth.Claims{UserID: user.GetId(), MainAccountID: user.GetMainAccountId(), RoleCodes: user.GetRoleCodes()}
	tenant := auth.EffectiveMainAccountID(current)
	if tenant == "" || tenant != auth.EffectiveMainAccountID(claims) {
		return "", apperr.ErrPermissionDenied
	}
	for _, permission := range permissions {
		if !userbiz.UserHasPermission(user, permission) {
			return "", apperr.ErrPermissionDenied
		}
	}
	return tenant, nil
}

// GetAdminLot provides exact-ID price lookup under current permission and tenant scope.
func (s *AuctionService) GetAdminLot(ctx context.Context, req *v1.GetLotRequest) (*v1.GetLotReply, error) {
	mainAccountID, err := s.requireCurrentQueryMainAccount(ctx, userbiz.PermissionLotViewAdmin)
	if err != nil {
		return &v1.GetLotReply{Result: ErrorResult(ctx, err)}, nil
	}
	lot, err := s.auction.GetLot(ctx, req.GetLotId())
	if err != nil {
		return &v1.GetLotReply{Result: ErrorResult(ctx, err)}, nil
	}
	if lot.GetMainAccountId() != mainAccountID {
		return &v1.GetLotReply{Result: ErrorResult(ctx, apperr.ErrPermissionDenied)}, nil
	}
	return &v1.GetLotReply{Result: okResult(ctx), Lot: lot}, nil
}
