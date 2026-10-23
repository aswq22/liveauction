package data

import (
	"context"
	"errors"
	"strings"
	"testing"

	"live-auction-bid/backend/app/auction/service/internal/agentapproval"
	"live-auction-bid/backend/app/auction/service/internal/agentconversation"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

func TestAgentApprovalMySQLStaleFailuresBecomeRecoverableRevision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *approvalFixture, *Store)
		want   error
	}{
		{"snapshot", func(t *testing.T, f *approvalFixture, s *Store) {
			mustUpdate(t, s.db.Model(&AgentApprovalSnapshotModel{}).Where("session_id = ?", f.sessionID).Update("content_hash", strings.Repeat("b", 64)))
		}, apperr.ErrAgentReviewSnapshotStale},
		{"candidate", func(t *testing.T, f *approvalFixture, s *Store) {
			mustUpdate(t, s.db.Model(&AgentDraftCandidateModel{}).Where("id = ?", "candidate-new-revision-candidate").Update("version", 2))
		}, apperr.ErrAgentCandidateChanged},
		{"asset", func(t *testing.T, f *approvalFixture, s *Store) {
			mustUpdate(t, s.db.Model(&AssetFileModel{}).Where("id = ?", f.assetID).Update("status", AssetStatusDeleting))
		}, apperr.ErrAgentImageAssetInvalid},
		{"existing-lot", func(t *testing.T, f *approvalFixture, s *Store) {
			mustUpdate(t, s.db.Model(&AuctionLotModel{}).Where("id = ?", f.existingLotID).Update("version", 2))
		}, apperr.ErrAgentCandidateChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openAgentApprovalMySQL(t)
			resetAgentApprovalSchema(t, db)
			f := seedAgentApprovalFixture(t, db, "revision-"+tc.name, 1, false)
			store := &Store{db: db}
			tc.mutate(t, &f, store)
			service := agentapproval.NewService(NewAgentConversationRepository(store), store)
			_, err := service.Apply(context.Background(), f.command)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v", err)
			}
			var session AgentSessionModel
			if err := db.First(&session, "id = ?", f.sessionID).Error; err != nil {
				t.Fatal(err)
			}
			if session.Status != string(agentconversation.StatusNeedsRevision) || session.ActiveSnapshotVersion != 0 || session.Version != 4 {
				t.Fatalf("session=%+v", session)
			}
		})
	}
}

func TestAgentApprovalMySQLNonStaleFailuresDoNotInvalidateReview(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*testing.T, *approvalFixture, *Store)
		command func(*approvalFixture) agentapproval.ApplyCommand
	}{
		{"permission", func(t *testing.T, f *approvalFixture, s *Store) {
			mustUpdate(t, s.db.Model(&AuctionRoomModel{}).Where("id = ?", f.roomID).Update("main_account_id", "tenant-other"))
		}, func(f *approvalFixture) agentapproval.ApplyCommand { return f.command }},
		{"different payload", func(*testing.T, *approvalFixture, *Store) {}, func(f *approvalFixture) agentapproval.ApplyCommand {
			c := f.command
			c.ContentHash = strings.Repeat("b", 64)
			return c
		}},
		{"cross operator stale concealment", func(t *testing.T, f *approvalFixture, s *Store) {
			mustUpdate(t, s.db.Model(&AgentApprovalSnapshotModel{}).Where("session_id = ?", f.sessionID).Update("snapshot_json", `{}`))
		}, func(f *approvalFixture) agentapproval.ApplyCommand {
			c := f.command
			c.OperatorID = "operator-other"
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openAgentApprovalMySQL(t)
			resetAgentApprovalSchema(t, db)
			f := seedAgentApprovalFixture(t, db, "nonstale-"+strings.ReplaceAll(tc.name, " ", "-"), 1, false)
			store := &Store{db: db}
			tc.mutate(t, &f, store)
			service := agentapproval.NewService(NewAgentConversationRepository(store), store)
			_, _ = service.Apply(context.Background(), tc.command(&f))
			var session AgentSessionModel
			db.First(&session, "id = ?", f.sessionID)
			if session.Status != string(agentconversation.StatusReadyForReview) || session.ActiveSnapshotVersion != 1 || session.Version != 3 {
				t.Fatalf("session=%+v", session)
			}
		})
	}
}

func TestAgentApprovalMySQLInvalidationCASDoesNotClobberAppliedOrNewSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updates map[string]any
	}{
		{"applied", map[string]any{"status": string(agentconversation.StatusApplied)}}, {"new snapshot", map[string]any{"active_snapshot_version": int64(2), "version": int64(9)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openAgentApprovalMySQL(t)
			resetAgentApprovalSchema(t, db)
			f := seedAgentApprovalFixture(t, db, "cas-"+strings.ReplaceAll(tc.name, " ", "-"), 1, false)
			mustUpdate(t, db.Model(&AgentSessionModel{}).Where("id = ?", f.sessionID).Updates(tc.updates))
			var before AgentSessionModel
			db.First(&before, "id = ?", f.sessionID)
			repo := NewAgentApprovalRepository(db)
			if err := repo.InvalidateReviewSnapshot(context.Background(), f.command, apperr.ErrAgentCandidateChanged); err != nil {
				t.Fatal(err)
			}
			var after AgentSessionModel
			db.First(&after, "id = ?", f.sessionID)
			if before.Status != after.Status || before.Version != after.Version || before.ActiveSnapshotVersion != after.ActiveSnapshotVersion {
				t.Fatalf("clobbered before=%+v after=%+v", before, after)
			}
		})
	}
}
