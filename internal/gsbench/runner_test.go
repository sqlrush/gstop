package gsbench

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeScenario struct {
	name      string
	mu        sync.Mutex
	phases    []Phase
	failPhase Phase
	outcome   Outcome
}

func (s *fakeScenario) Name() string { return s.name }
func (s *fakeScenario) Code() ScenarioCode {
	return 0
}
func (s *fakeScenario) record(phase Phase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phases = append(s.phases, phase)
	if s.failPhase == phase {
		return errors.New("phase failed")
	}
	return nil
}
func (s *fakeScenario) Prepare(context.Context, *Runtime) error { return s.record(PhasePrepare) }
func (s *fakeScenario) Ramp(context.Context, *Runtime) error    { return s.record(PhaseRamp) }
func (s *fakeScenario) Hold(context.Context, *Runtime) error    { return s.record(PhaseHold) }
func (s *fakeScenario) Verify(context.Context, *Runtime) (Result, error) {
	err := s.record(PhaseVerify)
	return Result{Scenario: s.name, Outcome: s.outcome}, err
}
func (s *fakeScenario) Stop(context.Context, *Runtime) error    { return s.record(PhaseStop) }
func (s *fakeScenario) Restore(context.Context, *Runtime) error { return s.record(PhaseRestore) }

type durationScenario struct {
	fakeScenario
	rampDelay time.Duration
}

type fixedWorkerDurationScenario struct {
	fakeScenario
	rampDelay       time.Duration
	holdStartedAt   time.Time
	holdEndedAt     time.Time
	holdHadDeadline bool
}

type executionReportingScenario struct {
	fakeScenario
	snapshot WorkerSnapshot
}

type runtimeEvidenceScenario struct {
	fakeScenario
	evidence []Evidence
}

func (s *runtimeEvidenceScenario) RuntimeEvidence() []Evidence {
	return s.evidence
}

func (s *executionReportingScenario) ExecutionSnapshot() WorkerSnapshot {
	return s.snapshot
}

type deadlineAtRampScenario struct {
	fakeScenario
	expiredAtRamp bool
}

type runnerRestoreService struct {
	mu       sync.Mutex
	requests []RestoreRequest
	result   RestoreSummary
}

type testCodeScenario struct {
	Scenario
	code ScenarioCode
}

func (s testCodeScenario) Code() ScenarioCode { return s.code }

func (s testCodeScenario) OwnsWorkloadDuration() bool {
	owner, ok := s.Scenario.(workloadDurationOwner)
	return ok && owner.OwnsWorkloadDuration()
}

type testCodeExecutionScenario struct {
	testCodeScenario
	reporter executionReporter
}

func (s testCodeExecutionScenario) ExecutionSnapshot() WorkerSnapshot {
	return s.reporter.ExecutionSnapshot()
}

type testCodeRuntimeEvidenceScenario struct {
	testCodeScenario
	reporter runtimeEvidenceReporter
}

func (s testCodeRuntimeEvidenceScenario) RuntimeEvidence() []Evidence {
	return s.reporter.RuntimeEvidence()
}

func newTestRunner(
	t *testing.T,
	runtime *Runtime,
	scenarios []Scenario,
) (*Runner, []ScenarioCode) {
	t.Helper()
	runtime.Config.Run.ValidationEnabled = true
	if !runtime.Environment.Supported {
		runtime.Environment = Environment{
			Product:      ProductOpenGauss,
			Topology:     TopologyStandalone,
			Capabilities: make(CapabilitySet),
			Supported:    true,
		}
	}
	if runtime.RestoreService == nil {
		runtime.RestoreService = &runnerRestoreService{
			result: RestoreSummary{Outcome: OutcomeSuccess},
		}
	}
	definitions := make([]ScenarioDefinition, len(scenarios))
	factories := make(map[ScenarioCode]ScenarioFactory, len(scenarios))
	codes := make([]ScenarioCode, len(scenarios))
	for i, scenario := range scenarios {
		code := ScenarioCode(101 + i)
		codes[i] = code
		definitions[i] = ScenarioDefinition{
			Code:      code,
			Name:      scenario.Name(),
			Category:  CategoryCPU,
			Risk:      RiskA,
			AppliesTo: []EnvironmentClass{EnvironmentOpenGauss},
		}
		var candidate Scenario = testCodeScenario{Scenario: scenario, code: code}
		if reporter, ok := scenario.(executionReporter); ok {
			candidate = testCodeExecutionScenario{
				testCodeScenario: testCodeScenario{Scenario: scenario, code: code},
				reporter:         reporter,
			}
		} else if reporter, ok := scenario.(runtimeEvidenceReporter); ok {
			candidate = testCodeRuntimeEvidenceScenario{
				testCodeScenario: testCodeScenario{Scenario: scenario, code: code},
				reporter:         reporter,
			}
		}
		factories[code] = func(
			ScenarioDefinition,
			Environment,
		) (Scenario, error) {
			return candidate, nil
		}
	}
	catalog, err := NewScenarioCatalog(definitions)
	if err != nil {
		t.Fatal(err)
	}
	return NewRunner(runtime, catalog, factories), codes
}

func runTestScenarios(
	t *testing.T,
	ctx context.Context,
	runtime *Runtime,
	scenarios []Scenario,
) RunSummary {
	t.Helper()
	runner, codes := newTestRunner(t, runtime, scenarios)
	return runner.Run(ctx, codes)
}

func (s *runnerRestoreService) Restore(
	_ context.Context,
	request RestoreRequest,
) RestoreSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	return s.result
}

func (s *deadlineAtRampScenario) Ramp(ctx context.Context, _ *Runtime) error {
	s.expiredAtRamp = ctx.Err() != nil
	return s.record(PhaseRamp)
}

func (s *durationScenario) Ramp(ctx context.Context, _ *Runtime) error {
	if err := s.record(PhaseRamp); err != nil {
		return err
	}
	return waitContext(ctx, s.rampDelay)
}

func (s *durationScenario) Hold(ctx context.Context, _ *Runtime) error {
	if err := s.record(PhaseHold); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *fixedWorkerDurationScenario) OwnsWorkloadDuration() bool { return true }

func (s *fixedWorkerDurationScenario) Ramp(
	ctx context.Context,
	_ *Runtime,
) error {
	if err := s.record(PhaseRamp); err != nil {
		return err
	}
	return waitContext(ctx, s.rampDelay)
}

func (s *fixedWorkerDurationScenario) Hold(
	ctx context.Context,
	rt *Runtime,
) error {
	if err := s.record(PhaseHold); err != nil {
		return err
	}
	_, s.holdHadDeadline = ctx.Deadline()
	s.holdStartedAt = time.Now()
	if err := waitContext(ctx, rt.Config.Run.Duration); err != nil {
		return err
	}
	s.holdEndedAt = time.Now()
	return nil
}

type resourceLifetimeScenario struct {
	fakeScenario
	resourceCtx context.Context
}

type preflightOrderScenario struct {
	fakeScenario
	preflightSeen *bool
}

type eventScenario struct {
	fakeScenario
	events *[]string
}

func (s *eventScenario) Prepare(context.Context, *Runtime) error {
	*s.events = append(*s.events, "prepare")
	return s.record(PhasePrepare)
}

