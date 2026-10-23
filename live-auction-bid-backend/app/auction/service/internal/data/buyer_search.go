package data

import (
	"context"
	"errors"
	"fmt"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
	"gorm.io/gorm"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	bs "live-auction-bid/backend/app/auction/service/internal/biz/buyersearch"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"strconv"
	"time"
)

// NewBuyerSearchReader performs no migration or seeding. It is intended for
// explicit offline evaluation against a pre-provisioned database.
func NewBuyerSearchReader(db *gorm.DB, client *redis.Client) bs.Reader {
	return &Store{db: db, redis: client}
}

// ReadPublicFacts is a bounded, batched authorization and runtime read. Neither
// search-index visibility nor cached prices grant access to a lot.
func (s *Store) ReadPublicFacts(ctx context.Context, ids []string) (map[string]bs.Fact, error) {
	out := map[string]bs.Fact{}
	if len(ids) == 0 {
		return out, nil
	}
	if len(ids) > 400 {
		return nil, errors.New("search facts batch exceeds 400")
	}
	var rows []AuctionLotModel
	err := s.db.WithContext(ctx).Model(&AuctionLotModel{}).Select("auction_lots.*").
		Joins("JOIN auction_rooms AS r ON r.id=auction_lots.room_id AND r.main_account_id=auction_lots.main_account_id").
		Joins("JOIN auction_users AS u ON u.id=auction_lots.main_account_id").
		Where("auction_lots.id IN ? AND r.status=? AND u.status=?", ids, "ACTIVE", int32(v1.UserStatus_USER_STATUS_ACTIVE)).
		Where("EXISTS(SELECT 1 FROM auction_user_roles ur WHERE ur.user_id=u.id AND ur.role_code=?)", userbiz.RoleMerchantOwner).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	lots := make([]*v1.Lot, 0, len(rows))
	for _, row := range rows {
		lot, e := modelToLot(&row)
		if e != nil {
			return nil, e
		}
		lots = append(lots, lot)
	}
	if len(lots) == 0 {
		return out, nil
	}
	if s.redis == nil {
		return nil, errors.New("search runtime unavailable")
	}
	pipe := s.redis.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(lots))
	for i, lot := range lots {
		cmds[i] = pipe.HGetAll(ctx, runtimeStateKey(lot.Id))
	}
	if _, err = pipe.Exec(ctx); err != nil {
		return nil, err
	}
	now := time.Now()
	for i, lot := range lots {
		current, e := buyerSearchRuntimeFact(lot, cmds[i].Val(), now)
		if e != nil {
			return nil, e
		}
		if current != nil && bs.PublicStatus(current.Status) {
			out[current.Id] = bs.Fact{Lot: current, Public: true, ReadAtUnixMs: now.UnixMilli()}
		}
	}
	return out, nil
}
func buyerSearchRuntimeFact(base *v1.Lot, v map[string]string, now time.Time) (*v1.Lot, error) {
	if base == nil {
		return nil, errors.New("missing base lot")
	}
	if len(v) == 0 {
		if base.Status == v1.LotStatus_LOT_STATUS_LIVE || base.Status == v1.LotStatus_LOT_STATUS_EXTENDED {
			return nil, errors.New("live lot runtime missing")
		}
		if !bs.PublicStatus(base.Status) {
			return nil, nil
		}
		lot := proto.Clone(base).(*v1.Lot)
		if lot.GetRule().GetStartPrice() == nil {
			return nil, errors.New("queued start price missing")
		}
		lot.CurrentPrice = proto.Clone(lot.Rule.StartPrice).(*v1.Money)
		return lot, nil
	}
	if v["lot_id"] != base.Id || v["room_id"] != base.RoomId || v["main_account_id"] != base.MainAccountId {
		return nil, errors.New("search runtime identity mismatch")
	}
	for _, field := range []string{"status", "current_amount", "version", "ends_at_unix_ms"} {
		n, e := strconv.ParseInt(v[field], 10, 64)
		if e != nil || n < 0 {
			return nil, fmt.Errorf("invalid runtime %s", field)
		}
	}
	if v["current_currency"] != "CNY" {
		return nil, errors.New("invalid runtime currency")
	}
	lot := runtimeStateToLot(base, v)
	if lot.Version < base.Version {
		return nil, errors.New("runtime version behind SQL")
	}
	if !bs.PublicStatus(lot.Status) {
		return nil, nil
	}
	if (lot.Status == v1.LotStatus_LOT_STATUS_LIVE || lot.Status == v1.LotStatus_LOT_STATUS_EXTENDED) && lot.EndsAtUnixMs <= now.UnixMilli() {
		return nil, nil
	}
	if lot.Status == v1.LotStatus_LOT_STATUS_QUEUED {
		if lot.GetRule().GetStartPrice() == nil {
			return nil, errors.New("queued start price missing")
		}
		lot.CurrentPrice = proto.Clone(lot.Rule.StartPrice).(*v1.Money)
	}
	return lot, nil
}
