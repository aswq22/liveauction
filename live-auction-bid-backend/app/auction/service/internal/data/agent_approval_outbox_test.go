package data

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestAgentApprovalOutboxContainsOnlySafeIdentifiers(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	fixture := seedAgentApprovalFixture(t, db, "safe-outbox", 1, false)
	fixture.command.TraceID = "0123456789abcdef0123456789abcdef"
	result, err := NewAgentApprovalRepository(db).Apply(context.Background(), fixture.command)
	if err != nil {
		t.Fatal(err)
	}
	var row AgentApprovalOutboxModel
	if err := db.Where("execution_id = ?", result.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(row.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"sessionId": true, "executionId": true, "lotIds": true, "roomId": true, "queuePositions": true, "traceId": true}
	for key := range payload {
		if !allowed[key] {
			t.Fatalf("unsafe outbox key %q payload=%s", key, row.PayloadJSON)
		}
	}
	for key := range allowed {
		if _, ok := payload[key]; !ok {
			t.Fatalf("missing %q payload=%s", key, row.PayloadJSON)
		}
	}
	for _, forbidden := range []string{"title", "description", "prompt", "provider", "secret", "candidate"} {
		if containsInsensitive(row.PayloadJSON, forbidden) {
			t.Fatalf("outbox leaks %q: %s", forbidden, row.PayloadJSON)
		}
	}
	if row.TraceID != fixture.command.TraceID || row.VisibleAtUnixMs <= 0 {
		t.Fatalf("row=%+v", row)
	}
	resetAgentApprovalSchema(t, db)
	dangerous := seedAgentApprovalFixture(t, db, "unsafe-trace", 20, false)
	dangerous.command.TraceID = "prompt-provider-secret"
	dangerousResult, err := NewAgentApprovalRepository(db).Apply(context.Background(), dangerous.command)
	if err != nil {
		t.Fatal(err)
	}
	var dangerousRow AgentApprovalOutboxModel
	if err := db.Where("execution_id = ?", dangerousResult.ID).First(&dangerousRow).Error; err != nil {
		t.Fatal(err)
	}
	if dangerousRow.TraceID != "" || containsInsensitive(dangerousRow.PayloadJSON, "prompt") || containsInsensitive(dangerousRow.PayloadJSON, "secret") {
		t.Fatalf("unsafe trace persisted: %+v", dangerousRow)
	}
}

func TestAgentApprovalOutboxFailureRollsBackEntireBatch(t *testing.T) {
	db := openAgentApprovalMySQL(t)
	resetAgentApprovalSchema(t, db)
	fixture := seedAgentApprovalFixture(t, db, "outbox-rollback", 4, true)
	before := captureApprovalCounts(t, db, fixture)
	if err := db.Exec(`CREATE TRIGGER agent_test_fail_approval_outbox BEFORE INSERT ON agent_approval_outbox FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'forced outbox failure'`).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP TRIGGER IF EXISTS agent_test_fail_approval_outbox")
	if _, err := NewAgentApprovalRepository(db).Apply(context.Background(), fixture.command); err == nil {
		t.Fatal("Apply succeeded despite outbox failure")
	}
	after := captureApprovalCounts(t, db, fixture)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("partial mutation before=%+v after=%+v", before, after)
	}
	var outboxCount int64
	if err := db.Model(&AgentApprovalOutboxModel{}).Count(&outboxCount).Error; err != nil || outboxCount != 0 {
		t.Fatalf("outbox count=%d err=%v", outboxCount, err)
	}
}

func containsInsensitive(value, part string) bool {
	b := []byte(value)
	p := []byte(part)
	for i := 0; i+len(p) <= len(b); i++ {
		match := true
		for j := range p {
			x, y := b[i+j], p[j]
			if x >= 'A' && x <= 'Z' {
				x += 32
			}
			if y >= 'A' && y <= 'Z' {
				y += 32
			}
			if x != y {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
