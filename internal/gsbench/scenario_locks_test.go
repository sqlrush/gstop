package gsbench

import (
	"strings"
	"testing"
)

func TestLockTopologyBuildsChainAndFanout(t *testing.T) {
	plan, err := BuildLockTopology(5, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) != 7 || len(plan.Waiters) != 70 {
		t.Fatalf("blockers=%d waiters=%d", len(plan.Blockers), len(plan.Waiters))
	}
	kinds := map[string]bool{}
	for _, role := range append(append([]LockRole{}, plan.Blockers...), plan.Waiters...) {
		kinds[role.Kind] = true
	}
	for _, kind := range []string{"row", "table", "ddl"} {
		if !kinds[kind] {
			t.Fatalf("lock topology missing %s roles: %+v", kind, plan)
		}
	}
	if plan.Deadlock {
		t.Fatal("deadlock should default off")
	}
}

func TestLockTopologyRejectsUnsafeSizes(t *testing.T) {
	if _, err := BuildLockTopology(0, 10, false); err == nil {
		t.Fatal("expected depth error")
	}
	if _, err := BuildLockTopology(5, 0, false); err == nil {
		t.Fatal("expected fanout error")
	}
}

func TestLiteralRowUpdateHasNoBindMarker(t *testing.T) {
	got := literalRowUpdate("gsbench", "lock_targets", 101)
	if strings.Contains(got, "$1") || strings.Contains(got, "?") {
		t.Fatalf("SQL contains bind marker: %s", got)
	}
	if !strings.Contains(got, "WHERE id=101") {
		t.Fatalf("SQL does not contain literal row id: %s", got)
	}
}
