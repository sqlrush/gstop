package gsbench

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"
)

type Runtime struct {
	Config           BenchConfig
	Database         *Database
	Capabilities     Capabilities
	Environment      Environment
	Catalog          *ScenarioCatalog
	Provider         FaultProvider
	Ledger           RecoveryLedger
	Journal          *Journal
	Log              *RunLog
	RunID            string
	CPU              CPUSampler
	ReportPhase      func(context.Context, string, Phase)
	PlanPreflight    func(context.Context, string, []string) error
	RiskPreflight    func(context.Context, ScenarioDefinition) error
	RestorePreflight func(
		context.Context,
		ScenarioDefinition,
	) error
	AllowRisk      RiskLevel
	RestoreService restoreService
}

type Scenario interface {
	Code() ScenarioCode
	Name() string
	Prepare(context.Context, *Runtime) error
	Ramp(context.Context, *Runtime) error
	Hold(context.Context, *Runtime) error
	Verify(context.Context, *Runtime) (Result, error)
	Stop(context.Context, *Runtime) error
	Restore(context.Context, *Runtime) error
}

type ScenarioFactory func(
	ScenarioDefinition,
	Environment,
) (Scenario, error)

type ScenarioStrategy interface {
	Strategy() string
}

type RunSummary struct {
	RunID   string
	Outcome Outcome
	Results []Result
}

type Runner struct {
	runtime   *Runtime
	catalog   *ScenarioCatalog
	factories map[ScenarioCode]ScenarioFactory
}

type workloadBarrier struct {
	ready chan struct{}
	ctx   context.Context
}

func NewRunner(
	runtime *Runtime,
	catalog *ScenarioCatalog,
	factories map[ScenarioCode]ScenarioFactory,
) *Runner {
	if runtime == nil {
		runtime = &Runtime{}
	}
	runtime.Catalog = catalog
	registered := make(map[ScenarioCode]ScenarioFactory, len(factories))
	for code, factory := range factories {
		registered[code] = factory
	}
	return &Runner{
		runtime: runtime, catalog: catalog, factories: registered,
	}
}

func DefaultScenarioFactories() map[ScenarioCode]ScenarioFactory {
	return map[ScenarioCode]ScenarioFactory{
		101: func(
			ScenarioDefinition,
			Environment,
		) (Scenario, error) {
			return NewTPScenario(), nil
		},
		102: func(
			ScenarioDefinition,
			Environment,
		) (Scenario, error) {
			return NewAPScenario(), nil
		},
		103: func(
			ScenarioDefinition,
			Environment,
		) (Scenario, error) {
			return NewMixedScenario(), nil
		},
		401: func(
			ScenarioDefinition,
			Environment,
		) (Scenario, error) {
			return NewConnectionScenario(), nil
		},
		402: func(
			ScenarioDefinition,
			Environment,
		) (Scenario, error) {
			return NewThreadScenario(), nil
		},
		801: func(
			ScenarioDefinition,
			Environment,
		) (Scenario, error) {
			return NewVacuumScenario(), nil
		},
	}
}

func (r *Runner) Run(ctx context.Context, codes []ScenarioCode) RunSummary {
	results := make([]Result, len(codes))
	constructed := make([]bool, len(codes))
	var wg sync.WaitGroup
	var prepareWG sync.WaitGroup
	barrier := &workloadBarrier{ready: make(chan struct{})}
	for i, code := range codes {
		i, code := i, code
		definition, err := r.lookupDefinition(code)
		if err != nil {
			results[i] = terminalResult(
				code,
				"",
				OutcomeFailed,
				err.Error(),
			)
			continue
		}
		if !r.runtime.Environment.Applicable(definition) {
			results[i] = catalogTerminalResult(
				definition,
				r.runtime.Environment,
				OutcomeNotApplicable,
				"scenario does not apply to the detected environment",
				"not_applicable",
			)
			continue
		}
		factory := r.factories[code]
		if factory == nil {
			results[i] = catalogTerminalResult(
				definition,
				r.runtime.Environment,
				OutcomeNotImplemented,
				"scenario factory is not implemented",
				"not_implemented",
			)
			continue
		}
		wg.Add(1)
		prepareWG.Add(1)
		go func() {
			defer wg.Done()
			results[i], constructed[i] = r.runOne(
				ctx,
				barrier,
				prepareWG.Done,
				definition,
				factory,
			)
		}()
	}
	prepareWG.Wait()
	workloadCtx := ctx
	var cancelWorkload context.CancelFunc
	if duration := r.runtime.Config.Run.Duration; duration > 0 {
		workloadCtx, cancelWorkload = context.WithTimeout(ctx, duration)
		defer cancelWorkload()
	}
	barrier.ctx = workloadCtx
	close(barrier.ready)
	wg.Wait()
	summary := RunSummary{RunID: r.runtime.RunID, Outcome: OutcomeSuccess, Results: results}
	for _, result := range results {
		summary.Outcome = worseOutcome(summary.Outcome, result.Outcome)
	}
	if !restoreServiceIsNil(r.runtime.RestoreService) {
		restoreCtx := context.WithoutCancel(ctx)
		for index, result := range results {
			if constructed[index] {
				r.reportPhase(
					restoreCtx,
					result.Scenario,
					PhaseRestore,
				)
			}
		}
		restored := r.runtime.RestoreService.Restore(
			restoreCtx,
			RestoreRequest{
				RunID:            r.runtime.RunID,
				completedOutcome: summary.Outcome,
			},
		)
		restoreEvidence := evidenceForRestoreSummary(restored)
		for index := range summary.Results {
			summary.Results[index].Restore = restoreEvidence
			summary.Results[index].EndedAt = time.Now()
		}
		for index, result := range summary.Results {
			if constructed[index] {
				r.reportPhase(
					restoreCtx,
					result.Scenario,
					PhaseVerifyRestore,
				)
			}
		}
		if restored.Failed {
			summary.Outcome = OutcomeRestoreFailed
			for index := range summary.Results {
				summary.Results[index].Outcome = OutcomeRestoreFailed
			}
		}
	}
	return summary
}

