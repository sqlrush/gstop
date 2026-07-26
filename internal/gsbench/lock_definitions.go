package gsbench

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

func BusinessLockDefinitions(schema, runID string) []LockDefinition {
	if !identifierRE.MatchString(schema) || !tagComponentRE.MatchString(runID) {
		return nil
	}
	token := lockRunToken(runID)
	ddlColumn := "ddl_" + token
	index := "lock_ci_" + token + "_idx"
	return []LockDefinition{
		{Code: 501, Name: "lock_row_chain", Object: "lock_targets", HolderSQL: []string{rowUpdate(schema, "lock_targets", 1)}, WaiterSQL: []string{rowUpdate(schema, "lock_targets", 2), rowUpdate(schema, "lock_targets", 1)}, HolderMode: "Exclusive", WaiterMode: "Share", ExpectedKind: "row_chain", HolderTag: "blocker", WaiterTag: "chain-2", HolderTransactional: true, WaiterTransactional: true, ChainRows: []int{1, 2, 3}, ChainTags: []string{"blocker", "chain-2", "chain-3"}},
		{Code: 502, Name: "lock_table_exclusive", Object: "lock_table_targets", HolderSQL: []string{lockTableSQL(schema, "lock_table_targets", string(LockModeAccessExclusive))}, WaiterSQL: []string{"SELECT count(*) FROM " + schema + ".lock_table_targets"}, HolderMode: "AccessExclusive", WaiterMode: "AccessShare", ExpectedKind: "table", HolderTag: "blocker", WaiterTag: "waiter", HolderTransactional: true, WaiterTransactional: true},
		{Code: 503, Name: "lock_ddl_wait", Object: "lock_ddl_targets", HolderSQL: []string{rowUpdate(schema, "lock_ddl_targets", 1)}, WaiterSQL: []string{addColumnSQL(schema, "lock_ddl_targets", ddlColumn)}, HolderMode: "RowExclusive", WaiterMode: "AccessExclusive", ExpectedKind: "dml_blocks_ddl", HolderTag: "blocker", WaiterTag: "waiter", HolderTransactional: true, WaiterTransactional: true},
		{Code: 504, Name: "lock_deadlock", Object: "lock_targets", HolderSQL: []string{rowUpdate(schema, "lock_targets", 100), rowUpdate(schema, "lock_targets", 101)}, WaiterSQL: []string{rowUpdate(schema, "lock_targets", 101), rowUpdate(schema, "lock_targets", 100)}, HolderMode: "Exclusive", WaiterMode: "Exclusive", ExpectedKind: "deadlock", Deadlock: true, HolderTag: "blocker", WaiterTag: "waiter", HolderTransactional: true, WaiterTransactional: true},
		{Code: 505, Name: "lock_ddl_blocks_dml", Object: "lock_ddl_targets", HolderSQL: []string{addColumnSQL(schema, "lock_ddl_targets", ddlColumn)}, WaiterSQL: []string{rowUpdate(schema, "lock_ddl_targets", 1)}, HolderMode: "AccessExclusive", WaiterMode: "RowExclusive", ExpectedKind: "ddl_blocks_dml", HolderTag: "blocker", WaiterTag: "waiter", HolderTransactional: true, WaiterTransactional: true},
		{Code: 506, Name: "lock_select_blocks_ddl", Object: "lock_ddl_targets", HolderSQL: []string{"SELECT count(*) FROM " + schema + ".lock_ddl_targets"}, WaiterSQL: []string{addColumnSQL(schema, "lock_ddl_targets", ddlColumn)}, HolderMode: "AccessShare", WaiterMode: "AccessExclusive", ExpectedKind: "select_blocks_ddl", HolderTag: "blocker", WaiterTag: "waiter", HolderTransactional: true, WaiterTransactional: true},
		{Code: 507, Name: "lock_vacuum_blocks_ddl", Object: "vacuum_targets", HolderSQL: []string{"VACUUM " + schema + ".vacuum_targets"}, WaiterSQL: []string{addColumnSQL(schema, "vacuum_targets", ddlColumn)}, HolderMode: "ShareUpdateExclusive", WaiterMode: "AccessExclusive", ExpectedKind: "vacuum_blocks_ddl", HolderTag: "blocker", WaiterTag: "waiter", HolderTransactional: false, WaiterTransactional: true},
		{Code: 508, Name: "lock_ddl_blocks_vacuum", Object: "vacuum_targets", HolderSQL: []string{lockTableSQL(schema, "vacuum_targets", string(LockModeAccessExclusive))}, WaiterSQL: []string{"VACUUM " + schema + ".vacuum_targets"}, HolderMode: "AccessExclusive", WaiterMode: "ShareUpdateExclusive", ExpectedKind: "ddl_blocks_vacuum", HolderTag: "blocker", WaiterTag: "waiter", HolderTransactional: true, WaiterTransactional: false},
		{Code: 509, Name: "lock_createindex_blocks_dml", Object: "vacuum_targets", HolderSQL: []string{createIndexSQL(schema, index)}, WaiterSQL: []string{vacuumRowUpdate(schema, 1)}, HolderMode: "Share", WaiterMode: "RowExclusive", ExpectedKind: "createindex_blocks_dml", HolderTag: "blocker", WaiterTag: "waiter", HolderTransactional: true, WaiterTransactional: true},
		{Code: 510, Name: "lock_dml_blocks_createindex", Object: "vacuum_targets", HolderSQL: []string{vacuumRowUpdate(schema, 1)}, WaiterSQL: []string{createIndexSQL(schema, index)}, HolderMode: "RowExclusive", WaiterMode: "Share", ExpectedKind: "dml_blocks_createindex", HolderTag: "blocker", WaiterTag: "waiter", HolderTransactional: true, WaiterTransactional: true},
	}
}

