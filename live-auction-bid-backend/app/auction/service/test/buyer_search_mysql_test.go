package test

import (
	"context"
	"fmt"
	drivermysql "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"live-auction-bid/backend/app/auction/service/internal/data"
	"os"
	"strings"
	"testing"
	"time"
)

// This deliberately does not install schema, seed a production DB, or treat an
// absent database as production acceptance. Use a dedicated migrated test DB.
func TestBuyerSearchMySQLCaptureRollbackAndTombstones(t *testing.T) {
	dsn := os.Getenv("BUYER_SEARCH_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("real MySQL integration pending: BUYER_SEARCH_MYSQL_TEST_DSN")
	}
	cfg, e := drivermysql.ParseDSN(dsn)
	if e != nil || !strings.Contains(strings.ToLower(cfg.DBName), "test") {
		t.Fatal("must use a dedicated test database")
	}
	db, e := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	jobs := data.NewBuyerSearchJobs(db)
	ctx := context.Background()
	if e = jobs.CheckSchema(ctx); e != nil {
		t.Fatal(e)
	}
	id := fmt.Sprintf("search-it-%d", time.Now().UnixNano())
	lot := data.AuctionLotModel{ID: id, RoomID: id, MainAccountID: id, Title: "test", Description: "test", Status: 6, StartPriceCurrency: "CNY", CurrentPriceCurrency: "CNY", MinIncrementCurrency: "CNY", Payload: `{}`}
	t.Cleanup(func() {
		db.Delete(&data.AuctionLotModel{}, "id=?", id)
		db.Exec("DELETE FROM buyer_search_jobs WHERE lot_id=?", id)
	})
	tx := db.Begin()
	if e = tx.Create(&lot).Error; e != nil {
		tx.Rollback()
		t.Fatal(e)
	}
	var count int64
	tx.Raw("SELECT COUNT(*) FROM buyer_search_jobs WHERE lot_id=?", id).Scan(&count)
	if count != 1 {
		tx.Rollback()
		t.Fatal("capture not in writer transaction")
	}
	tx.Rollback()
	db.Raw("SELECT COUNT(*) FROM buyer_search_jobs WHERE lot_id=?", id).Scan(&count)
	if count != 0 {
		t.Fatal("rollback leaked durable job")
	}
	if e = db.Create(&lot).Error; e != nil {
		t.Fatal(e)
	}
	var revision struct {
		SemanticRevision, MetadataRevision uint64
		Deleted                            bool
		State                              string
	}
	read := func() {
		if e = db.Raw("SELECT semantic_revision,metadata_revision,deleted,state FROM buyer_search_jobs WHERE lot_id=?", id).Scan(&revision).Error; e != nil {
			t.Fatal(e)
		}
	}
	read()
	if revision.MetadataRevision != 1 {
		t.Fatal(revision)
	}
	db.Model(&data.AuctionLotModel{}).Where("id=?", id).Update("current_price_amount", 200)
	read()
	if revision.SemanticRevision != 1 || revision.MetadataRevision != 1 {
		t.Fatal("price update re-embedded", revision)
	}
	db.Model(&data.AuctionLotModel{}).Where("id=?", id).Update("title", "changed")
	read()
	if revision.SemanticRevision != 2 || revision.MetadataRevision != 2 {
		t.Fatal("semantic update lost", revision)
	}
	db.Delete(&data.AuctionLotModel{}, "id=?", id)
	read()
	if !revision.Deleted || revision.MetadataRevision != 3 || revision.State != "pending" {
		t.Fatal("delete tombstone missing", revision)
	}
}
