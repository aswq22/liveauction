package auction

import (
	"testing"

	v1 "live-auction-bid/backend/api/auction/service/v1"
)

func TestNewRelistLotFromFailedCopiesSellableFieldsAndClearsRuntimeState(t *testing.T) {
	source, err := NewLotFromRequest("lot-old", &v1.CreateLotRequest{
		RoomId:           "room-1",
		Title:            "failed lot",
		Description:      "can be listed again",
		ImageUrl:         "https://example.com/lot.png",
		Rule:             testBidRule(),
		TrustCards:       []*v1.TrustRevealCard{{Id: "card-1", Title: "certificate", Content: "A"}},
		GalleryImageUrls: []string{"https://example.com/lot-2.png"},
		Category:         "collectible",
		Tags:             []string{"rare", "live"},
		EstimatePrice:    &v1.Money{Amount: 20000, Currency: "CNY"},
		Stock:            1,
		AfterSaleNotes:   "ship after payment",
		DepositAmount:    &v1.Money{Amount: 1000, Currency: "CNY"},
	})
	if err != nil {
		t.Fatalf("NewLotFromRequest() error = %v", err)
	}
	source.Status = v1.LotStatus_LOT_STATUS_FAILED
	source.QueueStatus = v1.LotQueueStatus_LOT_QUEUE_STATUS_NONE
	source.QueuePosition = 0
	source.CurrentPrice = &v1.Money{Amount: 15000, Currency: "CNY"}
	source.FinalPrice = &v1.Money{Amount: 15000, Currency: "CNY"}
	source.LeadingUserId = "buyer-1"
	source.LeadingNickname = "buyer"
	source.WinnerUserId = "buyer-1"
	source.WinnerNickname = "buyer"
	source.StartedAtUnixMs = 100
	source.EndsAtUnixMs = 200
	source.CancelReason = "auction expired without accepted bid"
	source.CancelledAtUnixMs = 300
	source.Version = 9
	source.DuelState = &v1.DuelState{Active: true, LotId: source.Id, StartedAtUnixMs: 120}

	relisted, err := NewRelistLotFromFailed("lot-new", source)
	if err != nil {
		t.Fatalf("NewRelistLotFromFailed() error = %v", err)
	}

	if relisted.Id != "lot-new" {
		t.Fatalf("relisted.Id = %q, want lot-new", relisted.Id)
	}
	if source.Id != "lot-old" || source.Status != v1.LotStatus_LOT_STATUS_FAILED {
		t.Fatalf("source mutated: id=%q status=%s", source.Id, source.Status)
	}
	if relisted.Status != v1.LotStatus_LOT_STATUS_DRAFT {
		t.Fatalf("relisted.Status = %s, want DRAFT", relisted.Status)
	}
	if relisted.QueueStatus != v1.LotQueueStatus_LOT_QUEUE_STATUS_NONE || relisted.QueuePosition != 0 {
		t.Fatalf("relisted queue state = %s/%d, want NONE/0", relisted.QueueStatus, relisted.QueuePosition)
	}
	if relisted.CurrentPrice.GetAmount() != source.Rule.StartPrice.GetAmount() {
		t.Fatalf("relisted current price = %d, want start price %d", relisted.CurrentPrice.GetAmount(), source.Rule.StartPrice.GetAmount())
	}
	if relisted.FinalPrice.GetAmount() != 0 || relisted.FinalPrice.GetCurrency() != source.Rule.StartPrice.GetCurrency() {
		t.Fatalf("relisted final price = %+v, want zero amount with start currency", relisted.FinalPrice)
	}
	if relisted.LeadingUserId != "" || relisted.WinnerUserId != "" || relisted.StartedAtUnixMs != 0 || relisted.EndsAtUnixMs != 0 || relisted.CancelReason != "" || relisted.CancelledAtUnixMs != 0 {
		t.Fatalf("relisted runtime state was not cleared: %+v", relisted)
	}
	if relisted.DuelState == nil || relisted.DuelState.Active {
		t.Fatalf("relisted duel state = %+v, want inactive state", relisted.DuelState)
	}
	if relisted.Title != source.Title || relisted.ImageUrl != source.ImageUrl || relisted.Rule.GetStartPrice().GetAmount() != source.Rule.GetStartPrice().GetAmount() {
		t.Fatalf("relisted sellable fields were not copied")
	}
	if len(relisted.TrustCards) != 1 || len(relisted.GalleryImageUrls) != 1 || len(relisted.Tags) != 2 {
		t.Fatalf("relisted rich fields were not copied: trust=%d gallery=%d tags=%d", len(relisted.TrustCards), len(relisted.GalleryImageUrls), len(relisted.Tags))
	}
	if relisted.TrustCards[0] == source.TrustCards[0] {
		t.Fatalf("trust cards share pointer with source")
	}
}

func TestNewRelistLotFromFailedRejectsNonFailedSource(t *testing.T) {
	source := &v1.Lot{Id: "lot-old", Status: v1.LotStatus_LOT_STATUS_SETTLED}

	if _, err := NewRelistLotFromFailed("lot-new", source); err == nil {
		t.Fatal("NewRelistLotFromFailed() error = nil, want error")
	}
}
