package gsbench

import (
	"strings"
	"testing"
)

func TestStopTaggedSQLUsesExactRunBoundary(t *testing.T) {
	query, arg, err := StopTaggedSQL("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if arg != "gsbench/run-1/%" || !strings.Contains(query, "pg_terminate_session") {
		t.Fatalf("query=%q arg=%q", query, arg)
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
