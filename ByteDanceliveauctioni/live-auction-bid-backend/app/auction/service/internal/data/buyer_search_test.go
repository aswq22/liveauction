package data

import (
	v1 "live-auction-bid/backend/api/auction/service/v1"
	"testing"
	"time"
)

func TestBuyerSearchRuntimeIdentityAndPrice(t *testing.T) {
	base := &v1.Lot{Id: "a", RoomId: "r", MainAccountId: "m", Status: v1.LotStatus_LOT_STATUS_LIVE, CurrentPrice: &v1.Money{Amount: 100, Currency: "CNY"}}
	valid := map[string]string{"lot_id": "a", "room_id": "r", "main_account_id": "m", "status": "2", "current_amount": "301", "current_currency": "CNY", "version": "2", "ends_at_unix_ms": "9999999999999"}
	got, err := buyerSearchRuntimeFact(base, valid, time.Now())
	if err != nil || got.GetCurrentPrice().GetAmount() != 301 {
		t.Fatal(got, err)
	}
	for _, field := range []string{"lot_id", "room_id", "main_account_id", "current_amount", "version"} {
		v := map[string]string{}
		for k, x := range valid {
			v[k] = x
		}
		v[field] = ""
		if _, err = buyerSearchRuntimeFact(base, v, time.Now()); err == nil {
			t.Fatal("accepted missing", field)
		}
	}
	if _, err = buyerSearchRuntimeFact(base, nil, time.Now()); err == nil {
		t.Fatal("stale SQL live price accepted")
	}
	valid["ends_at_unix_ms"] = "1"
	got, err = buyerSearchRuntimeFact(base, valid, time.Now())
	if err != nil || got != nil {
		t.Fatal("ended live lot not excluded", got, err)
	}
	base.Status = v1.LotStatus_LOT_STATUS_QUEUED
	base.Rule = &v1.BidRule{StartPrice: &v1.Money{Amount: 50, Currency: "CNY"}}
	got, err = buyerSearchRuntimeFact(base, nil, time.Now())
	if err != nil || got.GetCurrentPrice().GetAmount() != 50 {
		t.Fatal(got, err)
	}
}
