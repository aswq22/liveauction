package realtime

import (
	"strings"
	"testing"

	v1 "live-auction-bid/backend/api/auction/service/v1"
)

func TestPrepareBroadcastMessageCachesAnonymousPublicPayload(t *testing.T) {
	hub := NewHub(nil)
	event := v1.AuctionEvent{
		Id:            "evt_1",
		Type:          v1.AuctionEventType_AUCTION_EVENT_TYPE_LOT_UPDATED,
		RoomId:        "room_1",
		MainAccountId: "main_private",
		Lot: &v1.Lot{
			Id:            "lot_1",
			RoomId:        "room_1",
			MainAccountId: "main_private",
			LeadingUserId: "buyer_private",
		},
	}

	message, err := hub.prepareBroadcastMessage(event)
	if err != nil {
		t.Fatalf("prepareBroadcastMessage() error = %v", err)
	}
	if len(message.anonymousPublicPayload) == 0 {
		t.Fatal("anonymous public payload was not precomputed")
	}
	payload := string(message.anonymousPublicPayload)
	if strings.Contains(payload, "main_private") || strings.Contains(payload, "buyer_private") {
		t.Fatalf("anonymous payload leaked private fields: %s", payload)
	}
}