func (r *Runner) reportPhase(
	ctx context.Context,
	scenario string,
	phase Phase,
) {
	if r != nil &&
		r.runtime != nil &&
		r.runtime.ReportPhase != nil {
		r.runtime.ReportPhase(ctx, scenario, phase)
	}
}

func (r *Runner) lookupDefinition(
	code ScenarioCode,
) (ScenarioDefinition, error) {
	if r == nil || r.catalog == nil {
		return ScenarioDefinition{}, fmt.Errorf(
			"scenario catalog is unavailable",
		)
	}
	return r.catalog.LookupCode(code)
}

func terminalResult(
	code ScenarioCode,
	name string,
	outcome Outcome,
	message string,
) Result {
	now := time.Now()
	return Result{
		ScenarioCode: code,
		Scenario:     name,
		Outcome:      outcome,
		Message:      message,
		StartedAt:    now,
		EndedAt:      now,
	}
}

func catalogTerminalResult(
	definition ScenarioDefinition,
	environment Environment,
	outcome Outcome,
	message string,
	strategy string,
) Result {
	result := terminalResult(
		definition.Code,
		definition.Name,
		outcome,
		message,
	)
	return enrichResult(
		result,
		definition,
		environment,
		strategy,
	)
}

func enrichResult(
	result Result,
	definition ScenarioDefinition,
	environment Environment,
	strategy string,
) Result {
	result.ScenarioCode = definition.Code
	result.Scenario = definition.Name
	result.Category = definition.Category
	result.Product = environment.Product
	result.Topology = environment.Topology
	result.Strategy = strategy
	result.Risk = definition.Risk
	result.Requirements = append(
		[]Requirement{},
		definition.Requires...,
	)
	if result.Targets == nil {
		result.Targets = targetsForEnvironment(environment)
	}
	return result
}

func targetsForEnvironment(
	environment Environment,
) []ScenarioTarget {
	if environment.Nodes == nil {
		return []ScenarioTarget{}
	}
	targets := make([]ScenarioTarget, len(environment.Nodes))
	for index, node := range environment.Nodes {
		targets[index] = ScenarioTarget{
			Node: node.Name, Role: node.Role, Shard: node.Shard,
			Host: node.Host, Port: node.Port,
		}
	}
	return targets
}

func evidenceForRestoreSummary(
	summary RestoreSummary,
) RestoreEvidence {
	state := "restored"
	if summary.Failed {
		state = "restore_failed"
	}
	var detail string
	if summary.Err != nil {
		detail = summary.Err.Error()
	}
	return RestoreEvidence{
		State:          state,
		Outcome:        summary.Outcome,
		RunIDs:         append([]string{}, summary.RunIDs...),
		PlannedActions: len(summary.PlannedActions),
		Failed:         summary.Failed,
		Error:          detail,
	}
}

