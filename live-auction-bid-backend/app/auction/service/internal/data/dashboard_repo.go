package data

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/biz/auction"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

var dashboardLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

// Dashboard reads authoritative rows, rather than the capped management list or
// asynchronously projected participant counts. Every root and joined subquery is scoped.
func (s *Store) GetDashboard(ctx context.Context, q auction.DashboardQuery) (auction.DashboardSnapshot, error) {
	var out auction.DashboardSnapshot
	if strings.TrimSpace(q.MainAccountID) == "" {
		return out, fmt.Errorf("%w: main account is required", apperr.ErrInvalidArgument)
	}
	var err error
	q, err = auction.NormalizeDashboardQuery(q)
	if err != nil {
		return out, err
	}
	if q.Range == auction.DashboardRangeLive {
		// Range discovery may select a room activated exactly now. Its event
		// window is then [now, now), and all aggregation queries below are empty.
		if err = s.db.WithContext(ctx).Model(&AuctionRoomModel{}).Where("main_account_id = ?", q.MainAccountID).
			Where("status = ? AND live_started_at_unix_ms > 0 AND live_started_at_unix_ms <= ?", "ACTIVE", q.EndUnixMs).
			Select("COALESCE(MIN(live_started_at_unix_ms), 0)").Scan(&q.StartUnixMs).Error; err != nil {
			return out, err
		}
		if q.StartUnixMs == 0 {
			now := time.UnixMilli(q.NowUnixMs).In(dashboardLocation)
			q.StartUnixMs = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, dashboardLocation).UnixMilli()
		}
	}
	out = auction.DashboardSnapshot{SnapshotAtUnixMs: q.NowUnixMs, Currency: "CNY", Range: auction.DashboardRangeWindow{Range: q.Range, Timezone: q.Timezone, StartUnixMs: q.StartUnixMs, EndUnixMs: q.EndUnixMs}, Trend: []auction.DashboardTrendPoint{}, TopLots: []auction.DashboardTopLot{}, Categories: []auction.DashboardCategoryPerformance{}, LotPerformance: []auction.DashboardLotPerformance{}, PartialErrors: []auction.DashboardPartialError{}}
	out.Summary, err = s.dashboardSummary(ctx, q)
	if err != nil {
		return auction.DashboardSnapshot{}, err
	}
	for _, section := range q.Sections {
		switch section {
		case auction.DashboardSectionFunnel:
			out.Funnel, err = s.dashboardFunnel(ctx, q)
		case auction.DashboardSectionTrend:
			out.Trend, err = s.dashboardTrend(ctx, q)
		case auction.DashboardSectionTopLots:
			out.TopLots, err = s.dashboardTopLots(ctx, q)
		case auction.DashboardSectionCategories:
			out.Categories, err = s.dashboardCategories(ctx, q)
		case auction.DashboardSectionOrderRisks:
			out.OrderRisks, err = s.dashboardOrderRisks(ctx, q)
		case auction.DashboardSectionLotPerformance:
			out.LotPerformance, err = s.dashboardLotPerformance(ctx, q)
		default:
			continue
		}
		if err != nil {
			out.PartialErrors = append(out.PartialErrors, auction.DashboardPartialError{Section: section, Code: "DATA_INCOMPLETE", Message: "This dashboard section could not be loaded."})
		}
	}
	return out, nil
}

func (s *Store) dashboardOrders(ctx context.Context, q auction.DashboardQuery) *gorm.DB {
	return s.db.WithContext(ctx).Model(&UserOrderModel{}).Where("main_account_id = ?", q.MainAccountID).Where("source = ?", userOrderSourceAuction)
}
func (s *Store) dashboardBids(ctx context.Context, q auction.DashboardQuery) *gorm.DB {
	return s.db.WithContext(ctx).Model(&AuctionBidModel{}).Where("main_account_id = ?", q.MainAccountID).Where("created_at_unix_ms >= ? AND created_at_unix_ms < ?", q.StartUnixMs, q.EndUnixMs)
}
func (s *Store) dashboardLots(ctx context.Context, q auction.DashboardQuery) *gorm.DB {
	return s.db.WithContext(ctx).Model(&AuctionLotModel{}).Where("main_account_id = ?", q.MainAccountID)
}

