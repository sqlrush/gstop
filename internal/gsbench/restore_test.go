package gsbench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeRestoreLock struct {
	events *[]string
	held   *bool
}

func (l fakeRestoreLock) Release() error {
	*l.events = append(*l.events, "unlock")
	if l.held != nil {
		*l.held = false
	}
	return nil
}

type fakeRestoreBackend struct {
	discovery             RestoreDiscovery
	events                []string
	fail                  map[string]error
	restoreLimit          time.Duration
	finalizationLimit     time.Duration
	blockOnAction         bool
	blockOnOutcome        bool
	rejectCanceledOutcome bool
	lockHeld              bool
	outcomeWithoutLock    bool
	outcomeDeadlines      []time.Time
	outcomeWriteFailures  map[string][]error
}

type localLockRestoreBackend struct {
	path      string
	discovery RestoreDiscovery
	journal   *Journal
}

func (b *localLockRestoreBackend) AcquireRestoreLock(
	ctx context.Context,
) (RestoreLock, error) {
	return acquireLocalRestoreLock(ctx, b.path)
}
func (b *localLockRestoreBackend) DiscoverRestore(
	context.Context,
	string,
) (RestoreDiscovery, error) {
	return b.discovery, nil
}
func (*localLockRestoreBackend) MarkRestoreRequested(
	context.Context,
	string,
) error {
	return nil
}
func (*localLockRestoreBackend) StopTaggedSessions(
	context.Context,
	string,
) error {
	return nil
}
func (b *localLockRestoreBackend) RestoreActionGroup(
	ctx context.Context,
	actions []Action,
) error {
	return b.journal.restoreCoordinatorActions(ctx, actions)
}
func (*localLockRestoreBackend) RepairBaseline(context.Context) error {
	return nil
}
func (*localLockRestoreBackend) RedetectTopology(context.Context) error {
	return nil
}
func (*localLockRestoreBackend) VerifyRestore(
	context.Context,
	[]string,
	[]Action,
) error {
	return nil
}
func (*localLockRestoreBackend) MarkRestoreOutcome(
	context.Context,
	string,
	Outcome,
) error {
	return nil
}

type gatedRestoreService struct {
	start   <-chan struct{}
	service restoreService
}

func (s gatedRestoreService) Restore(
	ctx context.Context,
	request RestoreRequest,
) RestoreSummary {
	select {
	case <-ctx.Done():
		return failedRestoreSummary(nil, nil, ctx.Err())
	case <-s.start:
	}
	return s.service.Restore(ctx, request)
}

func (f *fakeRestoreBackend) RestoreTimeout() time.Duration {
	return f.restoreLimit
}

func (f *fakeRestoreBackend) RestoreFinalizationTimeout() time.Duration {
	return f.finalizationLimit
}

func (f *fakeRestoreBackend) AcquireRestoreLock(context.Context) (RestoreLock, error) {
	f.events = append(f.events, "lock")
	if err := f.fail["lock"]; err != nil {
		return nil, err
	}
	f.lockHeld = true
	return fakeRestoreLock{events: &f.events, held: &f.lockHeld}, nil
}

func (f *fakeRestoreBackend) DiscoverRestore(
	_ context.Context,
	requestedRunID string,
) (RestoreDiscovery, error) {
	f.events = append(f.events, "discover:"+requestedRunID)
	return f.discovery, f.fail["discover"]
}

func (f *fakeRestoreBackend) MarkRestoreRequested(
	_ context.Context,
	runID string,
) error {
	f.events = append(f.events, "claim:"+runID)
	return f.fail["claim:"+runID]
}

func (f *fakeRestoreBackend) StopTaggedSessions(
	_ context.Context,
	runID string,
) error {
	f.events = append(f.events, "stop:"+runID)
	return f.fail["stop:"+runID]
}

func (f *fakeRestoreBackend) RestoreActionGroup(
	ctx context.Context,
	actions []Action,
) error {
	for _, action := range actions {
		f.events = append(f.events, "action:"+action.RunID+":"+
			string(action.Kind)+":"+action.Target)
	}
	if len(actions) == 0 {
		return nil
	}
	if f.blockOnAction {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.fail["action:"+actions[0].RunID+":"+string(actions[0].Kind)]
}

func (f *fakeRestoreBackend) RepairBaseline(context.Context) error {
	f.events = append(f.events, "baseline")
	return f.fail["baseline"]
}

func (f *fakeRestoreBackend) RedetectTopology(context.Context) error {
	f.events = append(f.events, "topology")
	return f.fail["topology"]
}

func (f *fakeRestoreBackend) VerifyRestore(
	_ context.Context,
	runIDs []string,
	_ []Action,
) error {
	f.events = append(f.events, "verify:"+strings.Join(runIDs, ","))
	return f.fail["verify"]
}

func (f *fakeRestoreBackend) MarkRestoreOutcome(
	ctx context.Context,
	runID string,
	outcome Outcome,
) error {
	if !f.lockHeld {
		f.outcomeWithoutLock = true
	}
	deadline, _ := ctx.Deadline()
	f.outcomeDeadlines = append(f.outcomeDeadlines, deadline)
	if f.rejectCanceledOutcome && ctx.Err() != nil {
		f.events = append(f.events, "outcome-rejected:"+runID)
		return fmt.Errorf("outcome context is canceled: %w", ctx.Err())
	}
	if f.blockOnOutcome {
		f.events = append(f.events, "outcome-start:"+runID)
		<-ctx.Done()
		return ctx.Err()
	}
	f.events = append(f.events, "outcome:"+runID+":"+string(outcome))
	key := runID + ":" + string(outcome)
	if failures := f.outcomeWriteFailures[key]; len(failures) != 0 {
		err := failures[0]
		f.outcomeWriteFailures[key] = failures[1:]
		if err != nil {
			return err
		}
	}
	return f.fail["outcome:"+runID]
}

func restoreTestAction(
	runID string,
	sequence int64,
	kind ActionKind,
	target string,
) Action {
	return Action{
		RunID: runID, Sequence: sequence, Kind: kind, Target: target,
		ScenarioCode: 601, TargetProduct: ProductGaussDB,
		Forward: []byte(`{"operation":"apply"}`),
		Inverse: []byte(`{"operation":"restore"}`),
	}
}

func TestRestoreCoordinatorRunsExactSafetyOrder(t *testing.T) {
	backend := &fakeRestoreBackend{discovery: RestoreDiscovery{
		Runs: []RestoreRun{{RunID: "run-1", StartedAt: time.Unix(10, 0)}},
		DatabaseActions: []Action{
			restoreTestAction("run-1", 9, ActionSQLMutation, "table"),
			restoreTestAction("run-1", 8, ActionGUCFileChange, "postgresql.conf"),
		},
		LocalActions: []Action{
			restoreTestAction("run-1", 7, ActionNetworkQDisc, "eth0"),
		},
	}}

	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{},
	)
	if summary.Failed {
		t.Fatal(summary.Err)
	}
	want := []string{
		"lock", "discover:", "claim:run-1", "stop:run-1",
		"action:run-1:NETWORK_QDISC:eth0",
		"action:run-1:GUC_FILE_CHANGE:postgresql.conf",
		"action:run-1:SQL_MUTATION:table",
		"baseline", "topology", "verify:run-1",
		"outcome:run-1:SUCCESS", "unlock",
	}
	if !reflect.DeepEqual(backend.events, want) {
		t.Fatalf("events=%v want=%v", backend.events, want)
	}
}

