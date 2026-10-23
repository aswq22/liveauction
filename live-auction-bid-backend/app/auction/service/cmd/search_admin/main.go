// search_admin performs explicit, operator-controlled index lifecycle actions.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	_ "github.com/lib/pq"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	bs "live-auction-bid/backend/app/auction/service/internal/biz/buyersearch"
	"live-auction-bid/backend/app/auction/service/internal/data"
	"live-auction-bid/backend/app/auction/service/internal/searchindex"
	"os"
	"os/signal"
	"time"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	action := flag.String("action", "status", "create, backfill, work, validate, activate, status, retry-dead")
	id := flag.String("generation", "", "generation or reconciliation run name")
	table := flag.String("table", "", "buyer_search_docs_<generation>")
	threshold := flag.Float64("max-distance", 0, "calibrated cosine distance ceiling")
	template := flag.String("template", "lot-v1", "content template version")
	provider := flag.String("provider-version", "", "provider/model deployment version")
	reportPath := flag.String("report", "", "operator-reviewed acceptance report JSON")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	pg, e := sql.Open("postgres", os.Getenv("AUCTION_SEARCH_PG_DSN"))
	if e != nil {
		return errors.New("invalid search PostgreSQL configuration")
	}
	defer pg.Close()
	pg.SetMaxOpenConns(5)
	startup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if e = pg.PingContext(startup); e != nil {
		return errors.New("search PostgreSQL unavailable")
	}
	index := searchindex.NewHybridIndex(pg)
	embed := searchindex.NewEmbeddingClientFromEnv(os.Getenv)
	switch *action {
	case "create":
		if !embed.Configured() || *provider == "" {
			return errors.New("configured embedding provider and provider-version required")
		}
		g := bs.Generation{ID: *id, Table: *table, Model: embed.Model(), Dimensions: embed.Dimensions(), TemplateVersion: *template, ProviderVersion: *provider, MaxDistance: *threshold}
		if _, e := bs.ResolveEmbedder(embed, g); e != nil {
			return errors.New("generation provider-version must match configured embedding deployment")
		}
		return index.CreateGeneration(ctx, g)
	case "activate":
		return index.ActivateGeneration(ctx, *id)
	case "validate":
		b, e := os.ReadFile(*reportPath)
		if e != nil {
			return e
		}
		var r searchindex.AcceptanceReport
		if e = json.Unmarshal(b, &r); e != nil {
			return e
		}
		if r.GenerationID != *id {
			return errors.New("report generation does not match")
		}
		return index.ValidateGeneration(ctx, r)
	case "status":
		rows, e := pg.QueryContext(ctx, `SELECT id,ready,active,writable FROM buyer_search_generations ORDER BY created_at`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var ready, active, writable bool
			if e = rows.Scan(&id, &ready, &active, &writable); e != nil {
				return e
			}
			fmt.Printf("%s ready=%t active=%t writable=%t\n", id, ready, active, writable)
		}
		return rows.Err()
	}
	if os.Getenv("AUCTION_MYSQL_DSN") == "" {
		return errors.New("AUCTION_MYSQL_DSN required")
	}
	db, e := gorm.Open(mysql.Open(os.Getenv("AUCTION_MYSQL_DSN")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		return errors.New("search MySQL unavailable")
	}
	sqlDB, e := db.DB()
	if e != nil {
		return e
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(5)
	jobs := data.NewBuyerSearchJobs(db)
	if e = jobs.CheckSchema(startup); e != nil {
		return errors.New("search MySQL migration missing")
	}
	switch *action {
	case "backfill":
		for {
			done, e := jobs.BackfillGenerationPage(ctx, *id)
			if e != nil {
				return e
			}
			if done {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	case "work":
		w := searchindex.NewReliableWorker(jobs, index, embed)
		w.Start(ctx)
		<-ctx.Done()
		return nil
	case "retry-dead":
		if *id == "" {
			return errors.New("explicit generation required for audit")
		}
		r := db.WithContext(ctx).Exec(`UPDATE buyer_search_jobs SET state='pending',attempts=0,next_attempt_at=NOW(6),lease_token='',lease_until=NULL WHERE state='dead'`)
		fmt.Printf("requeued=%d generation=%s\n", r.RowsAffected, *id)
		return r.Error
	default:
		return errors.New("unknown search admin action")
	}
}
