package gsbench

import "fmt"

func StopTaggedSQL(runID string) (query, arg string, err error) {
	predicate, arg, err := TaggedSessionPredicate(runID)
	if err != nil {
		return "", "", err
	}
	return "SELECT pg_terminate_session(pid,sessionid) FROM pg_stat_activity WHERE " + predicate + " AND pid<>pg_backend_pid()", arg, nil
}

func CleanupPlan(schema string, withData bool) ([]string, error) {
	if !identifierRE.MatchString(schema) {
		return nil, fmt.Errorf("unsafe schema %q", schema)
	}
	steps := []string{"stop tagged workload sessions", "restore unresolved mutation journal entries"}
	if withData {
		steps = append(steps, "DROP SCHEMA "+schema+" CASCADE")
	}
	return steps, nil
}