func TestRestoreCoordinatorSkipsTopologyAndStateValidationWhenDisabled(t *testing.T) {
	backend := &fakeRestoreBackend{
		discovery: RestoreDiscovery{
			Runs: []RestoreRun{{RunID: "run-1", StartedAt: time.Unix(10, 0)}},
		},
		fail: map[string]error{
			"topology": errors.New("topology validation failed"),
			"verify":   errors.New("restore validation failed"),
		},
	}
	summary := NewRestoreCoordinatorWithValidation(backend, false).Restore(
		context.Background(),
		RestoreRequest{},
	)
	if summary.Failed {
		t.Fatal(summary.Err)
	}
	for _, event := range backend.events {
		if event == "topology" || strings.HasPrefix(event, "verify:") {
			t.Fatalf("validation event executed: %s", event)
		}
	}
}

func TestRestoreCoordinatorRunsStartupCallbackBeforeUnlockForActiveOnlyRun(
	t *testing.T,
) {
	backend := &fakeRestoreBackend{discovery: RestoreDiscovery{
		Runs: []RestoreRun{{RunID: "stale-active"}},
	}}
	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{afterSuccess: func(context.Context) error {
			backend.events = append(backend.events, "start-new-run")
			return nil
		}},
	)
	if summary.Failed {
		t.Fatal(summary.Err)
	}
	wantTail := []string{
		"outcome:stale-active:SUCCESS",
		"start-new-run",
		"unlock",
	}
	if len(backend.events) < len(wantTail) ||
		!reflect.DeepEqual(
			backend.events[len(backend.events)-len(wantTail):],
			wantTail,
		) {
		t.Fatalf("events=%v want tail=%v", backend.events, wantTail)
	}
}

func TestRestoreCoordinatorUsesOneDeadlineAcrossRunsAndStages(t *testing.T) {
	backend := &fakeRestoreBackend{
		restoreLimit:  25 * time.Millisecond,
		blockOnAction: true,
		discovery: RestoreDiscovery{
			Runs: []RestoreRun{
				{RunID: "run-new", StartedAt: time.Unix(20, 0)},
				{RunID: "run-old", StartedAt: time.Unix(10, 0)},
			},
			DatabaseActions: []Action{
				restoreTestAction("run-new", 2, ActionSQLMutation, "new"),
				restoreTestAction("run-old", 1, ActionSQLMutation, "old"),
			},
		},
	}
	parent, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	summary := NewRestoreCoordinator(backend).Restore(parent, RestoreRequest{})
	elapsed := time.Since(started)

	if !summary.Failed || !errors.Is(summary.Err, context.DeadlineExceeded) {
		t.Fatalf("summary=%+v", summary)
	}
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("restore elapsed=%s; deadline reset across runs/stages", elapsed)
	}
	actionEvents := 0
	for _, event := range backend.events {
		if strings.HasPrefix(event, "action:") {
			actionEvents++
		}
	}
	if actionEvents != 2 {
		t.Fatalf("action events=%d want=2: %v", actionEvents, backend.events)
	}
}

func TestRestoreCoordinatorFinalizesFailureAfterActionDeadline(t *testing.T) {
	backend := &fakeRestoreBackend{
		restoreLimit:          10 * time.Millisecond,
		finalizationLimit:     50 * time.Millisecond,
		blockOnAction:         true,
		rejectCanceledOutcome: true,
		discovery: RestoreDiscovery{
			Runs: []RestoreRun{{RunID: "run-timeout"}},
			DatabaseActions: []Action{
				restoreTestAction(
					"run-timeout",
					1,
					ActionSQLMutation,
					"deadline-action",
				),
			},
		},
	}

	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{},
	)

	if !summary.Failed || !errors.Is(summary.Err, context.DeadlineExceeded) {
		t.Fatalf("summary=%+v", summary)
	}
	if !containsRestoreEvent(
		backend.events,
		"outcome:run-timeout:RESTORE_FAILED",
	) {
		t.Fatalf("terminal failure was not persisted: %v", backend.events)
	}
	if containsRestoreEvent(backend.events, "outcome-rejected:run-timeout") {
		t.Fatalf("outcome writer received canceled context: %v", backend.events)
	}
}

func TestRestoreCoordinatorBoundsIndependentFinalization(t *testing.T) {
	backend := &fakeRestoreBackend{
		restoreLimit:      10 * time.Millisecond,
		finalizationLimit: 15 * time.Millisecond,
		blockOnAction:     true,
		blockOnOutcome:    true,
		discovery: RestoreDiscovery{
			Runs: []RestoreRun{{RunID: "run-timeout"}},
			DatabaseActions: []Action{
				restoreTestAction(
					"run-timeout",
					1,
					ActionSQLMutation,
					"deadline-action",
				),
			},
		},
	}
	started := time.Now()

	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{},
	)
	elapsed := time.Since(started)

	if !summary.Failed || !errors.Is(summary.Err, context.DeadlineExceeded) {
		t.Fatalf("summary=%+v", summary)
	}
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("action plus finalization elapsed=%s; deadline reset", elapsed)
	}
	if !containsRestoreEvent(backend.events, "outcome-start:run-timeout") ||
		!containsRestoreEvent(backend.events, "unlock") {
		t.Fatalf("events=%v", backend.events)
	}
}

func TestRestoreCoordinatorPreservesBenchmarkOutcomeAfterCleanup(t *testing.T) {
	for _, benchmarkOutcome := range []Outcome{
		OutcomeFailed,
		OutcomeDegraded,
	} {
		t.Run(string(benchmarkOutcome), func(t *testing.T) {
			backend := &fakeRestoreBackend{discovery: RestoreDiscovery{
				Runs: []RestoreRun{{RunID: "run-result"}},
			}}

			summary := NewRestoreCoordinator(backend).Restore(
				context.Background(),
				RestoreRequest{
					RunID:            "run-result",
					completedOutcome: benchmarkOutcome,
				},
			)

			if summary.Failed || summary.Outcome != OutcomeSuccess {
				t.Fatalf("cleanup summary=%+v", summary)
			}
			want := "outcome:run-result:" + string(benchmarkOutcome)
			if !containsRestoreEvent(backend.events, want) {
				t.Fatalf("events=%v missing %q", backend.events, want)
			}
		})
	}
}

func TestRestoreCoordinatorRestoreFailureSupersedesBenchmarkOutcome(
	t *testing.T,
) {
	backend := &fakeRestoreBackend{
		discovery: RestoreDiscovery{
			Runs: []RestoreRun{{RunID: "run-result"}},
			DatabaseActions: []Action{
				restoreTestAction(
					"run-result",
					1,
					ActionSQLMutation,
					"failed-restore",
				),
			},
		},
		fail: map[string]error{
			"action:run-result:SQL_MUTATION": errors.New("restore failed"),
		},
	}

	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{
			RunID:            "run-result",
			completedOutcome: OutcomeFailed,
		},
	)

	if !summary.Failed || summary.Outcome != OutcomeRestoreFailed {
		t.Fatalf("summary=%+v", summary)
	}
	if !containsRestoreEvent(
		backend.events,
		"outcome:run-result:RESTORE_FAILED",
	) {
		t.Fatalf("events=%v", backend.events)
	}
}

