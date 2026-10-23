package auction

import (
	"context"
	"testing"
)

func TestCloseStalePreStartLotsKeepsUnstartedInventory(t *testing.T) {
	usecase := &AuctionUsecase{}
	summary, err := usecase.CloseStalePreStartLots(context.Background(), 1, 100)
	if err != nil {
		t.Fatalf("CloseStalePreStartLots() error = %v", err)
	}
	if summary != (CloseExpiredSummary{}) {
		t.Fatalf("CloseStalePreStartLots() summary = %+v, want no-op", summary)
	}
}
