package gsbench

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

type MutationState string

const (
	MutationPlanned       MutationState = "planned"
	MutationApplied       MutationState = "applied"
	MutationRestoring     MutationState = "restoring"
	MutationRestored      MutationState = "restored"
	MutationRestoreFailed MutationState = "restore_failed"
)

type Mutation struct {
	RunID       string
	Scenario    string
	Kind        string
	Target      string
	Original    string
	ForwardSQL  string
	InverseSQL  string
	VerifySQL   string
	VerifyValue string
}

type JournalEntry struct {
	ID int64
	Mutation
	State MutationState
	Error string
}

type JournalStore interface {
	InsertPlanned(ctx context.Context, mutation Mutation) (JournalEntry, error)
	SetState(ctx context.Context, id int64, state MutationState, detail string) error
	Pending(ctx context.Context, runID string) ([]JournalEntry, error)
	StaleRuns(ctx context.Context) ([]string, error)
}

type MutationExecutor interface {
	Exec(ctx context.Context, query string) error
	Verify(ctx context.Context, query, expected string) error
}

type Journal struct {
	store JournalStore
	exec  MutationExecutor
}

func NewJournal(store JournalStore, exec MutationExecutor) *Journal {
	return &Journal{store: store, exec: exec}
}

func (j *Journal) Apply(ctx context.Context, mutation Mutation) error {
	if mutation.RunID == "" || mutation.Scenario == "" || mutation.Target == "" {
		return fmt.Errorf("mutation run, scenario, and target are required")
	}
	if mutation.ForwardSQL == "" || mutation.InverseSQL == "" {
		return fmt.Errorf("mutation forward and inverse SQL are required")
	}
	entry, err := j.store.InsertPlanned(ctx, mutation)
	if err != nil {
		return fmt.Errorf("record mutation before apply: %w", err)
	}
	if err := j.exec.Exec(ctx, mutation.ForwardSQL); err != nil {
		_ = j.store.SetState(ctx, entry.ID, MutationRestoreFailed, err.Error())
		return fmt.Errorf("apply mutation %s: %w", mutation.Target, err)
	}
	if err := j.store.SetState(ctx, entry.ID, MutationApplied, ""); err != nil {
		return fmt.Errorf("mark mutation applied: %w", err)
	}
	return nil
}

func (j *Journal) RestoreRun(ctx context.Context, runID string) error {
	entries, err := j.store.Pending(ctx, runID)
	if err != nil {
		return fmt.Errorf("load pending mutations: %w", err)
	}
	return j.restoreEntries(ctx, entries)
}

func (j *Journal) RestoreScenario(ctx context.Context, runID, scenario string) error {
	entries, err := j.store.Pending(ctx, runID)
	if err != nil {
		return fmt.Errorf("load pending mutations: %w", err)
	}
	filtered := entries[:0]
	for _, entry := range entries {
		if entry.Scenario == scenario {
			filtered = append(filtered, entry)
		}
	}
	return j.restoreEntries(ctx, filtered)
}

func (j *Journal) restoreEntries(ctx context.Context, entries []JournalEntry) error {
	sort.SliceStable(entries, func(i, k int) bool { return entries[i].ID > entries[k].ID })
	var errs []error
	for _, entry := range entries {
		if entry.State == MutationRestored {
			continue
		}
		if err := j.store.SetState(ctx, entry.ID, MutationRestoring, ""); err != nil {
			errs = append(errs, err)
			continue
		}
		if entry.InverseSQL == "" {
			err := fmt.Errorf("journal entry %d has no inverse SQL", entry.ID)
			_ = j.store.SetState(ctx, entry.ID, MutationRestoreFailed, err.Error())
			errs = append(errs, err)
			continue
		}
		if err := j.exec.Exec(ctx, entry.InverseSQL); err != nil {
			_ = j.store.SetState(ctx, entry.ID, MutationRestoreFailed, err.Error())
			errs = append(errs, fmt.Errorf("restore %s: %w", entry.Target, err))
			continue
		}
		if entry.VerifySQL != "" {
			if err := j.exec.Verify(ctx, entry.VerifySQL, entry.VerifyValue); err != nil {
				_ = j.store.SetState(ctx, entry.ID, MutationRestoreFailed, err.Error())
				errs = append(errs, fmt.Errorf("verify restore %s: %w", entry.Target, err))
				continue
			}
		}
		if err := j.store.SetState(ctx, entry.ID, MutationRestored, ""); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (j *Journal) RecoverStale(ctx context.Context) error {
	runs, err := j.store.StaleRuns(ctx)
	if err != nil {
		return fmt.Errorf("list stale runs: %w", err)
	}
	var errs []error
	for _, runID := range runs {
		if err := j.RestoreRun(ctx, runID); err != nil {
			errs = append(errs, fmt.Errorf("recover %s: %w", runID, err))
		}
	}
	return errors.Join(errs...)
}