func TestRestoreCoordinatorOutcomeWriteFailureDominatesEverySelectedRun(
	t *testing.T,
) {
	tests := []struct {
		name       string
		failureKey string
		want       []string
	}{
		{
			name:       "first terminal write fails",
			failureKey: "run-new:FAILED",
			want: []string{
				"outcome:run-new:FAILED",
				"outcome:run-middle:RESTORE_FAILED",
				"outcome:run-old:RESTORE_FAILED",
				"outcome:run-new:RESTORE_FAILED",
			},
		},
		{
			name:       "second terminal write fails",
			failureKey: "run-middle:FAILED",
			want: []string{
				"outcome:run-new:FAILED",
				"outcome:run-middle:FAILED",
				"outcome:run-old:RESTORE_FAILED",
				"outcome:run-new:RESTORE_FAILED",
				"outcome:run-middle:RESTORE_FAILED",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writeErr := errors.New("terminal outcome write failed")
			backend := &fakeRestoreBackend{
				finalizationLimit: 50 * time.Millisecond,
				discovery: RestoreDiscovery{Runs: []RestoreRun{
					{RunID: "run-old", StartedAt: time.Unix(10, 0)},
					{RunID: "run-middle", StartedAt: time.Unix(20, 0)},
					{RunID: "run-new", StartedAt: time.Unix(30, 0)},
				}},
				outcomeWriteFailures: map[string][]error{
					test.failureKey: {writeErr},
				},
			}

			summary := NewRestoreCoordinator(backend).Restore(
				context.Background(),
				RestoreRequest{completedOutcome: OutcomeFailed},
			)

			if !summary.Failed ||
				summary.Outcome != OutcomeRestoreFailed ||
				!errors.Is(summary.Err, writeErr) {
				t.Fatalf("summary=%+v", summary)
			}
			var got []string
			for _, event := range backend.events {
				if strings.HasPrefix(event, "outcome:") {
					got = append(got, event)
				}
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("outcome writes=%v want=%v", got, test.want)
			}
			if backend.outcomeWithoutLock {
				t.Fatalf("outcome write occurred after unlock: %v", backend.events)
			}
			if backend.lockHeld {
				t.Fatalf("restore lock remained held: %v", backend.events)
			}
			if len(backend.outcomeDeadlines) != len(test.want) {
				t.Fatalf(
					"outcome deadlines=%v writes=%v",
					backend.outcomeDeadlines,
					got,
				)
			}
			firstDeadline := backend.outcomeDeadlines[0]
			if firstDeadline.IsZero() {
				t.Fatal("finalization context had no deadline")
			}
			for _, deadline := range backend.outcomeDeadlines[1:] {
				if !deadline.Equal(firstDeadline) {
					t.Fatalf(
						"finalization timeout reset: %v",
						backend.outcomeDeadlines,
					)
				}
			}
		})
	}
}

func TestRestoreCoordinatorOrdersRunsNewestFirstAndActionsBySafetyPriority(t *testing.T) {
	backend := &fakeRestoreBackend{discovery: RestoreDiscovery{
		Runs: []RestoreRun{
			{RunID: "run-old", StartedAt: time.Unix(10, 0)},
			{RunID: "run-new", StartedAt: time.Unix(20, 0)},
		},
		LocalActions: []Action{
			restoreTestAction("run-new", 40, ActionCloudFaultJob, "cloud"),
			restoreTestAction("run-new", 10, ActionNetworkFirewall, "nft"),
			restoreTestAction("run-new", 30, ActionProcessState, "process"),
			restoreTestAction("run-new", 20, ActionNetworkQDisc, "qdisc"),
			restoreTestAction("run-old", 99, ActionNetworkFirewall, "old-nft"),
		},
		DatabaseActions: []Action{
			restoreTestAction("run-new", 1, ActionSQLMutation, "new-low"),
			restoreTestAction("run-new", 3, ActionDataBaseline, "new-high"),
			restoreTestAction("run-old", 2, ActionSQLMutation, "old"),
		},
	}}

	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{},
	)
	if summary.Failed {
		t.Fatal(summary.Err)
	}
	var got []string
	for _, event := range backend.events {
		if strings.HasPrefix(event, "stop:") ||
			strings.HasPrefix(event, "action:") {
			got = append(got, event)
		}
	}
	want := []string{
		"stop:run-new", "stop:run-old",
		"action:run-new:NETWORK_FIREWALL:nft",
		"action:run-new:NETWORK_QDISC:qdisc",
		"action:run-new:PROCESS_STATE:process",
		"action:run-new:CLOUD_FAULT_JOB:cloud",
		"action:run-old:NETWORK_FIREWALL:old-nft",
		"action:run-new:DATA_BASELINE:new-high",
		"action:run-new:SQL_MUTATION:new-low",
		"action:run-old:SQL_MUTATION:old",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered events=%v want=%v", got, want)
	}
}

func TestRestoreCoordinatorMergesDatabaseAndLocalMirrorOnce(t *testing.T) {
	action := restoreTestAction("run-1", 7, ActionNetworkFirewall, "nft")
	localMirror := action
	localMirror.State = MutationRestoreFailed
	localMirror.LastError = "previous failure"
	backend := &fakeRestoreBackend{discovery: RestoreDiscovery{
		DatabaseActions: []Action{action},
		LocalActions:    []Action{localMirror},
	}}

	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{RunID: "run-1"},
	)
	if summary.Failed {
		t.Fatal(summary.Err)
	}
	if len(summary.PlannedActions) != 1 {
		t.Fatalf("planned=%+v, want one de-duplicated action", summary.PlannedActions)
	}
	count := 0
	for _, event := range backend.events {
		if event == "action:run-1:NETWORK_FIREWALL:nft" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("mirror restored %d times, events=%v", count, backend.events)
	}
}

func TestRestoreCoordinatorKeepsDistinctJournalIDsForSameTarget(t *testing.T) {
	backend := &fakeRestoreBackend{discovery: RestoreDiscovery{
		DatabaseActions: []Action{
			restoreTestAction("run-1", 8, ActionSQLMutation, "same-table"),
			restoreTestAction("run-1", 7, ActionSQLMutation, "same-table"),
		},
	}}

	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{RunID: "run-1"},
	)
	if summary.Failed {
		t.Fatal(summary.Err)
	}
	if len(summary.PlannedActions) != 2 {
		t.Fatalf("planned=%+v, want both journal IDs", summary.PlannedActions)
	}
	var sequences []string
	for _, event := range backend.events {
		if strings.HasPrefix(event, "action:run-1:SQL_MUTATION:") {
			sequences = append(sequences, event)
		}
	}
	if len(sequences) != 2 {
		t.Fatalf("restore events=%v, want two actions", backend.events)
	}
}

