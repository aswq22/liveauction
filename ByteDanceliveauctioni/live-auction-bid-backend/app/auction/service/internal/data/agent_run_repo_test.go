package data

import (
	"context"
	"database/sql/driver"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

func TestAgentRunRepositoryPreservesOwnerAndVersion(t *testing.T) {
	repo, mock := newAgentRunTestRepository(t)
	ctx := context.Background()
	run := agentcontrol.Run{
		ID:              "run-1",
		TenantID:        "merchant-1",
		OperatorID:      "user-1",
		Goal:            "清理库存",
		Status:          agentcontrol.StatusCreated,
		Version:         1,
		CreatedAtUnixMs: 101,
		UpdatedAtUnixMs: 102,
	}

	mock.ExpectExec("INSERT INTO `agent_runs`").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := repo.Create(ctx, run); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	mock.ExpectQuery("SELECT .* FROM `agent_runs` WHERE id = .*").
		WithArgs("run-1", 1).
		WillReturnRows(sqlmock.NewRows(agentRunColumns()).AddRow(
			"run-1", "merchant-1", "user-1", "", "", "清理库存", "CREATED", "", "", "", int64(1), int64(0), false, int64(101), int64(102), time.UnixMilli(101), time.UnixMilli(102),
		))
	got, err := repo.Find(ctx, "run-1")
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if got.TenantID != "merchant-1" || got.OperatorID != "user-1" {
		t.Fatalf("Find() owner = (%q, %q), want (%q, %q)", got.TenantID, got.OperatorID, "merchant-1", "user-1")
	}

	mock.ExpectExec("UPDATE `agent_runs` SET .* WHERE id = \\? AND version = \\?").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.UpdateStatus(ctx, "run-1", 99, agentcontrol.StatusPlanning, "", "", ""); !errors.Is(err, apperr.ErrConflict) {
		t.Fatalf("UpdateStatus() error = %v, want %v", err, apperr.ErrConflict)
	}
}

func TestAgentRunRepositoryUpdatesStatusWithExpectedVersion(t *testing.T) {
	repo, mock := newAgentRunTestRepository(t)
	ctx := context.Background()
	updatedAt := &unixMsArgument{}

	mock.ExpectExec("UPDATE `agent_runs` SET `error_code`=\\?,`error_message`=\\?,`proposal_json`=\\?,`status`=\\?,`updated_at_unix_ms`=\\?,`version`=version \\+ 1,`updated_at`=\\? WHERE id = \\? AND version = \\?").
		WithArgs("", "", `{"proposal":"ready"}`, "PLANNING", updatedAt, sqlmock.AnyArg(), "run-1", int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.UpdateStatus(ctx, "run-1", 1, agentcontrol.StatusPlanning, `{"proposal":"ready"}`, "", ""); err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}
	if updatedAt.value <= 0 {
		t.Fatalf("UpdateStatus() updated_at_unix_ms = %d, want positive value", updatedAt.value)
	}

	mock.ExpectQuery("SELECT .* FROM `agent_runs` WHERE id = .*").
		WithArgs("run-1", 1).
		WillReturnRows(sqlmock.NewRows(agentRunColumns()).AddRow(
			"run-1", "merchant-1", "user-1", "", "", "清理库存", "PLANNING", `{"proposal":"ready"}`, "", "", int64(2), int64(0), false, int64(101), updatedAt.value, time.UnixMilli(101), time.UnixMilli(updatedAt.value),
		))
	got, err := repo.Find(ctx, "run-1")
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if got.Version != 2 || got.Status != agentcontrol.StatusPlanning || got.UpdatedAtUnixMs != updatedAt.value {
		t.Fatalf("Find() after UpdateStatus() = %#v, want version 2, status %q, updated_at_unix_ms %d", got, agentcontrol.StatusPlanning, updatedAt.value)
	}
}

func TestAgentRunRepositoryCompletesProjectionWithoutChangingTerminalPayload(t *testing.T) {
	repo, mock := newAgentRunTestRepository(t)
	mock.ExpectExec("UPDATE `agent_runs` SET `projection_complete`=\\?,`version`=version \\+ 1,`updated_at`=\\? WHERE id = \\? AND version = \\?").
		WithArgs(true, sqlmock.AnyArg(), "run-terminal", int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.CompleteProjection(context.Background(), "run-terminal", 7); err != nil {
		t.Fatalf("CompleteProjection() error = %v", err)
	}
}

func TestAgentRunRepositoryAppendsAndListsEvents(t *testing.T) {
	repo, mock := newAgentRunTestRepository(t)
	ctx := context.Background()
	event := agentcontrol.Event{
		RunID:           "run-1",
		Seq:             2,
		EventType:       "STATUS_CHANGED",
		Message:         "planning started",
		PayloadJSON:     `{"stage":"planning"}`,
		CreatedAtUnixMs: 103,
	}

	mock.ExpectExec("INSERT INTO `agent_run_events`").WillReturnResult(sqlmock.NewResult(1, 1))
	if err := repo.AppendEvent(ctx, event); err != nil {
		t.Fatalf("AppendEvent() error = %v", err)
	}

	mock.ExpectQuery("SELECT .* FROM `agent_run_events` WHERE run_id = .*").
		WithArgs("run-1", int64(1), 10).
		WillReturnRows(sqlmock.NewRows(agentRunEventColumns()).AddRow(
			"run-1", int64(2), nil, "STATUS_CHANGED", "planning started", `{"stage":"planning"}`, int64(103), time.UnixMilli(103),
		))
	events, err := repo.ListEvents(ctx, "run-1", 1, 10)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != 1 || events[0] != event {
		t.Fatalf("ListEvents() = %#v, want []agentcontrol.Event{%#v}", events, event)
	}
}

func TestAgentRunRepositoryFailsRuntimeStartAndAppendsTerminalEventAtomically(t *testing.T) {
	event := agentcontrol.Event{
		RunID: "run-1", Seq: 1, EventType: "run.failed",
		Message:     agentcontrol.RuntimeUnavailableMessage,
		PayloadJSON: `{"errorCode":"AGENT_RUNTIME_UNAVAILABLE"}`, CreatedAtUnixMs: 103,
	}

	t.Run("commit", func(t *testing.T) {
		repo, mock := newAgentRunTestRepository(t)
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `agent_runs` SET .* WHERE id = \\? AND version = \\?").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("INSERT INTO `agent_run_events`").WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		err := repo.FailRuntimeStart(context.Background(), "run-1", 1, agentcontrol.ErrorCodeRuntimeUnavailable, agentcontrol.RuntimeUnavailableMessage, event)
		if err != nil {
			t.Fatalf("FailRuntimeStart() error = %v", err)
		}
	})

	t.Run("rollback when event append fails", func(t *testing.T) {
		repo, mock := newAgentRunTestRepository(t)
		wantErr := errors.New("event insert failed")
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `agent_runs` SET .* WHERE id = \\? AND version = \\?").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("INSERT INTO `agent_run_events`").WillReturnError(wantErr)
		mock.ExpectRollback()

		err := repo.FailRuntimeStart(context.Background(), "run-1", 1, agentcontrol.ErrorCodeRuntimeUnavailable, agentcontrol.RuntimeUnavailableMessage, event)
		if !errors.Is(err, wantErr) {
			t.Fatalf("FailRuntimeStart() error = %v, want %v", err, wantErr)
		}
	})
}

func TestAgentRunModelsDeclareRequiredIndexes(t *testing.T) {
	assertSchemaIndex(t, &AgentRunModel{}, "idx_agent_run_tenant_updated", "", "TenantID", "UpdatedAt")
	assertSchemaIndex(t, &AgentRunModel{}, "uidx_agent_run_idempotency", "UNIQUE", "TenantID", "OperatorID", "IdempotencyKey")
	assertSchemaIndex(t, &AgentRunEventModel{}, "uidx_agent_run_event_seq", "UNIQUE", "RunID", "Seq")
	assertSchemaIndex(t, &AgentRunEventModel{}, "uidx_agent_run_runtime_source_seq", "UNIQUE", "RunID", "RuntimeSourceSeq")
}

func TestLegacyProjectionBackfillCompletesOnlyTerminalRunsWithoutTouchingPayloads(t *testing.T) {
	repo, mock := newAgentRunTestRepository(t)
	mock.ExpectExec("UPDATE agent_runs SET projection_complete = TRUE WHERE status IN \\(\\?,\\?,\\?\\) AND projection_complete = FALSE").
		WithArgs("COMPLETED", "FAILED", "CANCELLED").
		WillReturnResult(sqlmock.NewResult(0, 2))

	if err := backfillLegacyAgentRunProjectionCompletion(context.Background(), repo.db); err != nil {
		t.Fatalf("backfillLegacyAgentRunProjectionCompletion() error = %v", err)
	}
}

func TestPrepareExistingAgentRunSchemaBackfillsOnlyWhenAddingProjectionCompleteColumn(t *testing.T) {
	testCases := []struct {
		name                     string
		projectionColumnExists   bool
		expectProjectionBackfill bool
	}{
		{name: "existing column preserves incomplete terminal runs", projectionColumnExists: true, expectProjectionBackfill: false},
		{name: "new legacy column backfills terminal runs", projectionColumnExists: false, expectProjectionBackfill: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			repo, mock := newAgentRunTestRepository(t)
			expectCurrentDatabase := func() {
				mock.ExpectQuery("SELECT DATABASE\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"DATABASE()"}).AddRow("live_auction"))
				mock.ExpectQuery("SELECT SCHEMA_NAME from Information_schema.SCHEMATA").
					WithArgs("live_auction%", "live_auction").
					WillReturnRows(sqlmock.NewRows([]string{"SCHEMA_NAME"}).AddRow("live_auction"))
			}
			expectCurrentDatabase()
			mock.ExpectQuery("(?i)SELECT count\\(\\*\\) FROM information_schema.tables").
				WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(1))
			for range 2 {
				expectCurrentDatabase()
				mock.ExpectQuery("(?i)SELECT count\\(\\*\\) FROM information_schema.columns").
					WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(1))
			}
			projectionCount := 0
			if testCase.projectionColumnExists {
				projectionCount = 1
			}
			expectCurrentDatabase()
			mock.ExpectQuery("(?i)SELECT count\\(\\*\\) FROM information_schema.columns").
				WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(projectionCount))
			if !testCase.projectionColumnExists {
				mock.ExpectExec("ALTER TABLE .*agent_runs.* ADD .*projection_complete").
					WillReturnResult(sqlmock.NewResult(0, 0))
			}
			mock.ExpectExec("UPDATE agent_runs SET idempotency_key = CONCAT").
				WillReturnResult(sqlmock.NewResult(0, 0))
			if testCase.expectProjectionBackfill {
				mock.ExpectExec("UPDATE agent_runs SET projection_complete = TRUE").
					WithArgs("COMPLETED", "FAILED", "CANCELLED").
					WillReturnResult(sqlmock.NewResult(0, 2))
			}
			for _, column := range []string{"session_id", "run_type", "session_version", "turn_input_json", "turn_result_json"} {
				expectCurrentDatabase()
				mock.ExpectQuery("(?i)SELECT count\\(\\*\\) FROM information_schema.columns").
					WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(0))
				mock.ExpectExec("ALTER TABLE .*agent_runs.* ADD .*" + column).
					WillReturnResult(sqlmock.NewResult(0, 0))
			}
			expectCurrentDatabase()
			mock.ExpectQuery("(?i)SELECT count\\(\\*\\) FROM information_schema.tables").
				WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(0))

			store := &Store{db: repo.db}
			if err := store.prepareExistingAgentRunSchema(context.Background()); err != nil {
				t.Fatalf("prepareExistingAgentRunSchema() error = %v", err)
			}
		})
	}
}

