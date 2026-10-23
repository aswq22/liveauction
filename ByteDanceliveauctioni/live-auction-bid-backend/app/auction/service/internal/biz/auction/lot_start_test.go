package auction

import (
	"testing"

	v1 "live-auction-bid/backend/api/auction/service/v1"
)

func TestStartLotAcceptsEveryUnstartedTechnicalStatus(t *testing.T) {
	statuses := []v1.LotStatus{
		v1.LotStatus_LOT_STATUS_DRAFT,
		v1.LotStatus_LOT_STATUS_READY,
		v1.LotStatus_LOT_STATUS_QUEUED,
	}
	for _, status := range statuses {
		t.Run(status.String(), func(t *testing.T) {
			lot := &v1.Lot{
				Status:  status,
				Version: 2,
				Rule: &v1.BidRule{
					StartPrice:      &v1.Money{Amount: 10000, Currency: "CNY"},
					DurationSeconds: 600,
				},
			}

			if err := StartLot(lot, 1000); err != nil {
				t.Fatalf("StartLot() error = %v", err)
			}
			if lot.GetStatus() != v1.LotStatus_LOT_STATUS_LIVE || lot.GetStartedAtUnixMs() != 1000 || lot.GetEndsAtUnixMs() != 601000 {
				t.Fatalf("StartLot() lot = %+v", lot)
			}
		})
	}
}

func TestDequeueLotReturnsQueuedInventoryToReady(t *testing.T) {
	lot := &v1.Lot{
		Id:            "lot-1",
		Status:        v1.LotStatus_LOT_STATUS_QUEUED,
		QueueStatus:   v1.LotQueueStatus_LOT_QUEUE_STATUS_QUEUED,
		QueuePosition: 18,
		Version:       7,
	}

	if err := DequeueLot(lot); err != nil {
		t.Fatalf("DequeueLot() error = %v", err)
	}
	if lot.GetStatus() != v1.LotStatus_LOT_STATUS_READY || lot.GetQueueStatus() != v1.LotQueueStatus_LOT_QUEUE_STATUS_NONE || lot.GetQueuePosition() != 0 {
		t.Fatalf("DequeueLot() lot = %+v", lot)
	}
	if lot.GetVersion() != 8 {
		t.Fatalf("DequeueLot() version = %d, want 8", lot.GetVersion())
	}
}

func TestDequeueLotRejectsStartedAuction(t *testing.T) {
	lot := &v1.Lot{Id: "lot-live", Status: v1.LotStatus_LOT_STATUS_LIVE, StartedAtUnixMs: 100, QueueStatus: v1.LotQueueStatus_LOT_QUEUE_STATUS_NONE}
	if err := DequeueLot(lot); err == nil {
		t.Fatal("DequeueLot() error = nil, want rejection")
	}
}