func TestRestoreCoordinatorKeepsMultipleLocalActionsWithoutDatabaseIDs(t *testing.T) {
	first := restoreTestAction(
		"run-1", 0, ActionNetworkFirewall, "firewall-a",
	)
	second := restoreTestAction(
		"run-1", 0, ActionNetworkQDisc, "qdisc-b",
	)
	backend := &fakeRestoreBackend{discovery: RestoreDiscovery{
		LocalActions: []Action{first, second},
	}}

	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{RunID: "run-1"},
	)
	if summary.Failed {
		t.Fatal(summary.Err)
	}
	if len(summary.PlannedActions) != 2 {
		t.Fatalf("planned=%+v, want both local-only actions", summary.PlannedActions)
	}
}

func TestRestoreDryRunDoesNotMutateOrAcquireLocks(t *testing.T) {
	backend := &fakeRestoreBackend{discovery: RestoreDiscovery{
		DatabaseActions: []Action{
			restoreTestAction("run-1", 7, ActionNetworkQDisc, "eth0"),
		},
	}}

	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{RunID: "run-1", DryRun: true},
	)
	if summary.Failed {
		t.Fatal(summary.Err)
	}
	if len(summary.PlannedActions) != 1 ||
		summary.PlannedActions[0].Sequence != 7 {
		t.Fatalf("planned=%+v", summary.PlannedActions)
	}
	if want := []string{"discover:run-1"}; !reflect.DeepEqual(
		backend.events, want,
	) {
		t.Fatalf("dry-run events=%v want=%v", backend.events, want)
	}
}

func TestDryRunDiscoveryOfMissingLocalLedgerCreatesNoFiles(t *testing.T) {
	parent := t.TempDir()
	ledger := NewFileRecoveryLedger(filepath.Join(parent, "recovery.json"))
	pending, err := ledger.Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending=%+v", pending)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("read-only discovery created files: %+v", entries)
	}
}

func TestRestoreCoordinatorReturnsBusyWithoutMutation(t *testing.T) {
	backend := &fakeRestoreBackend{
		discovery: RestoreDiscovery{
			Runs: []RestoreRun{{RunID: "run-1"}},
		},
		fail: map[string]error{"lock": errors.New("restore busy")},
	}
	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{},
	)
	if !summary.Failed || summary.Outcome != OutcomeRestoreFailed ||
		!strings.Contains(summary.Err.Error(), "busy") {
		t.Fatalf("summary=%+v", summary)
	}
	want := []string{"lock"}
	if !reflect.DeepEqual(backend.events, want) {
		t.Fatalf("busy events=%v want=%v", backend.events, want)
	}
}

func TestRestoreCoordinatorAccumulatesRetryableErrorsAndContinues(t *testing.T) {
	backend := &fakeRestoreBackend{
		discovery: RestoreDiscovery{
			Runs: []RestoreRun{{RunID: "run-1"}},
			LocalActions: []Action{
				restoreTestAction(
					"run-1", 3, ActionNetworkFirewall, "nft",
				),
			},
			DatabaseActions: []Action{
				restoreTestAction(
					"run-1", 2, ActionSQLMutation, "table",
				),
			},
		},
		fail: map[string]error{
			"stop:run-1":                    errors.New("stop failed"),
			"action:run-1:NETWORK_FIREWALL": errors.New("external failed"),
			"baseline":                      errors.New("baseline failed"),
			"verify":                        errors.New("verify failed"),
		},
	}

	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{},
	)
	if !summary.Failed || summary.Outcome != OutcomeRestoreFailed {
		t.Fatalf("summary=%+v", summary)
	}
	for _, detail := range []string{
		"stop failed", "external failed", "baseline failed", "verify failed",
	} {
		if !strings.Contains(summary.Err.Error(), detail) {
			t.Fatalf("summary error %q missing %q", summary.Err, detail)
		}
	}
	for _, event := range []string{
		"action:run-1:SQL_MUTATION:table",
		"topology",
		"outcome:run-1:RESTORE_FAILED",
		"unlock",
	} {
		if !containsRestoreEvent(backend.events, event) {
			t.Fatalf("events=%v missing %q", backend.events, event)
		}
	}
}

func containsRestoreEvent(events []string, want string) bool {
	for _, event := range events {
		if event == want {
			return true
		}
	}
	return false
}

type recordingRestoreProvider struct {
	preflight []Action
	restored  []Action
	verified  []Action
}

func (p *recordingRestoreProvider) Name() string {
	return "recording"
}

func (p *recordingRestoreProvider) Preflight(
	_ context.Context,
	_ Environment,
	action Action,
) error {
	p.preflight = append(p.preflight, action)
	return nil
}

func (p *recordingRestoreProvider) Apply(context.Context, Action) error {
	return errors.New("apply must not be called during restore")
}

func (p *recordingRestoreProvider) Restore(
	_ context.Context,
	action Action,
) error {
	p.restored = append(p.restored, action)
	return nil
}

func (p *recordingRestoreProvider) VerifyRestored(
	_ context.Context,
	action Action,
) error {
	p.verified = append(p.verified, action)
	return nil
}

func TestCoordinatorJournalRestoresMirrorOnceAndSynchronizesStores(t *testing.T) {
	action := validLedgerAction("run-1", "firewall-rule-1")
	action.Sequence = 7
	action.State = MutationApplied
	databaseStore := &memoryActionStore{entries: []Action{action}}
	ledger := NewFileRecoveryLedger(filepath.Join(t.TempDir(), "recovery.json"))
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	provider := &recordingRestoreProvider{}
	store := newCoordinatorActionStore(
		databaseStore,
		ledger,
		[]Action{action},
		[]Action{action},
	)
	executor := newRestoreDispatchExecutor(
		&memoryActionExecutor{},
		provider,
		centralizedFixture(),
	)

	if err := NewJournal(store, executor).restoreCoordinatorActions(
		context.Background(),
		[]Action{action},
	); err != nil {
		t.Fatal(err)
	}
	if len(provider.restored) != 1 || len(provider.verified) != 1 {
		t.Fatalf(
			"provider restore=%d verify=%d, want one each",
			len(provider.restored),
			len(provider.verified),
		)
	}
	if databaseStore.states[action.Sequence] != MutationRestored {
		t.Fatalf(
			"database state=%q want=%q",
			databaseStore.states[action.Sequence],
			MutationRestored,
		)
	}
	pending, err := ledger.Pending(context.Background(), action.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("local mirror remains pending: %+v", pending)
	}
}

func TestCoordinatorJournalRestoresLocalMirrorWhenDatabaseStateIsUnavailable(t *testing.T) {
	action := validLedgerAction("run-1", "firewall-rule-1")
	action.Sequence = 7
	action.State = MutationApplied
	databaseStore := &memoryActionStore{
		entries:    []Action{action},
		stateError: errors.New("database unavailable"),
	}
	ledger := NewFileRecoveryLedger(filepath.Join(t.TempDir(), "recovery.json"))
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	provider := &recordingRestoreProvider{}
	store := newCoordinatorActionStore(
		databaseStore,
		ledger,
		[]Action{action},
		[]Action{action},
	)
	executor := newRestoreDispatchExecutor(
		&memoryActionExecutor{},
		provider,
		centralizedFixture(),
	)

	journalErr := NewJournal(store, executor).restoreCoordinatorActions(
		context.Background(),
		[]Action{action},
	)
	if journalErr != nil {
		t.Fatalf("local recovery was blocked by database mirror: %v", journalErr)
	}
	if len(provider.restored) != 1 {
		t.Fatalf("provider restore calls=%d want=1", len(provider.restored))
	}
	pending, err := ledger.Pending(context.Background(), action.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("local action remains pending: %+v", pending)
	}
	deferred := store.DrainErrors()
	if deferred == nil || !strings.Contains(
		deferred.Error(),
		"database unavailable",
	) {
		t.Fatalf("deferred database mirror error=%v", deferred)
	}
}

