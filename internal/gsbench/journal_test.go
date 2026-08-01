package gsbench

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type memoryActionStore struct {
	onInsert    func(Action)
	entries     []Action
	states      map[int64]MutationState
	details     map[int64]string
	stale       []string
	insertError error
	stateError  error
}

func (s *memoryActionStore) InsertPlanned(_ context.Context, action Action) (Action, error) {
	if s.insertError != nil {
		return Action{}, s.insertError
	}
	if s.onInsert != nil {
		s.onInsert(action)
	}
	if action.Sequence == 0 {
		action.Sequence = int64(len(s.entries) + 1)
	}
	action.State = MutationPlanned
	s.entries = append(s.entries, action)
	return action, nil
}

func (s *memoryActionStore) SetState(
	_ context.Context,
	_ string,
	id int64,
	state MutationState,
	detail string,
) error {
	if s.stateError != nil {
		return s.stateError
	}
	if s.states == nil {
		s.states = map[int64]MutationState{}
	}
	if s.details == nil {
		s.details = map[int64]string{}
	}
	s.states[id] = state
	s.details[id] = detail
	return nil
}

func (s *memoryActionStore) Pending(_ context.Context, runID string) ([]Action, error) {
	var out []Action
	for _, action := range s.entries {
		if action.RunID != runID {
			continue
		}
		if state, ok := s.states[action.Sequence]; ok {
			action.State = state
		}
		if action.State != MutationRestored {
			out = append(out, action)
		}
	}
	return out, nil
}

func (s *memoryActionStore) StaleRuns(context.Context) ([]string, error) {
	return append([]string(nil), s.stale...), nil
}

type memoryActionExecutor struct {
	onPreflight    func(Action)
	onApply        func(Action)
	onRestore      func(Action)
	preflightError error
	applyError     error
	restoreError   error
	verifyError    error
	verifyActions  []Action
}

type racingClaimStore struct {
	mu             sync.Mutex
	action         Action
	restoringCalls int
	restoringGate  chan struct{}
	claimed        bool
}

func (s *racingClaimStore) InsertPlanned(
	context.Context,
	Action,
) (Action, error) {
	return Action{}, errors.New("not supported")
}

func (s *racingClaimStore) SetState(
	_ context.Context,
	_ string,
	_ int64,
	state MutationState,
	_ string,
) error {
	if state != MutationRestoring {
		return nil
	}
	s.mu.Lock()
	s.restoringCalls++
	if s.restoringCalls == 2 {
		close(s.restoringGate)
	}
	s.mu.Unlock()
	<-s.restoringGate
	return nil
}

func (s *racingClaimStore) ClaimAction(
	context.Context,
	Action,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed {
		return false, nil
	}
	s.claimed = true
	return true, nil
}

func (s *racingClaimStore) Pending(
	context.Context,
	string,
) ([]Action, error) {
	return []Action{s.action}, nil
}

func (s *racingClaimStore) StaleRuns(context.Context) ([]string, error) {
	return []string{s.action.RunID}, nil
}

type countingRestoreExecutor struct {
	mu       sync.Mutex
	restores int
}

func (*countingRestoreExecutor) Preflight(context.Context, Action) error {
	return nil
}
func (*countingRestoreExecutor) Apply(context.Context, Action) error {
	return nil
}
func (e *countingRestoreExecutor) Restore(context.Context, Action) error {
	e.mu.Lock()
	e.restores++
	e.mu.Unlock()
	return nil
}
func (*countingRestoreExecutor) VerifyRestored(context.Context, Action) error {
	return nil
}

func TestJournalCASClaimPreventsConcurrentDoubleInverse(t *testing.T) {
	action := validSQLJournalAction()
	action.RunID = "run-race"
	action.Sequence = 7
	action.Target = "target"
	action.State = MutationApplied
	store := &racingClaimStore{
		action: action, restoringGate: make(chan struct{}),
	}
	executor := &countingRestoreExecutor{}
	journal := NewJournal(store, executor)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = journal.restoreCoordinatorActions(context.Background(), []Action{action})
		}()
	}
	wg.Wait()

	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.restores != 1 {
		t.Fatalf("inverse executions=%d want=1", executor.restores)
	}
}

func (e *memoryActionExecutor) Preflight(_ context.Context, action Action) error {
	if e.onPreflight != nil {
		e.onPreflight(action)
	}
	return e.preflightError
}

func (e *memoryActionExecutor) Apply(_ context.Context, action Action) error {
	if e.onApply != nil {
		e.onApply(action)
	}
	return e.applyError
}

