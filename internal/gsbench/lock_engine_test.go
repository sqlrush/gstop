package gsbench

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type recordingRollback struct {
	events *[]string
	name   string
}

func (r recordingRollback) Rollback() error {
	*r.events = append(*r.events, "rollback_"+r.name)
	return nil
}

func TestLockEngineCancelsWaiterBeforeBlocker(t *testing.T) {
	events := []string{}
	engine := &LockEngine{
		cancelWaiter: func() { events = append(events, "cancel_waiter") },
		cancelHolder: func() { events = append(events, "cancel_blocker") },
		waiterTx:     recordingRollback{&events, "waiter"},
		chainTx:      []lockRollback{recordingRollback{&events, "leaf"}},
		holderTx:     recordingRollback{&events, "blocker"},
	}
	if err := engine.Stop(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"cancel_waiter", "rollback_leaf", "rollback_waiter", "cancel_blocker", "rollback_blocker"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestLockEvidenceRequiresUngrantedTaggedExpectedModesAndObject(t *testing.T) {
	def := LockDefinition{
		Code: 502, HolderMode: "AccessExclusive", WaiterMode: "AccessShare",
		Object: "lock_table_targets", HolderTag: "blocker", WaiterTag: "waiter",
	}
	valid := LockEvidence{
		Granted: false, HolderMode: "AccessExclusive", WaiterMode: "AccessShare",
		Object: "lock_table_targets", BlockerTag: "blocker", WaiterTag: "waiter",
	}
	if got := verifyLock(def, []LockEvidence{valid}); got.Outcome != OutcomeSuccess {
		t.Fatalf("valid evidence=%+v", got)
	}
	valid.Granted = true
	if got := verifyLock(def, []LockEvidence{valid}); got.Outcome == OutcomeSuccess {
		t.Fatal("granted waiter reported success")
	}
}

func TestDeadlockRequiresDatabaseErrorAndTwoEdgeCycle(t *testing.T) {
	def := LockDefinition{Code: 504, Deadlock: true, HolderTag: "a", WaiterTag: "b"}
	evidence := []LockEvidence{
		{Granted: false, BlockerTag: "a", WaiterTag: "b"},
		{Granted: false, BlockerTag: "b", WaiterTag: "a"},
	}
	if got := verifyDeadlock(def, evidence, nil); got.Outcome == OutcomeSuccess {
		t.Fatal("cycle without actual deadlock error reported success")
	}
	if got := verifyDeadlock(def, evidence, deadlockError{state: "40P01"}); got.Outcome != OutcomeSuccess {
		t.Fatalf("deadlock evidence=%+v", got)
	}
	if got := verifyDeadlock(def, evidence, errors.New("deadlock detected")); got.Outcome == OutcomeSuccess {
		t.Fatalf("untyped text error reported success: %+v", got)
	}
}

func TestLockEvidenceRetainsEdgesObservedAcrossPolls(t *testing.T) {
	observed := appendLockEvidence(nil, []LockEvidence{{Granted: false, BlockerTag: "a", WaiterTag: "b"}})
	observed = appendLockEvidence(observed, []LockEvidence{{Granted: false, BlockerTag: "b", WaiterTag: "a"}})
	if !hasTwoEdgeCycle(observed, "a", "b") {
		t.Fatalf("observed=%+v", observed)
	}
}

func TestDistributedLockEvidenceUsesGlobalLocksWithNodeIdentity(t *testing.T) {
	query := lockEvidenceQuery(true)
	if !strings.Contains(query, "dbe_perf.global_locks") || !strings.Contains(query, "node_name") {
		t.Fatalf("query=%s", query)
	}
}

func TestLockEvidenceQueryJoinsTransactionAndVirtualXIDIdentity(t *testing.T) {
	query := lockEvidenceQuery(false)
	for _, identity := range []string{"transactionid", "virtualxid"} {
		if !strings.Contains(query, "h."+identity+" IS NOT DISTINCT FROM w."+identity) {
			t.Fatalf("missing %s identity join: %s", identity, query)
		}
	}
}

func TestRowChainRequiresTwoTransactionIDWaitEdges(t *testing.T) {
	def := LockDefinition{
		Code: 501, Name: "lock_row_chain", Object: "lock_targets", ExpectedKind: "row_chain",
		HolderMode: "Exclusive", WaiterMode: "Share", ChainTags: []string{"blocker", "chain-2", "chain-3"},
	}
	evidence := []LockEvidence{
		{Granted: false, LockType: "transactionid", Object: "lock_targets", HolderMode: "Exclusive", WaiterMode: "Share", BlockerTag: "blocker", WaiterTag: "chain-2"},
		{Granted: false, LockType: "transactionid", Object: "lock_targets", HolderMode: "Exclusive", WaiterMode: "Share", BlockerTag: "chain-2", WaiterTag: "chain-3"},
	}
	if got := verifyLock(def, evidence); got.Outcome != OutcomeSuccess {
		t.Fatalf("row-chain evidence=%+v", got)
	}
	if got := verifyLock(def, evidence[:1]); got.Outcome == OutcomeSuccess {
		t.Fatalf("single row edge reported success: %+v", got)
	}
}

func TestRowChainVerifyRetainsEvidenceAfterWorkloadDeadline(t *testing.T) {
	definition := LockDefinition{
		Code: 501, Name: "lock_row_chain", Object: "lock_targets", ExpectedKind: "row_chain",
		HolderMode: "Exclusive", WaiterMode: "Share", ChainTags: []string{"blocker", "chain-2", "chain-3"},
	}
	workloadCtx, cancel := context.WithCancel(context.Background())
	engine := NewLockEngine(definition)
	engine.observe = func(context.Context, *Runtime, LockDefinition) ([]LockEvidence, error) {
		return []LockEvidence{
			{Granted: false, LockType: "transactionid", Object: "lock_targets", HolderMode: "Exclusive", WaiterMode: "Share", BlockerTag: "blocker", WaiterTag: "chain-2"},
			{Granted: false, LockType: "transactionid", Object: "lock_targets", HolderMode: "Exclusive", WaiterMode: "Share", BlockerTag: "chain-2", WaiterTag: "chain-3"},
		}, nil
	}
	if err := engine.captureExpectedEvidence(workloadCtx, nil); err != nil {
		t.Fatal(err)
	}
	cancel() // mirrors the runner's duration-expired workload context.
	result, err := engine.Verify(context.Background(), nil)
	if err != nil || result.Outcome != OutcomeSuccess {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

type retainedLockLifecycleScenario struct {
	definition LockDefinition
	engine     *LockEngine
	waiterLive atomic.Bool
	canceled   atomic.Bool
}

func newRetainedLockLifecycleScenario(definition LockDefinition) *retainedLockLifecycleScenario {
	scenario := &retainedLockLifecycleScenario{
		definition: definition,
		engine:     NewLockEngine(definition),
	}
	scenario.engine.observe = func(context.Context, *Runtime, LockDefinition) ([]LockEvidence, error) {
		if !scenario.waiterLive.Load() {
			return nil, errors.New("waiter was canceled before lock evidence observation")
		}
		return []LockEvidence{{
			Node:       "local",
			Object:     definition.Object,
			LockType:   "relation",
			HolderMode: definition.HolderMode,
			WaiterMode: definition.WaiterMode,
			Granted:    false,
			BlockerTag: "gsbench/run-1/" + definition.Name + "/" + definition.HolderTag,
			WaiterTag:  "gsbench/run-1/" + definition.Name + "/" + definition.WaiterTag,
		}}, nil
	}
	return scenario
}

func (s *retainedLockLifecycleScenario) Code() ScenarioCode { return s.definition.Code }
func (s *retainedLockLifecycleScenario) Name() string       { return s.definition.Name }
func (s *retainedLockLifecycleScenario) Prepare(context.Context, *Runtime) error {
	return nil
}
func (s *retainedLockLifecycleScenario) Ramp(ctx context.Context, _ *Runtime) error {
	s.waiterLive.Store(true)
	go func() {
		<-ctx.Done()
		s.waiterLive.Store(false)
		s.canceled.Store(true)
	}()
	return nil
}
func (s *retainedLockLifecycleScenario) Hold(ctx context.Context, rt *Runtime) error {
	return s.engine.Hold(ctx, rt)
}
func (s *retainedLockLifecycleScenario) Verify(ctx context.Context, rt *Runtime) (Result, error) {
	deadline := time.Now().Add(100 * time.Millisecond)
	for !s.canceled.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !s.canceled.Load() {
		return Result{}, errors.New("workload duration did not cancel the live waiter before Verify")
	}
	return s.engine.Verify(ctx, rt)
}
func (s *retainedLockLifecycleScenario) Stop(ctx context.Context, rt *Runtime) error {
	return s.engine.Stop(ctx, rt)
}
func (s *retainedLockLifecycleScenario) Restore(context.Context, *Runtime) error {
	return nil
}

func TestLockRunnerLifecycleRetainsLiveEvidenceAfterDurationCancellation(t *testing.T) {
	relation := lockDefinitionsByCode(BusinessLockDefinitions("gsbench", "run-1"))[502]
	matrix := TableLockMatrixDefinitions("gsbench")[0]
	for _, definition := range []LockDefinition{relation, matrix} {
		t.Run(definition.Name, func(t *testing.T) {
			scenario := newRetainedLockLifecycleScenario(definition)
			runtime := &Runtime{
				RunID: "run-1",
				Config: BenchConfig{
					Run:    RunConfig{Duration: 30 * time.Millisecond},
					Safety: SafetyConfig{QueryTimeout: 100 * time.Millisecond},
				},
			}
			summary := runTestScenarios(t, context.Background(), runtime, []Scenario{scenario})
			if summary.Outcome != OutcomeSuccess {
				t.Fatalf("summary=%+v", summary)
			}
		})
	}
}

type deadlockError struct{ state string }

func (e deadlockError) Error() string    { return "deadlock detected" }
func (e deadlockError) SQLState() string { return e.state }