type restoreRaceScenario struct {
	fakeScenario
	stopped chan struct{}
	once    sync.Once
}

type restoreBoundaryEvents struct {
	mu     sync.Mutex
	values []string
}

func (e *restoreBoundaryEvents) add(value string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.values = append(e.values, value)
}

func (e *restoreBoundaryEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.values...)
}

type workloadCancelScenario struct {
	fakeScenario
	cancel context.CancelFunc
	events *restoreBoundaryEvents
}

func (s *workloadCancelScenario) Ramp(
	ctx context.Context,
	_ *Runtime,
) error {
	if err := s.record(PhaseRamp); err != nil {
		return err
	}
	if s.cancel != nil {
		s.cancel()
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *workloadCancelScenario) Stop(
	context.Context,
	*Runtime,
) error {
	err := s.record(PhaseStop)
	s.events.add("stop:" + s.name)
	return err
}

type restoreContextValueKey struct{}

type cancellationRejectingRestoreBackend struct {
	path                 string
	discovery            RestoreDiscovery
	journal              *Journal
	store                *memoryActionStore
	events               *restoreBoundaryEvents
	expectedContextValue string
	restoreLimit         time.Duration

	mu                 sync.Mutex
	coordinatorCalls   int
	canceledContexts   int
	missingValues      int
	unboundedContexts  int
	verificationPassed bool
}

func (b *cancellationRejectingRestoreBackend) RestoreTimeout() time.Duration {
	return b.restoreLimit
}

func (b *cancellationRejectingRestoreBackend) checkContext(
	ctx context.Context,
	event string,
) error {
	b.events.add(event)
	b.mu.Lock()
	defer b.mu.Unlock()
	if event == "coordinator" {
		b.coordinatorCalls++
	}
	if err := ctx.Err(); err != nil {
		b.canceledContexts++
		return fmt.Errorf("%s received canceled context: %w", event, err)
	}
	if got, _ := ctx.Value(restoreContextValueKey{}).(string); got != b.expectedContextValue {
		b.missingValues++
		return fmt.Errorf(
			"%s context value=%q want=%q",
			event,
			got,
			b.expectedContextValue,
		)
	}
	if _, ok := ctx.Deadline(); !ok {
		b.unboundedContexts++
		return fmt.Errorf("%s restore context has no deadline", event)
	}
	return nil
}

func (b *cancellationRejectingRestoreBackend) AcquireRestoreLock(
	ctx context.Context,
) (RestoreLock, error) {
	if err := b.checkContext(ctx, "coordinator"); err != nil {
		return nil, err
	}
	return acquireLocalRestoreLock(ctx, b.path)
}

func (b *cancellationRejectingRestoreBackend) DiscoverRestore(
	ctx context.Context,
	_ string,
	_ bool,
) (RestoreDiscovery, error) {
	if err := b.checkContext(ctx, "discover"); err != nil {
		return RestoreDiscovery{}, err
	}
	return b.discovery, nil
}

func (b *cancellationRejectingRestoreBackend) MarkRestoreRequested(
	ctx context.Context,
	_ string,
) error {
	return b.checkContext(ctx, "claim")
}

func (b *cancellationRejectingRestoreBackend) StopTaggedSessions(
	ctx context.Context,
	_ string,
) error {
	return b.checkContext(ctx, "stop_sessions")
}

func (b *cancellationRejectingRestoreBackend) RestoreActionGroup(
	ctx context.Context,
	actions []Action,
) error {
	if err := b.checkContext(ctx, "inverse"); err != nil {
		return err
	}
	return b.journal.restoreCoordinatorActions(ctx, actions)
}

func (b *cancellationRejectingRestoreBackend) RepairBaseline(
	ctx context.Context,
) error {
	return b.checkContext(ctx, "baseline")
}

func (b *cancellationRejectingRestoreBackend) RedetectTopology(
	ctx context.Context,
) error {
	return b.checkContext(ctx, "topology")
}

func (b *cancellationRejectingRestoreBackend) VerifyRestore(
	ctx context.Context,
	runIDs []string,
	_ []Action,
) error {
	if err := b.checkContext(ctx, "verify"); err != nil {
		return err
	}
	for _, runID := range runIDs {
		pending, err := b.store.Pending(ctx, runID)
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			return fmt.Errorf(
				"run %s still has %d pending actions",
				runID,
				len(pending),
			)
		}
	}
	b.mu.Lock()
	b.verificationPassed = true
	b.mu.Unlock()
	return nil
}

func (b *cancellationRejectingRestoreBackend) MarkRestoreOutcome(
	ctx context.Context,
	_ string,
	_ Outcome,
) error {
	return b.checkContext(ctx, "outcome")
}

func (s *restoreRaceScenario) Stop(
	context.Context,
	*Runtime,
) error {
	if err := s.record(PhaseStop); err != nil {
		return err
	}
	s.once.Do(func() { close(s.stopped) })
	return nil
}

func (s *preflightOrderScenario) Prepare(context.Context, *Runtime) error {
	if s.preflightSeen == nil || !*s.preflightSeen {
		return errors.New("prepare ran before plan preflight")
	}
	return s.record(PhasePrepare)
}

func (s *resourceLifetimeScenario) Prepare(ctx context.Context, _ *Runtime) error {
	s.resourceCtx = ctx
	return s.record(PhasePrepare)
}

func (s *resourceLifetimeScenario) Ramp(context.Context, *Runtime) error {
	return s.record(PhaseRamp)
}

func (s *resourceLifetimeScenario) Hold(ctx context.Context, _ *Runtime) error {
	if err := s.record(PhaseHold); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *resourceLifetimeScenario) Verify(context.Context, *Runtime) (Result, error) {
	if err := s.record(PhaseVerify); err != nil {
		return Result{}, err
	}
	if err := s.resourceCtx.Err(); err != nil {
		return Result{}, errors.New("prepare resource context expired before stop")
	}
	return Result{Scenario: s.name, Outcome: s.outcome}, nil
}

func TestRunnerUsesExactLifecycleOrderWithoutPersistentRestore(t *testing.T) {
	s := &fakeScenario{name: "one", outcome: OutcomeSuccess}
	service := &runnerRestoreService{result: RestoreSummary{
		Outcome: OutcomeSuccess,
	}}
	summary := runTestScenarios(
		t,
		context.Background(),
		&Runtime{RunID: "run-1", RestoreService: service},
		[]Scenario{s},
	)
	want := []Phase{PhasePrepare, PhaseRamp, PhaseHold, PhaseVerify, PhaseStop}
	if !reflect.DeepEqual(s.phases, want) {
		t.Fatalf("phases=%v want=%v", s.phases, want)
	}
	if len(service.requests) != 0 {
		t.Fatalf("coordinator calls=%d want=0", len(service.requests))
	}
	if summary.Outcome != OutcomeSuccess {
		t.Fatalf("summary=%+v", summary)
	}
	if summary.Results[0].StartedAt.IsZero() || summary.Results[0].EndedAt.IsZero() {
		t.Fatalf("runner timestamps missing: %+v", summary.Results[0])
	}
	if summary.Results[0].Restore.State != "manual_recovery" {
		t.Fatalf("restore evidence=%+v", summary.Results[0].Restore)
	}
}

