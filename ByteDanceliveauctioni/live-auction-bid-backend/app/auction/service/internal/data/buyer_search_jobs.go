package data

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"gorm.io/gorm"
	bs "live-auction-bid/backend/app/auction/service/internal/biz/buyersearch"
	"live-auction-bid/backend/app/auction/service/internal/searchindex"
	"strings"
	"time"
)

type BuyerSearchJobs struct{ db *gorm.DB }

func (s *Store) BuyerSearchJobs() *BuyerSearchJobs    { return &BuyerSearchJobs{db: s.db} }
func NewBuyerSearchJobs(db *gorm.DB) *BuyerSearchJobs { return &BuyerSearchJobs{db: db} }
func (j *BuyerSearchJobs) CheckSchema(ctx context.Context) error {
	var count int64
	if e := j.db.WithContext(ctx).Raw(`SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=DATABASE() AND TRIGGER_NAME IN ('buyer_search_lot_insert','buyer_search_lot_update','buyer_search_lot_delete','buyer_search_room_update','buyer_search_room_delete','buyer_search_owner_update','buyer_search_owner_delete','buyer_search_role_insert','buyer_search_role_update','buyer_search_role_delete')`).Scan(&count).Error; e != nil {
		return e
	}
	if count != 10 {
		return errors.New("buyer search triggers missing")
	}
	return j.db.WithContext(ctx).Exec(`SELECT lot_id,metadata_revision,state FROM buyer_search_jobs LIMIT 0`).Error
}

type buyerSearchJobRow struct {
	LotID, RoomID, MainAccountID, LeaseToken string
	SemanticRevision, MetadataRevision       uint64
	Deleted                                  bool
	Attempts                                 int
}

func (j *BuyerSearchJobs) Claim(ctx context.Context) (searchindex.SearchJob, bool, error) {
	if e := j.ExpandScope(ctx); e != nil {
		return searchindex.SearchJob{}, false, e
	}
	var job searchindex.SearchJob
	found := false
	e := j.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []buyerSearchJobRow
		if e := tx.Raw(`SELECT lot_id,metadata_revision,attempts FROM buyer_search_jobs WHERE (state='pending' AND next_attempt_at<=NOW(6)) OR (state='running' AND lease_until<NOW(6)) ORDER BY next_attempt_at,lot_id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&rows).Error; e != nil {
			return e
		}
		if len(rows) == 0 {
			return nil
		}
		b := make([]byte, 16)
		if _, e := rand.Read(b); e != nil {
			return e
		}
		r := rows[0]
		job = searchindex.SearchJob{LotID: r.LotID, Revision: r.MetadataRevision, Token: hex.EncodeToString(b), Attempts: r.Attempts}
		found = true
		return tx.Exec(`UPDATE buyer_search_jobs SET state='running',lease_token=?,lease_until=DATE_ADD(NOW(6),INTERVAL 30 SECOND) WHERE lot_id=?`, job.Token, job.LotID).Error
	})
	return job, found, e
}
func (j *BuyerSearchJobs) Load(ctx context.Context, job searchindex.SearchJob) (bs.Document, error) {
	var r buyerSearchJobRow
	if e := j.db.WithContext(ctx).Raw(`SELECT lot_id,room_id,main_account_id,semantic_revision,metadata_revision,deleted FROM buyer_search_jobs WHERE lot_id=?`, job.LotID).Scan(&r).Error; e != nil {
		return bs.Document{}, e
	}
	if r.LotID == "" {
		return bs.Document{}, errors.New("search source revision missing")
	}
	d := bs.Document{LotID: r.LotID, RoomID: r.RoomID, MainAccountID: r.MainAccountID, Revision: bs.Revision{Semantic: r.SemanticRevision, Metadata: r.MetadataRevision}, Deleted: r.Deleted}
	if r.Deleted {
		return d, nil
	}
	var row lotSearchDocumentRow
	tx := j.db.WithContext(ctx).Model(&AuctionLotModel{}).Select(`auction_lots.*,COALESCE(r.status,'') AS room_status,COALESCE(u.status,0) AS main_user_status,EXISTS(SELECT 1 FROM auction_user_roles ur WHERE ur.user_id=auction_lots.main_account_id AND ur.role_code='merchant_owner') AS main_has_owner_role`).
		Joins(`LEFT JOIN auction_rooms r ON r.id=auction_lots.room_id AND r.main_account_id=auction_lots.main_account_id`).Joins(`LEFT JOIN auction_users u ON u.id=auction_lots.main_account_id`).Where(`auction_lots.id=?`, job.LotID).Take(&row)
	if errors.Is(tx.Error, gorm.ErrRecordNotFound) {
		d.Deleted = true
		return d, nil
	}
	if tx.Error != nil {
		return d, tx.Error
	}
	lot, e := modelToLot(&row.AuctionLotModel)
	if e != nil {
		return d, e
	}
	d.RoomID = lot.RoomId
	d.MainAccountID = lot.MainAccountId
	d.Title = lot.Title
	d.Category = lot.Category
	d.Tags = append([]string(nil), lot.Tags...)
	d.Status = lot.Status
	d.Public = row.RoomStatus == "ACTIVE" && row.MainUserStatus == 1 && row.MainHasOwnerRole && bs.PublicStatus(lot.Status)
	// Stable descriptive content only: prices, room popularity and auction
	// countdowns never cause a paid re-embedding.
	d.Text = strings.Join(nonEmptyText([]string{lot.Title, lot.Description, lot.Category, strings.Join(lot.Tags, " ")}), "\n")
	return d, nil
}
func (j *BuyerSearchJobs) Ack(ctx context.Context, job searchindex.SearchJob) error {
	return j.db.WithContext(ctx).Exec(`UPDATE buyer_search_jobs SET state='done',lease_token='',lease_until=NULL,error_class='' WHERE lot_id=? AND lease_token=? AND metadata_revision=? AND state='running'`, job.LotID, job.Token, job.Revision).Error
}
func (j *BuyerSearchJobs) Retry(ctx context.Context, job searchindex.SearchJob, cause error) error {
	attempt := job.Attempts + 1
	state := "pending"
	if attempt >= 8 {
		state = "dead"
	}
	shift := attempt - 1
	if shift > 6 {
		shift = 6
	}
	delay := time.Duration(1<<shift) * time.Second
	var pe *searchindex.ProviderError
	if errors.As(cause, &pe) && pe.RetryAfter > delay {
		delay = pe.RetryAfter
	}
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	b := make([]byte, 1)
	_, _ = rand.Read(b)
	delay += time.Duration(b[0]) * time.Millisecond
	class := "dependency"
	if pe != nil {
		class = "embedding_provider"
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		class = "timeout"
	}
	return j.db.WithContext(ctx).Exec(`UPDATE buyer_search_jobs SET state=?,attempts=?,next_attempt_at=DATE_ADD(NOW(6),INTERVAL ? MICROSECOND),error_class=?,lease_token='',lease_until=NULL WHERE lot_id=? AND lease_token=? AND metadata_revision=? AND state='running'`, state, attempt, delay.Microseconds(), class, job.LotID, job.Token, job.Revision).Error
}

// Scope fan-out is bounded and serialized separately from auction writers.
func (j *BuyerSearchJobs) ExpandScope(ctx context.Context) error {
	return j.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var scopes []struct{ ScopeKind, ScopeID, LastLotID string }
		if e := tx.Raw(`SELECT scope_kind,scope_id,last_lot_id FROM buyer_search_scope_jobs WHERE state='pending' ORDER BY changed_at LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&scopes).Error; e != nil {
			return e
		}
		if len(scopes) == 0 {
			return nil
		}
		s := scopes[0]
		column := "room_id"
		if s.ScopeKind == "owner" {
			column = "main_account_id"
		} else if s.ScopeKind != "room" {
			return errors.New("invalid search scope")
		}
		var rows []AuctionLotModel
		if e := tx.Select("id,room_id,main_account_id").Where(column+"=? AND id>?", s.ScopeID, s.LastLotID).Order("id").Limit(500).Find(&rows).Error; e != nil {
			return e
		}
		last := s.LastLotID
		for _, r := range rows {
			if e := tx.Exec(`CALL buyer_search_enqueue(?,?,?,FALSE,FALSE)`, r.ID, r.RoomID, r.MainAccountID).Error; e != nil {
				return e
			}
			last = r.ID
		}
		state := "pending"
		if len(rows) < 500 {
			state = "done"
		}
		return tx.Exec(`UPDATE buyer_search_scope_jobs SET last_lot_id=?,state=? WHERE scope_kind=? AND scope_id=?`, last, state, s.ScopeKind, s.ScopeID).Error
	})
}

