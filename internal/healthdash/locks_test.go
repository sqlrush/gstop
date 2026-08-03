package healthdash

import (
	"testing"

	"gstop/internal/dbconn"
)

func TestParseLockHealthDeduplicatesAndRanksTopFive(t *testing.T) {
	rows := []dbconn.Row{
		{int64(11), "11.1", int64(21), "21.1", "relation", "RowExclusiveLock", "tag-a", int64(101), "update a", float64(9_000_000)},
		{int64(11), "11.1", int64(21), "21.1", "relation", "RowExclusiveLock", "tag-a", int64(101), "update a", float64(9_000_000)},
		{int64(12), "12.1", int64(21), "21.1", "tuple", "ExclusiveLock", "tag-b", int64(102), "update b", float64(8_000_000)},
		{int64(13), "13.1", int64(22), "22.1", "tuple", "ExclusiveLock", "tag-c", int64(103), "update c", float64(7_000_000)},
		{int64(14), "14.1", int64(22), "22.1", "tuple", "ExclusiveLock", "tag-d", int64(104), "update d", float64(6_000_000)},
		{int64(15), "15.1", int64(23), "23.1", "tuple", "ExclusiveLock", "tag-e", int64(105), "update e", float64(5_000_000)},
		{int64(16), "16.1", int64(23), "23.1", "tuple", "ExclusiveLock", "tag-f", int64(106), "update f", float64(4_000_000)},
	}

	got, ok := parseLockHealth(rows)
	if !ok || got.Waiters != 6 || got.Blockers != 3 || len(got.Chains) != 5 || got.LongestWaitUS != 9_000_000 {
		t.Fatalf("lock health=%+v ok=%v", got, ok)
	}
	if got.Chains[0].WaiterPID != 11 || got.Chains[4].WaiterPID != 15 {
		t.Fatalf("ranked chains=%+v", got.Chains)
	}
}

func TestParseLockHealthDistinguishesEmptyFromFailed(t *testing.T) {
	if got, ok := parseLockHealth([]dbconn.Row{}); !ok || got.Waiters != 0 || len(got.Chains) != 0 {
		t.Fatalf("empty result=%+v ok=%v", got, ok)
	}
	if got, ok := parseLockHealth(nil); ok || got.Waiters != 0 || len(got.Chains) != 0 {
		t.Fatalf("failed result=%+v ok=%v", got, ok)
	}
}
