package service

import (
	"context"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	"testing"
)

func TestBuyerSearchServiceDisabledAndInvalid(t *testing.T) {
	s := NewAuctionService(nil)
	r, e := s.SearchBuyerLots(context.Background(), &v1.BuyerSearchRequest{Query: "玉"})
	if e != nil || r.GetResult().GetCode() != 503301 {
		t.Fatal(r, e)
	}
	r, e = s.SearchBuyerLots(context.Background(), &v1.BuyerSearchRequest{Query: ""})
	if e != nil || r.GetResult().GetCode() != 400301 {
		t.Fatal(r, e)
	}
}