func (r *Runner) runOne(
	ctx context.Context,
	barrier *workloadBarrier,
	prepared func(),
	definition ScenarioDefinition,
	factory ScenarioFactory,
) (Result, bool) {
	startedAt := time.Now()
	result := Result{
		ScenarioCode: definition.Code,
		Scenario:     definition.Name,
		Outcome:      OutcomeSuccess,
		StartedAt:    startedAt,
	}
	fail := func(phase Phase, err error) {
		result.Outcome = OutcomeFailed
		result.Message = fmt.Sprintf("%s: %v", phase, err)
	}
	report := func(phase Phase) {
		if r.runtime.ReportPhase != nil {
			r.runtime.ReportPhase(ctx, definition.Name, phase)
		}
	}
	report(PhasePreflight)
	var prepareErr error
	failurePhase := PhasePreflight
	missing := r.runtime.Environment.Missing(definition.Requires)
	if len(missing) != 0 &&
		!definitionHasFallback(definition, missing) {
		prepareErr = fmt.Errorf(
			"missing requirements: %s",
			joinRequirements(missing),
		)
	}
	if prepareErr == nil {
		if r.runtime.RiskPreflight != nil {
			prepareErr = r.runtime.RiskPreflight(ctx, definition)
		} else {
			prepareErr = AuthorizeScenario(
				definition,
				r.runtime.Config,
				CLIOptions{AllowRisk: r.runtime.AllowRisk},
				r.runtime.Environment,
			)
		}
	}
	if prepareErr == nil {
		if r.runtime.RestorePreflight != nil {
			prepareErr = r.runtime.RestorePreflight(ctx, definition)
		} else if restoreServiceIsNil(r.runtime.RestoreService) {
			prepareErr = fmt.Errorf(
				"restore coordinator is unavailable",
			)
		} else if definition.Risk == RiskC &&
			(faultProviderIsNil(r.runtime.Provider) ||
				r.runtime.Ledger == nil) {
			prepareErr = fmt.Errorf(
				"fault provider and recovery ledger are unavailable",
			)
		}
	}
	var scenario Scenario
	if prepareErr == nil {
		scenario, prepareErr = factory(
			definition,
			r.runtime.Environment,
		)
		switch {
		case prepareErr != nil:
			scenario = nil
			prepareErr = fmt.Errorf(
				"construct scenario: %w",
				prepareErr,
			)
		case scenario == nil:
			prepareErr = fmt.Errorf(
				"scenario factory returned no scenario",
			)
		case scenario.Code() != definition.Code:
			returnedCode := scenario.Code()
			scenario = nil
			prepareErr = fmt.Errorf(
				"scenario factory returned code %03d",
				returnedCode,
			)
		}
	}
	if prepareErr == nil && r.runtime.PlanPreflight != nil {
		var statements []string
		statements, prepareErr = ScenarioWorkloadStatements(
			r.runtime,
			definition.Name,
		)
		if prepareErr == nil {
			prepareErr = r.runtime.PlanPreflight(
				ctx,
				definition.Name,
				statements,
			)
		}
	}
	if prepareErr == nil {
		report(PhasePrepare)
		prepareErr = scenario.Prepare(ctx, r.runtime)
		failurePhase = PhasePrepare
	}
	prepared()
	<-barrier.ready
	workloadCtx := barrier.ctx
	if prepareErr != nil {
		fail(failurePhase, prepareErr)
	} else {
		verify := false
		report(PhaseRamp)
		if err := scenario.Ramp(workloadCtx, r.runtime); err != nil {
			if runDurationElapsed(ctx, workloadCtx, err) {
				verify = true
			} else {
				fail(PhaseRamp, err)
			}
		} else {
			report(PhaseHold)
			if err := scenario.Hold(workloadCtx, r.runtime); err != nil {
				if runDurationElapsed(ctx, workloadCtx, err) {
					verify = true
				} else {
					fail(PhaseHold, err)
				}
			} else {
				verify = true
			}
		}
		if verify {
			report(PhaseVerify)
			verified, err := scenario.Verify(ctx, r.runtime)
			if err != nil {
				fail(PhaseVerify, err)
			} else {
				result = verified
				result.StartedAt = startedAt
			}
		}
	}
	cleanupTimeout := r.runtime.Config.Safety.QueryTimeout
	if cleanupTimeout <= 0 {
		cleanupTimeout = 30 * time.Second
	}
	if r.runtime.Config.Safety.AllowDatabaseRestart && cleanupTimeout < 3*time.Minute {
		cleanupTimeout = 3 * time.Minute
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if scenario != nil {
		report(PhaseStop)
		if err := scenario.Stop(cleanupCtx, r.runtime); err != nil {
			fail(PhaseStop, err)
		}
	}
	strategy := "catalog_preflight"
	if scenario != nil {
		strategy = "builtin_" + definition.Name
		if reporter, ok := scenario.(ScenarioStrategy); ok {
			if reported := reporter.Strategy(); reported != "" {
				strategy = reported
			}
		}
	}
	result = enrichResult(
		result,
		definition,
		r.runtime.Environment,
		strategy,
	)
	result.EndedAt = time.Now()
	return result, scenario != nil
}

func restoreServiceIsNil(service restoreService) bool {
	if service == nil {
		return true
	}
	value := reflect.ValueOf(service)
	switch value.Kind() {
	case reflect.Chan,
		reflect.Func,
		reflect.Interface,
		reflect.Map,
		reflect.Pointer,
		reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func runDurationElapsed(parentCtx, workloadCtx context.Context, err error) bool {
	return parentCtx.Err() == nil &&
		errors.Is(workloadCtx.Err(), context.DeadlineExceeded) &&
		errors.Is(err, context.DeadlineExceeded)
}

func worseOutcome(a, b Outcome) Outcome {
	if outcomeRank[b] > outcomeRank[a] {
		return b
	}
	return a
}
