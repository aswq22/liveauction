package server

import (
	"net/http"
	"strings"

	"live-auction-bid/backend/app/auction/service/internal/realtime"

	httptransport "github.com/go-kratos/kratos/v2/transport/http"
)

func registerRealtimeHTTP(srv *httptransport.Server, hub *realtime.Hub) {
	srv.HandleFunc("/api/realtime/ws-ticket", hub.ServeTicket)
	srv.HandlePrefix("/api/realtime/rooms/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/realtime/rooms/"), "/")
		roomID, suffix, ok := strings.Cut(path, "/")
		if !ok || roomID == "" || suffix != "barrages" {
			http.NotFound(w, r)
			return
		}
		hub.ServeBarrage(w, r, roomID)
	}))
	srv.HandlePrefix("/ws/rooms/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		roomID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/ws/rooms/"), "/")
		if roomID == "" {
			http.Error(w, "room id is required", http.StatusBadRequest)
			return
		}
		hub.ServeRoom(w, r, roomID)
	}))
}
