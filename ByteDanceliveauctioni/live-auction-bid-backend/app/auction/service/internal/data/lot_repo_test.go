package data

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	auctionv1 "live-auction-bid/backend/api/auction/service/v1"
	"live-auction-bid/backend/app/auction/service/internal/biz/auction"
)

func TestListLotsOrdersByUpdatedAtBeforeQueuedLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Error(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
	gormDB, err := gorm.Open(mysql.New(mysql.Config{Conn: db, SkipInitializeWithVersion: true}), &gorm.Config{SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	store := &Store{db: gormDB}

	mock.ExpectQuery("SELECT count\\(\\*\\) FROM `auction_lots` WHERE main_account_id = \\? AND status = \\?").
		WithArgs("merchant-a", int32(auctionv1.LotStatus_LOT_STATUS_QUEUED)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(150))
	mock.ExpectQuery("SELECT .* FROM `auction_lots` WHERE main_account_id = \\? AND status = \\? ORDER BY updated_at DESC,id ASC LIMIT \\?").
		WithArgs("merchant-a", int32(auctionv1.LotStatus_LOT_STATUS_QUEUED), 100).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	_, err = store.ListLots(context.Background(), auction.LotQuery{
		Page: 1, PageSize: 100, MainAccountID: "merchant-a",
		Status: auctionv1.LotStatus_LOT_STATUS_QUEUED, OrderByUpdatedAt: true,
	})
	if err != nil {
		t.Fatalf("ListLots() error = %v", err)
	}
}