func (s *Store) dashboardSummary(ctx context.Context, q auction.DashboardQuery) (auction.DashboardSummary, error) {
	var out auction.DashboardSummary
	var paid struct{ Amount, Count int64 }
	err := s.dashboardOrders(ctx, q).Where("status = 'paid' AND payment_status = 'success' AND paid_at_unix_ms >= ? AND paid_at_unix_ms < ?", q.StartUnixMs, q.EndUnixMs).
		Select("COALESCE(SUM(total_amount),0) AS amount, COUNT(*) AS count").Scan(&paid).Error
	if err != nil {
		return out, err
	}
	var cohort struct{ OrderCount, CancelledCount, PendingAmount, PaidCount, PayableCount int64 }
	err = s.dashboardOrders(ctx, q).Where("created_at_unix_ms >= ? AND created_at_unix_ms < ?", q.StartUnixMs, q.EndUnixMs).
		Select(`COALESCE(SUM(CASE WHEN status <> 'cancelled' THEN 1 ELSE 0 END),0) AS order_count,
		COALESCE(SUM(CASE WHEN status = 'cancelled' THEN 1 ELSE 0 END),0) AS cancelled_count,
		COALESCE(SUM(CASE WHEN status = 'pending_payment' AND (expires_at_unix_ms = 0 OR expires_at_unix_ms > ?) THEN total_amount ELSE 0 END),0) AS pending_amount,
		COALESCE(SUM(CASE WHEN status = 'paid' AND payment_status = 'success' THEN 1 ELSE 0 END),0) AS paid_count,
		COALESCE(SUM(CASE WHEN status IN ('created','pending_payment','paid','expired') THEN 1 ELSE 0 END),0) AS payable_count`, q.NowUnixMs).Scan(&cohort).Error
	if err != nil {
		return out, err
	}
	var lots struct{ Started, Settled int64 }
	err = s.dashboardLots(ctx, q).Where("started_at_unix_ms > 0 AND started_at_unix_ms >= ? AND started_at_unix_ms < ?", q.StartUnixMs, q.EndUnixMs).
		Select("COUNT(*) AS started, COALESCE(SUM(CASE WHEN status = ? AND settled_at_unix_ms > 0 AND settled_at_unix_ms < ? THEN 1 ELSE 0 END),0) AS settled", int32(v1.LotStatus_LOT_STATUS_SETTLED), q.EndUnixMs).Scan(&lots).Error
	if err != nil {
		return out, err
	}
	var participants int64
	if err = s.dashboardBids(ctx, q).Distinct("user_id").Count(&participants).Error; err != nil {
		return out, err
	}
	out = auction.DashboardSummary{GMVAmount: paid.Amount, PaidAmount: paid.Amount, PendingAmount: cohort.PendingAmount, OrderCount: cohort.OrderCount, CancelledOrderCount: cohort.CancelledCount, ParticipantCount: participants, PaymentRate: auction.DashboardRatio{Numerator: cohort.PaidCount, Denominator: cohort.PayableCount}, DealRate: auction.DashboardRatio{Numerator: lots.Settled, Denominator: lots.Started}}
	if paid.Count > 0 {
		out.AverageDealAmount = paid.Amount / paid.Count
	}
	return out, nil
}