func TestCoordinatorJournalFailsClosedWithoutExternalAdapter(t *testing.T) {
	action := validLedgerAction("run-1", "firewall-rule-1")
	action.Sequence = 7
	action.State = MutationApplied
	ledger := NewFileRecoveryLedger(filepath.Join(t.TempDir(), "recovery.json"))
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	provider, err := NewFaultProvider(
		NewFaultProviderRegistry(),
		FaultProviderConfig{Type: "none"},
	)
	if err != nil {
		t.Fatal(err)
	}
	store := newCoordinatorActionStore(
		&memoryActionStore{},
		ledger,
		nil,
		[]Action{action},
	)
	executor := newRestoreDispatchExecutor(
		&memoryActionExecutor{},
		provider,
		centralizedFixture(),
	)

	err = NewJournal(store, executor).restoreCoordinatorActions(
		context.Background(),
		[]Action{action},
	)
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("RestoreActions() error=%v", err)
	}
	pending, pendingErr := ledger.Pending(context.Background(), action.RunID)
	if pendingErr != nil {
		t.Fatal(pendingErr)
	}
	if len(pending) != 1 || pending[0].State != MutationRestoreFailed {
		t.Fatalf("retryable local action=%+v", pending)
	}
}

func TestCoordinatorJournalTracksLocalActionsWithoutDatabaseIDsByIdentity(t *testing.T) {
	first := validLedgerAction("run-1", "firewall-rule-1")
	first.Sequence = 0
	second := validLedgerAction("run-1", "qdisc-rule-1")
	second.Sequence = 0
	second.Kind = ActionNetworkQDisc
	ledger := NewFileRecoveryLedger(filepath.Join(t.TempDir(), "recovery.json"))
	for _, action := range []Action{first, second} {
		if err := ledger.Put(context.Background(), action); err != nil {
			t.Fatal(err)
		}
	}
	provider := &recordingRestoreProvider{}
	store := newCoordinatorActionStore(
		&memoryActionStore{},
		ledger,
		nil,
		[]Action{first, second},
	)
	executor := newRestoreDispatchExecutor(
		&memoryActionExecutor{},
		provider,
		centralizedFixture(),
	)

	if err := NewJournal(store, executor).restoreCoordinatorActions(
		context.Background(),
		[]Action{first, second},
	); err != nil {
		t.Fatal(err)
	}
	if len(provider.restored) != 2 {
		t.Fatalf("provider restored=%+v", provider.restored)
	}
	pending, err := ledger.Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("local actions remain pending: %+v", pending)
	}
}

