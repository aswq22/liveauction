package agenttool

import (
	"context"
	"errors"
	"sort"
	"strings"

	agentv1 "live-auction-bid/backend/api/agent/v1"
	auctionv1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
	"live-auction-bid/backend/app/auction/service/internal/biz/auction"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxToolResults = 100

var inventoryStatuses = []auctionv1.LotStatus{
	auctionv1.LotStatus_LOT_STATUS_DRAFT,
	auctionv1.LotStatus_LOT_STATUS_READY,
}

var sessionStatuses = []auctionv1.LotStatus{
	auctionv1.LotStatus_LOT_STATUS_LIVE,
	auctionv1.LotStatus_LOT_STATUS_EXTENDED,
	auctionv1.LotStatus_LOT_STATUS_QUEUED,
}

type AuctionReader interface {
	ListLotsByQuery(context.Context, auction.LotQuery) (auction.LotList, error)
	ListOrders(context.Context, auction.OrderQuery) (auction.OrderList, error)
	ValidateRoomInMainAccount(context.Context, string, string) error
}

type RunFinder interface {
	Find(context.Context, string) (agentcontrol.Run, error)
}

// Service is the read-only business-data boundary exposed to the agent runtime.
// Tenant and operator identity are always resolved from the persisted agent run.
type Service struct {
	agentv1.UnimplementedAuctionAgentToolServiceServer
	token    string
	runs     RunFinder
	auctions AuctionReader
}

var _ agentv1.AuctionAgentToolServiceServer = (*Service)(nil)

func NewService(token string, runs RunFinder, auctions AuctionReader) (*Service, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("internal service token is required")
	}
	if runs == nil {
		return nil, errors.New("agent run repository is required")
	}
	if auctions == nil {
		return nil, errors.New("auction reader is required")
	}
	return &Service{token: token, runs: runs, auctions: auctions}, nil
}

func (s *Service) QueryInventory(ctx context.Context, req *agentv1.QueryInventoryRequest) (*agentv1.QueryInventoryReply, error) {
	run, err := s.authorize(ctx, req.GetContext())
	if err != nil {
		return nil, err
	}

	limit := toolLimit(req.GetLimit())
	lots := make([]*auctionv1.Lot, 0, maxToolResults)
	seen := make(map[string]struct{})
	for _, lotStatus := range inventoryStatuses {
		list, listErr := s.auctions.ListLotsByQuery(ctx, auction.LotQuery{
			Page:             1,
			PageSize:         maxToolResults,
			MainAccountID:    run.TenantID,
			Status:           lotStatus,
			OrderByUpdatedAt: true,
		})
		if listErr != nil {
			return nil, toolError(listErr)
		}
		for _, lot := range list.Lots {
			if !isPersistedTenantLot(lot, run.TenantID) || !isAppendableInventoryLot(lot) {
				continue
			}
			if _, ok := seen[lot.GetId()]; ok {
				continue
			}
			seen[lot.GetId()] = struct{}{}
			lots = append(lots, lot)
		}
	}
	sort.SliceStable(lots, func(i, j int) bool {
		if lots[i].GetUpdatedAtUnixMs() == lots[j].GetUpdatedAtUnixMs() {
			return lots[i].GetId() < lots[j].GetId()
		}
		return lots[i].GetUpdatedAtUnixMs() > lots[j].GetUpdatedAtUnixMs()
	})
	if len(lots) > limit {
		lots = lots[:limit]
	}
	reply := &agentv1.QueryInventoryReply{Lots: make([]*agentv1.InventoryLot, 0, len(lots))}
	for _, lot := range lots {
		reply.Lots = append(reply.Lots, inventoryLotFromAuctionLot(lot))
	}
	return reply, nil
}

func isAppendableInventoryLot(lot *auctionv1.Lot) bool {
	if lot == nil || lot.GetStartedAtUnixMs() != 0 || lot.GetQueueStatus() != auctionv1.LotQueueStatus_LOT_QUEUE_STATUS_NONE || lot.GetQueuePosition() != 0 {
		return false
	}
	return lot.GetStatus() == auctionv1.LotStatus_LOT_STATUS_DRAFT || lot.GetStatus() == auctionv1.LotStatus_LOT_STATUS_READY
}

