package gsbench

import (
	"context"
	"database/sql"
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

type recordingClose struct {
	events *[]string
	name   string
}

func (c recordingClose) Close() error {
	*c.events = append(*c.events, "close_"+c.name)
	return nil
}

type blockingConfiguredLockExecutor struct {
	tag            string
	setupRemaining int
	started        chan<- string
}

func (e *blockingConfiguredLockExecutor) ExecContext(
	ctx context.Context,
	_ string,
	_ ...any,
) (sql.Result, error) {
	if e.setupRemaining > 0 {
		e.setupRemaining--
		return nil, nil
	}
	e.started <- e.tag
	<-ctx.Done()
	return nil, ctx.Err()
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

func TestLockEngineStartsEveryConfiguredWaiter(t *testing.T) {
	definition, err := configureLockDefinition(
		businessLockDefinitionForTest(t, 501),
		LockWorkloadConfig{RowChainSessions: 8, RowChainDepth: 3},
		"gsbench",
		"run-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewLockEngine(definition)
	started := make(chan string, len(definition.Waiters))
	opened := []string{}
	cleanupEvents := []string{}
	engine.openConfiguredWaiter = func(
		_ context.Context,
		_ *Runtime,
		role LockWaiterRole,
	) (*lockWaiterSession, error) {
		opened = append(opened, role.Tag)
		return &lockWaiterSession{
			role: role,
			conn: recordingClose{
				events: &cleanupEvents,
				name:   role.Tag,
			},
			tx: recordingRollback{
				events: &cleanupEvents,
				name:   role.Tag,
			},
			executor: &blockingConfiguredLockExecutor{
				tag:            role.Tag,
				setupRemaining: len(role.SetupSQL),
				started:        started,
			},
		}, nil
	}
	if err := engine.startConfiguredWaiters(
		context.Background(),
		&Runtime{},
	); err != nil {
		t.Fatal(err)
	}
	startedTags := make(map[string]bool, len(definition.Waiters))
	for range definition.Waiters {
		select {
		case tag := <-started:
			startedTags[tag] = true
		case <-time.After(time.Second):
			t.Fatalf("only %d configured waiters started", len(startedTags))
		}
	}
	if len(opened) != 7 || len(startedTags) != 7 {
		t.Fatalf("opened=%v started=%v", opened, startedTags)
	}
	if err := engine.Stop(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(cleanupEvents) != 14 {
		t.Fatalf("cleanup events=%v", cleanupEvents)
	}
}

func TestLockEngineCleansPartialConfiguredWaitersInReverseOrder(t *testing.T) {
	definition, err := configureLockDefinition(
		businessLockDefinitionForTest(t, 502),
		LockWorkloadConfig{TableExclusiveSessions: 6},
		"gsbench",
		"run-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	events := []string{}
	engine := NewLockEngine(definition)
	engine.holderTx = recordingRollback{events: &events, name: "blocker"}
	engine.cancelHolder = func() { events = append(events, "cancel_blocker") }
	engine.openConfiguredWaiter = func(
		_ context.Context,
		_ *Runtime,
		role LockWaiterRole,
	) (*lockWaiterSession, error) {
		if role.Tag == "waiter-4" {
			return nil, errors.New("connection refused")
		}
		return &lockWaiterSession{
			role: role,
			conn: recordingClose{events: &events, name: role.Tag},
			tx:   recordingRollback{events: &events, name: role.Tag},
			executor: &blockingConfiguredLockExecutor{
				tag:     role.Tag,
				started: make(chan string, 1),
			},
		}, nil
	}
	if err := engine.startConfiguredWaiters(
		context.Background(),
		&Runtime{},
	); err == nil || !strings.Contains(err.Error(), "waiter waiter-4 open") {
		t.Fatalf("start error=%v", err)
	}
	if err := engine.Stop(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"rollback_waiter-3", "close_waiter-3",
		"rollback_waiter-2", "close_waiter-2",
		"rollback_waiter-1", "close_waiter-1",
		"cancel_blocker", "rollback_blocker",
	}
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

func TestConfiguredRowChainRequiresEveryExpectedEdge(t *testing.T) {
	definition, err := configureLockDefinition(
		businessLockDefinitionForTest(t, 501),
		LockWorkloadConfig{RowChainSessions: 8, RowChainDepth: 3},
		"gsbench",
		"run-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	pairs := [][2]string{
		{"blocker", "chain-1-1"},
		{"chain-1-1", "chain-1-2"},
		{"chain-1-2", "chain-1-3"},
		{"blocker", "chain-2-1"},
		{"chain-2-1", "chain-2-2"},
		{"chain-2-2", "chain-2-3"},
		{"blocker", "chain-3-1"},
	}
	evidence := make([]LockEvidence, 0, len(pairs))
	for _, pair := range pairs {
		evidence = append(evidence, LockEvidence{
			Granted:    false,
			LockType:   "transactionid",
			Object:     "lock_targets",
			HolderMode: "Exclusive",
			WaiterMode: "Share",
			BlockerTag: "gsbench/run-1/lock_row_chain/" + pair[0],
			WaiterTag:  "gsbench/run-1/lock_row_chain/" + pair[1],
		})
	}
	if got := verifyLock(definition, evidence[:6]); got.Outcome == OutcomeSuccess {
		t.Fatalf("six of seven edges reported success: %+v", got)
	}
	got := verifyLock(definition, evidence)
	if got.Outcome != OutcomeSuccess {
		t.Fatalf("all configured edges failed: %+v", got)
	}
	metrics := map[string]Evidence{}
	for _, item := range got.Evidence {
		metrics[item.Metric] = item
	}
	for metric, target := range map[string]float64{
		"lock_sessions":        8,
		"lock_waiters":         7,
		"row_lock_chain_edges": 7,
		"chain_depth":          3,
	} {
		item, ok := metrics[metric]
		if !ok || item.Target != target || item.Actual != target {
			t.Fatalf("metric %s=%+v want target/actual %.0f", metric, item, target)
		}
	}
}

func TestConfiguredLockEvidenceCountsUniqueWaiterTags(t *testing.T) {
	definition, err := configureLockDefinition(
		businessLockDefinitionForTest(t, 503),
		LockWorkloadConfig{DDLWaitSessions: 4},
		"gsbench",
		"run-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	evidenceFor := func(waiter, waiterMode string) LockEvidence {
		return LockEvidence{
			Granted:    false,
			LockType:   "relation",
			Object:     "lock_ddl_targets",
			HolderMode: "RowExclusive",
			WaiterMode: waiterMode,
			BlockerTag: "gsbench/run-1/lock_ddl_wait/blocker",
			WaiterTag:  "gsbench/run-1/lock_ddl_wait/" + waiter,
		}
	}
	evidence := []LockEvidence{
		evidenceFor("waiter-1", "AccessExclusive"),
		evidenceFor("waiter-1", "AccessExclusive"),
		evidenceFor("waiter-2", "AccessShare"),
	}
	if got := verifyLock(definition, evidence); got.Outcome == OutcomeSuccess {
		t.Fatalf("duplicate waiter satisfied target: %+v", got)
	}
	evidence = append(evidence, evidenceFor("waiter-3", "AccessShare"))
	got := verifyLock(definition, evidence)
	if got.Outcome != OutcomeSuccess {
		t.Fatalf("three unique waiters failed: %+v", got)
	}
	metrics := map[string]Evidence{}
	for _, item := range got.Evidence {
		metrics[item.Metric] = item
	}
	if metrics["lock_sessions"].Target != 4 ||
		metrics["lock_sessions"].Actual != 4 ||
		metrics["lock_waiters"].Target != 3 ||
		metrics["lock_waiters"].Actual != 3 {
		t.Fatalf("metrics=%+v", metrics)
	}
}

func TestConfiguredLockEvidenceMatchesCompleteRoleNotPrefix(t *testing.T) {
	definition, err := configureLockDefinition(
		businessLockDefinitionForTest(t, 502),
		LockWorkloadConfig{TableExclusiveSessions: 11},
		"gsbench",
		"run-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	evidence := []LockEvidence{{
		Granted:    false,
		LockType:   "relation",
		Object:     "lock_table_targets",
		HolderMode: "AccessExclusive",
		WaiterMode: "AccessShare",
		BlockerTag: "gsbench/run-1/lock_table_exclusive/blocker",
		WaiterTag:  "gsbench/run-1/lock_table_exclusive/waiter-10",
	}}
	result := verifyLock(definition, evidence)
	if result.Outcome == OutcomeSuccess {
		t.Fatalf("one waiter with a shared prefix satisfied ten: %+v", result)
	}
}

func TestConfiguredLockEvidenceMatchesCompressedWorkerRoles(t *testing.T) {
	t.Run("row chain", func(t *testing.T) {
		runID := strings.Repeat("r", 31)
		definition, err := configureLockDefinition(
			businessLockDefinitionForTest(t, 501),
			LockWorkloadConfig{RowChainSessions: 4, RowChainDepth: 2},
			"gsbench",
			runID,
		)
		if err != nil {
			t.Fatal(err)
		}
		evidence := make([]LockEvidence, 0, len(definition.ExpectedEdges))
		compressed := false
		for _, edge := range definition.ExpectedEdges {
			blocker, err := ApplicationName(runID, definition.Name, edge.BlockerTag)
			if err != nil {
				t.Fatal(err)
			}
			waiter, err := ApplicationName(runID, definition.Name, edge.WaiterTag)
			if err != nil {
				t.Fatal(err)
			}
			compressed = compressed ||
				!strings.HasSuffix(blocker, "/"+edge.BlockerTag) ||
				!strings.HasSuffix(waiter, "/"+edge.WaiterTag)
			evidence = append(evidence, LockEvidence{
				Granted: false, LockType: "transactionid",
				Object:     definition.Object,
				HolderMode: definition.HolderMode,
				WaiterMode: definition.WaiterMode,
				BlockerTag: blocker,
				WaiterTag:  waiter,
			})
		}
		if !compressed {
			t.Fatal("test run ID did not force a compressed worker role")
		}
		if got := verifyLock(definition, evidence); got.Outcome != OutcomeSuccess {
			t.Fatalf("compressed row-chain evidence=%+v", got)
		}
	})

	t.Run("table waiters", func(t *testing.T) {
		runID := strings.Repeat("r", 26)
		definition, err := configureLockDefinition(
			businessLockDefinitionForTest(t, 502),
			LockWorkloadConfig{TableExclusiveSessions: 3},
			"gsbench",
			runID,
		)
		if err != nil {
			t.Fatal(err)
		}
		blocker, err := ApplicationName(
			runID, definition.Name, definition.HolderTag,
		)
		if err != nil {
			t.Fatal(err)
		}
		evidence := make([]LockEvidence, 0, len(definition.Waiters))
		for _, role := range definition.Waiters {
			waiter, err := ApplicationName(runID, definition.Name, role.Tag)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(waiter, "/"+role.Tag) {
				t.Fatalf("worker role %q was not compressed in %q", role.Tag, waiter)
			}
			evidence = append(evidence, LockEvidence{
				Granted: false, LockType: "relation",
				Object:     definition.Object,
				HolderMode: definition.HolderMode,
				WaiterMode: definition.WaiterMode,
				BlockerTag: blocker,
				WaiterTag:  waiter,
			})
		}
		if got := verifyLock(definition, evidence); got.Outcome != OutcomeSuccess {
			t.Fatalf("compressed table-waiter evidence=%+v", got)
		}
	})
}

func TestConfiguredLockEvidenceTimeoutIsBoundedByQueryTimeout(t *testing.T) {
	for _, test := range []struct {
		name         string
		queryTimeout time.Duration
		want         time.Duration
	}{
		{name: "default cap", queryTimeout: 30 * time.Second, want: 5 * time.Second},
		{name: "smaller query timeout", queryTimeout: 2 * time.Second, want: 2 * time.Second},
		{name: "missing query timeout", queryTimeout: 0, want: 5 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &Runtime{Config: BenchConfig{
				Safety: SafetyConfig{QueryTimeout: test.queryTimeout},
			}}
			if got := configuredLockEvidenceTimeout(runtime); got != test.want {
				t.Fatalf("timeout=%v want=%v", got, test.want)
			}
		})
	}
}

func TestConfiguredLockEvidenceReturnsWaiterExecutionError(t *testing.T) {
	definition, err := configureLockDefinition(
		businessLockDefinitionForTest(t, 502),
		LockWorkloadConfig{TableExclusiveSessions: 2},
		"gsbench",
		"run-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewLockEngine(definition)
	engine.setWaiterError(errors.New(
		"waiter waiter-1 wait: permission denied",
	))
	engine.observe = func(
		context.Context,
		*Runtime,
		LockDefinition,
	) ([]LockEvidence, error) {
		return nil, nil
	}
	runtime := &Runtime{Config: BenchConfig{
		Safety: SafetyConfig{QueryTimeout: 10 * time.Millisecond},
	}}
	err = engine.captureExpectedEvidence(
		context.Background(),
		runtime,
	)
	if err == nil || !strings.Contains(err.Error(), "waiter-1") ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error=%v", err)
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
