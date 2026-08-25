package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	v1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
	"live-auction-bid/backend/app/auction/service/internal/pkg/auth"
	"live-auction-bid/backend/app/auction/service/internal/pkg/requestctx"
	"live-auction-bid/backend/app/auction/service/internal/realtime"
	appsvc "live-auction-bid/backend/app/auction/service/internal/service"
	"live-auction-bid/backend/app/auction/service/internal/storage"

	kerrors "github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/middleware/recovery"
	httptransport "github.com/go-kratos/kratos/v2/transport/http"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func NewHTTPServer(addr string, auction *appsvc.AuctionService, users *appsvc.UserService, shop *appsvc.ShopService, orders *appsvc.OrderService, hub *realtime.Hub, health HealthChecker, authManager *auth.Manager, authMiddleware middleware.Middleware, imageStorage storage.StorageProvider, assetStore assetStore) *httptransport.Server {
	middlewares := []middleware.Middleware{recovery.Recovery()}
	if authMiddleware != nil {
		middlewares = append(middlewares, authMiddleware)
	}
	srv := httptransport.NewServer(
		httptransport.Address(addr),
		httptransport.Middleware(middlewares...),
		httptransport.Filter(localDevCORSFilter, requestctx.HTTPMiddleware),
		httptransport.ErrorEncoder(resultEnvelopeErrorEncoder),
	)

	registerAuctionHTTP(srv, auction, authManager)
	registerShopHTTP(srv, shop, orders, auction)
	registerUserHTTP(srv, users)
	registerRealtimeHTTP(srv, hub)
	registerUploadHTTP(srv, authManager, assetStore, imageStorage)
	registerLocalAssetsHTTP(srv)
	registerOperationHTTP(srv, health)

	return srv
}

func registerAuctionHTTP(srv *httptransport.Server, auction *appsvc.AuctionService, authManager *auth.Manager) {
	v1.RegisterAuctionServiceHTTPServer(srv, auction)
	srv.HandleFunc("/api/admin/lots/relist", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ctx := auctionHTTPAuthContext(r, authManager)
		var req struct {
			LotID string `json:"lotId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONResult(w, appsvc.ErrorResult(ctx, fmt.Errorf("%w: invalid request body", apperr.ErrInvalidArgument)), nil)
			return
		}
		req.LotID = strings.TrimSpace(req.LotID)
		if req.LotID == "" {
			writeJSONResult(w, appsvc.ErrorResult(ctx, fmt.Errorf("%w: lot id is required", apperr.ErrInvalidArgument)), nil)
			return
		}
		reply, err := auction.RelistLot(ctx, req.LotID)
		if err != nil {
			writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
			return
		}
		writeJSONResult(w, reply.GetResult(), map[string]any{
			"lot":           reply.GetLot(),
			"queuePosition": reply.GetQueuePosition(),
		})
	})
	srv.HandleFunc("/api/admin/payments/reconcile", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		limit := 100
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed <= 0 {
				writeJSONResult(w, appsvc.ErrorResult(r.Context(), fmt.Errorf("%w: invalid limit", apperr.ErrInvalidArgument)), nil)
				return
			}
			limit = parsed
		}
		summary, err := auction.ReconcilePayments(r.Context(), time.Now().UnixMilli(), limit)
		if err != nil {
			writeJSONResult(w, appsvc.ErrorResult(r.Context(), err), nil)
			return
		}
		writeJSONResult(w, appsvc.OKResult(r.Context()), map[string]any{"summary": summary})
	})
}

func auctionHTTPAuthContext(r *http.Request, authManager *auth.Manager) context.Context {
	ctx := r.Context()
	if authManager == nil {
		return ctx
	}
	value := r.Header.Get("Authorization")
	if value == "" {
		value = r.Header.Get("authorization")
	}
	return authManager.WithAuthContextFromBearer(ctx, value)
}

func registerUserHTTP(srv *httptransport.Server, users *appsvc.UserService) {
	v1.RegisterUserServiceHTTPServer(srv, users)
}

func registerShopHTTP(srv *httptransport.Server, shop *appsvc.ShopService, orders *appsvc.OrderService, auction *appsvc.AuctionService) {
	v1.RegisterShopServiceHTTPServer(srv, appsvc.NewShopServiceHTTPAdapter(shop, orders, auction))
}

func registerLocalAssetsHTTP(srv *httptransport.Server) {
	if strings.ToLower(strings.TrimSpace(os.Getenv("AUCTION_STORAGE_PROVIDER"))) != "local" {
		return
	}
	root := strings.TrimSpace(os.Getenv("AUCTION_LOCAL_STORAGE_DIR"))
	if root == "" {
		root = "/tmp/live-auction-assets"
	}
	srv.HandlePrefix("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir(root))))
}

func resultEnvelopeErrorEncoder(w http.ResponseWriter, r *http.Request, err error) {
	mapped := err
	if se := kerrors.FromError(err); se != nil && se.Code == int32(http.StatusBadRequest) {
		mapped = fmt.Errorf("%w: invalid request", apperr.ErrInvalidArgument)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"result": appsvc.ErrorResult(r.Context(), mapped),
	})
}

func writeJSONResult(w http.ResponseWriter, result *v1.ReplyResult, extra map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	body := map[string]any{"result": result}
	for key, value := range extra {
		normalized, err := protoJSONValue(value)
		if err != nil {
			body[key] = value
			continue
		}
		body[key] = normalized
	}
	_ = json.NewEncoder(w).Encode(body)
}

func protoJSONValue(value any) (any, error) {
	message, ok := value.(proto.Message)
	if !ok || message == nil {
		return value, nil
	}
	payload, err := protojson.MarshalOptions{UseEnumNumbers: false}.Marshal(message)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func localDevCORSFilter(next http.Handler) http.Handler {
	allowed := allowedCORSOrigins()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" && allowed[origin] {
			header := w.Header()
			header.Set("Access-Control-Allow-Origin", origin)
			header.Set("Vary", "Origin")
			header.Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
			header.Set("Access-Control-Allow-Headers", "Authorization,Content-Type,X-Request-Id,X-Trace-Id,X-Client-App,X-Client-Version,X-Client-Time,Idempotency-Key")
			header.Set("Access-Control-Expose-Headers", "X-Request-Id,X-Trace-Id,X-Server-Time")
			header.Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func allowedCORSOrigins() map[string]bool {
	origins := map[string]bool{
		"http://localhost:5173": true,
		"http://localhost:5174": true,
		"http://127.0.0.1:5173": true,
		"http://127.0.0.1:5174": true,
	}
	for _, origin := range strings.Split(os.Getenv("AUCTION_HTTP_CORS_ORIGINS"), ",") {
		origin = strings.TrimSpace(origin)
		if origin != "" {
			origins[origin] = true
		}
	}
	return origins
}
