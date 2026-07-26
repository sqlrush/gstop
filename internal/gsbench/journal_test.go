package gsbench

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type memoryJournalStore struct {
	events  *[]string
	entries []JournalEntry
	states  map[int64]MutationState
	stale   []string
}

func (s *memoryJournalStore) InsertPlanned(_ context.Context, mutation Mutation) (JournalEntry, error) {
	*s.events = append(*s.events, "journal:"+mutation.ForwardSQL)
	entry := JournalEntry{ID: int64(len(s.entries) + 1), Mutation: mutation, State: MutationPlanned}
	s.entries = append(s.entries, entry)
	return entry, nil
}

func (s *memoryJournalStore) SetState(_ context.Context, id int64, state MutationState, _ string) error {
	if s.states == nil {
		s.states = map[int64]MutationState{}
	}
	s.states[id] = state
	return nil
}

func (s *memoryJournalStore) Pending(_ context.Context, runID string) ([]JournalEntry, error) {
	var out []JournalEntry
	for _, entry := range s.entries {
		if entry.RunID == runID && entry.State != MutationRestored {
			if state, ok := s.states[entry.ID]; ok {
				entry.State = state
			}
			out = append(out, entry)
		}
	}
	return out, nil
}

func (s *memoryJournalStore) StaleRuns(context.Context) ([]string, error) {
	return append([]string(nil), s.stale...), nil
}

type memoryMutationExecutor struct {
	events     *[]string
	failSQL    string
	verifyFail bool
}

func (e *memoryMutationExecutor) Exec(_ context.Context, sql string) error {
	*e.events = append(*e.events, "exec:"+sql)
	if sql == e.failSQL {
		return errors.New("exec failed")
	}
	return nil
}

func (e *memoryMutationExecutor) Verify(_ context.Context, _, _ string) error {
	if e.verifyFail {
		return errors.New("verify failed")
	}
	return nil
}

func TestJournalRecordsBeforeForwardMutation(t *testing.T) {
	var events []string
	store := &memoryJournalStore{events: &events}
	exec := &memoryMutationExecutor{events: &events}
	j := NewJournal(store, exec)
	err := j.Apply(context.Background(), Mutation{
		RunID: "run-1", Scenario: "plan_regression", Target: "gsbench.idx_plan",
		ForwardSQL: "ALTER INDEX unusable", InverseSQL: "REINDEX INDEX gsbench.idx_plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"journal:ALTER INDEX unusable", "exec:ALTER INDEX unusable"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if store.states[1] != MutationApplied {
		t.Fatalf("state = %q", store.states[1])
	}
}

func TestJournalRestoresMutationsInReverseOrder(t *testing.T) {
	var events []string
	store := &memoryJournalStore{events: &events, entries: []JournalEntry{
		{ID: 1, Mutation: Mutation{RunID: "run-1", InverseSQL: "undo-1"}, State: MutationApplied},
		{ID: 2, Mutation: Mutation{RunID: "run-1", InverseSQL: "undo-2"}, State: MutationApplied},
	}}
	exec := &memoryMutationExecutor{events: &events}
	if err := NewJournal(store, exec).RestoreRun(context.Background(), "run-1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"exec:undo-2", "exec:undo-1"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if store.states[1] != MutationRestored || store.states[2] != MutationRestored {
		t.Fatalf("states = %v", store.states)
	}
}

func TestJournalRetainsFailedRestoreForRetry(t *testing.T) {
	var events []string
	store := &memoryJournalStore{events: &events, entries: []JournalEntry{
		{ID: 1, Mutation: Mutation{RunID: "run-1", InverseSQL: "undo"}, State: MutationApplied},
	}}
	exec := &memoryMutationExecutor{events: &events, failSQL: "undo"}
	if err := NewJournal(store, exec).RestoreRun(context.Background(), "run-1"); err == nil {
		t.Fatal("expected restore failure")
	}
	if store.states[1] != MutationRestoreFailed {
		t.Fatalf("state = %q", store.states[1])
	}
}

func TestJournalRestoreScenarioLeavesOtherScenarioApplied(t *testing.T) {
	var events []string
	store := &memoryJournalStore{events: &events, entries: []JournalEntry{
		{ID: 1, Mutation: Mutation{RunID: "run-1", Scenario: "plan_regression", InverseSQL: "undo-plan"}, State: MutationApplied},
		{ID: 2, Mutation: Mutation{RunID: "run-1", Scenario: "vacuum_pressure", InverseSQL: "undo-vacuum"}, State: MutationApplied},
	}}
	if err := NewJournal(store, &memoryMutationExecutor{events: &events}).RestoreScenario(context.Background(), "run-1", "plan_regression"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"exec:undo-plan"}) {
		t.Fatalf("events=%v", events)
	}
}
