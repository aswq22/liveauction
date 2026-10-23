package service

import (
	"context"
	"errors"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/biz/auction"
	bs "live-auction-bid/backend/app/auction/service/internal/biz/buyersearch"
)

func (s *AuctionService) SetBuyerRetrieval(search *bs.Service) *AuctionService {
	s.buyerRetrieval = search
	return s
}
func buyerSearchResult(ctx context.Context, e error) *v1.ReplyResult {
	r := OKResult(ctx)
	switch {
	case errors.Is(e, bs.ErrInvalidQuery):
		r.Code = 400301
		r.Message = "SEARCH_INVALID_QUERY"
	case errors.Is(e, bs.ErrInvalidCursor):
		r.Code = 400302
		r.Message = "SEARCH_INVALID_CURSOR"
	case errors.Is(e, bs.ErrCursorExpired):
		r.Code = 410301
		r.Message = "SEARCH_CURSOR_EXPIRED"
	case errors.Is(e, bs.ErrCapacity):
		r.Code = 429301
		r.Message = "SEARCH_CAPACITY_EXHAUSTED"
	default:
		r.Code = 503301
		r.Message = "SEARCH_UNAVAILABLE"
	}
	return r
}
func (s *AuctionService) SearchBuyerLots(ctx context.Context, req *v1.BuyerSearchRequest) (*v1.BuyerSearchReply, error) {
	q := bs.Query{Text: req.GetQuery(), RoomID: req.GetRoomId(), MinPrice: req.MinPriceAmount, MaxPrice: req.MaxPriceAmount, Statuses: req.Statuses, PageSize: int(req.GetPageSize()), Cursor: req.GetCursor()}
	if _, e := bs.Parse(q); e != nil {
		return &v1.BuyerSearchReply{Result: buyerSearchResult(ctx, e)}, nil
	}
	if s.buyerRetrieval == nil {
		return &v1.BuyerSearchReply{Result: buyerSearchResult(ctx, bs.ErrUnavailable)}, nil
	}
	page, e := s.buyerRetrieval.Search(ctx, q)
	if e != nil {
		return &v1.BuyerSearchReply{Result: buyerSearchResult(ctx, e)}, nil
	}
	out := &v1.BuyerSearchReply{Result: OKResult(ctx), Hits: []*v1.BuyerSearchHit{}, AppliedFilters: &v1.BuyerSearchFilters{RoomId: page.Applied.RoomID, MinPriceAmount: page.Applied.MinPrice, MaxPriceAmount: page.Applied.MaxPrice, Statuses: page.Applied.Statuses}, Warnings: page.Warnings, Mode: page.Mode, DegradedReason: page.DegradedReason, NextCursor: page.NextCursor}
	for _, h := range page.Hits {
		lot := auction.LotForViewer(h.Fact.Lot, lotResultViewerFromContext(ctx))
		out.Hits = append(out.Hits, &v1.BuyerSearchHit{Lot: lot, MatchReason: h.MatchReason, FactsReadAtUnixMs: h.Fact.ReadAtUnixMs, Href: "/m/room/" + lot.GetRoomId()})
	}
	return out, nil
}
