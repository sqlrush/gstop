package model

import (
	"fmt"
	"sync"
	"testing"
)

func TestDBInfoSnapshotRoundTrip(t *testing.T) {
	info := NewDBInfo()
	want := DBInfoSnapshot{Version: "v1", User: "u1", Role: "primary"}
	info.SetSnapshot(want)

	if got := info.Snapshot(); got != want {
		t.Fatalf("snapshot=%+v, want %+v", got, want)
	}
	if info.Version() != want.Version || info.User() != want.User || info.Role() != want.Role {
		t.Fatalf("legacy getters=%q/%q/%q", info.Version(), info.User(), info.Role())
	}
}

func TestDBInfoSnapshotNeverMixesConcurrentTupleUpdates(t *testing.T) {
	info := NewDBInfo()
	a := DBInfoSnapshot{Version: "version-a", User: "user-a", Role: "role-a"}
	b := DBInfoSnapshot{Version: "version-b", User: "user-b", Role: "role-b"}
	info.SetSnapshot(a)

	const iterations = 100_000
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if i%2 == 0 {
				info.SetSnapshot(b)
			} else {
				info.SetSnapshot(a)
			}
		}
	}()

	for i := 0; i < iterations; i++ {
		got := info.Snapshot()
		if got != a && got != b {
			t.Fatalf("mixed snapshot at iteration %d: %s", i, fmt.Sprintf("%+v", got))
		}
	}
	wg.Wait()
}
