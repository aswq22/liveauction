package main

import (
	"database/sql"
	"errors"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	bs "live-auction-bid/backend/app/auction/service/internal/biz/buyersearch"
	"live-auction-bid/backend/app/auction/service/internal/data"
	"live-auction-bid/backend/app/auction/service/internal/searchindex"
	appsvc "live-auction-bid/backend/app/auction/service/internal/service"
	"os"
)

func newDirectEvaluation(generation string) (*appsvc.AuctionService, *searchindex.HybridIndex, *searchindex.EmbeddingClient, func(), error) {
	if os.Getenv("AUCTION_SEARCH_PG_DSN") == "" || os.Getenv("AUCTION_MYSQL_DSN") == "" || os.Getenv("AUCTION_REDIS_ADDR") == "" {
		return nil, nil, nil, nil, errors.New("direct evaluation requires explicit PostgreSQL, MySQL and Redis configuration")
	}
	pg, e := sql.Open("postgres", os.Getenv("AUCTION_SEARCH_PG_DSN"))
	if e != nil {
		return nil, nil, nil, nil, e
	}
	pg.SetMaxOpenConns(20)
	db, e := gorm.Open(mysql.Open(os.Getenv("AUCTION_MYSQL_DSN")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		pg.Close()
		return nil, nil, nil, nil, errors.New("evaluation MySQL unavailable")
	}
	sqlDB, e := db.DB()
	if e != nil {
		pg.Close()
		return nil, nil, nil, nil, e
	}
	sqlDB.SetMaxOpenConns(20)
	client := redis.NewClient(&redis.Options{Addr: os.Getenv("AUCTION_REDIS_ADDR"), Password: os.Getenv("AUCTION_REDIS_PASSWORD"), PoolSize: 128})
	closeAll := func() { pg.Close(); sqlDB.Close(); client.Close() }
	index := searchindex.NewHybridIndex(pg).ForValidation(generation)
	embed := searchindex.NewEmbeddingClientFromEnv(os.Getenv)
	if !embed.Configured() {
		closeAll()
		return nil, nil, nil, nil, errors.New("evaluation requires embedding provider")
	}
	reader := data.NewBuyerSearchReader(db, client)
	service := appsvc.NewAuctionService(nil).SetBuyerRetrieval(bs.NewService(index, embed, reader, nil, bs.Config{}))
	return service, index, embed, closeAll, nil
}
