package gsbench

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

type StaleRecoveryStatus struct {
	RunIDs           []string
	DatabaseRunCount int
	LocalActionCount int
}

type staleRunJournal interface {
	StaleRunIDs(context.Context) ([]string, error)
}

func ReadStaleRecoveryStatus(
	ctx context.Context,
	journal staleRunJournal,
	ledger RecoveryLedger,
) (StaleRecoveryStatus, error) {
	var status StaleRecoveryStatus
	var errs []error
	seen := make(map[string]bool)
	if journal == nil {
		errs = append(errs, fmt.Errorf("database action journal is unavailable"))
	} else {
		runs, err := journal.StaleRunIDs(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"read pending database actions: %w", err,
			))
		} else {
			status.DatabaseRunCount = len(runs)
			for _, runID := range runs {
				seen[runID] = true
			}
		}
	}
	if ledger == nil {
		errs = append(errs, fmt.Errorf("local recovery ledger is unavailable"))
	} else {
		actions, err := ledger.Pending(ctx, "")
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"read pending local actions: %w", err,
			))
		} else {
			status.LocalActionCount = len(actions)
			for _, action := range actions {
				seen[action.RunID] = true
			}
		}
	}
	status.RunIDs = make([]string, 0, len(seen))
	for runID := range seen {
		status.RunIDs = append(status.RunIDs, runID)
	}
	sort.Strings(status.RunIDs)
	return status, errors.Join(errs...)
}

func StopTaggedSQL(runID string) (query string, args []any, err error) {
	predicate, args, err := TaggedSessionPredicate(runID)
	if err != nil {
		return "", nil, err
	}
	return "SELECT pg_terminate_session(pid,sessionid) FROM pg_stat_activity WHERE " + predicate + " AND pid<>pg_backend_pid()", args, nil
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