func (s *Store) dashboardOrderRisks(ctx context.Context, q auction.DashboardQuery) (auction.DashboardOrderRisks, error) {
	var out auction.DashboardOrderRisks
	err := s.dashboardOrders(ctx, q).Where("created_at_unix_ms >= ? AND created_at_unix_ms < ?", q.StartUnixMs, q.EndUnixMs).
		Select(`COALESCE(SUM(CASE WHEN status = 'pending_payment' AND (expires_at_unix_ms = 0 OR expires_at_unix_ms > ?) THEN 1 ELSE 0 END),0) AS pending_payment_count,
		COALESCE(SUM(CASE WHEN status = 'pending_payment' AND expires_at_unix_ms > ? AND expires_at_unix_ms <= ? THEN 1 ELSE 0 END),0) AS expiring_soon_count,
		COALESCE(SUM(CASE WHEN status = 'expired' OR (status IN ('created','pending_payment') AND expires_at_unix_ms > 0 AND expires_at_unix_ms <= ?) THEN 1 ELSE 0 END),0) AS expired_count,
		COALESCE(SUM(CASE WHEN payment_status = 'failed' THEN 1 ELSE 0 END),0) AS payment_failed_count`, q.NowUnixMs, q.NowUnixMs, q.NowUnixMs+600000, q.NowUnixMs).Scan(&out).Error
	return out, err
}

// Auction orders have one lot item. Grouping item totals also avoids multiplying
// order totals if an order ever has multiple item rows.
func (s *Store) dashboardPaidLots(ctx context.Context, q auction.DashboardQuery) *gorm.DB {
	return s.db.WithContext(ctx).Table("user_orders AS o").Where("o.main_account_id = ?", q.MainAccountID).
		Joins("JOIN user_order_items AS i ON i.order_id = o.id AND i.source = ?", userOrderSourceAuction).
		Where("o.source = ? AND o.status = 'paid' AND o.payment_status = 'success' AND o.paid_at_unix_ms >= ? AND o.paid_at_unix_ms < ?", userOrderSourceAuction, q.StartUnixMs, q.EndUnixMs).
		Select("i.lot_id, SUM(i.total_amount) AS paid_amount, COUNT(DISTINCT o.id) AS paid_orders").Group("i.lot_id")
}

func (s *Store) dashboardFunnel(ctx context.Context, q auction.DashboardQuery) (auction.DashboardFunnel, error) {
	var out auction.DashboardFunnel
	err := s.dashboardLots(ctx, q).Select(`
		COALESCE(SUM(CASE WHEN created_at >= ? AND created_at < ? THEN 1 ELSE 0 END),0) AS created_lot_count,
		COALESCE(SUM(CASE WHEN created_at >= ? AND created_at < ? AND (queue_status = ? OR started_at_unix_ms > 0) THEN 1 ELSE 0 END),0) AS listed_lot_count,
		COALESCE(SUM(CASE WHEN started_at_unix_ms > 0 AND started_at_unix_ms >= ? AND started_at_unix_ms < ? THEN 1 ELSE 0 END),0) AS started_lot_count,
		COALESCE(SUM(CASE WHEN started_at_unix_ms > 0 AND started_at_unix_ms >= ? AND started_at_unix_ms < ? AND status = ? AND settled_at_unix_ms > 0 AND settled_at_unix_ms < ? THEN 1 ELSE 0 END),0) AS settled_lot_count`,
		time.UnixMilli(q.StartUnixMs), time.UnixMilli(q.EndUnixMs), time.UnixMilli(q.StartUnixMs), time.UnixMilli(q.EndUnixMs), int32(v1.LotQueueStatus_LOT_QUEUE_STATUS_QUEUED), q.StartUnixMs, q.EndUnixMs, q.StartUnixMs, q.EndUnixMs, int32(v1.LotStatus_LOT_STATUS_SETTLED), q.EndUnixMs).Scan(&out).Error
	if err != nil {
		return out, err
	}
	if err = s.dashboardBids(ctx, q).Distinct("lot_id").Count(&out.BidLotCount).Error; err != nil {
		return out, err
	}
	err = s.dashboardLots(ctx, q).Where("id IN (?)", s.dashboardPaidLots(ctx, q).Select("i.lot_id")).Count(&out.PaidLotCount).Error
	return out, err
}

