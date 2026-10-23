package searchindex

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	bs "live-auction-bid/backend/app/auction/service/internal/biz/buyersearch"
	"testing"
)

func TestIndexGenerationIsolation(t *testing.T) {
	db, m, e := sqlmock.New()
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	idx := NewHybridIndex(db)
	m.ExpectQuery("SELECT.*buyer_search_generations").WillReturnRows(sqlmock.NewRows([]string{"id", "table_name", "model", "provider_version", "template_version", "dimensions", "max_distance"}).AddRow("g1", "buyer_search_docs_g1", "m", "1", "1", 2, 0.4))
	g, e := idx.Active(context.Background())
	if e != nil || g.ID != "g1" {
		t.Fatal(g, e)
	}
	_, e = idx.Lexical(context.Background(), bs.Generation{Table: "docs;DROP TABLE users"}, bs.ParsedQuery{}, 20)
	if e == nil {
		t.Fatal("unsafe table accepted")
	}
	if e = m.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}
func TestGenerationRejectsIncompatibleWritableDeployment(t *testing.T) {
	db, m, _ := sqlmock.New()
	defer db.Close()
	m.ExpectBegin()
	m.ExpectExec("SELECT pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectQuery("SELECT COUNT.*buyer_search_generations").WithArgs("deployment-new").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	m.ExpectRollback()
	idx := NewHybridIndex(db)
	err := idx.CreateGeneration(context.Background(), bs.Generation{ID: "g", Table: "buyer_search_docs_g", Model: "m", ProviderVersion: "deployment-new", Dimensions: 2, MaxDistance: .4})
	if err == nil {
		t.Fatal("incompatible writable deployment accepted")
	}
	if err = m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestVectorRejectsInvalidDimensions(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	idx := NewHybridIndex(db)
	_, e := idx.Vector(context.Background(), bs.Generation{Table: "buyer_search_docs_g1", Dimensions: 2}, []float64{1}, bs.Filters{}, 20)
	if e == nil {
		t.Fatal("dimension mismatch accepted")
	}
}
