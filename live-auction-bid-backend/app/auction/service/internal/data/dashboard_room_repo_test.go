package data

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"live-auction-bid/backend/app/auction/service/internal/pkg/idgen"
)

func TestEnsureDefaultRoomReusesEarliestRoomWithoutTenantUniqueConstraint(t *testing.T) {
	s := dashboardTestStore(t)
	if err := s.db.AutoMigrate(&AuctionUserModel{}, &AuctionUserRoleModel{}, &AuctionRoomStateModel{}); err != nil {
		t.Fatal(err)
	}
	dashboardInsert(t, s, &AuctionUserModel{ID: "merchant-a", Username: "merchant-a"})
	first, err := s.EnsureDefaultRoom(context.Background(), "merchant-a", "merchant-a", dashboardNow)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EnsureDefaultRoom(context.Background(), "merchant-a", "merchant-a", dashboardNow+1)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("repeated ensure created %s then %s", first.ID, second.ID)
	}
	dashboardInsert(t, s, &AuctionRoomModel{ID: "extra", MainAccountID: "merchant-a", CreatedAtUnixMs: dashboardNow + 10})
	third, err := s.EnsureDefaultRoom(context.Background(), "merchant-a", "merchant-a", dashboardNow+11)
	if err != nil {
		t.Fatal(err)
	}
	var rooms, states int64
	if err = s.db.Model(&AuctionRoomModel{}).Where("main_account_id = ?", "merchant-a").Count(&rooms).Error; err != nil {
		t.Fatal(err)
	}
	if err = s.db.Model(&AuctionRoomStateModel{}).Where("main_account_id = ?", "merchant-a").Count(&states).Error; err != nil {
		t.Fatal(err)
	}
	if third.ID != first.ID || rooms != 2 || states != 1 || third.LiveSourceURL == "" {
		t.Fatalf("room/state = %#v %d/%d", third, rooms, states)
	}
}

func dashboardMySQLRoomStore(t *testing.T) (*Store, string) {
	t.Helper()
	db := openAgentApprovalMySQL(t).Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	s := &Store{db: db}
	if err := db.AutoMigrate(&AuctionUserModel{}, &AuctionUserRoleModel{}, &AuctionRoomStateModel{}); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateAuctionRooms(context.Background()); err != nil {
		t.Fatal(err)
	}
	merchant := idgen.New("dashboard-test")
	dashboardInsert(t, s, &AuctionUserModel{ID: merchant, Username: merchant})
	t.Cleanup(func() {
		for _, model := range []any{&AuctionRoomStateModel{}, &AuctionRoomModel{}} {
			if err := db.Where("main_account_id = ?", merchant).Delete(model).Error; err != nil {
				t.Error(err)
			}
		}
		if err := db.Where("id = ?", merchant).Delete(&AuctionUserModel{}).Error; err != nil {
			t.Error(err)
		}
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, merchant
}

func TestEnsureDefaultRoomMySQLSerializesConcurrentFirstCallsPerMerchant(t *testing.T) {
	s, merchant := dashboardMySQLRoomStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	ids := make(chan string, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			room, err := s.EnsureDefaultRoom(ctx, merchant, merchant, dashboardNow)
			if err != nil {
				errs <- err
				return
			}
			ids <- room.ID
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Error(err)
	}
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	var count int64
	if err := s.db.Model(&AuctionRoomModel{}).Where("main_account_id = ?", merchant).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(unique) != 1 {
		t.Fatalf("concurrent ensure created %d rooms, returned %d IDs", count, len(unique))
	}
}

func TestEnsureDefaultRoomMySQLCannotCreateWhileMerchantRowIsLocked(t *testing.T) {
	s, merchant := dashboardMySQLRoomStore(t)
	tx := s.db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	var main AuctionUserModel
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", merchant).First(&main).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := s.EnsureDefaultRoom(ctx, merchant, merchant, dashboardNow)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ensure bypassed locked merchant: %v", err)
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := s.db.Model(&AuctionRoomModel{}).Where("main_account_id = ?", merchant).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("timed-out ensure left %d rooms", count)
	}
}