func (e *memoryActionExecutor) Restore(_ context.Context, action Action) error {
	if e.onRestore != nil {
		e.onRestore(action)
	}
	return e.restoreError
}

func (e *memoryActionExecutor) VerifyRestored(_ context.Context, action Action) error {
	e.verifyActions = append(e.verifyActions, action)
	return e.verifyError
}

func validSQLJournalAction() Action {
	return Action{
		RunID:         "run-1",
		ScenarioCode:  601,
		Kind:          ActionSQLMutation,
		TargetProduct: ProductGaussDB,
		Target:        "gsbench.plan_data",
		Forward:       []byte(`{"sql":"ALTER TABLE forward"}`),
		Inverse:       []byte(`{"sql":"ALTER TABLE inverse"}`),
	}
}

func TestApplyActionPersistsBeforeForwardExecution(t *testing.T) {
	var order []string
	store := &memoryActionStore{
		onInsert: func(Action) { order = append(order, "journal") },
	}
	executor := &memoryActionExecutor{
		onPreflight: func(Action) { order = append(order, "preflight") },
		onApply:     func(Action) { order = append(order, "apply") },
	}
	if err := NewJournal(store, executor).ApplyAction(
		context.Background(), validSQLJournalAction(),
	); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"preflight", "journal", "apply"}) {
		t.Fatalf("order = %v", order)
	}
	if store.states[1] != MutationApplied {
		t.Fatalf("state = %q, want %q", store.states[1], MutationApplied)
	}
}

func TestJournalKeepsApplyPreflightWhenModelValidationDisabled(t *testing.T) {
	store := &memoryActionStore{}
	executor := &memoryActionExecutor{
		preflightError: errors.New("preflight validation failed"),
	}
	err := NewJournalWithValidation(store, executor, false).ApplyAction(
		context.Background(),
		validSQLJournalAction(),
	)
	if err == nil || !strings.Contains(err.Error(), "preflight validation failed") {
		t.Fatalf("error=%v", err)
	}
	if len(store.entries) != 0 {
		t.Fatalf("entries=%+v states=%+v", store.entries, store.states)
	}
}

