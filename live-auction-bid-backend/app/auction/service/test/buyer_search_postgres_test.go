package test

import (
	"context"
	"database/sql"
	"fmt"
	_ "github.com/lib/pq"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	bs "live-auction-bid/backend/app/auction/service/internal/biz/buyersearch"
	"live-auction-bid/backend/app/auction/service/internal/searchindex"
	"os"
	"testing"
	"time"
)

func TestBuyerSearchPostgresGenerationAndRevisionIsolation(t *testing.T) {
	dsn := os.Getenv("BUYER_SEARCH_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("real pgvector integration pending: BUYER_SEARCH_PG_TEST_DSN")
	}
	db, e := sql.Open("postgres", dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	suffix := fmt.Sprintf("it_%d", time.Now().UnixNano())
	g := bs.Generation{ID: suffix, Table: "buyer_search_docs_" + suffix, Model: "fixture", Dimensions: 2, MaxDistance: .4, ProviderVersion: "fixture", TemplateVersion: "fixture"}
	index := searchindex.NewHybridIndex(db)
	if e = index.CreateGeneration(ctx, g); e != nil {
		t.Fatal(e)
	}
	// Identifiers are exclusively constructed from the numeric test suffix.
	t.Cleanup(func() {
		db.Exec(`DELETE FROM buyer_search_generations WHERE id=$1`, g.ID)
		db.Exec(`DROP TABLE "` + g.Table + `"`)
	})
	d := bs.Document{LotID: "a", RoomID: "r", MainAccountID: "m", Text: "翡翠 手镯", Title: "翡翠手镯", Public: true, Status: v1.LotStatus_LOT_STATUS_QUEUED, Revision: bs.Revision{Semantic: 2, Metadata: 2}}
	d.Hash = bs.SemanticHash(d, g)
	if e = index.Upsert(ctx, g, d, []float64{1, 0}); e != nil {
		t.Fatal(e)
	}
	q, _ := bs.Parse(bs.Query{Text: "镯子", RoomID: "r"})
	cs, e := index.Vector(ctx, g, []float64{1, 0}, q.Filters, 50)
	if e != nil || len(cs) != 1 {
		t.Fatal(cs, e)
	}
	cs, e = index.Lexical(ctx, g, q, 50)
	if e != nil || len(cs) != 1 {
		t.Fatal(cs, e)
	}
	d.Deleted = true
	d.Public = false
	d.Revision.Metadata = 3
	if e = index.Upsert(ctx, g, d, nil); e != nil {
		t.Fatal(e)
	}
	d.Deleted = false
	d.Public = true
	d.Revision.Metadata = 2
	if e = index.Upsert(ctx, g, d, []float64{1, 0}); e != nil {
		t.Fatal(e)
	}
	cs, e = index.Vector(ctx, g, []float64{1, 0}, q.Filters, 50)
	if e != nil || len(cs) != 0 {
		t.Fatal("old job resurrected tombstone", cs, e)
	}
}