func TestRunnerTreatsLegacyValidationFlagAsAdvisory(t *testing.T) {
	scenario := &fakeScenario{
		name:    "one",
		outcome: OutcomeSuccess,
	}
	runtime := &Runtime{
		RunID: "run-1",
		PlanPreflight: func(context.Context, string, []string) error {
			return errors.New("plan validation failed")
		},
	}
	runner, codes := newTestRunner(t, runtime, []Scenario{scenario})
	runner.runtime.Config.Run.ValidationEnabled = false
	summary := runner.Run(context.Background(), codes)
	if summary.Outcome != OutcomeCompletedWithWarnings {
		t.Fatalf("summary=%+v", summary)
	}
	want := []Phase{PhasePrepare, PhaseRamp, PhaseHold, PhaseVerify, PhaseStop}
	if !reflect.DeepEqual(scenario.phases, want) {
		t.Fatalf("phases=%v want=%v", scenario.phases, want)
	}
	assertPrecheckWarning(t, summary.Results[0], "workload_plan")
}

func TestRunnerVerificationFailureWarnsAndCompletes(t *testing.T) {
	scenario := &fakeScenario{
		name:      "one",
		outcome:   OutcomeFailed,
		failPhase: PhaseVerify,
	}
	summary := runTestScenarios(
		t,
		context.Background(),
		&Runtime{RunID: "run-1"},
		[]Scenario{scenario},
	)
	if summary.Outcome != OutcomeCompletedWithWarnings {
		t.Fatalf("summary=%+v", summary)
	}
	want := []Phase{PhasePrepare, PhaseRamp, PhaseHold, PhaseVerify, PhaseStop}
	if !reflect.DeepEqual(scenario.phases, want) {
		t.Fatalf("phases=%v want=%v", scenario.phases, want)
	}
	assertPrecheckWarning(t, summary.Results[0], "runtime_verification")
}

func TestRunnerFailsOnExecutionErrorsWhenValidationDisabled(t *testing.T) {
	scenario := &executionReportingScenario{
		fakeScenario: fakeScenario{name: "one", outcome: OutcomeSuccess},
		snapshot: WorkerSnapshot{
			Operations: 7,
			Errors:     2,
			FirstError: "sentinel workload error",
		},
	}
	runtime := &Runtime{RunID: "run-1"}
	runner, codes := newTestRunner(t, runtime, []Scenario{scenario})
	runner.runtime.Config.Run.ValidationEnabled = false
	summary := runner.Run(context.Background(), codes)
	if summary.Outcome != OutcomeFailed {
		t.Fatalf("summary=%+v", summary)
	}
	result := summary.Results[0]
	if !strings.Contains(result.Message, "sentinel workload error") {
		t.Fatalf("result=%+v", result)
	}
	if len(result.Evidence) < 2 || result.Evidence[0].Metric != "operations" ||
		result.Evidence[1].Metric != "errors" {
		t.Fatalf("execution evidence=%+v", result.Evidence)
	}
}

func TestRunnerVerifiesCleanExecutionWhenLegacyValidationFlagIsDisabled(t *testing.T) {
	scenario := &executionReportingScenario{
		fakeScenario: fakeScenario{name: "one", outcome: OutcomeSuccess},
		snapshot:     WorkerSnapshot{Operations: 7},
	}
	runtime := &Runtime{RunID: "run-1"}
	runner, codes := newTestRunner(t, runtime, []Scenario{scenario})
	runner.runtime.Config.Run.ValidationEnabled = false
	summary := runner.Run(context.Background(), codes)
	if summary.Outcome != OutcomeSuccess {
		t.Fatalf("summary=%+v", summary)
	}
	result := summary.Results[0]
	if len(result.Evidence) != 2 || result.Evidence[0].Metric != "operations" ||
		result.Evidence[1].Metric != "errors" {
		t.Fatalf("execution evidence=%+v", result.Evidence)
	}
}

func TestRunnerKeepsRuntimeMeasurementsRegardlessOfLegacyValidationFlag(t *testing.T) {
	for _, validationEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("validation_%t", validationEnabled), func(t *testing.T) {
			scenario := &runtimeEvidenceScenario{
				fakeScenario: fakeScenario{name: "one", outcome: OutcomeSuccess},
				evidence: []Evidence{{
					Metric: "cpu_percent", Target: 95, Actual: 94, Available: true,
				}},
			}
			runtime := &Runtime{RunID: "run-1"}
			runner, codes := newTestRunner(t, runtime, []Scenario{scenario})
			runner.runtime.Config.Run.ValidationEnabled = validationEnabled
			summary := runner.Run(context.Background(), codes)
			if summary.Outcome != OutcomeSuccess {
				t.Fatalf("summary=%+v", summary)
			}
			var found bool
			for _, evidence := range summary.Results[0].Evidence {
				if evidence.Metric == "cpu_percent" && evidence.Target == 95 &&
					evidence.Actual == 94 && evidence.Available {
					found = true
				}
			}
			if !found {
				t.Fatalf("runtime measurements were discarded: %+v", summary.Results[0])
			}
		})
	}
}

func TestRunnerStopsAllScenariosWithoutCallingCoordinator(t *testing.T) {
	first := &fakeScenario{name: "one", outcome: OutcomeSuccess}
	second := &fakeScenario{name: "two", outcome: OutcomeSuccess}
	service := &runnerRestoreService{result: RestoreSummary{
		RunIDs:  []string{"run-1"},
		Outcome: OutcomeSuccess,
	}}
	runtime := &Runtime{
		RunID:          "run-1",
		RestoreService: service,
	}

	summary := runTestScenarios(
		t,
		context.Background(),
		runtime,
		[]Scenario{first, second},
	)
	if summary.Outcome != OutcomeSuccess {
		t.Fatalf("summary=%+v", summary)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.requests) != 0 {
		t.Fatalf("restore requests=%+v", service.requests)
	}
	for _, scenario := range []*fakeScenario{first, second} {
		if len(scenario.phases) == 0 ||
			scenario.phases[len(scenario.phases)-1] != PhaseStop {
			t.Fatalf(
				"scenario %s was not stopped before coordinator: %v",
				scenario.name,
				scenario.phases,
			)
		}
		for _, phase := range scenario.phases {
			if phase == PhaseRestore {
				t.Fatalf(
					"scenario %s directly restored: %v",
					scenario.name,
					scenario.phases,
				)
			}
		}
	}
}