func (s *Service) QueryAuctionHistory(ctx context.Context, req *agentv1.QueryAuctionHistoryRequest) (*agentv1.QueryAuctionHistoryReply, error) {
	run, err := s.authorize(ctx, req.GetContext())
	if err != nil {
		return nil, err
	}
	limit := toolLimit(req.GetLimit())
	lotList, err := s.auctions.ListLotsByQuery(ctx, auction.LotQuery{
		Page:          1,
		PageSize:      limit,
		MainAccountID: run.TenantID,
		View:          "history",
	})
	if err != nil {
		return nil, toolError(err)
	}
	reply := &agentv1.QueryAuctionHistoryReply{Items: make([]*agentv1.AuctionHistoryItem, 0, limit)}
	for _, lot := range lotList.Lots {
		if len(reply.Items) == limit {
			break
		}
		if !isPersistedTenantLot(lot, run.TenantID) || !isHistoryStatus(lot.GetStatus()) {
			continue
		}
		orderList, orderErr := s.auctions.ListOrders(ctx, auction.OrderQuery{
			Page:          1,
			PageSize:      1,
			MainAccountID: run.TenantID,
			LotID:         lot.GetId(),
		})
		if orderErr != nil {
			return nil, toolError(orderErr)
		}
		var order auction.OrderSummary
		var hasOrder bool
		for _, candidate := range orderList.Orders {
			if candidate.MainAccountID == run.TenantID && candidate.LotID == lot.GetId() {
				order, hasOrder = candidate, true
				break
			}
		}
		item := &agentv1.AuctionHistoryItem{
			LotId:               lot.GetId(),
			RoomId:              lot.GetRoomId(),
			LotStatus:           lotStatusText(lot.GetStatus()),
			FinalPrice:          agentMoney(lot.GetFinalPrice()),
			ParticipationResult: historyResult(lot.GetStatus(), hasOrder),
			SettledAtUnixMs:     lot.GetSettledAtUnixMs(),
		}
		if hasOrder {
			item.OrderStatus = string(order.Status)
			item.PaymentStatus = string(order.PaymentStatus)
		}
		reply.Items = append(reply.Items, item)
	}
	return reply, nil
}

func (s *Service) GetSessionSnapshot(ctx context.Context, req *agentv1.GetSessionSnapshotRequest) (*agentv1.GetSessionSnapshotReply, error) {
	run, err := s.authorize(ctx, req.GetContext())
	if err != nil {
		return nil, err
	}
	roomID := strings.TrimSpace(req.GetRoomId())
	if roomID == "" {
		return nil, status.Error(codes.InvalidArgument, "room_id is required")
	}
	if err := s.auctions.ValidateRoomInMainAccount(ctx, roomID, run.TenantID); err != nil {
		return nil, toolError(err)
	}
	lots := make([]*auctionv1.Lot, 0)
	seen := make(map[string]struct{})
	for _, lotStatus := range sessionStatuses {
		lotList, listErr := s.auctions.ListLotsByQuery(ctx, auction.LotQuery{
			Page:          1,
			PageSize:      maxToolResults,
			MainAccountID: run.TenantID,
			RoomID:        roomID,
			Status:        lotStatus,
		})
		if listErr != nil {
			return nil, toolError(listErr)
		}
		for _, lot := range lotList.Lots {
			if lot == nil {
				continue
			}
			if _, ok := seen[lot.GetId()]; ok {
				continue
			}
			seen[lot.GetId()] = struct{}{}
			lots = append(lots, lot)
		}
	}

	var current *auctionv1.Lot
	queued := make([]*auctionv1.Lot, 0)
	for _, lot := range lots {
		if !isPersistedTenantLot(lot, run.TenantID) || lot.GetRoomId() != roomID {
			continue
		}
		switch lot.GetStatus() {
		case auctionv1.LotStatus_LOT_STATUS_LIVE, auctionv1.LotStatus_LOT_STATUS_EXTENDED:
			if current == nil || lot.GetUpdatedAtUnixMs() > current.GetUpdatedAtUnixMs() {
				current = lot
			}
		case auctionv1.LotStatus_LOT_STATUS_QUEUED:
			queued = append(queued, lot)
		}
	}
	sort.SliceStable(queued, func(i, j int) bool {
		left, right := queued[i].GetQueuePosition(), queued[j].GetQueuePosition()
		if left == right {
			return queued[i].GetUpdatedAtUnixMs() > queued[j].GetUpdatedAtUnixMs()
		}
		if left <= 0 {
			return false
		}
		if right <= 0 {
			return true
		}
		return left < right
	})
	snapshot := &agentv1.SessionSnapshot{RoomId: roomID, QueuedLots: make([]*agentv1.InventoryLot, 0, len(queued))}
	if current != nil {
		snapshot.CurrentLot = inventoryLotFromAuctionLot(current)
		snapshot.UpdatedAtUnixMs = current.GetUpdatedAtUnixMs()
	}
	for _, lot := range queued {
		snapshot.QueuedLots = append(snapshot.QueuedLots, inventoryLotFromAuctionLot(lot))
		if lot.GetUpdatedAtUnixMs() > snapshot.UpdatedAtUnixMs {
			snapshot.UpdatedAtUnixMs = lot.GetUpdatedAtUnixMs()
		}
	}
	return &agentv1.GetSessionSnapshotReply{Snapshot: snapshot}, nil
}