func TestRestoreDispatchExecutorUsesDatabaseOnlyForDatabaseActions(t *testing.T) {
	database := &memoryActionExecutor{}
	provider := &recordingRestoreProvider{}
	executor := newRestoreDispatchExecutor(
		database,
		provider,
		centralizedFixture(),
	)
	action := validSQLJournalAction()
	action.Sequence = 1
	action.State = MutationApplied

	if err := executor.Preflight(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if err := executor.Restore(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if err := executor.VerifyRestored(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if len(provider.preflight) != 0 || len(provider.restored) != 0 ||
		len(provider.verified) != 0 {
		t.Fatalf("SQL action reached provider: %+v", provider)
	}
}

func TestLocalRestoreLockIsBusyForSameConfigIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config-identity_recovery.json")
	first, err := acquireLocalRestoreLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	second, err := acquireLocalRestoreLock(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "busy") {
		if second != nil {
			_ = second.Release()
		}
		t.Fatalf("second lock=%T error=%v, want busy", second, err)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err = acquireLocalRestoreLock(context.Background(), path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalRestoreLockDoesNotConflictAcrossConfigIdentities(t *testing.T) {
	parent := t.TempDir()
	first, err := acquireLocalRestoreLock(
		context.Background(),
		filepath.Join(parent, "first_recovery.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := acquireLocalRestoreLock(
		context.Background(),
		filepath.Join(parent, "second_recovery.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
}

type recordingRestoreService struct {
	request RestoreRequest
	result  RestoreSummary
}

type failingDiscoveryActionStore struct {
	err error
}

func (*failingDiscoveryActionStore) InsertPlanned(
	context.Context,
	Action,
) (Action, error) {
	return Action{}, errors.New("not supported")
}
func (*failingDiscoveryActionStore) SetState(
	context.Context,
	string,
	int64,
	MutationState,
	string,
) error {
	return nil
}
func (s *failingDiscoveryActionStore) Pending(
	context.Context,
	string,
) ([]Action, error) {
	return nil, s.err
}
func (s *failingDiscoveryActionStore) StaleRuns(
	context.Context,
) ([]string, error) {
	return nil, s.err
}

func (s *recordingRestoreService) Restore(
	_ context.Context,
	request RestoreRequest,
) RestoreSummary {
	s.request = request
	return s.result
}

type scriptedRestorePinger struct {
	failures int
	attempts int
}

func (p *scriptedRestorePinger) Ping(context.Context) error {
	p.attempts++
	if p.attempts <= p.failures {
		return errors.New("database unavailable")
	}
	return nil
}

func TestWaitForRestoreDatabaseRetriesUntilReachable(t *testing.T) {
	pinger := &scriptedRestorePinger{failures: 2}
	if err := waitForRestoreDatabase(
		context.Background(),
		pinger,
		100*time.Millisecond,
		time.Millisecond,
	); err != nil {
		t.Fatal(err)
	}
	if pinger.attempts != 3 {
		t.Fatalf("ping attempts=%d want=3", pinger.attempts)
	}
}

func TestWaitForRestoreDatabaseHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pinger := &scriptedRestorePinger{failures: 100}
	err := waitForRestoreDatabase(
		ctx,
		pinger,
		time.Second,
		time.Millisecond,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error=%v want context cancellation", err)
	}
	if pinger.attempts != 0 {
		t.Fatalf("canceled wait attempted %d pings", pinger.attempts)
	}
}

func TestDatabaseRestoreBackendWaitsForExactRunSessionsAndLocks(
	t *testing.T,
) {
	var events []string
	samples := [][2]int{{2, 3}, {1, 1}, {0, 0}}
	backend := &databaseRestoreBackend{
		cancelTagged: func(context.Context, string) error {
			events = append(events, "cancel")
			return errors.New("one cancel signal was rejected")
		},
		terminateTagged: func(context.Context, string) error {
			events = append(events, "terminate")
			return nil
		},
		taggedSessionState: func(
			_ context.Context,
			runID string,
		) (int, int, error) {
			if runID != "run-1" {
				t.Fatalf("runID=%q", runID)
			}
			sample := samples[0]
			samples = samples[1:]
			events = append(events, fmt.Sprintf(
				"poll:%d:%d", sample[0], sample[1],
			))
			return sample[0], sample[1], nil
		},
		restorePollInterval: time.Millisecond,
	}

	err := backend.StopTaggedSessions(context.Background(), "run-1")
	if err == nil || !strings.Contains(err.Error(), "cancel signal") {
		t.Fatalf("StopTaggedSessions() error=%v", err)
	}
	want := []string{
		"cancel", "terminate", "poll:2:3", "poll:1:1", "poll:0:0",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestDatabaseRestoreBackendSessionQuiescenceUsesSharedDeadline(
	t *testing.T,
) {
	backend := &databaseRestoreBackend{
		cancelTagged:    func(context.Context, string) error { return nil },
		terminateTagged: func(context.Context, string) error { return nil },
		taggedSessionState: func(
			context.Context,
			string,
		) (int, int, error) {
			return 1, 1, nil
		},
		restorePollInterval: time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := backend.StopTaggedSessions(ctx, "run-1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopTaggedSessions() error=%v", err)
	}
}

func TestDatabaseRestoreBackendRestoresLocalControlPlaneBeforeDatabaseLock(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	action := restoreTestAction(
		"run-offline",
		7,
		ActionNetworkFirewall,
		"firewall-rule",
	)
	action.Node = "dn-1"
	action.State = MutationApplied
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	provider := &recordingRestoreProvider{}
	backend := &databaseRestoreBackend{
		cfg: BenchConfig{
			Database: DatabaseConfig{Database: "postgres"},
			Data:     DataConfig{Schema: "gsbench"},
			Safety: SafetyConfig{
				RestoreTimeout: 25 * time.Millisecond,
			},
			FaultProvider: FaultProviderConfig{LedgerPath: path},
		},
		ledger:      ledger,
		provider:    provider,
		environment: Environment{Product: ProductGaussDB, Supported: true},
		executor: newRestoreDispatchExecutor(
			nil,
			provider,
			Environment{Product: ProductGaussDB, Supported: true},
		),
		acquireDatabaseRestoreLock: func(
			context.Context,
			RestoreLock,
		) (RestoreLock, error) {
			return nil, newRestoreDatabaseConnectivityError(
				errors.New("database advisory session unavailable"),
			)
		},
		waitForDatabaseFn: func(context.Context) error {
			return errors.New("database remains unreachable")
		},
	}

	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{},
	)
	if !summary.Failed ||
		!strings.Contains(summary.Err.Error(), "database remains unreachable") {
		t.Fatalf("summary=%+v", summary)
	}
	if len(provider.restored) != 1 ||
		provider.restored[0].Target != "firewall-rule" {
		t.Fatalf("provider restores=%v want exactly once", provider.restored)
	}
	pending, err := ledger.Pending(context.Background(), "run-offline")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("local action was not durably restored: %+v", pending)
	}
}

func TestDatabaseRestoreBackendAdvisoryBusyDoesNotRunOfflineInverse(
	t *testing.T,
) {
	parent := t.TempDir()
	ownerLedgerPath := filepath.Join(parent, "owner", "recovery.json")
	contenderLedgerPath := filepath.Join(
		parent,
		"contender",
		"recovery.json",
	)
	ownerLock, err := acquireLocalRestoreLock(
		context.Background(),
		ownerLedgerPath,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerLock.Release()

	ledger := NewFileRecoveryLedger(contenderLedgerPath)
	action := restoreTestAction(
		"run-contender",
		7,
		ActionNetworkFirewall,
		"firewall-rule",
	)
	action.Node = "dn-1"
	action.State = MutationApplied
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	provider := &recordingRestoreProvider{}
	waited := false
	backend := &databaseRestoreBackend{
		cfg: BenchConfig{
			Database: DatabaseConfig{Database: "postgres"},
			Data:     DataConfig{Schema: "gsbench"},
			FaultProvider: FaultProviderConfig{
				LedgerPath: contenderLedgerPath,
			},
		},
		ledger:      ledger,
		provider:    provider,
		environment: Environment{Product: ProductGaussDB, Supported: true},
		executor: newRestoreDispatchExecutor(
			nil,
			provider,
			Environment{Product: ProductGaussDB, Supported: true},
		),
		acquireDatabaseRestoreLock: func(
			context.Context,
			RestoreLock,
		) (RestoreLock, error) {
			return nil, newRestoreBusyError(
				"database postgres schema gsbench",
			)
		},
		waitForDatabaseFn: func(context.Context) error {
			waited = true
			return nil
		},
	}

	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{},
	)

	if !summary.Failed || !errors.Is(summary.Err, errRestoreBusy) {
		t.Fatalf("summary=%+v", summary)
	}
	if waited {
		t.Fatal("advisory contention entered reconnect path")
	}
	if len(provider.restored) != 0 {
		t.Fatalf("provider inverses=%v want zero", provider.restored)
	}
	pending, err := ledger.Pending(context.Background(), "run-contender")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].State != MutationApplied {
		t.Fatalf("pending action mutated: %+v", pending)
	}
	reacquired, err := acquireLocalRestoreLock(
		context.Background(),
		contenderLedgerPath,
	)
	if err != nil {
		t.Fatalf("contender local lock was not released: %v", err)
	}
	if err := reacquired.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseRestoreBackendAdvisoryQueryErrorFailsClosed(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	action := restoreTestAction(
		"run-query-error",
		7,
		ActionNetworkQDisc,
		"qdisc-rule",
	)
	action.Node = "dn-1"
	action.State = MutationApplied
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	provider := &recordingRestoreProvider{}
	waited := false
	backend := &databaseRestoreBackend{
		cfg: BenchConfig{
			Database: DatabaseConfig{Database: "postgres"},
			Data:     DataConfig{Schema: "gsbench"},
			FaultProvider: FaultProviderConfig{
				LedgerPath: path,
			},
		},
		ledger:      ledger,
		provider:    provider,
		environment: Environment{Product: ProductGaussDB, Supported: true},
		executor: newRestoreDispatchExecutor(
			nil,
			provider,
			Environment{Product: ProductGaussDB, Supported: true},
		),
		acquireDatabaseRestoreLock: func(
			context.Context,
			RestoreLock,
		) (RestoreLock, error) {
			return nil, errors.New("advisory query returned malformed value")
		},
		waitForDatabaseFn: func(context.Context) error {
			waited = true
			return nil
		},
	}

	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{},
	)

	if !summary.Failed ||
		!strings.Contains(summary.Err.Error(), "malformed value") {
		t.Fatalf("summary=%+v", summary)
	}
	if waited {
		t.Fatal("advisory query error entered reconnect path")
	}
	if len(provider.restored) != 0 {
		t.Fatalf("provider inverses=%v want zero", provider.restored)
	}
}

func TestDatabaseRestoreBackendDiscoveryFailureStillRestoresLocalControlPlaneOnce(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	action := restoreTestAction(
		"run-offline",
		7,
		ActionNetworkQDisc,
		"qdisc-rule",
	)
	action.Node = "dn-1"
	action.State = MutationApplied
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	provider := &recordingRestoreProvider{}
	environment := Environment{
		Product: ProductGaussDB, Supported: true,
		Capabilities: make(CapabilitySet),
	}
	backend := &databaseRestoreBackend{
		cfg: BenchConfig{
			Database: DatabaseConfig{Database: "postgres"},
			Data:     DataConfig{Schema: "gsbench"},
			FaultProvider: FaultProviderConfig{
				LedgerPath: path,
			},
		},
		store:       &failingDiscoveryActionStore{err: errors.New("discovery unavailable")},
		ledger:      ledger,
		provider:    provider,
		environment: environment,
		executor: newRestoreDispatchExecutor(
			nil,
			provider,
			environment,
		),
		acquireDatabaseRestoreLock: func(
			_ context.Context,
			local RestoreLock,
		) (RestoreLock, error) {
			return local, nil
		},
	}

	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{},
	)
	if !summary.Failed ||
		!strings.Contains(summary.Err.Error(), "discovery unavailable") {
		t.Fatalf("summary=%+v", summary)
	}
	if len(provider.restored) != 1 ||
		provider.restored[0].Target != action.Target {
		t.Fatalf("provider restores=%v want exactly once", provider.restored)
	}
}

func TestDatabaseRestoreBackendPersistsBenchmarkOutcomeAfterCleanup(
	t *testing.T,
) {
	var persisted []Outcome
	backend := &databaseRestoreBackend{
		cfg: BenchConfig{Data: DataConfig{Schema: "gsbench"}},
		finishRunFn: func(
			_ context.Context,
			schema string,
			runID string,
			outcome Outcome,
			_ string,
		) error {
			if schema != "gsbench" || runID != "run-result" {
				t.Fatalf("schema=%q runID=%q", schema, runID)
			}
			persisted = append(persisted, outcome)
			return nil
		},
	}

	for _, outcome := range []Outcome{OutcomeFailed, OutcomeDegraded} {
		if err := backend.MarkRestoreOutcome(
			context.Background(),
			"run-result",
			outcome,
		); err != nil {
			t.Fatal(err)
		}
	}

	want := []Outcome{OutcomeFailed, OutcomeDegraded}
	if !reflect.DeepEqual(persisted, want) {
		t.Fatalf("persisted=%v want=%v", persisted, want)
	}
}

func TestOpenRestoreDatabaseDoesNotRequireInitialReachability(t *testing.T) {
	cfg := BenchConfig{
		Database: DatabaseConfig{
			Host:            "127.0.0.1",
			Port:            1,
			Database:        "postgres",
			User:            "bench",
			Password:        "test-only",
			SSLMode:         "disable",
			ApplicationName: "gsbench",
			ConnectTimeout:  time.Millisecond,
		},
		Safety: SafetyConfig{QueryTimeout: 5 * time.Millisecond},
	}
	db, err := OpenRestoreDatabase(context.Background(), cfg)
	if err != nil {
		t.Fatalf("OpenRestoreDatabase() error=%v", err)
	}
	defer db.Close()
	if err := db.Ping(context.Background()); err == nil {
		t.Fatal("unreachable database unexpectedly pinged")
	}
}

func TestDatabaseRestoreBackendReconcilesOfflineRestoredMirrorWithoutInverse(
	t *testing.T,
) {
	databaseAction := restoreTestAction(
		"run-offline",
		7,
		ActionNetworkFirewall,
		"firewall-rule",
	)
	databaseAction.Node = "dn-1"
	databaseAction.State = MutationApplied
	localAction := databaseAction
	localAction.State = MutationRestored
	store := &memoryActionStore{entries: []Action{databaseAction}}
	backend := &databaseRestoreBackend{store: store}

	pending, err := backend.syncRestoredLocalMirrors(
		context.Background(),
		[]Action{databaseAction},
		[]Action{localAction},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("reconciled database actions remained pending: %+v", pending)
	}
	if got := store.states[databaseAction.Sequence]; got != MutationRestored {
		t.Fatalf("database mirror state=%q want=%q", got, MutationRestored)
	}
}

func TestReplicationHealthRequiresLiveConnectivityAndBoundedOrClosingGap(
	t *testing.T,
) {
	tests := []struct {
		name   string
		first  replicationHealthSample
		second replicationHealthSample
		wantOK bool
	}{
		{
			name: "idle standby is healthy without replay timestamp",
			first: replicationHealthSample{
				InRecovery: true, ReceiverConnected: true,
				ReceiveLocation: "0/10", ReplayLocation: "0/10",
			},
			second: replicationHealthSample{
				InRecovery: true, ReceiverConnected: true,
				ReceiveLocation: "0/10", ReplayLocation: "0/10",
			},
			wantOK: true,
		},
		{
			name: "historical replay positions do not hide disconnect",
			first: replicationHealthSample{
				InRecovery: true, ReceiverConnected: false,
				ReceiveLocation: "0/10", ReplayLocation: "0/10",
			},
			second: replicationHealthSample{
				InRecovery: true, ReceiverConnected: false,
				ReceiveLocation: "0/10", ReplayLocation: "0/10",
			},
		},
		{
			name: "primary requires every configured standby",
			first: replicationHealthSample{
				RequiredStandbys: 2, StreamingStandbys: 1,
				ReplayGapBytes: 0,
			},
			second: replicationHealthSample{
				RequiredStandbys: 2, StreamingStandbys: 1,
				ReplayGapBytes: 0,
			},
		},
		{
			name: "large primary gap is accepted only while converging",
			first: replicationHealthSample{
				RequiredStandbys: 2, StreamingStandbys: 2,
				ReplayGapBytes: restoreReplicationGapLimit + 200,
			},
			second: replicationHealthSample{
				RequiredStandbys: 2, StreamingStandbys: 2,
				ReplayGapBytes: restoreReplicationGapLimit + 100,
			},
			wantOK: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := evaluateReplicationHealth(test.first, test.second)
			if test.wantOK && err != nil {
				t.Fatal(err)
			}
			if !test.wantOK && err == nil {
				t.Fatal("unhealthy replication was accepted")
			}
		})
	}
}

func TestDistributedHealthRequiresEveryRuntimeMemberAndKnownGTMMode(
	t *testing.T,
) {
	healthy := distributedHealthSample{
		CatalogCN: 2, CatalogDN: 3,
		RuntimeCN: 2, RuntimeDN: 3,
		GTMMode: "gtm-free",
	}
	if err := evaluateDistributedHealth(healthy); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*distributedHealthSample){
		func(sample *distributedHealthSample) { sample.RuntimeDN-- },
		func(sample *distributedHealthSample) { sample.RuntimeCN-- },
		func(sample *distributedHealthSample) {
			sample.GTMMode = "unknown"
		},
		func(sample *distributedHealthSample) {
			sample.GTMMode = "gtm"
			sample.GTMSnapshot = nil
		},
	} {
		sample := healthy
		mutate(&sample)
		if err := evaluateDistributedHealth(sample); err == nil {
			t.Fatalf("unhealthy distributed sample accepted: %+v", sample)
		}
	}
}

func TestGTMSnapshotHealthUsesDocumentedRecordBoundary(t *testing.T) {
	var gotQuery string
	verifier := databaseRestoreHealthVerifier{
		probeValue: func(
			_ context.Context,
			name string,
			_ string,
		) (string, error) {
			switch name {
			case "gtm_free_mode", "gtm_lite_mode":
				return "off", nil
			default:
				return "", fmt.Errorf("unexpected probe %q", name)
			}
		},
		scanValue: func(
			_ context.Context,
			query string,
			_ []any,
			dest ...any,
		) error {
			gotQuery = query
			*dest[0].(*string) = "34730350588"
			*dest[1].(*string) = "34730350600"
			*dest[2].(*int64) = 300
			*dest[3].(*string) = "34730350500"
			*dest[4].(*int64) = 2
			*dest[5].(*string) = "34730350588,34730350589"
			return nil
		},
	}

	evidence, err := verifier.sampleGTMHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sample := distributedHealthSample{
		CatalogCN: 1, CatalogDN: 1,
		RuntimeCN: 1, RuntimeDN: 1,
		GTMMode: evidence.Mode, GTMSnapshot: evidence.Snapshot,
	}
	if err := evaluateDistributedHealth(sample); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"xmin",
		"xmax",
		"csn",
		"oldestxmin",
		"xcnt",
		"running_xids",
	} {
		if !strings.Contains(gotQuery, field) {
			t.Fatalf("classic GTM record query missing %q: %s", field, gotQuery)
		}
	}
	if strings.Contains(gotQuery, "::text") &&
		strings.Contains(gotQuery, "pgxc_gtm_snapshot_status()::text") {
		t.Fatalf("classic GTM was collapsed to an enum-like text value: %s", gotQuery)
	}
}

func TestGTMSnapshotHealthRejectsMalformedRecord(t *testing.T) {
	fields := []struct {
		name string
		set  func(*gtmSnapshot, string)
	}{
		{"xmin", func(snapshot *gtmSnapshot, value string) {
			snapshot.XMin = value
		}},
		{"xmax", func(snapshot *gtmSnapshot, value string) {
			snapshot.XMax = value
		}},
		{"oldestxmin", func(snapshot *gtmSnapshot, value string) {
			snapshot.OldestXMin = value
		}},
	}
	for _, field := range fields {
		for _, invalidXID := range []string{
			"",
			"   ",
			"-1",
			"+1",
			"0x10",
			"10.5",
			"not-an-xid",
		} {
			snapshot := &gtmSnapshot{
				XMin: "34730350588", XMax: "34730350600", CSN: 300,
				OldestXMin: "34730350500", TransactionCount: 0,
			}
			field.set(snapshot, invalidXID)
			sample := distributedHealthSample{
				CatalogCN: 1, CatalogDN: 1,
				RuntimeCN: 1, RuntimeDN: 1,
				GTMMode: "gtm", GTMSnapshot: snapshot,
			}
			if err := evaluateDistributedHealth(sample); err == nil {
				t.Fatalf(
					"malformed GTM %s %q accepted: %+v",
					field.name,
					invalidXID,
					sample,
				)
			}
		}
	}
}

func TestGTMLiteFailsClosedWithoutDocumentedRuntimeEvidence(t *testing.T) {
	verifier := databaseRestoreHealthVerifier{
		probeValue: func(
			_ context.Context,
			name string,
			_ string,
		) (string, error) {
			switch name {
			case "gtm_free_mode":
				return "off", nil
			case "gtm_lite_mode":
				return "on", nil
			default:
				return "", fmt.Errorf("unexpected probe %q", name)
			}
		},
	}

	_, err := verifier.sampleGTMHealth(context.Background())
	if err == nil || !strings.Contains(err.Error(), "NOT_SUPPORTED") {
		t.Fatalf("GTM-LITE configuration-only evidence was accepted: %v", err)
	}
}

func TestRestoreHealthSQLUsesDocumentedLocationsAndRuntimeNodeProbes(
	t *testing.T,
) {
	for _, token := range []string{
		"sender_sent_location",
		"receiver_replay_location",
		"pg_xlog_location_diff",
		"replconninfo",
	} {
		if !strings.Contains(primaryReplicationHealthSQL(), token) {
			t.Fatalf("primary health SQL missing %q", token)
		}
	}
	for _, token := range []string{
		"pg_stat_get_wal_receiver",
		"pg_last_xlog_receive_location",
		"pg_last_xlog_replay_location",
		"pg_xlog_location_diff",
	} {
		if !strings.Contains(standbyReplicationHealthSQL(), token) {
			t.Fatalf("standby health SQL missing %q", token)
		}
	}
	if got := distributedRuntimeHealthSQL("DATANODES"); got !=
		"EXECUTE DIRECT ON DATANODES 'SELECT 1'" {
		t.Fatalf("distributed runtime SQL=%q", got)
	}
	for _, token := range []string{
		"xmin",
		"xmax",
		"csn",
		"oldestxmin",
		"xcnt",
		"running_xids",
		"pgxc_gtm_snapshot_status()",
	} {
		if !strings.Contains(classicGTMSnapshotSQL(), token) {
			t.Fatalf("classic GTM SQL missing %q", token)
		}
	}
}

func TestRestoreHealthVerifierUsesExplicitProductTopologyBranches(
	t *testing.T,
) {
	verifier := databaseRestoreHealthVerifier{}
	tests := []struct {
		name    string
		env     Environment
		wantErr bool
	}{
		{
			name: "openGauss standalone",
			env: Environment{
				Product: ProductOpenGauss, Topology: TopologyStandalone,
				Supported: true, Capabilities: make(CapabilitySet),
			},
		},
		{
			name: "GaussDB centralized without replication deployment",
			env: Environment{
				Product: ProductGaussDB, Topology: TopologyCentralized,
				Supported: true, Capabilities: make(CapabilitySet),
			},
		},
		{
			name: "GaussDB standalone is not a proven product branch",
			env: Environment{
				Product: ProductGaussDB, Topology: TopologyStandalone,
				Supported: true, Capabilities: make(CapabilitySet),
			},
			wantErr: true,
		},
		{
			name: "openGauss distributed is not supported",
			env: Environment{
				Product: ProductOpenGauss, Topology: TopologyDistributed,
				Supported: true, Capabilities: make(CapabilitySet),
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifier.Verify(context.Background(), test.env)
			if test.wantErr && err == nil {
				t.Fatal("unsupported product/topology branch was accepted")
			}
			if !test.wantErr && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStopAndRestoreCommandsShareCoordinatorAndPreserveCommandName(t *testing.T) {
	for _, command := range []string{"stop", "restore"} {
		t.Run(command, func(t *testing.T) {
			var output bytes.Buffer
			log, err := NewRunLog(&output, "", Version)
			if err != nil {
				t.Fatal(err)
			}
			service := &recordingRestoreService{result: RestoreSummary{
				RunIDs: []string{"run-1"}, Outcome: OutcomeSuccess,
			}}
			code := executeRestoreService(
				context.Background(),
				service,
				RestoreRequest{RunID: "run-1"},
				command,
				log,
			)
			if code != 0 {
				t.Fatalf("exit code=%d output=%s", code, output.String())
			}
			if service.request.RunID != "run-1" {
				t.Fatalf("request=%+v", service.request)
			}
			if !strings.Contains(output.String(), command+" SUCCESS") {
				t.Fatalf("command name lost in output: %s", output.String())
			}
		})
	}
}
