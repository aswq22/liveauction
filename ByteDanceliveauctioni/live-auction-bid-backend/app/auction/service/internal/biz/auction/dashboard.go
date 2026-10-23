package auction

import (
	"fmt"
	"time"

	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

const dashboardTimezone = "Asia/Shanghai"

var dashboardShanghaiLocation = time.FixedZone(dashboardTimezone, 8*60*60)

type DashboardRange string

const (
	DashboardRangeToday DashboardRange = "today"
	DashboardRangeLive  DashboardRange = "live"
	DashboardRange7D    DashboardRange = "7d"
	DashboardRange30D   DashboardRange = "30d"
)

type DashboardSection string

const (
	DashboardSectionSummary        DashboardSection = "summary"
	DashboardSectionFunnel         DashboardSection = "funnel"
	DashboardSectionTrend          DashboardSection = "trend"
	DashboardSectionTopLots        DashboardSection = "topLots"
	DashboardSectionCategories     DashboardSection = "categoryPerformance"
	DashboardSectionOrderRisks     DashboardSection = "orderRisks"
	DashboardSectionLotPerformance DashboardSection = "lotPerformance"
)

var allDashboardSections = []DashboardSection{
	DashboardSectionSummary,
	DashboardSectionFunnel,
	DashboardSectionTrend,
	DashboardSectionTopLots,
	DashboardSectionCategories,
	DashboardSectionOrderRisks,
	DashboardSectionLotPerformance,
}

type DashboardQuery struct {
	MainAccountID string
	Range         DashboardRange
	Timezone      string
	Sections      []DashboardSection
	NowUnixMs     int64
	StartUnixMs   int64
	EndUnixMs     int64
}

// DashboardRatio keeps its source counts so callers never have to reconstruct a percentage
// from an already-rounded value.
type DashboardRatio struct {
	Numerator   int64
	Denominator int64
}

type DashboardSummary struct {
	GMVAmount           int64
	PaidAmount          int64
	PendingAmount       int64
	OrderCount          int64
	CancelledOrderCount int64
	ParticipantCount    int64
	AverageDealAmount   int64
	PaymentRate         DashboardRatio
	DealRate            DashboardRatio
}

type DashboardFunnel struct {
	CreatedLotCount int64
	ListedLotCount  int64
	StartedLotCount int64
	BidLotCount     int64
	SettledLotCount int64
	PaidLotCount    int64
}

type DashboardTrendPoint struct {
	StartUnixMs int64
	EndUnixMs   int64
	PaidAmount  int64
	PaidOrders  int64
	Bidders     int64
}

type DashboardTopLot struct {
	LotID            string
	Title            string
	Category         string
	PaidAmount       int64
	FinalAmount      int64
	BidCount         int64
	ParticipantCount int64
}

type DashboardCategoryPerformance struct {
	Category        string
	PaidAmount      int64
	StartedLotCount int64
	SettledLotCount int64
	DealRate        DashboardRatio
}

type DashboardOrderRisks struct {
	PendingPaymentCount int64
	ExpiringSoonCount   int64
	ExpiredCount        int64
	PaymentFailedCount  int64
}

type DashboardLotPerformance struct {
	LotID            string
	Title            string
	Category         string
	Status           string
	BidCount         int64
	ParticipantCount int64
	FinalAmount      int64
	PaidAmount       int64
	ConversionRate   DashboardRatio
}

type DashboardRangeWindow struct {
	Range       DashboardRange
	Timezone    string
	StartUnixMs int64
	EndUnixMs   int64
}

type DashboardPartialError struct {
	Section DashboardSection
	Code    string
	Message string
}

type DashboardSnapshot struct {
	SnapshotAtUnixMs int64
	Currency         string
	Range            DashboardRangeWindow
	Summary          DashboardSummary
	Funnel           DashboardFunnel
	Trend            []DashboardTrendPoint
	TopLots          []DashboardTopLot
	Categories       []DashboardCategoryPerformance
	OrderRisks       DashboardOrderRisks
	LotPerformance   []DashboardLotPerformance
	PartialErrors    []DashboardPartialError
}

func NormalizeDashboardQuery(query DashboardQuery) (DashboardQuery, error) {
	if query.NowUnixMs == 0 {
		query.NowUnixMs = time.Now().UnixMilli()
	}
	query.Timezone = dashboardTimezone
	query.EndUnixMs = query.NowUnixMs

	if query.Range == "" {
		query.Range = DashboardRangeToday
	}

	now := time.UnixMilli(query.NowUnixMs).In(dashboardShanghaiLocation)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, dashboardShanghaiLocation)
	switch query.Range {
	case DashboardRangeToday:
		query.StartUnixMs = dayStart.UnixMilli()
	case DashboardRange7D:
		query.StartUnixMs = dayStart.AddDate(0, 0, -6).UnixMilli()
	case DashboardRange30D:
		query.StartUnixMs = dayStart.AddDate(0, 0, -29).UnixMilli()
	case DashboardRangeLive:
		query.StartUnixMs = 0
	default:
		return DashboardQuery{}, fmt.Errorf("%w: unknown dashboard range %q", apperr.ErrInvalidArgument, query.Range)
	}

	if len(query.Sections) == 0 {
		query.Sections = append([]DashboardSection(nil), allDashboardSections...)
		return query, nil
	}

	sections := make([]DashboardSection, 0, len(query.Sections))
	seen := make(map[DashboardSection]struct{}, len(query.Sections))
	for _, section := range query.Sections {
		if !isDashboardSection(section) {
			return DashboardQuery{}, fmt.Errorf("%w: unknown dashboard section %q", apperr.ErrInvalidArgument, section)
		}
		if _, exists := seen[section]; exists {
			continue
		}
		seen[section] = struct{}{}
		sections = append(sections, section)
	}
	query.Sections = sections
	return query, nil
}

func isDashboardSection(section DashboardSection) bool {
	for _, allowed := range allDashboardSections {
		if section == allowed {
			return true
		}
	}
	return false
}