func TestAgentRunTextColumnsDoNotDeclareMySQLIncompatibleDefaults(t *testing.T) {
	testCases := []struct {
		model  any
		fields []string
	}{
		{model: &AgentRunModel{}, fields: []string{"Goal", "ProposalJSON", "TurnInputJSON", "TurnResultJSON", "ErrorMessage"}},
		{model: &AgentRunEventModel{}, fields: []string{"Message", "PayloadJSON"}},
	}
	for _, testCase := range testCases {
		model := testCase.model
		parsed, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
		if err != nil {
			t.Fatalf("schema.Parse(%T) error = %v", model, err)
		}
		for _, fieldName := range testCase.fields {
			field := parsed.FieldsByName[fieldName]
			if field == nil {
				t.Fatalf("schema.Parse(%T) missing field %q", model, fieldName)
			}
			if field.HasDefaultValue {
				t.Errorf("%T.%s declares a TEXT default unsupported by the Compose MySQL server", model, field.Name)
			}
		}
	}
}

func newAgentRunTestRepository(t *testing.T) (*AgentRunRepo, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Error(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	gormDB, err := gorm.Open(mysql.New(mysql.Config{Conn: db, SkipInitializeWithVersion: true}), &gorm.Config{SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	return NewAgentRunRepository(&Store{db: gormDB}), mock
}

func agentRunColumns() []string {
	return []string{
		"id", "tenant_id", "operator_id", "idempotency_key", "trace_id", "goal", "status", "proposal_json", "error_code", "error_message", "version", "last_runtime_source_seq", "projection_complete", "created_at_unix_ms", "updated_at_unix_ms", "created_at", "updated_at",
	}
}

func agentRunEventColumns() []string {
	return []string{"run_id", "seq", "runtime_source_seq", "event_type", "message", "payload_json", "created_at_unix_ms", "created_at"}
}

type unixMsArgument struct {
	value int64
}

func (a *unixMsArgument) Match(value driver.Value) bool {
	unixMs, ok := value.(int64)
	if !ok || unixMs <= 0 {
		return false
	}
	a.value = unixMs
	return true
}

func assertSchemaIndex(t *testing.T, model any, name, class string, fieldNames ...string) {
	t.Helper()
	parsed, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("schema.Parse(%T) error = %v", model, err)
	}
	index, ok := parsed.ParseIndexes()[name]
	if !ok {
		t.Fatalf("schema.Parse(%T) missing index %q", model, name)
	}
	if index.Class != class {
		t.Fatalf("index %q class = %q, want %q", name, index.Class, class)
	}
	if len(index.Fields) != len(fieldNames) {
		t.Fatalf("index %q fields = %d, want %d", name, len(index.Fields), len(fieldNames))
	}
	for i, want := range fieldNames {
		if got := index.Fields[i].Name; got != want {
			t.Fatalf("index %q field %d = %q, want %q", name, i, got, want)
		}
	}
}
