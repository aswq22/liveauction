package main

import (
	"context"
	"database/sql"
	"errors"
	"github.com/lib/pq"
	bs "live-auction-bid/backend/app/auction/service/internal/biz/buyersearch"
	"live-auction-bid/backend/app/auction/service/internal/data"
	"live-auction-bid/backend/app/auction/service/internal/observability"
	"live-auction-bid/backend/app/auction/service/internal/searchindex"
	"log"
	"os"
	"strings"
	"time"
)

func newProductionBuyerSearch(ctx context.Context, store *data.Store) (*bs.Service, func(), error) {
	if !getenvBool("AUCTION_BUYER_SEARCH_ENABLED", false) {
		return nil, func() {}, nil
	}
	dsn := strings.TrimSpace(os.Getenv("AUCTION_SEARCH_PG_DSN"))
	key := []byte(os.Getenv("AUCTION_SEARCH_CURSOR_KEY"))
	if dsn == "" || len(key) < 32 {
		return nil, nil, errors.New("buyer search requires PostgreSQL DSN and cursor key >=32 bytes")
	}
	embed := searchindex.NewEmbeddingClientFromEnv(os.Getenv)
	if !embed.Configured() || strings.TrimSpace(os.Getenv("AUCTION_EMBEDDING_PROVIDER_VERSION")) == "" {
		return nil, nil, errors.New("buyer search requires configured embedding provider")
	}
	index, closeIndex, e := openProductionSearchPostgres(ctx, dsn, embed)
	if e != nil {
		return nil, nil, e
	}
	startup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if e = store.BuyerSearchJobs().CheckSchema(startup); e != nil {
		closeIndex()
		return nil, nil, errors.New("buyer search requires explicit MySQL job migration and all capture triggers")
	}
	worker := searchindex.NewReliableWorker(store.BuyerSearchJobs(), index, embed)
	worker.Start(ctx)
	service := bs.NewService(index, embed, store, store.BuyerSearchCache(), bs.Config{CursorKey: key, MaxConcurrent: getenvInt("AUCTION_SEARCH_MAX_CONCURRENT", 128), Observer: observability.BuyerSearchObserver{}})
	return service, closeIndex, nil
}
func openProductionSearchPostgres(ctx context.Context, dsn string, embed bs.Embedder) (*searchindex.HybridIndex, func(), error) {
	connector, e := pq.NewConnector(dsn)
	if e != nil {
		return nil, nil, errors.New("buyer search PostgreSQL configuration invalid")
	}
	db := sql.OpenDB(connector)
	closeDB := func() { db.Close() }
	fail := func(e error) (*searchindex.HybridIndex, func(), error) { closeDB(); return nil, nil, e }
	db.SetMaxOpenConns(getenvInt("AUCTION_SEARCH_PG_MAX_OPEN_CONNS", 20))
	db.SetMaxIdleConns(getenvInt("AUCTION_SEARCH_PG_MAX_IDLE_CONNS", 5))
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(2 * time.Minute)
	index := searchindex.NewHybridIndex(db)
	startup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if e = db.PingContext(startup); e != nil {
		log.Print("buyer search PostgreSQL temporarily unavailable; core startup continues")
		return index, closeDB, nil
	}
	g, e := index.Active(startup)
	if e != nil {
		var pe *pq.Error
		if errors.Is(e, sql.ErrNoRows) || (errors.As(e, &pe) && pe.Code == "42P01") {
			return fail(errors.New("buyer search migration or validated active generation missing"))
		}
		log.Print("buyer search temporarily unavailable; core startup continues")
		return index, closeDB, nil
	}
	if _, e = bs.ResolveEmbedder(embed, g); e != nil {
		return fail(errors.New("buyer search deployment does not match active generation"))
	}
	return index, closeDB, nil
}
