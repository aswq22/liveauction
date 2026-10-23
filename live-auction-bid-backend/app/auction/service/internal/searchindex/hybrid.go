package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/lib/pq"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	bs "live-auction-bid/backend/app/auction/service/internal/biz/buyersearch"
	"math"
	"regexp"
	"strings"
)

var generationTable = regexp.MustCompile(`^buyer_search_docs_[a-z0-9_]{1,48}$`)

type HybridIndex struct {
	db           *sql.DB
	generationID string
	exact        bool
}

func NewHybridIndex(db *sql.DB) *HybridIndex { return &HybridIndex{db: db} }

// ForValidation pins a non-active generation for offline evaluation only.
// Production bootstrap never calls this method.
func (i *HybridIndex) ForValidation(id string) *HybridIndex {
	return &HybridIndex{db: i.db, generationID: id}
}
func (i *HybridIndex) ExactVector(ctx context.Context, g bs.Generation, v []float64, f bs.Filters, limit int) ([]bs.Candidate, error) {
	exact := &HybridIndex{db: i.db, exact: true}
	return exact.Vector(ctx, g, v, f, limit)
}
func (i *HybridIndex) WritableGenerations(ctx context.Context) ([]bs.Generation, error) {
	rows, e := i.db.QueryContext(ctx, `SELECT id,table_name,model,provider_version,template_version,dimensions,max_distance FROM buyer_search_generations WHERE writable=TRUE ORDER BY id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []bs.Generation{}
	for rows.Next() {
		var g bs.Generation
		if e = rows.Scan(&g.ID, &g.Table, &g.Model, &g.ProviderVersion, &g.TemplateVersion, &g.Dimensions, &g.MaxDistance); e != nil {
			return nil, e
		}
		if _, e = tableFor(g); e != nil {
			return nil, e
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
func (i *HybridIndex) HasEmbedding(ctx context.Context, g bs.Generation, d bs.Document) (bool, error) {
	table, e := tableFor(g)
	if e != nil {
		return false, e
	}
	var found bool
	e = i.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE lot_id=$1 AND embedding_hash=$2 AND semantic_revision=$3 AND embedding IS NOT NULL AND NOT deleted)`, table), d.LotID, d.Hash, d.Revision.Semantic).Scan(&found)
	return found, e
}
func (i *HybridIndex) Active(ctx context.Context) (bs.Generation, error) {
	var g bs.Generation
	query := `SELECT id,table_name,model,provider_version,template_version,dimensions,max_distance FROM buyer_search_generations WHERE active=TRUE`
	args := []any{}
	if i.generationID != "" {
		query = `SELECT id,table_name,model,provider_version,template_version,dimensions,max_distance FROM buyer_search_generations WHERE id=$1`
		args = append(args, i.generationID)
	}
	err := i.db.QueryRowContext(ctx, query, args...).Scan(&g.ID, &g.Table, &g.Model, &g.ProviderVersion, &g.TemplateVersion, &g.Dimensions, &g.MaxDistance)
	if err != nil {
		return g, err
	}
	if !generationTable.MatchString(g.Table) || g.Dimensions < 1 || g.Dimensions > 2000 || g.MaxDistance <= 0 || g.MaxDistance > 2 {
		return g, errors.New("invalid active search generation")
	}
	return g, nil
}
func tableFor(g bs.Generation) (string, error) {
	if !generationTable.MatchString(g.Table) {
		return "", errors.New("invalid search table")
	}
	return pq.QuoteIdentifier(g.Table), nil
}
func statuses(f bs.Filters) []int64 {
	out := []int64{}
	for _, v := range f.Statuses {
		if bs.PublicStatus(v) {
			out = append(out, int64(v))
		}
	}
	if len(out) == 0 {
		out = []int64{int64(v1.LotStatus_LOT_STATUS_LIVE), int64(v1.LotStatus_LOT_STATUS_QUEUED), int64(v1.LotStatus_LOT_STATUS_EXTENDED)}
	}
	return out
}
func validVector(v []float64, n int) bool {
	if len(v) != n {
		return false
	}
	var sum float64
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
		sum += x * x
	}
	return sum > 0 && !math.IsInf(sum, 0)
}
func (i *HybridIndex) Vector(ctx context.Context, g bs.Generation, v []float64, f bs.Filters, limit int) ([]bs.Candidate, error) {
	table, e := tableFor(g)
	if e != nil {
		return nil, e
	}
	if !validVector(v, g.Dimensions) {
		return nil, errors.New("invalid query vector")
	}
	if limit < 1 || limit > 400 {
		return nil, errors.New("invalid candidate limit")
	}
	tx, e := i.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SET LOCAL hnsw.ef_search=100; SET LOCAL hnsw.iterative_scan=strict_order; SET LOCAL hnsw.max_scan_tuples=20000; SET LOCAL statement_timeout='200ms'`); e != nil {
		return nil, e
	}
	var eligible int
	if e = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM (SELECT 1 FROM %s WHERE public_visible=TRUE AND NOT deleted AND embedding IS NOT NULL AND ($1='' OR room_id=$1) AND status=ANY($2) LIMIT 2001) eligible`, table), f.RoomID, pq.Array(statuses(f))).Scan(&eligible); e != nil {
		return nil, e
	}
	order := "embedding <=> $1::vector"
	if eligible <= 2000 || i.exact {
		order = "(embedding <=> $1::vector)+0"
	}
	rows, e := tx.QueryContext(ctx, fmt.Sprintf(`SELECT lot_id,embedding <=> $1::vector FROM %s WHERE public_visible=TRUE AND NOT deleted AND embedding IS NOT NULL AND ($2='' OR room_id=$2) AND status=ANY($3) ORDER BY %s LIMIT $4`, table, order), VectorLiteral(v), f.RoomID, pq.Array(statuses(f)), limit)
	if e != nil {
		return nil, e
	}
	out := []bs.Candidate{}
	for rows.Next() {
		var c bs.Candidate
		var d float64
		if e = rows.Scan(&c.LotID, &d); e != nil {
			rows.Close()
			return nil, e
		}
		c.Distance = &d
		out = append(out, c)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return out, nil
}
func (i *HybridIndex) Lexical(ctx context.Context, g bs.Generation, q bs.ParsedQuery, limit int) ([]bs.Candidate, error) {
	table, e := tableFor(g)
	if e != nil {
		return nil, e
	}
	if limit < 1 || limit > 400 {
		return nil, errors.New("invalid candidate limit")
	}
	tokens := bs.Tokenize(strings.Join(q.Tokens, " "))
	if len(tokens) == 0 {
		return []bs.Candidate{}, nil
	}
	// Each token is letters/digits from our tokenizer; bind the tsquery as data.
	query := strings.Join(tokens, " | ")
	rows, e := i.db.QueryContext(ctx, fmt.Sprintf(`SELECT lot_id,ts_rank_cd(lexical,to_tsquery('simple',$1)) AS score FROM %s WHERE public_visible=TRUE AND NOT deleted AND ($2='' OR room_id=$2) AND status=ANY($3) AND lexical @@ to_tsquery('simple',$1) ORDER BY score DESC,lot_id ASC LIMIT $4`, table), query, q.Filters.RoomID, pq.Array(statuses(q.Filters)), limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []bs.Candidate{}
	for rows.Next() {
		var c bs.Candidate
		if e = rows.Scan(&c.LotID, &c.LexicalScore); e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (i *HybridIndex) Upsert(ctx context.Context, g bs.Generation, d bs.Document, v []float64) error {
	table, e := tableFor(g)
	if e != nil {
		return e
	}
	if d.LotID == "" || d.Revision.Metadata == 0 || d.Revision.Semantic == 0 {
		return errors.New("invalid document revision")
	}
	var vector any
	if len(v) > 0 {
		if !validVector(v, g.Dimensions) {
			return errors.New("invalid document vector")
		}
		vector = VectorLiteral(v)
	}
	lexicalTitle := strings.Join(bs.Tokenize(d.Title), " ")
	lexicalTags := strings.Join(bs.Tokenize(d.Category+" "+strings.Join(d.Tags, " ")), " ")
	lexicalBody := strings.Join(bs.Tokenize(d.Text), " ")
	_, e = i.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s AS current (lot_id,room_id,main_account_id,title,search_text,status,public_visible,deleted,semantic_revision,metadata_revision,embedding_hash,embedding,lexical) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::vector,setweight(to_tsvector('simple',$13),'A')||setweight(to_tsvector('simple',$14),'B')||setweight(to_tsvector('simple',$15),'D')) ON CONFLICT(lot_id) DO UPDATE SET room_id=EXCLUDED.room_id,main_account_id=EXCLUDED.main_account_id,status=EXCLUDED.status,public_visible=EXCLUDED.public_visible,deleted=EXCLUDED.deleted,metadata_revision=EXCLUDED.metadata_revision,title=CASE WHEN EXCLUDED.semantic_revision>=current.semantic_revision THEN EXCLUDED.title ELSE current.title END,search_text=CASE WHEN EXCLUDED.semantic_revision>=current.semantic_revision THEN EXCLUDED.search_text ELSE current.search_text END,lexical=CASE WHEN EXCLUDED.semantic_revision>=current.semantic_revision THEN EXCLUDED.lexical ELSE current.lexical END,embedding=CASE WHEN EXCLUDED.deleted OR NOT EXCLUDED.public_visible THEN NULL WHEN EXCLUDED.semantic_revision>current.semantic_revision THEN EXCLUDED.embedding WHEN EXCLUDED.semantic_revision=current.semantic_revision AND EXCLUDED.embedding IS NOT NULL THEN EXCLUDED.embedding ELSE current.embedding END,embedding_hash=CASE WHEN EXCLUDED.semantic_revision>=current.semantic_revision THEN EXCLUDED.embedding_hash ELSE current.embedding_hash END,semantic_revision=GREATEST(current.semantic_revision,EXCLUDED.semantic_revision),indexed_at=NOW() WHERE EXCLUDED.metadata_revision>=current.metadata_revision`, table), d.LotID, d.RoomID, d.MainAccountID, d.Title, d.Text, int32(d.Status), d.Public, d.Deleted, d.Revision.Semantic, d.Revision.Metadata, d.Hash, vector, lexicalTitle, lexicalTags, lexicalBody)
	return e
}
func (i *HybridIndex) CreateGeneration(ctx context.Context, g bs.Generation) error {
	table, e := tableFor(g)
	if e != nil {
		return e
	}
	if g.ID == "" || g.Dimensions < 1 || g.Dimensions > 2000 || g.Model == "" || g.MaxDistance <= 0 || g.MaxDistance > 2 {
		return errors.New("invalid generation")
	}
	tx, e := i.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	// Serialize creation so concurrent operators cannot introduce incompatible
	// provider deployments into the shared indexing queue.
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(72649202)`); e != nil {
		return e
	}
	var incompatible int
	if e = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM buyer_search_generations WHERE writable=TRUE AND provider_version<>$1`, g.ProviderVersion).Scan(&incompatible); e != nil {
		return e
	}
	if incompatible > 0 {
		return errors.New("writable generations require the same configured provider deployment")
	}
	if _, e = tx.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE %s(lot_id varchar(64) PRIMARY KEY,room_id varchar(64) NOT NULL,main_account_id varchar(64) NOT NULL,title text NOT NULL,search_text text NOT NULL,status integer NOT NULL,public_visible boolean NOT NULL,deleted boolean NOT NULL DEFAULT FALSE,semantic_revision bigint NOT NULL,metadata_revision bigint NOT NULL,embedding_hash text NOT NULL,embedding vector(%d),lexical tsvector NOT NULL,indexed_at timestamptz NOT NULL DEFAULT NOW())`, table, g.Dimensions)); e != nil {
		return e
	}
	for _, ddl := range []string{fmt.Sprintf(`CREATE INDEX ON %s USING hnsw(embedding vector_cosine_ops) WHERE public_visible AND NOT deleted`, table), fmt.Sprintf(`CREATE INDEX ON %s USING gin(lexical)`, table), fmt.Sprintf(`CREATE INDEX ON %s(room_id,public_visible,status)`, table), fmt.Sprintf(`CREATE INDEX ON %s USING gin(title gin_trgm_ops)`, table)} {
		if _, e = tx.ExecContext(ctx, ddl); e != nil {
			return e
		}
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO buyer_search_generations(id,table_name,model,provider_version,template_version,dimensions,max_distance) VALUES($1,$2,$3,$4,$5,$6,$7)`, g.ID, g.Table, g.Model, g.ProviderVersion, g.TemplateVersion, g.Dimensions, g.MaxDistance)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (i *HybridIndex) ActivateGeneration(ctx context.Context, id string) error {
	tx, e := i.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(72649201)`); e != nil {
		return e
	}
	var ready bool
	if e = tx.QueryRowContext(ctx, `SELECT ready FROM buyer_search_generations WHERE id=$1 FOR UPDATE`, id).Scan(&ready); e != nil {
		return e
	}
	if !ready {
		return errors.New("generation not validated for activation")
	}
	if _, e = tx.ExecContext(ctx, `UPDATE buyer_search_generations SET active=FALSE WHERE active=TRUE`); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE buyer_search_generations SET active=TRUE WHERE id=$1`, id); e != nil {
		return e
	}
	return tx.Commit()
}