func TestRunnerCompletionAndExternalRestoreRaceExecutesInverseOnce(
	t *testing.T,
) {
	action := validSQLJournalAction()
	action.RunID = "run-race"
	action.Sequence = 7
	action.State = MutationApplied
	store := &racingClaimStore{
		action: action, restoringGate: make(chan struct{}),
	}
	executor := &countingRestoreExecutor{}
	journal := NewJournal(store, executor)
	discovery := RestoreDiscovery{
		Runs:            []RestoreRun{{RunID: action.RunID}},
		DatabaseActions: []Action{action},
	}
	path := filepath.Join(t.TempDir(), "recovery.json")
	normalCoordinator := NewRestoreCoordinator(&localLockRestoreBackend{
		path: path, discovery: discovery, journal: journal,
	})
	externalCoordinator := NewRestoreCoordinator(&localLockRestoreBackend{
		path: path, discovery: discovery, journal: journal,
	})
	start := make(chan struct{})
	scenario := &restoreRaceScenario{
		fakeScenario: fakeScenario{
			name: "one", outcome: OutcomeSuccess,
		},
		stopped: make(chan struct{}),
	}
	runtime := &Runtime{
		RunID: action.RunID,
		RestoreService: gatedRestoreService{
			start: start, service: normalCoordinator,
		},
	}
	runnerDone := make(chan RunSummary, 1)
	runner, codes := newTestRunner(t, runtime, []Scenario{scenario})
	go func() {
		runnerDone <- runner.Run(context.Background(), codes)
	}()
	<-scenario.stopped
	externalDone := make(chan RestoreSummary, 1)
	go func() {
		<-start
		externalDone <- externalCoordinator.Restore(
			context.Background(),
			RestoreRequest{RunID: action.RunID},
		)
	}()
	close(start)
	runnerSummary := <-runnerDone
	externalSummary := <-externalDone

	executor.mu.Lock()
	restores := executor.restores
	executor.mu.Unlock()
	if restores != 1 {
		t.Fatalf(
			"inverse executions=%d runner=%+v external=%+v",
			restores,
			runnerSummary,
			externalSummary,
		)
	}
	if runnerSummary.Outcome != OutcomeSuccess &&
		externalSummary.Outcome != OutcomeSuccess {
		t.Fatalf(
			"neither racing coordinator completed recovery: runner=%+v external=%+v",
			runnerSummary,
			externalSummary,
		)
	}
}

func TestRunnerCancellationStopsOwnedResourcesWithoutPersistentRestore(
	t *testing.T,
) {
	const (
		runID        = "run-cancel"
		contextValue = "restore-trace"
	)
	action := validSQLJournalAction()
	action.RunID = runID
	action.Sequence = 1
	action.State = MutationApplied
	store := &memoryActionStore{entries: []Action{action}}
	inverseCalls := 0
	executor := &memoryActionExecutor{
		onRestore: func(Action) {
			inverseCalls++
		},
	}
	events := &restoreBoundaryEvents{}
	backend := &cancellationRejectingRestoreBackend{
		path: filepath.Join(t.TempDir(), "recovery.json"),
		discovery: RestoreDiscovery{
			Runs:            []RestoreRun{{RunID: runID}},
			DatabaseActions: []Action{action},
		},
		journal:              NewJournal(store, executor),
		store:                store,
		events:               events,
		expectedContextValue: contextValue,
		restoreLimit:         2 * time.Second,
	}
	parent := context.WithValue(
		context.Background(),
		restoreContextValueKey{},
		contextValue,
	)
	workloadCtx, cancelWorkload := context.WithCancel(parent)
	defer cancelWorkload()
	first := &workloadCancelScenario{
		fakeScenario: fakeScenario{
			name: "first", outcome: OutcomeSuccess,
		},
		cancel: cancelWorkload,
		events: events,
	}
	second := &workloadCancelScenario{
		fakeScenario: fakeScenario{
			name: "second", outcome: OutcomeSuccess,
		},
		events: events,
	}
	var phaseMu sync.Mutex
	restorePhaseContexts := 0
	canceledRestorePhaseContexts := 0
	missingRestorePhaseValues := 0
	runtime := &Runtime{
		RunID:          runID,
		RestoreService: NewRestoreCoordinator(backend),
		ReportPhase: func(
			ctx context.Context,
			_ string,
			phase Phase,
		) {
			if phase != PhaseRestore &&
				phase != PhaseVerifyRestore {
				return
			}
			phaseMu.Lock()
			defer phaseMu.Unlock()
			restorePhaseContexts++
			if ctx.Err() != nil {
				canceledRestorePhaseContexts++
			}
			if got, _ := ctx.Value(
				restoreContextValueKey{},
			).(string); got != contextValue {
				missingRestorePhaseValues++
			}
		},
	}
	runner, codes := newTestRunner(
		t,
		runtime,
		[]Scenario{first, second},
	)

	summary := runner.Run(workloadCtx, codes)

	if summary.Outcome != OutcomeFailed {
		t.Fatalf(
			"outcome=%s want=%s; restore=%+v",
			summary.Outcome,
			OutcomeFailed,
			summary.Results[0].Restore,
		)
	}
	if summary.Results[0].Restore.Failed ||
		summary.Results[0].Restore.State != "manual_recovery" {
		t.Fatalf("restore evidence=%+v", summary.Results[0].Restore)
	}
	backend.mu.Lock()
	coordinatorCalls := backend.coordinatorCalls
	canceledContexts := backend.canceledContexts
	missingValues := backend.missingValues
	unboundedContexts := backend.unboundedContexts
	verificationPassed := backend.verificationPassed
	backend.mu.Unlock()
	if coordinatorCalls != 0 ||
		canceledContexts != 0 ||
		missingValues != 0 ||
		unboundedContexts != 0 ||
		verificationPassed {
		t.Fatalf(
			"coordinator calls=%d canceled=%d missing_values=%d "+
				"unbounded=%d verified=%v events=%v",
			coordinatorCalls,
			canceledContexts,
			missingValues,
			unboundedContexts,
			verificationPassed,
			events.snapshot(),
		)
	}
	if inverseCalls != 0 || len(executor.verifyActions) != 0 {
		t.Fatalf(
			"inverse calls=%d action verifications=%d",
			inverseCalls,
			len(executor.verifyActions),
		)
	}
	pending, err := store.Pending(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending actions must remain visible=%+v", pending)
	}
	gotEvents := events.snapshot()
	if len(gotEvents) < 2 {
		t.Fatalf("events=%v", gotEvents)
	}
	stopped := map[string]bool{}
	for _, event := range gotEvents {
		stopped[event] = true
	}
	if !stopped["stop:first"] || !stopped["stop:second"] {
		t.Fatalf(
			"not every scenario stopped: %v",
			gotEvents,
		)
	}
	phaseMu.Lock()
	defer phaseMu.Unlock()
	if restorePhaseContexts != 0 ||
		canceledRestorePhaseContexts != 0 ||
		missingRestorePhaseValues != 0 {
		t.Fatalf(
			"restore phase contexts=%d canceled=%d missing_values=%d",
			restorePhaseContexts,
			canceledRestorePhaseContexts,
			missingRestorePhaseValues,
		)
	}
}

