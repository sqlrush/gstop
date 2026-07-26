package gsbench

import (
	"strings"
	"testing"
)

func TestBusinessLockDefinitionsPreserveDirections(t *testing.T) {
	defs := lockDefinitionsByCode(BusinessLockDefinitions("gsbench", "run-1"))
	wantNames := map[ScenarioCode]string{
		501: "lock_row_chain", 502: "lock_table_exclusive", 503: "lock_ddl_wait", 504: "lock_deadlock",
		505: "lock_ddl_blocks_dml", 506: "lock_select_blocks_ddl", 507: "lock_vacuum_blocks_ddl",
		508: "lock_ddl_blocks_vacuum", 509: "lock_createindex_blocks_dml", 510: "lock_dml_blocks_createindex",
	}
	if len(defs) != len(wantNames) {
		t.Fatalf("definitions=%d want=%d", len(defs), len(wantNames))
	}
	for code, name := range wantNames {
		if defs[code].Name != name {
			t.Fatalf("code=%d got=%q want=%q", code, defs[code].Name, name)
		}
	}
	if !strings.Contains(strings.Join(defs[503].HolderSQL, "\n"), "UPDATE") ||
		!strings.Contains(strings.Join(defs[503].WaiterSQL, "\n"), "ALTER TABLE") {
		t.Fatalf("503=%+v", defs[503])
	}
	if !strings.Contains(strings.Join(defs[505].HolderSQL, "\n"), "ALTER TABLE") ||
		!strings.Contains(strings.Join(defs[505].WaiterSQL, "\n"), "UPDATE") {
		t.Fatalf("505=%+v", defs[505])
	}
}

func TestBusinessLockDefinitionsUseOnlyBenchmarkObjects(t *testing.T) {
	for _, def := range BusinessLockDefinitions("gsbench", "run-1") {
		for _, sql := range append(append([]string{}, def.HolderSQL...), def.WaiterSQL...) {
			if strings.Contains(sql, "public.") || strings.Contains(sql, "pg_catalog") {
				t.Fatalf("%d uses a non-benchmark object: %s", def.Code, sql)
			}
		}
	}
}

func TestBusinessLockDefinitionsUseVacuumAndCreateIndexWorkloads(t *testing.T) {
	defs := lockDefinitionsByCode(BusinessLockDefinitions("gsbench", "run-1"))
	if !strings.Contains(strings.Join(defs[507].HolderSQL, "\n"), "VACUUM") {
		t.Fatalf("507 holder=%v", defs[507].HolderSQL)
	}
	if !strings.Contains(strings.Join(defs[508].WaiterSQL, "\n"), "VACUUM") {
		t.Fatalf("508 waiter=%v", defs[508].WaiterSQL)
	}
	if !strings.Contains(strings.Join(defs[509].HolderSQL, "\n"), "CREATE INDEX") {
		t.Fatalf("509 holder=%v", defs[509].HolderSQL)
	}
}

func TestRowChainWaiterLocksItsOwnRowBeforeWaitingOnRoot(t *testing.T) {
	definition := lockDefinitionsByCode(BusinessLockDefinitions("gsbench", "run-1"))[501]
	joined := strings.Join(definition.WaiterSQL, "\n")
	if !strings.Contains(joined, "id=2") || !strings.Contains(joined, "id=1") {
		t.Fatalf("waiter SQL=%s", joined)
	}
}

func TestLockTableSQLUsesSpacedAllowlistedModeTokens(t *testing.T) {
	got := lockTableSQL("gsbench", "lock_table_targets", string(LockModeAccessExclusive))
	if !strings.Contains(got, "IN ACCESS EXCLUSIVE MODE") || strings.Contains(got, "AccessExclusive") {
		t.Fatalf("sql=%s", got)
	}
}

func TestBusinessDefinitionsDeriveIdentifierSafeTokenFromDefaultRunID(t *testing.T) {
	runID := newRunID()
	if !strings.Contains(runID, "-") {
		t.Fatalf("test requires normal run ID shape: %q", runID)
	}
	defs := lockDefinitionsByCode(BusinessLockDefinitions("gsbench", runID))
	for _, code := range []ScenarioCode{503, 505, 506, 507, 509, 510} {
		for _, sql := range append(defs[code].HolderSQL, defs[code].WaiterSQL...) {
			if strings.Contains(sql, runID) {
				t.Fatalf("code=%d retained unsafe run id in SQL: %s", code, sql)
			}
		}
	}
}

func TestCreateIndexLockUpdatesVacuumVersionColumn(t *testing.T) {
	defs := lockDefinitionsByCode(BusinessLockDefinitions("gsbench", "run-1"))
	for _, code := range []ScenarioCode{509, 510} {
		statements := append(defs[code].HolderSQL, defs[code].WaiterSQL...)
		var update string
		for _, statement := range statements {
			if strings.HasPrefix(statement, "UPDATE ") {
				update = statement
			}
		}
		if !strings.Contains(update, "vacuum_targets SET version=version+1") || strings.Contains(update, "value=value+1") {
			t.Fatalf("code=%d update=%s", code, update)
		}
	}
}
