package auction

import (
	"context"
	"testing"

	v1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/biz/shop"
)

func testBidRule() *v1.BidRule {
	return &v1.BidRule{
		StartPrice:             &v1.Money{Amount: 10000, Currency: "CNY"},
		MinIncrement:           &v1.Money{Amount: 1000, Currency: "CNY"},
		DurationSeconds:        300,
		AntiSnipeWindowSeconds: 10,
		AntiSnipeExtendSeconds: 15,
		MaxExtendCount:         5,
	}
}

func TestAuctionLotStockIsPreservedAndConsumedOnSettle(t *testing.T) {
	lot, err := NewLotFromRequest("lot-1", &v1.CreateLotRequest{
		RoomId:      "room-1",
		Title:       "stocked lot",
		Description: "auction lot",
		ImageUrl:    "https://example.com/lot.png",
		Rule:        testBidRule(),
		Stock:       2,
	})
	if err != nil {
		t.Fatalf("NewLotFromRequest() error = %v", err)
	}
	if lot.GetStock() != 2 {
		t.Fatalf("lot.Stock = %d, want 2", lot.GetStock())
	}

	lot.Status = v1.LotStatus_LOT_STATUS_LIVE
	lot.LeadingUserId = "buyer-1"
	lot.LeadingNickname = "buyer"
	lot.CurrentPrice = &v1.Money{Amount: 12000, Currency: "CNY"}

	if err := SettleLot(lot, 123); err != nil {
		t.Fatalf("SettleLot() error = %v", err)
	}
	if lot.GetStock() != 1 {
		t.Fatalf("settled lot.Stock = %d, want 1", lot.GetStock())
	}
}

func TestSettleLotRejectsSoldOutInventory(t *testing.T) {
	lot := &v1.Lot{
		Id:              "lot-1",
		Status:          v1.LotStatus_LOT_STATUS_LIVE,
		LeadingUserId:   "buyer-1",
		LeadingNickname: "buyer",
		CurrentPrice:    &v1.Money{Amount: 12000, Currency: "CNY"},
		Stock:           0,
	}

	if err := SettleLot(lot, 123); err == nil {
		t.Fatal("SettleLot() error = nil, want sold out error")
	}
}

type countingDepositRepo struct {
	findByLotBuyerCalls int
}

func (r *countingDepositRepo) FindDepositHoldByLotBuyer(context.Context, string, string) (*DepositHold, bool, error) {
	r.findByLotBuyerCalls++
	return nil, false, nil
}

func (r *countingDepositRepo) FindDepositHoldByIdempotencyKey(context.Context, string, string, string) (*DepositHold, bool, error) {
	return nil, false, nil
}

func (r *countingDepositRepo) CommitDepositHold(context.Context, DepositHold) (*DepositHold, error) {
	return nil, nil
}

func TestEnsureDepositHeldSkipsZeroDepositLots(t *testing.T) {
	repo := &countingDepositRepo{}
	uc := &AuctionUsecase{deposits: repo}
	lot := &v1.Lot{
		Id:            "lot-1",
		DepositAmount: &v1.Money{Amount: 0, Currency: "CNY"},
	}

	if err := uc.ensureDepositHeld(context.Background(), lot, "buyer-1"); err != nil {
		t.Fatalf("ensureDepositHeld() error = %v", err)
	}
	if repo.findByLotBuyerCalls != 0 {
		t.Fatalf("FindDepositHoldByLotBuyer calls = %d, want 0", repo.findByLotBuyerCalls)
	}
}

func TestReleaseDepositHoldMarksHeldHoldReleased(t *testing.T) {
	hold := &DepositHold{
		ID:              "deposit-1",
		Status:          DepositStatusHeld,
		Amount:          1000,
		Currency:        "CNY",
		AddressID:       "addr-1",
		AddressSnapshot: shop.DeliveryAddressSnapshot{AddressID: "addr-1"},
	}

	if err := ReleaseDepositHold(hold, 456); err != nil {
		t.Fatalf("ReleaseDepositHold() error = %v", err)
	}
	if hold.Status != DepositStatusReleased {
		t.Fatalf("hold.Status = %s, want RELEASED", hold.Status)
	}
	if hold.ReleasedAtUnixMs != 456 {
		t.Fatalf("hold.ReleasedAtUnixMs = %d, want 456", hold.ReleasedAtUnixMs)
	}
}
