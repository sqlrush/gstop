package gsbench

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStopTaggedSQLUsesExactRunBoundary(t *testing.T) {
	query, args, err := StopTaggedSQL("run-1")
	if err != nil {
		t.Fatal(err)
	}
	arg := onlyStringArgument(t, args)
	if arg != "gsbench/run-1/%" || !strings.Contains(query, "pg_terminate_session") {
		t.Fatalf("query=%q arg=%q", query, arg)
	}
	if !strings.Contains(query, "COALESCE(sessionid,0)<>0") ||
		!strings.Contains(query, "backend_start IS NOT NULL") {
		t.Fatalf("terminate query includes thread-pool worker rows: %q", query)
	}
}

func TestStopTaggedSQLAllowsAllGSBenchRuns(t *testing.T) {
	query, args, err := StopTaggedSQL("")
	if err != nil {
		t.Fatal(err)
	}
	arg := onlyStringArgument(t, args)
	if arg != "gsbench/%" || !strings.Contains(query, "pg_terminate_session") {
		t.Fatalf("query=%q arg=%q", query, arg)
	}
}

func TestTaggedSessionStateExcludesItsOwnControlSession(t *testing.T) {
	query, args, err := taggedSessionStateSQL("")
	if err != nil {
		t.Fatal(err)
	}
	if onlyStringArgument(t, args) != "gsbench/%" {
		t.Fatalf("args=%v", args)
	}
	if !strings.Contains(query, "a.pid <> pg_backend_pid()") {
		t.Fatalf("state query counts its own control session: %s", query)
	}
}

func TestCleanupPlanDropsSchemaOnlyWhenRequested(t *testing.T) {
	withoutData, err := CleanupPlan("gsbench", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(withoutData, " "), "DROP SCHEMA") {
		t.Fatalf("unexpected drop: %v", withoutData)
	}
	withData, err := CleanupPlan("gsbench", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(withData, " "), "DROP SCHEMA gsbench CASCADE") {
		t.Fatalf("missing drop: %v", withData)
	}
}

func TestReadStaleRecoveryStatusUnionsSourcesWithoutRestoring(t *testing.T) {
	store := &memoryActionStore{stale: []string{"run-c", "run-a"}}
	executor := &memoryActionExecutor{
		onRestore: func(Action) {
			t.Fatal("status must never restore an action")
		},
	}
	journal := NewJournal(store, executor)
	ledger := NewFileRecoveryLedger(filepath.Join(t.TempDir(), "recovery.json"))
	for _, action := range []Action{
		validLedgerAction("run-b", "target-b"),
		validLedgerAction("run-a", "target-a"),
	} {
		if err := ledger.Put(context.Background(), action); err != nil {
			t.Fatal(err)
		}
	}

	status, err := ReadStaleRecoveryStatus(
		context.Background(),
		journal,
		ledger,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"run-a", "run-b", "run-c"}; !reflect.DeepEqual(
		status.RunIDs, want,
	) {
		t.Fatalf("run IDs=%v want=%v", status.RunIDs, want)
	}
	if status.DatabaseRunCount != 2 || status.LocalActionCount != 2 {
		t.Fatalf("status=%+v", status)
	}
	pending, err := ledger.Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || len(store.states) != 0 {
		t.Fatalf(
			"status mutated recovery state: local=%+v database=%v",
			pending,
			store.states,
		)
	}
}
