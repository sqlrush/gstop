package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

type planControlTestDB struct {
	execQuery string
	execArgs  []any
	queries   []string
	rows      [][]any
	queryErr  error
	forceZero bool
}

func (*planControlTestDB) Scan(
	context.Context,
	string,
	[]any,
	...any,
) error {
	return errors.New("unexpected scan")
}

func (d *planControlTestDB) Exec(
	_ context.Context,
	query string,
	args ...any,
) (sql.Result, error) {
	d.execQuery = query
	d.execArgs = append([]any(nil), args...)
	if d.forceZero {
		return fakeJournalResult(0), nil
	}
	return fakeJournalResult(1), nil
}

func TestPlanControlRejectsMissingRowsDuringFinalization(t *testing.T) {
	db := &planControlTestDB{forceZero: true}
	store, err := newPlanControlStore(db, "gsbench")
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		call func() error
	}{
		{name: "finish workload", call: func() error {
			return store.FinishWorkload(
				context.Background(), "missing-run", OutcomeSuccess, "complete",
			)
		}},
		{name: "finish fault audit", call: func() error {
			return store.RecordFaultFinish(
				context.Background(), "missing-run", OutcomeFailed, "failed",
			)
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.call(); err == nil || !strings.Contains(err.Error(), "missing-run") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func (d *planControlTestDB) Query(
	_ context.Context,
	query string,
	_ ...any,
) (journalRows, error) {
	d.queries = append(d.queries, query)
	if d.queryErr != nil {
		return nil, d.queryErr
	}
	return &sliceJournalRows{rows: d.rows}, nil
}

func TestPlanActivityLockIdentityCoversPlanFamily(t *testing.T) {
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "gsbench_test"},
	}
	if got := planActivityLockIdentity(cfg); got !=
		"gsbench:plan-workload:postgres:gsbench_test" {
		t.Fatalf("identity=%q", got)
	}
}

func TestPlanControlStartsAndFinishesResidentWorkload(t *testing.T) {
	db := &planControlTestDB{}
	store, err := newPlanControlStore(db, "gsbench")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartWorkload(
		context.Background(), "workload-1", 601, 10,
	); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{
		`INSERT INTO "gsbench".meta_runs`,
		"plan_baseline",
		"workload_preparing",
	} {
		if !strings.Contains(db.execQuery, token) &&
			!containsAnyString(db.execArgs, token) {
			t.Fatalf("start query=%q args=%v missing %q", db.execQuery, db.execArgs, token)
		}
	}
	if err := store.MarkWorkloadRunning(
		context.Background(), "workload-1",
	); err != nil {
		t.Fatal(err)
	}
	if !containsAnyString(db.execArgs, planWorkloadRunningStatus) ||
		!containsAnyString(db.execArgs, planWorkloadPreparingStatus) {
		t.Fatalf("running query=%q args=%v", db.execQuery, db.execArgs)
	}
	if err := store.FinishWorkload(
		context.Background(), "workload-1", OutcomeUnverified, "duration complete",
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(db.execQuery, "UPDATE \"gsbench\".meta_runs") ||
		!containsAnyString(db.execArgs, string(OutcomeUnverified)) {
		t.Fatalf("finish query=%q args=%v", db.execQuery, db.execArgs)
	}
}

func TestPlanControlMarksRecordedWorkloadsStaleWithoutRecovery(t *testing.T) {
	db := &planControlTestDB{}
	store, err := newPlanControlStore(db, "gsbench")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkWorkloadsStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{
		"UPDATE \"gsbench\".meta_runs",
		"workload_preparing",
		"workload_running",
		"stale_report_only",
	} {
		if !strings.Contains(db.execQuery, token) &&
			!containsAnyString(db.execArgs, token) {
			t.Fatalf("query=%q args=%v missing %q", db.execQuery, db.execArgs, token)
		}
	}
	if strings.Contains(db.execQuery, "meta_journal") {
		t.Fatalf("stale workload marker touched recovery journal: %s", db.execQuery)
	}
}

func TestPlanControlResolvesLatestResidentWorkload(t *testing.T) {
	started := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	db := &planControlTestDB{rows: [][]any{
		{"workload-1", "601", "plan_baseline", "workload_running", started},
	}}
	store, err := newPlanControlStore(db, "gsbench")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ResolveWorkload(context.Background(), 601)
	if err != nil {
		t.Fatal(err)
	}
	if run.RunID != "workload-1" || run.Code != 601 ||
		run.Status != planWorkloadRunningStatus {
		t.Fatalf("run=%+v", run)
	}

	db.rows = [][]any{
		{"workload-2", "602", "plan_baseline", "workload_running", started.Add(time.Second)},
		{"workload-1", "601", "plan_baseline", "workload_running", started},
	}
	latest, err := store.ResolveAnyWorkload(context.Background())
	if err != nil || latest.RunID != "workload-2" {
		t.Fatalf("latest workload=%+v err=%v", latest, err)
	}
}

func TestPlanControlResolvesAnyResidentWorkloadForStaleCleanup(t *testing.T) {
	started := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	db := &planControlTestDB{rows: [][]any{
		{"workload-601", "601", "plan_baseline", "workload_running", started},
	}}
	store, err := newPlanControlStore(db, "gsbench")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ResolveAnyWorkload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.RunID != "workload-601" || run.Code != 601 {
		t.Fatalf("run=%+v", run)
	}
	if _, err := store.ResolveWorkload(context.Background(), 602); err == nil ||
		!strings.Contains(err.Error(), "not 602") {
		t.Fatalf("scenario mismatch error=%v", err)
	}
}

func TestPlanControlDoesNotExposePreparingWorkloadToFault(t *testing.T) {
	started := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	db := &planControlTestDB{rows: [][]any{
		{"workload-601", "601", "plan_baseline", "workload_preparing", started},
	}}
	store, err := newPlanControlStore(db, "gsbench")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveWorkload(context.Background(), 601); !errors.Is(
		err,
		errPlanWorkloadNotFound,
	) {
		t.Fatalf("preparing workload exposed to fault: %v", err)
	}
}

func TestDatabasePlanActionBackendRecordsFaultWithoutReadingHistoricalRows(t *testing.T) {
	db := &planControlTestDB{rows: [][]any{
		{"old-fault-601", "601", "hold", "running", time.Now()},
		{"old-fault-602", "602", "hold", "running", time.Now()},
	}}
	store, err := newPlanControlStore(db, "gsbench")
	if err != nil {
		t.Fatal(err)
	}
	backend := &databasePlanActionBackend{control: store}
	if err := backend.RecordFaultStart(
		context.Background(), "new-fault-602", 602,
	); err != nil {
		t.Fatal(err)
	}
	if len(db.queries) != 0 {
		t.Fatalf("fault admission read historical rows: %q", db.queries)
	}
	if !strings.Contains(db.execQuery, "INSERT INTO") ||
		!containsAnyString(db.execArgs, "new-fault-602") ||
		!containsAnyString(db.execArgs, "602") {
		t.Fatalf("query=%q args=%v", db.execQuery, db.execArgs)
	}
}

func TestPlanControlRecordsTerminalFaultAudit(t *testing.T) {
	db := &planControlTestDB{}
	store, err := newPlanControlStore(db, "gsbench")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordFaultFinish(
		context.Background(),
		"fault-601",
		OutcomeCompletedWithWarnings,
		"fault command completed; live catalog state is authoritative",
	); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{
		string(PhaseHold),
		string(OutcomeCompletedWithWarnings),
		"live catalog state is authoritative",
		"fault-601",
	} {
		if !containsAnyString(db.execArgs, token) {
			t.Fatalf("query=%q args=%v missing %q", db.execQuery, db.execArgs, token)
		}
	}
	if !strings.Contains(db.execQuery, "phase=$1,status=$2") {
		t.Fatalf("query=%q args=%v", db.execQuery, db.execArgs)
	}
}

func containsAnyString(values []any, token string) bool {
	for _, value := range values {
		if text, ok := value.(string); ok && strings.Contains(text, token) {
			return true
		}
	}
	return false
}
