package gsbench

import (
	"strings"
	"testing"
)

func TestTableLockMatrixDefinitionsAreContiguousAndAllowlisted(t *testing.T) {
	defs := TableLockMatrixDefinitions("gsbench")
	if len(defs) != 21 {
		t.Fatalf("definitions=%d", len(defs))
	}
	for offset, def := range defs {
		if def.Code != ScenarioCode(520+offset) {
			t.Fatalf("code[%d]=%d", offset, def.Code)
		}
		if !strings.Contains(strings.Join(def.HolderSQL, "\n"), " MODE") ||
			!strings.Contains(strings.Join(def.WaiterSQL, "\n"), " MODE") {
			t.Fatalf("definition does not use full mode SQL: %+v", def)
		}
	}
}

func TestNonConflictPairsAreCompatible(t *testing.T) {
	for _, holder := range allLockModes {
		for _, waiter := range allLockModes {
			if holder > waiter || TableLockModesConflict(holder, waiter) {
				continue
			}
			if !TableLockModesCompatible(holder, waiter) {
				t.Fatalf("%s and %s should be compatible", holder, waiter)
			}
		}
	}
}