func TestApplyActionPreflightRejectsUnusableSQLInverseBeforePersistence(t *testing.T) {
	tests := []struct {
		name    string
		inverse string
	}{
		{name: "empty object", inverse: `{}`},
		{name: "wrong field", inverse: `{"operation":"restore"}`},
		{name: "blank SQL", inverse: `{"sql":"   "}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			persisted := false
			store := &memoryActionStore{
				onInsert: func(Action) { persisted = true },
			}
			database := &fakeSQLActionDatabase{}
			action := validSQLJournalAction()
			action.TargetProduct = ProductGaussDB
			action.Inverse = []byte(tt.inverse)
			err := NewJournal(store, dbActionExecutor{db: database}).
				ApplyAction(context.Background(), action)
			if err == nil || !strings.Contains(err.Error(), "inverse") {
				t.Fatalf("ApplyAction() error = %v", err)
			}
			if persisted {
				t.Fatal("action with unusable inverse was persisted")
			}
			if len(database.executed) != 0 {
				t.Fatalf("executed SQL = %v", database.executed)
			}
		})
	}
}

func TestApplyActionPreflightRejectsNewMultiStatementSQLBeforePersistence(t *testing.T) {
	persisted := false
	store := &memoryActionStore{
		onInsert: func(Action) { persisted = true },
	}
	action := validSQLJournalAction()
	action.Forward = []byte(`{"sql":"SELECT 1; SELECT 2"}`)
	err := NewJournal(store, dbActionExecutor{db: &fakeSQLActionDatabase{}}).
		ApplyAction(context.Background(), action)
	if err == nil || !strings.Contains(err.Error(), "one executable statement") {
		t.Fatalf("ApplyAction() error = %v", err)
	}
	if persisted {
		t.Fatal("multi-statement typed SQL was persisted")
	}
}

func TestApplyActionRejectsCallerForgedLegacySQLProvenance(t *testing.T) {
	persisted := false
	store := &memoryActionStore{
		onInsert: func(Action) { persisted = true },
	}
	action := validSQLJournalAction()
	action.LegacySQL = true
	action.Forward = []byte(`{"sql":"SELECT 1; SELECT 2"}`)
	err := NewJournal(store, dbActionExecutor{db: &fakeSQLActionDatabase{}}).
		ApplyAction(context.Background(), action)
	if err == nil || !strings.Contains(err.Error(), "legacy SQL provenance") {
		t.Fatalf("ApplyAction() error = %v", err)
	}
	if persisted {
		t.Fatal("caller-forged legacy action was persisted")
	}
}

func TestApplyActionRejectsInvalidActionBeforePersistence(t *testing.T) {
	persisted := false
	store := &memoryActionStore{
		onInsert: func(Action) { persisted = true },
	}
	action := validSQLJournalAction()
	action.Kind = ActionKind("SQL")
	err := NewJournal(store, &memoryActionExecutor{}).
		ApplyAction(context.Background(), action)
	if err == nil || !strings.Contains(err.Error(), "action kind") {
		t.Fatalf("ApplyAction() error = %v", err)
	}
	if persisted {
		t.Fatal("invalid action was persisted")
	}
}

func TestApplyActionFailureRemainsPlannedForRestoreRetry(t *testing.T) {
	store := &memoryActionStore{}
	executor := &memoryActionExecutor{applyError: errors.New("forward failed")}
	err := NewJournal(store, executor).ApplyAction(
		context.Background(), validSQLJournalAction(),
	)
	if err == nil || !strings.Contains(err.Error(), "forward failed") {
		t.Fatalf("ApplyAction() error = %v", err)
	}
	if store.states[1] != MutationPlanned {
		t.Fatalf("state = %q, want retryable planned state", store.states[1])
	}
	if !strings.Contains(store.details[1], "forward failed") {
		t.Fatalf("last error = %q", store.details[1])
	}
	pending, pendingErr := store.Pending(context.Background(), "run-1")
	if pendingErr != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v, error = %v", pending, pendingErr)
	}
}

func TestJournalSanitizesEveryPersistedExecutorError(t *testing.T) {
	const secret = "journal-secret"
	tests := []struct {
		name     string
		executor *memoryActionExecutor
		restore  bool
	}{
		{
			name: "forward",
			executor: &memoryActionExecutor{
				applyError: errors.New("provider password=" + secret),
			},
		},
		{
			name: "restore",
			executor: &memoryActionExecutor{
				restoreError: errors.New(
					"https://bench:" + secret + "@provider/fault",
				),
			},
			restore: true,
		},
		{
			name: "verify",
			executor: &memoryActionExecutor{
				verifyError: errors.New("Authorization: Bearer " + secret),
			},
			restore: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action := validSQLJournalAction()
			action.Sequence = 1
			action.State = MutationApplied
			store := &memoryActionStore{}
			var err error
			if tt.restore {
				store.entries = []Action{action}
				err = NewJournal(store, tt.executor).
					restoreCoordinatorActions(
						context.Background(),
						[]Action{action},
					)
			} else {
				err = NewJournal(store, tt.executor).
					ApplyAction(context.Background(), action)
			}
			if err == nil {
				t.Fatal("expected executor failure")
			}
			detail := store.details[1]
			if !strings.Contains(detail, "redacted") {
				t.Fatalf("persisted detail = %q", detail)
			}
			if strings.Contains(detail, secret) {
				t.Fatalf("persisted detail leaked secret: %q", detail)
			}
		})
	}
}

func TestJournalSanitizesCallerSuppliedLastErrorBeforeInsert(t *testing.T) {
	const secret = "caller-token-value"
	var persisted Action
	store := &memoryActionStore{
		onInsert: func(action Action) { persisted = action },
	}
	action := validSQLJournalAction()
	action.LastError = "Bearer " + secret
	if err := NewJournal(store, &memoryActionExecutor{}).
		ApplyAction(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(persisted.LastError, "redacted") ||
		strings.Contains(persisted.LastError, secret) {
		t.Fatalf("persisted last error = %q", persisted.LastError)
	}
}

func TestJournalBoundsSafePersistedErrorDetail(t *testing.T) {
	store := &memoryActionStore{}
	executor := &memoryActionExecutor{
		applyError: errors.New(strings.Repeat("safe context ", 100)),
	}
	err := NewJournal(store, executor).ApplyAction(
		context.Background(), validSQLJournalAction(),
	)
	if err == nil {
		t.Fatal("expected executor failure")
	}
	if got := len([]rune(store.details[1])); got > 512 {
		t.Fatalf("persisted error length = %d, want <= 512", got)
	}
}

func TestApplyMutationUsesSQLActionAdapter(t *testing.T) {
	store := &memoryActionStore{}
	var applied Action
	executor := &memoryActionExecutor{
		onApply: func(action Action) { applied = action },
	}
	err := NewJournal(store, executor).Apply(context.Background(), Mutation{
		RunID:         "run-1",
		Scenario:      "plan_stats_target",
		Kind:          "statistics_target",
		Target:        "gsbench.plan_data.stats_target_key",
		ForwardSQL:    "ALTER TABLE forward",
		InverseSQL:    "ALTER TABLE inverse",
		VerifySQL:     "SELECT restored",
		VerifyValue:   "true",
		Original:      "-1",
		TargetProduct: ProductGaussDB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Kind != ActionSQLMutation || applied.ScenarioCode != 601 {
		t.Fatalf("adapter action = %+v", applied)
	}
	if string(applied.Forward) != `{"sql":"ALTER TABLE forward"}` ||
		string(applied.Inverse) != `{"sql":"ALTER TABLE inverse"}` {
		t.Fatalf("adapter payloads forward=%s inverse=%s", applied.Forward, applied.Inverse)
	}
}

func TestApplyMutationDefaultsConfiguredTargetProduct(t *testing.T) {
	store := &memoryActionStore{}
	var applied Action
	executor := &memoryActionExecutor{
		onApply: func(action Action) { applied = action },
	}
	err := NewJournal(store, executor, ProductOpenGauss).Apply(
		context.Background(),
		Mutation{
			RunID: "run-1", Scenario: "vacuum_pressure",
			Target:     "gsbench.vacuum_targets",
			ForwardSQL: "UPDATE gsbench.vacuum_targets SET version=1",
			InverseSQL: "UPDATE gsbench.vacuum_targets SET version=0",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if applied.TargetProduct != ProductOpenGauss {
		t.Fatalf("target product = %q", applied.TargetProduct)
	}
}

func TestRestoreActionsUsesJournalEngineAndPreservesCoordinatorOrder(t *testing.T) {
	var restored []ActionKind
	actions := []Action{
		{
			Sequence: 1, RunID: "run-1", ScenarioCode: 101,
			Kind: ActionNetworkFirewall, State: MutationApplied,
		},
		{
			Sequence: 99, RunID: "run-1", ScenarioCode: 102,
			Kind: ActionNetworkQDisc, State: MutationApplied,
		},
	}
	store := &memoryActionStore{entries: actions}
	executor := &memoryActionExecutor{
		onRestore: func(action Action) {
			restored = append(restored, action.Kind)
		},
	}

	if err := NewJournal(store, executor).restoreCoordinatorActions(
		context.Background(), actions,
	); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, []ActionKind{
		ActionNetworkFirewall,
		ActionNetworkQDisc,
	}) {
		t.Fatalf("coordinator restore order = %v", restored)
	}
	for _, action := range actions {
		if store.states[action.Sequence] != MutationRestored {
			t.Fatalf(
				"action %d state=%q want=%q",
				action.Sequence,
				store.states[action.Sequence],
				MutationRestored,
			)
		}
	}
}

func TestRestoreVerificationFailureRemainsPendingForRetry(t *testing.T) {
	action := validSQLJournalAction()
	action.Sequence = 7
	action.Verify = []byte(`{"sql":"SELECT restored","expected":"true"}`)
	action.State = MutationApplied
	store := &memoryActionStore{entries: []Action{action}}
	executor := &memoryActionExecutor{
		verifyError: errors.New("restore verification failed"),
	}
	err := NewJournal(store, executor).restoreCoordinatorActions(
		context.Background(),
		[]Action{action},
	)
	if err == nil || !strings.Contains(err.Error(), "restore verification failed") {
		t.Fatalf("RestoreActions() error = %v", err)
	}
	if store.states[7] != MutationRestoreFailed {
		t.Fatalf("state = %q, want %q", store.states[7], MutationRestoreFailed)
	}
	pending, pendingErr := store.Pending(context.Background(), "run-1")
	if pendingErr != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v, error = %v", pending, pendingErr)
	}
}

func TestJournalKeepsRestoreVerificationWhenModelValidationDisabled(t *testing.T) {
	action := validSQLJournalAction()
	action.Sequence = 7
	action.Verify = []byte(`{"sql":"SELECT restored","expected":"true"}`)
	action.State = MutationApplied
	store := &memoryActionStore{entries: []Action{action}}
	executor := &memoryActionExecutor{
		verifyError: errors.New("restore validation failed"),
	}
	err := NewJournalWithValidation(store, executor, false).
		restoreCoordinatorActions(context.Background(), []Action{action})
	if err == nil || !strings.Contains(err.Error(), "restore validation failed") {
		t.Fatalf("error=%v", err)
	}
	if store.states[7] != MutationRestoreFailed {
		t.Fatalf("state=%q want=%q", store.states[7], MutationRestoreFailed)
	}
	if len(executor.verifyActions) != 1 {
		t.Fatalf("restore verification calls=%d", len(executor.verifyActions))
	}
}
