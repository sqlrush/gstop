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
	return fakeJournalResult(1), nil
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

func TestPlanControlResolvesExactlyOneResidentWorkload(t *testing.T) {
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

	db.rows = append(db.rows, []any{
		"workload-2", "602", "plan_baseline", "workload_running", started.Add(time.Second),
	})
	if _, err := store.ResolveWorkload(context.Background(), 601); err == nil ||
		!strings.Contains(err.Error(), "multiple") {
		t.Fatalf("multiple workload error=%v", err)
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

func TestPlanControlResolvesFaultByScenarioAndRejectsAmbiguity(t *testing.T) {
	started := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	db := &planControlTestDB{rows: [][]any{
		{"fault-1", "605", "hold", "running", started},
	}}
	store, err := newPlanControlStore(db, "gsbench")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ResolveFault(context.Background(), 605)
	if err != nil {
		t.Fatal(err)
	}
	if run.RunID != "fault-1" || run.Code != 605 {
		t.Fatalf("run=%+v", run)
	}
	db.rows = append(db.rows, []any{
		"fault-2", "605", "ramp", "restore_failed", started.Add(time.Second),
	})
	if _, err := store.ResolveFault(context.Background(), 605); err == nil ||
		!strings.Contains(err.Error(), "multiple") {
		t.Fatalf("multiple fault error=%v", err)
	}
}

func TestPlanControlMarksFaultFailureRecoverable(t *testing.T) {
	db := &planControlTestDB{}
	store, err := newPlanControlStore(db, "gsbench")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFaultFailed(
		context.Background(), "fault-1", errors.New("DDL failed"),
	); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"restore_failed", "DDL failed", "fault-1"} {
		if !containsAnyString(db.execArgs, token) {
			t.Fatalf("query=%q args=%v missing %q", db.execQuery, db.execArgs, token)
		}
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