func toolLimit(limit int32) int {
	if limit <= 0 || limit > maxToolResults {
		return maxToolResults
	}
	return int(limit)
}

func isPersistedTenantLot(lot *auctionv1.Lot, tenantID string) bool {
	return lot != nil && lot.GetId() != "" && lot.GetMainAccountId() == tenantID
}

func isInventoryStatus(status auctionv1.LotStatus) bool {
	for _, allowed := range inventoryStatuses {
		if status == allowed {
			return true
		}
	}
	return false
}

func isHistoryStatus(status auctionv1.LotStatus) bool {
	switch status {
	case auctionv1.LotStatus_LOT_STATUS_SETTLED,
		auctionv1.LotStatus_LOT_STATUS_CANCELLED,
		auctionv1.LotStatus_LOT_STATUS_FAILED:
		return true
	default:
		return false
	}
}

func inventoryLotFromAuctionLot(lot *auctionv1.Lot) *agentv1.InventoryLot {
	return &agentv1.InventoryLot{
		Id:              lot.GetId(),
		RoomId:          lot.GetRoomId(),
		Title:           lot.GetTitle(),
		Category:        lot.GetCategory(),
		Description:     lot.GetDescription(),
		Tags:            append([]string(nil), lot.GetTags()...),
		Status:          lotStatusText(lot.GetStatus()),
		Stock:           int64(lot.GetStock()),
		StartPrice:      agentMoney(lot.GetRule().GetStartPrice()),
		CurrentPrice:    agentMoney(lot.GetCurrentPrice()),
		CreatedAt:       lot.GetCreatedAtUnixMs(),
		UpdatedAt:       lot.GetUpdatedAtUnixMs(),
		DurationSeconds: lot.GetRule().GetDurationSeconds(),
	}
}

func agentMoney(money *auctionv1.Money) *agentv1.Money {
	if money == nil {
		return nil
	}
	return &agentv1.Money{Amount: money.GetAmount(), Currency: money.GetCurrency()}
}

func lotStatusText(status auctionv1.LotStatus) string {
	return strings.TrimPrefix(status.String(), "LOT_STATUS_")
}

func historyResult(status auctionv1.LotStatus, hasOrder bool) string {
	if status == auctionv1.LotStatus_LOT_STATUS_SETTLED {
		if hasOrder {
			return "SOLD"
		}
		return "SETTLED"
	}
	return lotStatusText(status)
}