func rowUpdate(schema, table string, id int) string {
	return fmt.Sprintf("UPDATE %s.%s SET value=value+1 WHERE id=%d", schema, table, id)
}

func lockTableSQL(schema, table, mode string) string {
	return "LOCK TABLE " + schema + "." + table + " IN " + sqlLockMode(LockMode(mode)) + " MODE"
}

func lockRunToken(runID string) string {
	var token strings.Builder
	for _, character := range runID {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' {
			token.WriteRune(character)
		} else {
			token.WriteByte('_')
		}
	}
	base := strings.Trim(token.String(), "_")
	if base == "" {
		base = "run"
	}
	if len(base) > 36 {
		base = base[:36]
	}
	digest := sha256.Sum256([]byte(runID))
	return fmt.Sprintf("%s_%x", base, digest[:4])
}

func addColumnSQL(schema, table, column string) string {
	return "ALTER TABLE " + schema + "." + table + " ADD COLUMN " + column + " integer"
}

func createIndexSQL(schema, name string) string {
	return "CREATE INDEX " + schema + "." + name + " ON " + schema + ".vacuum_targets (version, id)"
}

func vacuumRowUpdate(schema string, id int) string {
	return fmt.Sprintf("UPDATE %s.vacuum_targets SET version=version+1 WHERE id=%d", schema, id)
}

func lockDefinitionsByCode(definitions []LockDefinition) map[ScenarioCode]LockDefinition {
	byCode := make(map[ScenarioCode]LockDefinition, len(definitions))
	for _, definition := range definitions {
		byCode[definition.Code] = definition
	}
	return byCode
}

func lockDefinitionForCode(code ScenarioCode, schema, runID string) (LockDefinition, bool) {
	for _, definition := range BusinessLockDefinitions(schema, runID) {
		if definition.Code == code {
			return definition, true
		}
	}
	for _, definition := range TableLockMatrixDefinitions(schema) {
		if definition.Code == code {
			return definition, true
		}
	}
	return LockDefinition{}, false
}
