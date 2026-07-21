package gsbench

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Runtime struct {
	Config       BenchConfig
	Database     *Database
	Capabilities Capabilities
	Journal      *Journal
	Log          *RunLog
	RunID        string
	CPU          CPUSampler
	ReportPhase  func(context.Context, string, Phase)
}

type Scenario interface {
	Name() string
	Prepare(context.Context, *Runtime) error
	Ramp(context.Context, *Runtime) error
	Hold(context.Context, *Runtime) error
	Verify(context.Context, *Runtime) (Result, error)
	Stop(context.Context, *Runtime) error
	Restore(context.Context, *Runtime) error
}

type RunSummary struct {
	RunID   string
	Outcome Outcome
	Results []Result
}

type Runner struct {
	runtime  *Runtime
	registry map[string]Scenario
}

func NewRunner(runtime *Runtime, scenarios []Scenario) *Runner {
	registry := make(map[string]Scenario, len(scenarios))
	for _, scenario := range scenarios {
		registry[scenario.Name()] = scenario
	}
	return &Runner{runtime: runtime, registry: registry}
}

func (r *Runner) Run(ctx context.Context, names []string) RunSummary {
	results := make([]Result, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		i, name := i, name
		scenario := r.registry[name]
		if scenario == nil {
			results[i] = Result{Scenario: name, Outcome: OutcomeFailed, Message: "scenario not registered"}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = r.runOne(ctx, scenario)
		}()
	}
	wg.Wait()
	summary := RunSummary{RunID: r.runtime.RunID, Outcome: OutcomeSuccess, Results: results}
	for _, result := range results {
		summary.Outcome = worseOutcome(summary.Outcome, result.Outcome)
	}
	return summary
}

func (r *Runner) runOne(ctx context.Context, scenario Scenario) Result {
	startedAt := time.Now()
	result := Result{Scenario: scenario.Name(), Outcome: OutcomeSuccess, StartedAt: startedAt}
	fail := func(phase Phase, err error) {
		result.Outcome = OutcomeFailed
		result.Message = fmt.Sprintf("%s: %v", phase, err)
	}
	report := func(phase Phase) {
		if r.runtime.ReportPhase != nil {
			r.runtime.ReportPhase(ctx, scenario.Name(), phase)
		}
	}
	report(PhasePrepare)
	if err := scenario.Prepare(ctx, r.runtime); err != nil {
		fail(PhasePrepare, err)
	} else {
		report(PhaseRamp)
		if err := scenario.Ramp(ctx, r.runtime); err != nil {
			fail(PhaseRamp, err)
		} else {
			report(PhaseHold)
			if err := scenario.Hold(ctx, r.runtime); err != nil {
				fail(PhaseHold, err)
			} else {
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
	report(PhaseStop)
	if err := scenario.Stop(cleanupCtx, r.runtime); err != nil {
		fail(PhaseStop, err)
	}
	report(PhaseRestore)
	if err := scenario.Restore(cleanupCtx, r.runtime); err != nil {
		fail(PhaseRestore, err)
	}
	result.Scenario = scenario.Name()
	result.EndedAt = time.Now()
	return result
}

func worseOutcome(a, b Outcome) Outcome {
	rank := map[Outcome]int{OutcomeSuccess: 0, OutcomeDegraded: 1, OutcomeFailed: 2, "": 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
