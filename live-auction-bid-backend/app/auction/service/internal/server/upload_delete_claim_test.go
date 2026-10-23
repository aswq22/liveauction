package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	auctionv1 "live-auction-bid/backend/api/auction/service/v1"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"live-auction-bid/backend/app/auction/service/internal/data"
	"live-auction-bid/backend/app/auction/service/internal/pkg/auth"
	appsvc "live-auction-bid/backend/app/auction/service/internal/service"
	"live-auction-bid/backend/app/auction/service/internal/storage"
)

func TestUploadDeleteDoesNotExposeRawDatabaseErrors(t *testing.T) {
	result := appsvc.ErrorResult(context.Background(), agentSessionHTTPError(errors.New("UNIQUE constraint failed: secret_table.secret_column")))
	if strings.Contains(result.GetMessage(), "secret_table") || strings.Contains(result.GetMessage(), "UNIQUE") {
		t.Fatalf("unsafe error message=%q", result.GetMessage())
	}
}

func TestUploadDeleteClaimsBeforeStorageAndRestoresOrFinalizes(t *testing.T) {
	for _, failDelete := range []bool{true, false} {
		t.Run(map[bool]string{true: "restore on storage failure", false: "finalize on success"}[failDelete], func(t *testing.T) {
			manager, err := auth.NewManager(auth.Config{Secret: "upload-delete-claim", Issuer: "test", AccessTTL: time.Hour, RefreshTTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			pair, err := manager.IssueTokenPair(&auctionv1.User{Id: "operator-a", MainAccountId: "tenant-a", RoleCodes: []string{userbiz.RoleOperator}, PermissionCodes: []string{userbiz.PermissionUploadImage}, Status: auctionv1.UserStatus_USER_STATUS_ACTIVE})
			if err != nil {
				t.Fatal(err)
			}
			store := &claimUploadStore{asset: &data.AssetFile{ID: "asset-a", OwnerUserID: "operator-a", Status: data.AssetStatusTemporary, ObjectKey: "object-a"}}
			provider := &claimUploadStorage{store: store, fail: failDelete}
			handler := &uploadHandler{auth: manager, store: store, storage: provider}
			request := httptest.NewRequest(http.MethodDelete, "/api/uploads/images/asset-a", nil)
			request.Header.Set("Authorization", "Bearer "+pair.AccessToken)
			response := httptest.NewRecorder()
			handler.handleImageDelete(response, request)
			if store.claimCalls != 1 || provider.statusAtDelete != data.AssetStatusDeleting {
				t.Fatalf("claimCalls=%d statusAtDelete=%q", store.claimCalls, provider.statusAtDelete)
			}
			if failDelete && (store.restoreCalls != 1 || store.finalizeCalls != 0) {
				t.Fatalf("restore=%d finalize=%d", store.restoreCalls, store.finalizeCalls)
			}
			if !failDelete && (store.restoreCalls != 0 || store.finalizeCalls != 1) {
				t.Fatalf("restore=%d finalize=%d", store.restoreCalls, store.finalizeCalls)
			}
		})
	}
}

func TestUploadDeleteRestoresClaimWhenStorageIsUnavailable(t *testing.T) {
	manager, err := auth.NewManager(auth.Config{Secret: "upload-delete-claim", Issuer: "test", AccessTTL: time.Hour, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := manager.IssueTokenPair(&auctionv1.User{Id: "operator-a", MainAccountId: "tenant-a", RoleCodes: []string{userbiz.RoleOperator}, PermissionCodes: []string{userbiz.PermissionUploadImage}, Status: auctionv1.UserStatus_USER_STATUS_ACTIVE})
	if err != nil {
		t.Fatal(err)
	}
	store := &claimUploadStore{asset: &data.AssetFile{ID: "asset-a", OwnerUserID: "operator-a", Status: data.AssetStatusTemporary, ObjectKey: "object-a"}}
	handler := &uploadHandler{auth: manager, store: store}
	request := httptest.NewRequest(http.MethodDelete, "/api/uploads/images/asset-a", nil)
	request.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	handler.handleImageDelete(httptest.NewRecorder(), request)
	if store.claimCalls != 1 || store.restoreCalls != 1 || store.finalizeCalls != 0 || store.asset.Status != data.AssetStatusTemporary {
		t.Fatalf("claim=%d restore=%d finalize=%d status=%q", store.claimCalls, store.restoreCalls, store.finalizeCalls, store.asset.Status)
	}
}

func TestUploadDeleteCancellationAfterClaimRestoresTemporaryWithoutPhysicalDelete(t *testing.T) {
	manager, err := auth.NewManager(auth.Config{Secret: "upload-delete-cancel", Issuer: "test", AccessTTL: time.Hour, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := manager.IssueTokenPair(&auctionv1.User{Id: "operator-a", MainAccountId: "tenant-a", RoleCodes: []string{userbiz.RoleOperator}, PermissionCodes: []string{userbiz.PermissionUploadImage}, Status: auctionv1.UserStatus_USER_STATUS_ACTIVE})
	if err != nil {
		t.Fatal(err)
	}
	store := &claimUploadStore{asset: &data.AssetFile{ID: "asset-cancel", OwnerUserID: "operator-a", Status: data.AssetStatusTemporary, ObjectKey: "object-cancel"}}
	provider := &cancellableClaimUploadStorage{store: store, entered: make(chan struct{}), cancelObserved: make(chan struct{}), release: make(chan struct{})}
	handler := &uploadHandler{auth: manager, store: store, storage: provider}
	requestCtx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodDelete, "/api/uploads/images/asset-cancel", nil).WithContext(requestCtx)
	request.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); handler.handleImageDelete(response, request) }()
	<-provider.entered
	if store.asset.Status != data.AssetStatusDeleting {
		t.Fatalf("status at storage call=%q", store.asset.Status)
	}
	cancel()
	select {
	case <-provider.cancelObserved:
	case <-time.After(250 * time.Millisecond):
		close(provider.release)
		<-done
		t.Fatal("storage delete context did not observe request cancellation")
	}
	<-done
	if provider.physicallyDeleted || store.restoreCalls != 1 || store.finalizeCalls != 0 || store.asset.Status != data.AssetStatusTemporary {
		t.Fatalf("physical=%v restore=%d finalize=%d status=%q", provider.physicallyDeleted, store.restoreCalls, store.finalizeCalls, store.asset.Status)
	}
	if strings.Contains(response.Body.String(), "context canceled") || strings.Contains(response.Body.String(), "object-cancel") {
		t.Fatalf("unsafe cancellation response=%s", response.Body.String())
	}
}

type claimUploadStore struct {
	asset                                   *data.AssetFile
	claimCalls, restoreCalls, finalizeCalls int
}

func (s *claimUploadStore) SaveAssetFile(context.Context, data.AssetFile) error { return nil }
func (s *claimUploadStore) ClaimTemporaryAssetForDelete(context.Context, string, string) (*data.AssetFile, error) {
	s.claimCalls++
	copy := *s.asset
	copy.Status = data.AssetStatusDeleting
	s.asset.Status = data.AssetStatusDeleting
	return &copy, nil
}
func (s *claimUploadStore) RestoreDeletingAsset(context.Context, string, string) error {
	s.restoreCalls++
	s.asset.Status = data.AssetStatusTemporary
	return nil
}
func (s *claimUploadStore) FinalizeDeletingAsset(context.Context, string, string) error {
	s.finalizeCalls++
	s.asset.Status = data.AssetStatusDeleted
	return nil
}

type claimUploadStorage struct {
	store          *claimUploadStore
	fail           bool
	statusAtDelete string
}

func (s *claimUploadStorage) PutObject(context.Context, storage.PutObjectInput) (*storage.StoredObject, error) {
	return nil, errors.New("unused")
}
func (s *claimUploadStorage) DeleteObject(context.Context, string) error {
	s.statusAtDelete = s.store.asset.Status
	if s.fail {
		return errors.New("delete failed")
	}
	return nil
}

type cancellableClaimUploadStorage struct {
	store                            *claimUploadStore
	entered, cancelObserved, release chan struct{}
	physicallyDeleted                bool
}

func (s *cancellableClaimUploadStorage) PutObject(context.Context, storage.PutObjectInput) (*storage.StoredObject, error) {
	return nil, errors.New("unused")
}
func (s *cancellableClaimUploadStorage) DeleteObject(ctx context.Context, _ string) error {
	if s.store.asset.Status != data.AssetStatusDeleting {
		return errors.New("asset was not claimed")
	}
	close(s.entered)
	select {
	case <-ctx.Done():
		close(s.cancelObserved)
		return ctx.Err()
	case <-s.release:
		s.physicallyDeleted = true
		return nil
	}
}
