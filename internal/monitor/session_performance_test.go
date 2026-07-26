package monitor

import (
	"strings"
	"testing"

	"gstop/internal/dbcompat"
	"gstop/internal/dbconn"
	"gstop/internal/model"
)

func TestSessionQueryUsesUniqueThreadIdentity(t *testing.T) {
	t.Parallel()

	query := sessionQueryFor(dbcompat.KindOpenGauss)
	want := "ON a.pid = t.tid AND a.sessionid = t.sessionid"
	if !strings.Contains(query, want) {
		t.Fatalf("session query must join on unique thread identity %q; query: %s", want, query)
	}
}

func TestBuildSessionRowIgnoresEmptyBlockers(t *testing.T) {
	t.Parallel()

	for _, blockerValue := range []any{nil, "", "  ", []byte(" ")} {
		raw := make(dbconn.Row, 12)
		raw[7] = blockerValue
		row, blocker := buildSessionRow(raw, nil, nil)
		if blocker != nil || row.Display(model.SIdxBlocker) != "" {
			t.Errorf("blocker %q became signal=%v display=%q", blockerValue, blocker, row.Display(model.SIdxBlocker))
		}
	}
}

func TestBlockRoleUsesRealBlockerSet(t *testing.T) {
	t.Parallel()

	blockers := newBlockerPIDSet([]any{int64(10), int64(20)})
	tests := []struct {
		name         string
		pid, blocker any
		want         string
	}{
		{name: "unblocked", pid: 30, blocker: "", want: ""},
		{name: "waiter", pid: 30, blocker: 10, want: lockWaiter},
		{name: "holder", pid: 10, blocker: "", want: lockHolder},
		{name: "holder and waiter", pid: 20, blocker: 10, want: lockHolderWaiter},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := blockRole(tc.pid, tc.blocker, blockers); got != tc.want {
				t.Fatalf("blockRole(%v,%v)=%q, want %q", tc.pid, tc.blocker, got, tc.want)
			}
		})
	}
}
