package data

import (
	"context"
	"testing"

	v1 "live-auction-bid/backend/api/auction/service/v1"
)

type fakeEventStreamer struct {
	events []v1.AuctionEvent
	err    error
}

func (f *fakeEventStreamer) Publish(_ context.Context, event v1.AuctionEvent) error {
	f.events = append(f.events, event)
	return f.err
}

func TestWriteEventToTransportUsesBoundPublisher(t *testing.T) {
	streamer := &fakeEventStreamer{}
	store := (&Store{}).SetEventStreamer(streamer)
	event := v1.AuctionEvent{
		Id:               "evt_1",
		Type:             v1.AuctionEventType_AUCTION_EVENT_TYPE_PAYMENT_SUCCESS,
		RoomId:           "room_1",
		OccurredAtUnixMs: 1,
	}

	streamID, err := store.writeEventToTransport(context.Background(), event, "{}")
	if err != nil {
		t.Fatalf("writeEventToTransport returned error: %v", err)
	}
	if streamID != "publisher:evt_1" {
		t.Fatalf("stream id = %q, want publisher:evt_1", streamID)
	}
	if len(streamer.events) != 1 {
		t.Fatalf("published events = %d, want 1", len(streamer.events))
	}
	if streamer.events[0].GetId() != event.GetId() {
		t.Fatalf("published event id = %q, want %q", streamer.events[0].GetId(), event.GetId())
	}
}