func (s *Store) dashboardTrend(ctx context.Context, q auction.DashboardQuery) ([]auction.DashboardTrendPoint, error) {
	start := time.UnixMilli(q.StartUnixMs).In(dashboardLocation)
	width := int64(time.Hour / time.Millisecond)
	n := 24
	switch q.Range {
	case auction.DashboardRange7D:
		width = 86400000
		n = 7
	case auction.DashboardRange30D:
		width = 86400000
		n = 30
	case auction.DashboardRangeLive:
		start = start.Truncate(time.Hour)
		n = int((q.EndUnixMs-start.UnixMilli())/width) + 1
		// Long-lived active rooms use daily buckets to keep the panel bounded.
		if n > 24*30 {
			width = 86400000
			start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, dashboardLocation)
			n = int((q.EndUnixMs-start.UnixMilli())/width) + 1
		}
		if n > 366 {
			width = ((int64(n) + 365) / 366) * 86400000
			n = int((q.EndUnixMs-start.UnixMilli())/width) + 1
		}
	}
	base := start.UnixMilli()
	out := make([]auction.DashboardTrendPoint, n)
	for i := range out {
		out[i] = auction.DashboardTrendPoint{StartUnixMs: base + int64(i)*width, EndUnixMs: base + int64(i+1)*width}
	}
	// FLOOR is supported by both SQLite and MySQL; integer casts differ by dialect.
	var paid []struct {
		Bucket         int
		Amount, Orders int64
	}
	err := s.dashboardOrders(ctx, q).Where("status = 'paid' AND payment_status = 'success' AND paid_at_unix_ms >= ? AND paid_at_unix_ms < ?", q.StartUnixMs, q.EndUnixMs).
		Select("FLOOR((paid_at_unix_ms - ?) / ?) AS bucket, SUM(total_amount) AS amount, COUNT(*) AS orders", base, width).Group("bucket").Scan(&paid).Error
	if err != nil {
		return nil, err
	}
	for _, p := range paid {
		if p.Bucket >= 0 && p.Bucket < n {
			out[p.Bucket].PaidAmount = p.Amount
			out[p.Bucket].PaidOrders = p.Orders
		}
	}
	var bids []struct {
		Bucket  int
		Bidders int64
	}
	err = s.dashboardBids(ctx, q).Select("FLOOR((created_at_unix_ms - ?) / ?) AS bucket, COUNT(DISTINCT user_id) AS bidders", base, width).Group("bucket").Scan(&bids).Error
	if err != nil {
		return nil, err
	}
	for _, b := range bids {
		if b.Bucket >= 0 && b.Bucket < n {
			out[b.Bucket].Bidders = b.Bidders
		}
	}
	return out, nil
}

type dashboardLotRow struct {
	LotID, Title, Payload                                                                             string
	Status                                                                                            int32
	StartedAtUnixMs, SettledAtUnixMs, FinalAmount, PaidAmount, PaidOrders, BidCount, ParticipantCount int64
}

func (s *Store) dashboardLotRows(ctx context.Context, q auction.DashboardQuery) *gorm.DB {
	paid := s.dashboardPaidLots(ctx, q)
	bids := s.dashboardBids(ctx, q).Select("lot_id, COUNT(*) AS bid_count, COUNT(DISTINCT user_id) AS participant_count").Group("lot_id")
	return s.db.WithContext(ctx).Table("auction_lots AS l").Where("l.main_account_id = ?", q.MainAccountID).
		Joins("LEFT JOIN (?) AS p ON p.lot_id = l.id", paid).Joins("LEFT JOIN (?) AS b ON b.lot_id = l.id", bids).
		Select(`l.id AS lot_id, l.title, l.payload, l.status, l.started_at_unix_ms, l.settled_at_unix_ms, l.final_price_amount AS final_amount,
		COALESCE(p.paid_amount,0) AS paid_amount, COALESCE(p.paid_orders,0) AS paid_orders, COALESCE(b.bid_count,0) AS bid_count, COALESCE(b.participant_count,0) AS participant_count`)
}

func dashboardCategory(payload string) (string, error) {
	var value struct {
		Category string `json:"category"`
	}
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return "", err
	}
	category := strings.TrimSpace(value.Category)
	if category == "" {
		category = "未分类"
	}
	return category, nil
}

