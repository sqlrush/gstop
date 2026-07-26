package gsbench

var allLockModes = []LockMode{
	LockModeAccessShare, LockModeRowShare, LockModeRowExclusive,
	LockModeShareUpdateExclusive, LockModeShare, LockModeShareRowExclusive,
	LockModeExclusive, LockModeAccessExclusive,
}

func TableLockMatrixDefinitions(schema string) []LockDefinition {
	if !identifierRE.MatchString(schema) {
		return nil
	}
	conflicts := TableLockConflictDefinitions()
	definitions := make([]LockDefinition, 0, len(conflicts))
	for _, conflict := range conflicts {
		definitions = append(definitions, LockDefinition{
			Code: conflict.Code, Name: conflict.Name, Object: "lock_mode_targets",
			HolderSQL:  []string{lockTableSQL(schema, "lock_mode_targets", string(conflict.Holder))},
			WaiterSQL:  []string{lockTableSQL(schema, "lock_mode_targets", string(conflict.Waiter))},
			HolderMode: fullLockMode(conflict.Holder), WaiterMode: fullLockMode(conflict.Waiter),
			ExpectedKind: "table_lock_matrix", HolderTag: "blocker", WaiterTag: "waiter",
			HolderTransactional: true, WaiterTransactional: true,
		})
	}
	return definitions
}

func fullLockMode(mode LockMode) string {
	switch mode {
	case LockModeAccessShare:
		return "AccessShare"
	case LockModeRowShare:
		return "RowShare"
	case LockModeRowExclusive:
		return "RowExclusive"
	case LockModeShareUpdateExclusive:
		return "ShareUpdateExclusive"
	case LockModeShare:
		return "Share"
	case LockModeShareRowExclusive:
		return "ShareRowExclusive"
	case LockModeExclusive:
		return "Exclusive"
	case LockModeAccessExclusive:
		return "AccessExclusive"
	default:
		return ""
	}
}

func sqlLockMode(mode LockMode) string {
	switch mode {
	case LockModeAccessShare:
		return "ACCESS SHARE"
	case LockModeRowShare:
		return "ROW SHARE"
	case LockModeRowExclusive:
		return "ROW EXCLUSIVE"
	case LockModeShareUpdateExclusive:
		return "SHARE UPDATE EXCLUSIVE"
	case LockModeShare:
		return "SHARE"
	case LockModeShareRowExclusive:
		return "SHARE ROW EXCLUSIVE"
	case LockModeExclusive:
		return "EXCLUSIVE"
	case LockModeAccessExclusive:
		return "ACCESS EXCLUSIVE"
	default:
		return ""
	}
}

func TableLockModesConflict(holder, waiter LockMode) bool {
	for _, definition := range TableLockConflictDefinitions() {
		if definition.Holder == holder && definition.Waiter == waiter || definition.Holder == waiter && definition.Waiter == holder {
			return true
		}
	}
	return false
}

func TableLockModesCompatible(holder, waiter LockMode) bool {
	return !TableLockModesConflict(holder, waiter)
}
