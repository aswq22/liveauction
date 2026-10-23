package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"unicode/utf8"

	"live-auction-bid/backend/app/auction/service/internal/pkg/auth"
	"live-auction-bid/backend/app/auction/service/internal/pkg/idgen"
)

const maxBarrageRequestBytes = 1024

var barrageRoomIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type PublicRoomValidator interface {
	PublicRoomExists(ctx context.Context, roomID string) (bool, error)
}

func (h *Hub) ServeBarrage(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if h.barrage == nil {
		http.Error(w, "barrage service unavailable", http.StatusServiceUnavailable)
		return
	}
	if h.auth == nil {
		http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, authCtx := h.authContextFromAuthorization(r.Context(), websocketAuthorization(r))
	if authCtx.TokenStatus != auth.TokenStatusValid || authCtx.Claims == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if !barrageRoomIDPattern.MatchString(roomID) {
		http.Error(w, "invalid room id", http.StatusBadRequest)
		return
	}
	request, status, err := decodeBarrageRequest(r.Body)
	if err != nil {
		if status == http.StatusRequestEntityTooLarge {
			http.Error(w, "request body too large", status)
			return
		}
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if h.publicRooms == nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "room lookup unavailable", http.StatusServiceUnavailable)
		return
	}
	exists, err := h.publicRooms.PublicRoomExists(ctx, roomID)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "room lookup unavailable", http.StatusServiceUnavailable)
		return
	}
	if !exists {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}
	message := Barrage{
		ID: idgen.New("barrage"), RoomID: roomID,
		UserID: authCtx.Claims.UserID, Content: request.Content,
	}
	if err := h.barrage.SubmitPublic(ctx, message); err != nil {
		switch {
		case errors.Is(err, ErrBarrageInvalid), errors.Is(err, ErrBarrageSensitive):
			http.Error(w, "barrage rejected", http.StatusBadRequest)
		case errors.Is(err, ErrBarrageMuted):
			http.Error(w, "barrage sender muted", http.StatusForbidden)
		case errors.Is(err, ErrBarrageRateLimited):
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many barrages", http.StatusTooManyRequests)
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "barrage pipeline busy", http.StatusServiceUnavailable)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "id": message.ID})
}

type barrageHTTPRequest struct {
	Content string `json:"content"`
}

func decodeBarrageRequest(body io.Reader) (barrageHTTPRequest, int, error) {
	var request barrageHTTPRequest
	raw, err := io.ReadAll(io.LimitReader(body, maxBarrageRequestBytes+1))
	if len(raw) > maxBarrageRequestBytes {
		return request, http.StatusRequestEntityTooLarge, errors.New("request body too large")
	}
	if err != nil {
		return request, http.StatusBadRequest, err
	}
	if !utf8.Valid(raw) {
		return request, http.StatusBadRequest, errors.New("request body is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, http.StatusBadRequest, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("request body contains multiple JSON documents")
		}
		return request, http.StatusBadRequest, err
	}
	return request, http.StatusOK, nil
}