func (s *Store) dashboardTopLots(ctx context.Context, q auction.DashboardQuery) ([]auction.DashboardTopLot, error) {
	var rows []dashboardLotRow
	if err := s.dashboardLotRows(ctx, q).Where("p.paid_orders > 0").Order("paid_amount DESC, l.id ASC").Limit(10).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]auction.DashboardTopLot, 0, len(rows))
	for _, r := range rows {
		category, err := dashboardCategory(r.Payload)
		if err != nil {
			return nil, err
		}
		out = append(out, auction.DashboardTopLot{LotID: r.LotID, Title: r.Title, Category: category, PaidAmount: r.PaidAmount, FinalAmount: r.FinalAmount, BidCount: r.BidCount, ParticipantCount: r.ParticipantCount})
	}
	return out, nil
}

func (s *Store) dashboardCategories(ctx context.Context, q auction.DashboardQuery) ([]auction.DashboardCategoryPerformance, error) {
	// Stream all qualifying lots: categories must not inherit Top/detail limits.
	rows, err := s.dashboardLotRows(ctx, q).Where("(l.started_at_unix_ms > 0 AND l.started_at_unix_ms >= ? AND l.started_at_unix_ms < ?) OR p.paid_orders > 0", q.StartUnixMs, q.EndUnixMs).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := map[string]*auction.DashboardCategoryPerformance{}
	for rows.Next() {
		var r dashboardLotRow
		if err = s.db.ScanRows(rows, &r); err != nil {
			return nil, err
		}
		category, e := dashboardCategory(r.Payload)
		if e != nil {
			return nil, e
		}
		g := groups[category]
		if g == nil {
			// All categories are returned, or the whole panel is marked incomplete.
			// Never present a silently truncated subset as complete performance.
			if len(groups) >= 1000 {
				return nil, fmt.Errorf("dashboard category limit exceeded")
			}
			g = &auction.DashboardCategoryPerformance{Category: category}
			groups[category] = g
		}
		g.PaidAmount += r.PaidAmount
		if r.StartedAtUnixMs > 0 && r.StartedAtUnixMs >= q.StartUnixMs && r.StartedAtUnixMs < q.EndUnixMs {
			g.StartedLotCount++
			if r.Status == int32(v1.LotStatus_LOT_STATUS_SETTLED) && r.SettledAtUnixMs > 0 && r.SettledAtUnixMs < q.EndUnixMs {
				g.SettledLotCount++
			}
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]auction.DashboardCategoryPerformance, 0, len(groups))
	for _, g := range groups {
		g.DealRate = auction.DashboardRatio{Numerator: g.SettledLotCount, Denominator: g.StartedLotCount}
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Category < out[j].Category })
	return out, nil
}

func (s *Store) dashboardLotPerformance(ctx context.Context, q auction.DashboardQuery) ([]auction.DashboardLotPerformance, error) {
	var rows []dashboardLotRow
	err := s.dashboardLotRows(ctx, q).Where("(l.created_at >= ? AND l.created_at < ?) OR (l.started_at_unix_ms > 0 AND l.started_at_unix_ms >= ? AND l.started_at_unix_ms < ?) OR b.bid_count > 0 OR p.paid_orders > 0", time.UnixMilli(q.StartUnixMs), time.UnixMilli(q.EndUnixMs), q.StartUnixMs, q.EndUnixMs).
		Order("paid_amount DESC, l.id ASC").Limit(100).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]auction.DashboardLotPerformance, 0, len(rows))
	for _, r := range rows {
		category, e := dashboardCategory(r.Payload)
		if e != nil {
			return nil, e
		}
		out = append(out, auction.DashboardLotPerformance{LotID: r.LotID, Title: r.Title, Category: category, Status: v1.LotStatus(r.Status).String(), BidCount: r.BidCount, ParticipantCount: r.ParticipantCount, FinalAmount: r.FinalAmount, PaidAmount: r.PaidAmount, ConversionRate: auction.DashboardRatio{Numerator: r.PaidOrders, Denominator: r.ParticipantCount}})
	}
	return out, nil
}