func TestRunnerDoesNotCallCoordinatorAfterRampFailure(t *testing.T) {
	s := &fakeScenario{name: "one", failPhase: PhaseRamp, outcome: OutcomeSuccess}
	service := &runnerRestoreService{result: RestoreSummary{
		Outcome: OutcomeSuccess,
	}}
	summary := runTestScenarios(
		t,
		context.Background(),
		&Runtime{RunID: "run-1", RestoreService: service},
		[]Scenario{s},
	)
	want := []Phase{PhasePrepare, PhaseRamp, PhaseStop}
	if !reflect.DeepEqual(s.phases, want) {
		t.Fatalf("phases=%v want=%v", s.phases, want)
	}
	if len(service.requests) != 0 {
		t.Fatalf("coordinator calls=%d want=0", len(service.requests))
	}
	if summary.Outcome != OutcomeFailed {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestRunnerTargetFailureDoesNotCancelOtherScenario(t *testing.T) {
	failed := &fakeScenario{
		name: "pool-target", failPhase: PhaseRamp, outcome: OutcomeSuccess,
	}
	other := &fakeScenario{name: "other", outcome: OutcomeSuccess}
	summary := runTestScenarios(
		t,
		context.Background(),
		&Runtime{RunID: "run-1"},
		[]Scenario{failed, other},
	)
	if summary.Outcome != OutcomeFailed {
		t.Fatalf("summary=%+v", summary)
	}
	want := []Phase{
		PhasePrepare, PhaseRamp, PhaseHold, PhaseVerify, PhaseStop,
	}
	if !reflect.DeepEqual(other.phases, want) {
		t.Fatalf("other phases=%v want=%v", other.phases, want)
	}
}

func TestRunnerDoesNotPassAdvisoryOutcomeToCoordinator(t *testing.T) {
	scenario := &fakeScenario{name: "one", outcome: OutcomeDegraded}
	service := &runnerRestoreService{result: RestoreSummary{
		Outcome: OutcomeSuccess,
	}}

	summary := runTestScenarios(
		t,
		context.Background(),
		&Runtime{RunID: "run-1", RestoreService: service},
		[]Scenario{scenario},
	)

	if summary.Outcome != OutcomeCompletedWithWarnings {
		t.Fatalf("summary=%+v", summary)
	}
	if len(service.requests) != 0 {
		t.Fatalf("restore requests=%+v", service.requests)
	}
}

func TestRunnerAggregatesWorstOutcome(t *testing.T) {
	scenarios := []Scenario{
		&fakeScenario{name: "ok", outcome: OutcomeSuccess},
		&fakeScenario{name: "degraded", outcome: OutcomeDegraded},
	}
	summary := runTestScenarios(
		t,
		context.Background(),
		&Runtime{},
		scenarios,
	)
	if summary.Outcome != OutcomeCompletedWithWarnings {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestRunnerTreatsVerificationOutcomeStatesAsAdvisory(t *testing.T) {
	tests := []struct {
		name     string
		outcomes []Outcome
		want     Outcome
	}{
		{"not applicable becomes advisory", []Outcome{OutcomeSuccess, OutcomeNotApplicable}, OutcomeCompletedWithWarnings},
		{"not implemented becomes advisory", []Outcome{OutcomeNotApplicable, OutcomeNotImplemented}, OutcomeCompletedWithWarnings},
		{"reported restore failure becomes advisory", []Outcome{OutcomeFailed, OutcomeRestoreFailed}, OutcomeCompletedWithWarnings},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scenarios := make([]Scenario, len(test.outcomes))
			for i, outcome := range test.outcomes {
				name := fmt.Sprintf("scenario-%d", i)
				scenarios[i] = &fakeScenario{name: name, outcome: outcome}
			}
			summary := runTestScenarios(
				t,
				context.Background(),
				&Runtime{},
				scenarios,
			)
			if summary.Outcome != test.want {
				t.Fatalf("outcome=%s want=%s", summary.Outcome, test.want)
			}
		})
	}
}

func TestRunnerDurationIncludesRampAndHold(t *testing.T) {
	s := &durationScenario{
		fakeScenario: fakeScenario{name: "one", outcome: OutcomeSuccess},
		rampDelay:    30 * time.Millisecond,
	}
	runtime := &Runtime{Config: BenchConfig{
		Run:    RunConfig{Duration: 80 * time.Millisecond},
		Safety: SafetyConfig{QueryTimeout: 100 * time.Millisecond},
	}}
	service := &runnerRestoreService{result: RestoreSummary{
		Outcome: OutcomeSuccess,
	}}
	runtime.RunID = "run-1"
	runtime.RestoreService = service
	outerCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	started := time.Now()
	summary := runTestScenarios(t, outerCtx, runtime, []Scenario{s})
	elapsed := time.Since(started)

	if elapsed < 50*time.Millisecond || elapsed > 250*time.Millisecond {
		t.Fatalf("elapsed=%s, want the shared 80ms ramp+hold duration", elapsed)
	}
	want := []Phase{PhasePrepare, PhaseRamp, PhaseHold, PhaseVerify, PhaseStop}
	if !reflect.DeepEqual(s.phases, want) {
		t.Fatalf("phases=%v want=%v", s.phases, want)
	}
	if summary.Outcome != OutcomeSuccess {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestRunnerFixedWorkerDurationStartsAfterRampReadiness(t *testing.T) {
	s := &fixedWorkerDurationScenario{
		fakeScenario: fakeScenario{name: "tp_cpu", outcome: OutcomeSuccess},
		rampDelay:    30 * time.Millisecond,
	}
	runtime := &Runtime{Config: BenchConfig{
		Run:    RunConfig{Duration: 80 * time.Millisecond},
		Safety: SafetyConfig{QueryTimeout: 100 * time.Millisecond},
	}}
	started := time.Now()
	summary := runTestScenarios(t, context.Background(), runtime, []Scenario{s})
	elapsed := time.Since(started)
	holdElapsed := s.holdEndedAt.Sub(s.holdStartedAt)

	if elapsed < 100*time.Millisecond || elapsed > 300*time.Millisecond {
		t.Fatalf("elapsed=%s, want ramp readiness plus full pressure duration", elapsed)
	}
	if holdElapsed < 75*time.Millisecond || holdElapsed > 200*time.Millisecond {
		t.Fatalf("hold elapsed=%s, want full fixed-worker duration", holdElapsed)
	}
	if s.holdHadDeadline {
		t.Fatal("fixed-worker Hold inherited the Runner ramp+hold deadline")
	}
	if summary.Outcome != OutcomeSuccess {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestRunnerStartsSharedWorkloadDurationAfterPreflightAndPrepareBarrier(t *testing.T) {
	s := &deadlineAtRampScenario{
		fakeScenario: fakeScenario{name: "tp_cpu", outcome: OutcomeSuccess},
	}
	runtime := &Runtime{
		Config: BenchConfig{
			Run:    RunConfig{Duration: 20 * time.Millisecond},
			Data:   DataConfig{Schema: "gsbench"},
			Safety: SafetyConfig{QueryTimeout: time.Second},
		},
		PlanPreflight: func(context.Context, string, []string) error {
			time.Sleep(35 * time.Millisecond)
			return nil
		},
	}

	summary := runTestScenarios(
		t,
		context.Background(),
		runtime,
		[]Scenario{s},
	)

	if s.expiredAtRamp {
		t.Fatal("workload duration expired during preflight/prepare")
	}
	if summary.Outcome != OutcomeSuccess {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestRunnerKeepsPrepareResourcesAliveUntilStop(t *testing.T) {
	s := &resourceLifetimeScenario{
		fakeScenario: fakeScenario{name: "one", outcome: OutcomeSuccess},
	}
	runtime := &Runtime{Config: BenchConfig{
		Run:    RunConfig{Duration: 30 * time.Millisecond},
		Safety: SafetyConfig{QueryTimeout: 100 * time.Millisecond},
	}}

	summary := runTestScenarios(
		t,
		context.Background(),
		runtime,
		[]Scenario{s},
	)

	if summary.Outcome != OutcomeSuccess {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestRunnerPreflightsPlansBeforePrepare(t *testing.T) {
	preflightSeen := false
	scenario := &preflightOrderScenario{
		fakeScenario:  fakeScenario{name: "tp_cpu", outcome: OutcomeSuccess},
		preflightSeen: &preflightSeen,
	}
	runtime := &Runtime{
		Config: BenchConfig{
			Data:   DataConfig{Schema: "gsbench"},
			Safety: SafetyConfig{QueryTimeout: time.Second},
		},
		PlanPreflight: func(_ context.Context, name string, sqls []string) error {
			if name != "tp_cpu" || len(sqls) == 0 {
				t.Fatalf("name=%s sqls=%v", name, sqls)
			}
			preflightSeen = true
			return nil
		},
	}
	summary := runTestScenarios(
		t,
		context.Background(),
		runtime,
		[]Scenario{scenario},
	)
	if summary.Outcome != OutcomeSuccess {
		t.Fatalf("summary=%+v", summary)
	}
	if !preflightSeen || len(scenario.phases) == 0 || scenario.phases[0] != PhasePrepare {
		t.Fatalf("preflight=%v phases=%v", preflightSeen, scenario.phases)
	}
}

func TestRunnerPlanPreflightFailureWarnsAndContinues(t *testing.T) {
	scenario := &fakeScenario{name: "tp_cpu", outcome: OutcomeSuccess}
	service := &runnerRestoreService{result: RestoreSummary{
		Outcome: OutcomeSuccess,
	}}
	runtime := &Runtime{
		RunID:          "run-1",
		RestoreService: service,
		Config: BenchConfig{
			Data:   DataConfig{Schema: "gsbench"},
			Safety: SafetyConfig{QueryTimeout: time.Second},
		},
		PlanPreflight: func(context.Context, string, []string) error {
			return errors.New("plan unavailable")
		},
	}
	summary := runTestScenarios(
		t,
		context.Background(),
		runtime,
		[]Scenario{scenario},
	)
	if summary.Outcome != OutcomeCompletedWithWarnings {
		t.Fatalf("summary=%+v", summary)
	}
	want := []Phase{PhasePrepare, PhaseRamp, PhaseHold, PhaseVerify, PhaseStop}
	if !reflect.DeepEqual(scenario.phases, want) {
		t.Fatalf("phases=%v", scenario.phases)
	}
	assertPrecheckWarning(t, summary.Results[0], "workload_plan")
	if len(service.requests) != 0 {
		t.Fatalf("coordinator calls=%d want=0", len(service.requests))
	}
}

func TestRunnerDeadlineStillStopsAndRestoresPlanWorkload(t *testing.T) {
	scenario := &durationScenario{
		fakeScenario: fakeScenario{name: "plan_index_drop", outcome: OutcomeSuccess},
	}
	runtime := &Runtime{Config: BenchConfig{
		Run:    RunConfig{Duration: 50 * time.Millisecond},
		Safety: SafetyConfig{QueryTimeout: time.Second},
	}}
	started := time.Now()
	summary := runTestScenarios(
		t,
		context.Background(),
		runtime,
		[]Scenario{scenario},
	)
	if time.Since(started) > time.Second {
		t.Fatal("runner exceeded bounded shutdown")
	}
	if summary.Results[0].Outcome != OutcomeSuccess {
		t.Fatalf("result=%+v", summary.Results[0])
	}
}

func TestRunnerInapplicableEnvironmentWarnsAndContinues(t *testing.T) {
	catalog, err := NewScenarioCatalog([]ScenarioDefinition{{
		Code:      721,
		Name:      "cluster_data_skew",
		Category:  CategoryReplicationCluster,
		Risk:      RiskA,
		AppliesTo: []EnvironmentClass{EnvironmentDistributedGaussDB},
	}})
	if err != nil {
		t.Fatal(err)
	}
	factoryCalled := false
	scenario := &fakeScenario{name: "cluster_data_skew", outcome: OutcomeSuccess}
	factories := map[ScenarioCode]ScenarioFactory{
		721: func(ScenarioDefinition, Environment) (Scenario, error) {
			factoryCalled = true
			return testCodeScenario{Scenario: scenario, code: 721}, nil
		},
	}
	runner := NewRunner(
		&Runtime{Environment: Environment{
			Product:   ProductOpenGauss,
			Topology:  TopologyStandalone,
			Supported: true,
		}},
		catalog,
		factories,
	)

	got := runner.Run(context.Background(), []ScenarioCode{721})

	if got.Results[0].Outcome != OutcomeCompletedWithWarnings {
		t.Fatalf("result=%+v", got.Results[0])
	}
	if !factoryCalled {
		t.Fatal("factory was not called for an inapplicable environment")
	}
	want := []Phase{PhasePrepare, PhaseRamp, PhaseHold, PhaseVerify, PhaseStop}
	if !reflect.DeepEqual(scenario.phases, want) {
		t.Fatalf("phases=%v want=%v", scenario.phases, want)
	}
	assertPrecheckWarning(t, got.Results[0], "environment_applicability")
}

func TestRunnerReturnsNotImplementedWithCatalogIdentityForMissingFactory(
	t *testing.T,
) {
	catalog, err := NewScenarioCatalog([]ScenarioDefinition{{
		Code:      601,
		Name:      "planchange_stats_target",
		Category:  CategoryPlan,
		Risk:      RiskA,
		AppliesTo: []EnvironmentClass{EnvironmentOpenGauss},
	}})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{
		Environment: Environment{
			Product: ProductOpenGauss, Topology: TopologyStandalone,
			Supported: true,
		},
	}

	got := NewRunner(runtime, catalog, nil).Run(
		context.Background(),
		[]ScenarioCode{601},
	)

	result := got.Results[0]
	if result.Outcome != OutcomeNotImplemented ||
		result.ScenarioCode != 601 ||
		result.Scenario != "planchange_stats_target" {
		t.Fatalf("result=%+v", result)
	}
}

func TestRunnerUnknownCodeFailsExplicitly(t *testing.T) {
	got := NewRunner(
		&Runtime{},
		DefaultScenarioCatalog(),
		DefaultScenarioFactories(),
	).Run(context.Background(), []ScenarioCode{999})

	if len(got.Results) != 1 ||
		got.Results[0].Outcome != OutcomeFailed ||
		!strings.Contains(got.Results[0].Message, "unknown scenario code 999") {
		t.Fatalf("summary=%+v", got)
	}
}

func TestDefaultScenarioFactoriesRegisterOnlyRunnableCatalogCodes(t *testing.T) {
	factories := DefaultScenarioFactories()
	for _, code := range []ScenarioCode{
		101, 102, 103,
		201, 202, 203, 204, 205, 207, 208,
		301, 302, 303, 304, 321, 322, 331, 332, 333,
		401, 402, 403, 404, 801,
	} {
		factory := factories[code]
		if factory == nil {
			t.Errorf("runnable scenario %03d has no factory", code)
			continue
		}
		definition := DefaultScenarioCatalog().MustCode(code)
		scenario, err := factory(definition, Environment{
			Product: ProductOpenGauss, Topology: TopologyStandalone,
			Supported: true,
		})
		if err != nil {
			t.Errorf("factory %03d: %v", code, err)
			continue
		}
		if scenario == nil || scenario.Code() != code {
			t.Errorf("factory %03d returned %#v", code, scenario)
		}
	}
	lockCodes := append(lockScenarioCodes(501, 506), lockScenarioCodes(508, 510)...)
	for _, code := range append(lockCodes, lockScenarioCodes(520, 540)...) {
		factory := factories[code]
		if factory == nil {
			t.Errorf("runnable scenario %03d has no factory", code)
			continue
		}
		definition := DefaultScenarioCatalog().MustCode(code)
		scenario, err := factory(definition, Environment{Product: ProductOpenGauss, Topology: TopologyStandalone, Supported: true})
		if err != nil || scenario == nil || scenario.Code() != code {
			t.Errorf("lock factory %03d returned %#v, %v", code, scenario, err)
		}
	}
	for _, code := range append(lockScenarioCodes(601, 606), lockScenarioCodes(621, 625)...) {
		factory := factories[code]
		if factory == nil {
			t.Errorf("runnable scenario %03d has no factory", code)
			continue
		}
		definition := DefaultScenarioCatalog().MustCode(code)
		scenario, err := factory(definition, Environment{Product: ProductOpenGauss, Topology: TopologyStandalone, Supported: true})
		if err != nil || scenario == nil || scenario.Code() != code {
			t.Errorf("plan factory %03d returned %#v, %v", code, scenario, err)
		}
	}
	for _, code := range []ScenarioCode{
		206, 209, 305, 341, 342, 343, 405, 507, 511, 512,
	} {
		if factories[code] != nil {
			t.Errorf("legacy/future scenario %03d is registered", code)
		}
	}
	if len(factories) != 65 {
		t.Fatalf("factory count=%d want=65", len(factories))
	}
}

func lockScenarioCodes(first, last ScenarioCode) []ScenarioCode {
	codes := make([]ScenarioCode, 0, last-first+1)
	for code := first; code <= last; code++ {
		codes = append(codes, code)
	}
	return codes
}

func TestRunnerRejectsFactoryScenarioCodeMismatch(t *testing.T) {
	catalog, err := NewScenarioCatalog([]ScenarioDefinition{{
		Code:      101,
		Name:      "catalog_name",
		Category:  CategoryCPU,
		Risk:      RiskA,
		AppliesTo: []EnvironmentClass{EnvironmentOpenGauss},
	}})
	if err != nil {
		t.Fatal(err)
	}
	scenario := testCodeScenario{
		Scenario: &fakeScenario{
			name: "factory_name", outcome: OutcomeSuccess,
		},
		code: 102,
	}
	got := NewRunner(
		&Runtime{
			Environment: Environment{
				Product: ProductOpenGauss, Topology: TopologyStandalone,
				Supported: true,
			},
			RestoreService: &runnerRestoreService{
				result: RestoreSummary{Outcome: OutcomeSuccess},
			},
		},
		catalog,
		map[ScenarioCode]ScenarioFactory{
			101: func(
				ScenarioDefinition,
				Environment,
			) (Scenario, error) {
				return scenario, nil
			},
		},
	).Run(context.Background(), []ScenarioCode{101})

	if got.Results[0].Outcome != OutcomeFailed ||
		got.Results[0].Scenario != "catalog_name" ||
		!strings.Contains(got.Results[0].Message, "returned code 102") {
		t.Fatalf("result=%+v", got.Results[0])
	}
}

func TestRunnerPreflightOrderBeforePrepare(t *testing.T) {
	var events []string
	scenario := &eventScenario{
		fakeScenario: fakeScenario{
			name: "factory_name", outcome: OutcomeSuccess,
		},
		events: &events,
	}
	catalog, err := NewScenarioCatalog([]ScenarioDefinition{{
		Code:      101,
		Name:      "tp_cpu",
		Category:  CategoryCPU,
		Risk:      RiskA,
		AppliesTo: []EnvironmentClass{EnvironmentOpenGauss},
	}})
	if err != nil {
		t.Fatal(err)
	}
	service := &runnerRestoreService{result: RestoreSummary{
		Outcome: OutcomeSuccess,
	}}
	runtime := &Runtime{
		Config: BenchConfig{
			Run:  RunConfig{ValidationEnabled: true},
			Data: DataConfig{Schema: "gsbench"},
		},
		Environment: Environment{
			Product: ProductOpenGauss, Topology: TopologyStandalone,
			Capabilities: make(CapabilitySet), Supported: true,
		},
		RestoreService: service,
		RiskPreflight: func(
			context.Context,
			ScenarioDefinition,
		) error {
			events = append(events, "risk")
			return nil
		},
		RestorePreflight: func(
			context.Context,
			ScenarioDefinition,
		) error {
			events = append(events, "restore")
			return nil
		},
		PlanPreflight: func(
			context.Context,
			string,
			[]string,
		) error {
			events = append(events, "plan")
			return nil
		},
	}
	factories := map[ScenarioCode]ScenarioFactory{
		101: func(
			ScenarioDefinition,
			Environment,
		) (Scenario, error) {
			events = append(events, "factory")
			return testCodeScenario{
				Scenario: scenario,
				code:     101,
			}, nil
		},
	}

	got := NewRunner(runtime, catalog, factories).Run(
		context.Background(),
		[]ScenarioCode{101},
	)

	if got.Outcome != OutcomeSuccess {
		t.Fatalf("summary=%+v", got)
	}
	want := []string{"risk", "factory", "plan", "prepare"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
	if got.Results[0].Scenario != "tp_cpu" {
		t.Fatalf("display name=%q", got.Results[0].Scenario)
	}
}

func TestRunnerMissingRequirementsWarnsAndContinues(t *testing.T) {
	catalog, err := NewScenarioCatalog([]ScenarioDefinition{{
		Code:      402,
		Name:      "thread_pool",
		Category:  CategoryConnectionThread,
		Risk:      RiskA,
		AppliesTo: []EnvironmentClass{EnvironmentOpenGauss},
		Requires:  []Requirement{RequirementThreadPool},
	}})
	if err != nil {
		t.Fatal(err)
	}
	riskCalled := false
	factoryCalled := false
	scenario := &fakeScenario{name: "thread_pool", outcome: OutcomeSuccess}
	got := NewRunner(
		&Runtime{
			Environment: Environment{
				Product: ProductOpenGauss, Topology: TopologyStandalone,
				Capabilities: make(CapabilitySet), Supported: true,
			},
			RiskPreflight: func(
				context.Context,
				ScenarioDefinition,
			) error {
				riskCalled = true
				return nil
			},
		},
		catalog,
		map[ScenarioCode]ScenarioFactory{
			402: func(
				ScenarioDefinition,
				Environment,
			) (Scenario, error) {
				factoryCalled = true
				return testCodeScenario{Scenario: scenario, code: 402}, nil
			},
		},
	).Run(context.Background(), []ScenarioCode{402})

	if got.Results[0].Outcome != OutcomeCompletedWithWarnings {
		t.Fatalf("result=%+v", got.Results[0])
	}
	if !riskCalled || !factoryCalled {
		t.Fatalf(
			"risk_called=%v factory_called=%v",
			riskCalled,
			factoryCalled,
		)
	}
	assertPrecheckWarning(t, got.Results[0], "requirements")
}

func assertPrecheckWarning(t *testing.T, result Result, check string) {
	t.Helper()
	for _, evidence := range result.Evidence {
		if evidence.Metric != "precheck_warning" {
			continue
		}
		if actual, ok := evidence.Details["check"].(string); ok && actual == check {
			return
		}
	}
	t.Fatalf("missing precheck warning %q in %+v", check, result.Evidence)
}

func TestRunnerPopulatesCatalogEnvironmentAndRestoreEvidence(t *testing.T) {
	catalog, err := NewScenarioCatalog([]ScenarioDefinition{{
		Code:      101,
		Name:      "tp_cpu",
		Category:  CategoryCPU,
		Risk:      RiskA,
		AppliesTo: []EnvironmentClass{EnvironmentOpenGauss},
		Requires:  []Requirement{RequirementStatementHistory},
	}})
	if err != nil {
		t.Fatal(err)
	}
	scenario := &fakeScenario{
		name:    "factory_name_must_not_escape",
		outcome: OutcomeDegraded,
	}
	service := &runnerRestoreService{result: RestoreSummary{
		RunIDs:         []string{"run-1"},
		PlannedActions: []Action{{Sequence: 1}, {Sequence: 2}},
		Outcome:        OutcomeSuccess,
	}}
	environment := Environment{
		Product: ProductOpenGauss, Topology: TopologyStandalone,
		Supported: true,
		Capabilities: CapabilitySet{
			CapabilityStatementHistory: true,
		},
		Nodes: []Node{{
			Name: "dn_1", Role: NodeRoleDNPrimary, Shard: "shard_1",
			Host: "db.internal", Port: 5432,
		}},
	}
	runtime := &Runtime{
		RunID: "run-1", Environment: environment,
		Config:         BenchConfig{Run: RunConfig{ValidationEnabled: true}},
		RestoreService: service,
	}

	got := NewRunner(
		runtime,
		catalog,
		map[ScenarioCode]ScenarioFactory{
			101: func(
				ScenarioDefinition,
				Environment,
			) (Scenario, error) {
				return testCodeScenario{
					Scenario: scenario,
					code:     101,
				}, nil
			},
		},
	).Run(context.Background(), []ScenarioCode{101})

	result := got.Results[0]
	if got.Outcome != OutcomeCompletedWithWarnings ||
		result.ScenarioCode != 101 ||
		result.Scenario != "tp_cpu" ||
		result.Category != CategoryCPU ||
		result.Product != ProductOpenGauss ||
		result.Topology != TopologyStandalone ||
		result.Strategy != "builtin_tp_cpu" ||
		result.Risk != RiskA ||
		len(result.Requirements) != 1 ||
		result.Requirements[0] != RequirementStatementHistory {
		t.Fatalf("result=%+v", result)
	}
	if len(result.Targets) != 1 ||
		result.Targets[0].Node != "dn_1" ||
		result.Targets[0].Role != NodeRoleDNPrimary ||
		result.Targets[0].Shard != "shard_1" ||
		result.Targets[0].Host != "db.internal" ||
		result.Targets[0].Port != 5432 {
		t.Fatalf("targets=%+v", result.Targets)
	}
	if result.Restore.State != "manual_recovery" ||
		result.Restore.Outcome != "" ||
		result.Restore.PlannedActions != 0 ||
		len(result.Restore.RunIDs) != 0 ||
		result.Restore.Failed {
		t.Fatalf("restore=%+v", result.Restore)
	}
	if result.StartedAt.IsZero() || result.EndedAt.IsZero() ||
		result.EndedAt.Before(result.StartedAt) {
		t.Fatalf("timestamps=%s..%s", result.StartedAt, result.EndedAt)
	}
}

func TestRunnerNeverReportsPersistentRestorePhases(
	t *testing.T,
) {
	first := &fakeScenario{name: "one", outcome: OutcomeSuccess}
	second := &fakeScenario{name: "two", outcome: OutcomeSuccess}
	service := &runnerRestoreService{result: RestoreSummary{
		RunIDs:  []string{"run-1"},
		Outcome: OutcomeRestoreFailed,
		Failed:  true,
		Err:     errors.New("verification failed"),
	}}
	var reported []string
	runtime := &Runtime{
		RunID:          "run-1",
		RestoreService: service,
		ReportPhase: func(
			_ context.Context,
			scenario string,
			phase Phase,
		) {
			if phase == PhaseRestore || phase == PhaseVerifyRestore {
				reported = append(
					reported,
					scenario+":"+string(phase),
				)
			}
		},
	}
	runner, codes := newTestRunner(
		t,
		runtime,
		[]Scenario{first, second},
	)

	got := runner.Run(context.Background(), codes)

	if len(service.requests) != 0 {
		t.Fatalf("coordinator calls=%d want=0", len(service.requests))
	}
	for _, scenario := range []*fakeScenario{first, second} {
		if len(scenario.phases) == 0 ||
			scenario.phases[len(scenario.phases)-1] != PhaseStop {
			t.Fatalf(
				"scenario %s phases=%v",
				scenario.name,
				scenario.phases,
			)
		}
	}
	var wantReported []string
	if !reflect.DeepEqual(reported, wantReported) {
		t.Fatalf("reported=%v want=%v", reported, wantReported)
	}
	if got.Outcome != OutcomeSuccess {
		t.Fatalf("summary=%+v", got)
	}
	for _, result := range got.Results {
		if result.Outcome != OutcomeSuccess ||
			result.Restore.State != "manual_recovery" ||
			result.Restore.Failed {
			t.Fatalf("result=%+v", result)
		}
	}
}

func TestRunnerRiskCRestorePreflightRequiresProviderAndLedger(
	t *testing.T,
) {
	catalog, err := NewScenarioCatalog([]ScenarioDefinition{{
		Code:      343,
		Name:      "network_partition",
		Category:  CategoryIONetwork,
		Risk:      RiskC,
		AppliesTo: []EnvironmentClass{EnvironmentOpenGauss},
		Requires:  []Requirement{RequirementExternalFaultProvider},
	}})
	if err != nil {
		t.Fatal(err)
	}
	factoryCalled := false
	runtime := &Runtime{
		Config: BenchConfig{Safety: SafetyConfig{
			AllowInfrastructureFault: true,
		}},
		Environment: Environment{
			Product: ProductOpenGauss, Topology: TopologyStandalone,
			Supported: true,
			Capabilities: CapabilitySet{
				CapabilityExternalFaultProvider: true,
			},
		},
		AllowRisk: RiskC,
		RestoreService: &runnerRestoreService{
			result: RestoreSummary{Outcome: OutcomeSuccess},
		},
	}

	got := NewRunner(
		runtime,
		catalog,
		map[ScenarioCode]ScenarioFactory{
			343: func(
				ScenarioDefinition,
				Environment,
			) (Scenario, error) {
				factoryCalled = true
				return testCodeScenario{
					Scenario: &fakeScenario{
						name:    "network_partition",
						outcome: OutcomeSuccess,
					},
					code: 343,
				}, nil
			},
		},
	).Run(context.Background(), []ScenarioCode{343})

	if got.Results[0].Outcome != OutcomeFailed ||
		!strings.Contains(
			got.Results[0].Message,
			"fault provider and recovery ledger",
		) {
		t.Fatalf("result=%+v", got.Results[0])
	}
	if factoryCalled {
		t.Fatal("factory ran before restore readiness was proven")
	}
}