// Backfill a resumable keyset page. Replaying a page is safe and refreshes
// metadata, preserving the semantic revision and any existing vector hash.
func (j *BuyerSearchJobs) BackfillPage(ctx context.Context, after string) (string, int, error) {
	var rows []AuctionLotModel
	last := after
	e := j.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if e := tx.Select("id,room_id,main_account_id").Where("id>?", after).Order("id").Limit(500).Find(&rows).Error; e != nil {
			return e
		}
		for _, r := range rows {
			if e := tx.Exec(`CALL buyer_search_enqueue(?,?,?,FALSE,FALSE)`, r.ID, r.RoomID, r.MainAccountID).Error; e != nil {
				return e
			}
			last = r.ID
		}
		return nil
	})
	return last, len(rows), e
}
func (j *BuyerSearchJobs) BackfillGenerationPage(ctx context.Context, name string) (bool, error) {
	if name == "" || len(name) > 64 {
		return false, errors.New("backfill name required")
	}
	done := false
	e := j.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if e := tx.Exec(`INSERT IGNORE INTO buyer_search_backfill_progress(name) VALUES(?)`, name).Error; e != nil {
			return e
		}
		var progress struct{ LastLotID, State string }
		if e := tx.Raw(`SELECT last_lot_id,state FROM buyer_search_backfill_progress WHERE name=? FOR UPDATE`, name).Scan(&progress).Error; e != nil {
			return e
		}
		if progress.State == "done" {
			done = true
			return nil
		}
		var rows []AuctionLotModel
		if e := tx.Select("id,room_id,main_account_id").Where("id>?", progress.LastLotID).Order("id").Limit(500).Find(&rows).Error; e != nil {
			return e
		}
		last := progress.LastLotID
		for _, r := range rows {
			if e := tx.Exec(`CALL buyer_search_enqueue(?,?,?,FALSE,FALSE)`, r.ID, r.RoomID, r.MainAccountID).Error; e != nil {
				return e
			}
			last = r.ID
		}
		state := "pending"
		if len(rows) < 500 {
			state = "done"
			done = true
		}
		return tx.Exec(`UPDATE buyer_search_backfill_progress SET last_lot_id=?,state=?,updated_at=NOW(6) WHERE name=?`, last, state, name).Error
	})
	return done, e
}
