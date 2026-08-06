# gsbench 401/402 Pool Target Percentage Implementation Plan

> **For implementation:** REQUIRED SUB-SKILL: Use superpowers:executing-plans and execute inline task-by-task. Use subagents only if the user explicitly requests delegation. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `gsbench run --percent N` for scenarios 401/402 so each scenario adds only the pressure required above its pre-run baseline, freezes after reaching the target, cleans up on every exit path, and safely allows action-free pool-only stale recovery failures to warn instead of blocking later tests.

**Architecture:** Parse one shared CLI percentage into typed connection/thread target configuration, then let 401 calculate a fixed connection delta and 402 ramp real thread-pool utilization to a minimum threshold before freezing workers. Keep Runner cleanup and failure isolation intact. Extend restore discovery with strict stored scenario identities, while leaving the RestoreCoordinator fail-closed and making the narrow pool-only continuation decision only in pre-run orchestration.

**Tech Stack:** Go, standard `flag`, `context`, `database/sql`, existing gsbench Runner/Controller/RestoreCoordinator, and Go's `testing` package.

## Global Constraints

- `--percent N` is valid only for `run`, accepts integers `1`–`100`, and requires at least one 401/402 scenario after CLI/config scenario resolution.
- One CLI value applies to every selected 401/402; unrelated selected scenarios keep their own configuration.
- A pool target must be strictly greater than its pre-run baseline.
- Target reachability is mandatory even when `run.validation_enabled=false`.
- Reaching the target stops further pressure increases; frozen workers/connections remain until the shared `--duration` ends.
- 402 uses real `dbe_perf.global_threadpool_status` evidence and never promotes active-backend fallback to a percentage result.
- Normal completion, failure, and context cancellation release all resources created by 401/402.
- One scenario failure does not cancel other scenarios in the same Runner.
- Only stale runs proven to contain non-empty 401/402-only scenario lists and zero pending actions may continue after recovery failure.
- Any unknown run identity, non-pool scenario, local/database action, or 402 instance-parameter mutation keeps stale recovery fail-closed.
- Keep the existing one-SIGINT immediate operating-system termination behavior.
- Keep version `v1.1.7`; do not change other scenario targets or concurrency models.

---

### Task 1: Add the CLI percentage and typed pool target configuration

**Files:**
- Modify: `internal/gsbench/cli.go:35-65,120-360,550-590`
- Modify: `internal/gsbench/cli_test.go:230-590`
- Modify: `internal/gsbench/config.go:20-85,200-335,540-650`
- Modify: `internal/gsbench/config_test.go:250-420,600-720`
- Modify: `internal/gsbench/app.go:45-70`

**Interfaces:**
- Produces: `CLIOptions.PoolPercent int`, `Overrides.PoolPercent int`, `PoolTargetConfig{ConnectionPercent, ThreadPercent int}`, and `BenchConfig.PoolTargets PoolTargetConfig`.
- Produces: `validatePoolPercentOverride(codes []ScenarioCode, percent int) error` and `applyPoolTargetOverride(cfg *BenchConfig, overrides Overrides) error`.
- Consumes: selected `ScenarioCode` values and existing `scenario.connection_pool.target_percent` / `scenario.thread_pool.target_percent` keys.

- [ ] **Step 1: Write failing CLI tests for accepted and rejected percentages**

Add to `internal/gsbench/cli_test.go`:

```go
func TestParseCLIArgsSupportsPoolTargetPercent(t *testing.T) {
	for _, test := range []struct {
		args  []string
		codes []ScenarioCode
		want  int
	}{
		{[]string{"run", "401", "--percent", "90"}, []ScenarioCode{401}, 90},
		{[]string{"run", "402", "--percent=100"}, []ScenarioCode{402}, 100},
		{[]string{"run", "301,401,402", "--percent", "1"}, []ScenarioCode{301, 401, 402}, 1},
		{[]string{"run", "--percent", "90"}, nil, 90},
	} {
		options, err := ParseCLIArgs(test.args)
		if err != nil {
			t.Fatalf("ParseCLIArgs(%v): %v", test.args, err)
		}
		if options.PoolPercent != test.want || !reflect.DeepEqual(options.ScenarioCodes, test.codes) {
			t.Fatalf("ParseCLIArgs(%v)=%+v", test.args, options)
		}
	}
}

func TestParseCLIArgsRejectsInvalidPoolTargetPercent(t *testing.T) {
	for _, args := range [][]string{
		{"run", "401", "--percent", "0"},
		{"run", "401", "--percent", "-1"},
		{"run", "402", "--percent", "101"},
		{"run", "401", "--percent", "1.5"},
		{"run", "301", "--percent", "90"},
		{"doctor", "--scenario", "401", "--percent", "90"},
	} {
		if _, err := ParseCLIArgs(args); err == nil {
			t.Fatalf("ParseCLIArgs(%v) accepted invalid pool target", args)
		}
	}
}

func TestCLIHelpDocumentsPoolTargetPercent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := RunCLI(context.Background(), []string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	for _, token := range []string{
		"--percent N", "gsbench run 401 --percent", "gsbench run 402 --percent",
	} {
		if !strings.Contains(stdout.String(), token) {
			t.Errorf("help missing %q:\n%s", token, stdout.String())
		}
	}
}
```

- [ ] **Step 2: Run the focused CLI tests and verify RED**

```bash
go test ./internal/gsbench -run 'Test(ParseCLIArgsSupportsPoolTargetPercent|ParseCLIArgsRejectsInvalidPoolTargetPercent|CLIHelpDocumentsPoolTargetPercent)$' -count=1
```

Expected: FAIL because `CLIOptions.PoolPercent` and `--percent` do not exist.

- [ ] **Step 3: Implement CLI parsing and compatibility validation**

Add `PoolPercent int` to `CLIOptions`. Register the flag:

```go
flags.IntVar(&options.PoolPercent, "percent", 0, "target percent for scenarios 401/402 (1-100)")
```

Track explicit use in `flags.Visit`:

```go
percentSet := false
// inside Visit:
case "percent":
	percentSet = true
```

Reject non-run and range errors before resolving scenarios, then validate applicability after `options.ScenarioCodes` is populated:

```go
if percentSet && command != "run" {
	return CLIOptions{}, fmt.Errorf("--percent is only valid with run")
}
if percentSet && (options.PoolPercent < 1 || options.PoolPercent > 100) {
	return CLIOptions{}, fmt.Errorf("--percent must be between 1 and 100")
}
// after scenario resolution; when no CLI scenarios were supplied, LoadConfig
// validates applicability against the scenarios selected by the config file.
if percentSet && len(options.ScenarioCodes) > 0 {
	if err := validatePoolPercentOverride(options.ScenarioCodes, options.PoolPercent); err != nil {
		return CLIOptions{}, err
	}
}
```

Implement:

```go
func validatePoolPercentOverride(codes []ScenarioCode, percent int) error {
	if percent < 1 || percent > 100 {
		return fmt.Errorf("pool target percent must be between 1 and 100")
	}
	for _, code := range codes {
		if code == 401 || code == 402 {
			return nil
		}
	}
	return fmt.Errorf("--percent requires scenario 401 or 402")
}
```

Document the new command examples and flag in `usageText()`.

- [ ] **Step 4: Run CLI tests and verify GREEN**

```bash
gofmt -w internal/gsbench/cli.go internal/gsbench/cli_test.go
go test ./internal/gsbench -run 'Test(ParseCLIArgsSupportsPoolTargetPercent|ParseCLIArgsRejectsInvalidPoolTargetPercent|CLIHelpDocumentsPoolTargetPercent)$' -count=1
```

Expected: PASS.

- [ ] **Step 5: Write failing configuration tests for defaults, overrides, and bounds**

Add to `internal/gsbench/config_test.go`:

```go
func TestConfigLoadsAndOverridesPoolTargets(t *testing.T) {
	body := minimalConfig() + `
[scenario.connection_pool]
target_percent = 81
[scenario.thread_pool]
target_percent = 82
`
	path := writeTestConfig(t, body)
	cfg, err := LoadConfig(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PoolTargets.ConnectionPercent != 81 || cfg.PoolTargets.ThreadPercent != 82 {
		t.Fatalf("pool targets=%+v", cfg.PoolTargets)
	}

	cfg, err = LoadConfig(path, Overrides{
		ScenarioCodes: []ScenarioCode{301, 401, 402}, PoolPercent: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PoolTargets.ConnectionPercent != 90 || cfg.PoolTargets.ThreadPercent != 90 {
		t.Fatalf("overridden pool targets=%+v", cfg.PoolTargets)
	}
}

func TestConfigPoolOverrideLeavesUnselectedPoolUntouched(t *testing.T) {
	body := minimalConfig() + `
[scenario.connection_pool]
target_percent = 81
[scenario.thread_pool]
target_percent = 82
`
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{
		ScenarioCodes: []ScenarioCode{301, 401}, PoolPercent: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PoolTargets.ConnectionPercent != 90 || cfg.PoolTargets.ThreadPercent != 82 {
		t.Fatalf("pool targets=%+v", cfg.PoolTargets)
	}
}

func TestConfigPoolOverrideUsesConfiguredScenarios(t *testing.T) {
	body := strings.Replace(
		minimalConfig(), "scenarios = tp_cpu", "scenarios = connection_pool", 1,
	)
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{PoolPercent: 90})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PoolTargets.ConnectionPercent != 90 ||
		cfg.PoolTargets.ThreadPercent != 95 {
		t.Fatalf("pool targets=%+v", cfg.PoolTargets)
	}
}

func TestConfigRejectsPoolOverrideWithoutPoolScenario(t *testing.T) {
	if _, err := LoadConfig(
		writeTestConfig(t, minimalConfig()), Overrides{PoolPercent: 90},
	); err == nil {
		t.Fatal("pool override without 401/402 was accepted")
	}
}

func TestConfigRejectsOutOfRangePoolTargets(t *testing.T) {
	for _, section := range []string{
		"[scenario.connection_pool]\ntarget_percent = 0\n",
		"[scenario.thread_pool]\ntarget_percent = 101\n",
	} {
		if _, err := LoadConfig(writeTestConfig(t, minimalConfig()+"\n"+section), Overrides{}); err == nil {
			t.Fatalf("accepted invalid pool target %q", section)
		}
	}
}
```

- [ ] **Step 6: Run configuration tests and verify RED**

```bash
go test ./internal/gsbench -run 'TestConfig(LoadsAndOverridesPoolTargets|PoolOverrideLeavesUnselectedPoolUntouched|PoolOverrideUsesConfiguredScenarios|RejectsPoolOverrideWithoutPoolScenario|RejectsOutOfRangePoolTargets)$' -count=1
```

Expected: FAIL because typed pool target configuration does not exist.

- [ ] **Step 7: Implement typed pool target loading and override propagation**

Add:

```go
type PoolTargetConfig struct {
	ConnectionPercent int
	ThreadPercent     int
}
```

Add `PoolPercent int` to `Overrides` and `PoolTargets PoolTargetConfig` to `BenchConfig`. Load defaults in `LoadConfig`:

```go
PoolTargets: PoolTargetConfig{
	ConnectionPercent: raw.GetInt("scenario.connection_pool.target_percent", 95),
	ThreadPercent:     raw.GetInt("scenario.thread_pool.target_percent", 95),
},
```

After scenario overrides, call:

```go
if err := applyPoolTargetOverride(&cfg, overrides); err != nil {
	return BenchConfig{}, err
}
```

Implement:

```go
func applyPoolTargetOverride(cfg *BenchConfig, overrides Overrides) error {
	if overrides.PoolPercent == 0 {
		return nil
	}
	if err := validatePoolPercentOverride(cfg.Run.ScenarioCodes, overrides.PoolPercent); err != nil {
		return err
	}
	for _, code := range cfg.Run.ScenarioCodes {
		switch code {
		case 401:
			cfg.PoolTargets.ConnectionPercent = overrides.PoolPercent
		case 402:
			cfg.PoolTargets.ThreadPercent = overrides.PoolPercent
		}
	}
	return nil
}
```

In `BenchConfig.Validate`, require both typed percentages to be within 1–100. Propagate the CLI field from `configOverridesFromCLI`:

```go
PoolPercent: options.PoolPercent,
```

- [ ] **Step 8: Run config and CLI package tests**

```bash
gofmt -w internal/gsbench/config.go internal/gsbench/config_test.go internal/gsbench/app.go
go test ./internal/gsbench -run 'Test(ParseCLIArgs.*PoolTargetPercent|Config.*PoolTarget)' -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit the user-facing parameter and typed configuration**

```bash
git add internal/gsbench/cli.go internal/gsbench/cli_test.go internal/gsbench/config.go internal/gsbench/config_test.go internal/gsbench/app.go
git commit -m "feat(gsbench): add pool target percentage override"
```

---

### Task 2: Make scenario 401 inject a fixed baseline delta and freeze

**Files:**
- Modify: `internal/gsbench/scenario_connections.go:15-370`
- Modify: `internal/gsbench/scenario_capacity_test.go:1-65`
- Modify: `internal/gsbench/scenario_connections_test.go:1-125`

**Interfaces:**
- Consumes: `BenchConfig.PoolTargets.ConnectionPercent`.
- Produces: `ConnectionBudget.BaselinePercent float64`, strict baseline/reachability errors, fixed `WorkloadTarget`, and hold-time tagged-session integrity checks without top-up.

- [ ] **Step 1: Write failing budget tests for delta, equal/lower baseline, and safety ceiling**

Extend `internal/gsbench/scenario_capacity_test.go`:

```go
func TestConnectionBudgetInjectsOnlyBaselineDelta(t *testing.T) {
	budget, err := calculateConnectionBudget(103, 3, 80, 90, 100)
	if err != nil {
		t.Fatal(err)
	}
	if budget.UsableCapacity != 100 || budget.DesiredTotal != 90 ||
		budget.WorkloadTarget != 10 || budget.BaselinePercent != 80 {
		t.Fatalf("budget=%+v", budget)
	}
}

func TestConnectionBudgetRejectsTargetAtOrBelowBaseline(t *testing.T) {
	for _, target := range []int{79, 80} {
		if _, err := calculateConnectionBudget(103, 3, 80, target, 100); err == nil {
			t.Fatalf("target %d accepted at 80%% baseline", target)
		}
	}
}

func TestConnectionScenarioRejectsUnreachableBudgetBeforeRamp(t *testing.T) {
	budget, err := calculateConnectionBudget(103, 3, 80, 90, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !budget.Limited || budget.WorkloadTarget != 5 {
		t.Fatalf("budget=%+v", budget)
	}
	if err := validateConnectionBudget(budget); err == nil {
		t.Fatal("unreachable connection budget was accepted")
	}
}
```

- [ ] **Step 2: Run the budget tests and verify RED**

```bash
go test ./internal/gsbench -run 'TestConnection(BudgetInjectsOnlyBaselineDelta|BudgetRejectsTargetAtOrBelowBaseline|ScenarioRejectsUnreachableBudgetBeforeRamp)$' -count=1
```

Expected: FAIL because `BaselinePercent` and `validateConnectionBudget` do not exist and equal/lower targets are currently accepted.

- [ ] **Step 3: Implement strict connection budget validation**

Add `BaselinePercent float64` to `ConnectionBudget`. In `calculateConnectionBudget` calculate:

```go
baselinePercent := float64(existing) / float64(usable) * 100
if float64(targetPercent) <= baselinePercent {
	return ConnectionBudget{}, fmt.Errorf(
		"connection target %.1f%% must be above baseline %.1f%%",
		float64(targetPercent), baselinePercent,
	)
}
```

Store the field in the returned budget and implement:

```go
func validateConnectionBudget(budget ConnectionBudget) error {
	if budget.Limited || budget.ReachableTotal < budget.DesiredTotal {
		return fmt.Errorf(
			"connection target is unreachable: target=%d reachable=%d ceiling=%.1f%%",
			budget.DesiredTotal, budget.ReachableTotal, budget.CeilingPercent,
		)
	}
	return nil
}
```

In `ConnectionScenario.Prepare`, read `rt.Config.PoolTargets.ConnectionPercent`, calculate the budget, then call `validateConnectionBudget` before creating any connection.

- [ ] **Step 4: Run budget tests and verify GREEN**

```bash
gofmt -w internal/gsbench/scenario_connections.go internal/gsbench/scenario_capacity_test.go
go test ./internal/gsbench -run 'TestConnection(BudgetInjectsOnlyBaselineDelta|BudgetRejectsTargetAtOrBelowBaseline|ScenarioRejectsUnreachableBudgetBeforeRamp)$' -count=1
```

Expected: PASS.

- [ ] **Step 5: Write failing lifecycle tests for frozen injection and lost tagged sessions**

Refactor the observable hold decision into a pure helper and test it in `scenario_connections_test.go`:

```go
func TestConnectionFrozenSampleNeverRequestsTopUp(t *testing.T) {
	scenario := &ConnectionScenario{budget: ConnectionBudget{
		DesiredTotal: 90, WorkloadTarget: 10,
	}}
	if err := scenario.acceptRampSample(90, 10); err != nil {
		t.Fatal(err)
	}
	if scenario.liveTagged != 10 {
		t.Fatalf("live tagged=%d", scenario.liveTagged)
	}
	if err := scenario.acceptFrozenSample(75, 10); err != nil {
		t.Fatalf("external connection loss changed frozen injection: %v", err)
	}
	if !scenario.targetReached {
		t.Fatal("a later external loss erased successful target evidence")
	}
}

func TestConnectionFrozenSampleFailsWhenInjectedSessionIsLost(t *testing.T) {
	scenario := &ConnectionScenario{budget: ConnectionBudget{WorkloadTarget: 10}}
	if err := scenario.acceptFrozenSample(89, 9); err == nil {
		t.Fatal("lost tagged session was accepted")
	}
}

func TestConnectionRampSampleMustReachTargetOnce(t *testing.T) {
	scenario := &ConnectionScenario{budget: ConnectionBudget{
		DesiredTotal: 90, WorkloadTarget: 10,
	}}
	if err := scenario.acceptRampSample(90, 10); err != nil {
		t.Fatal(err)
	}
	if err := scenario.acceptRampSample(89, 10); err == nil {
		t.Fatal("ramp accepted an unreached total target")
	}
}

func TestConnectionRampDeadlineRemainsFailureWithoutWrappingDeadline(t *testing.T) {
	err := connectionTargetRampError(context.DeadlineExceeded, 7, 10)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline target error=%v", err)
	}
}
```

- [ ] **Step 6: Run lifecycle tests and verify RED**

```bash
go test ./internal/gsbench -run 'TestConnection(FrozenSample|RampSample|RampDeadline)' -count=1
```

Expected: FAIL because `acceptFrozenSample` and `acceptRampSample` do not exist.

- [ ] **Step 7: Freeze the 401 connection count after ramp**

Implement:

```go
func (s *ConnectionScenario) acceptFrozenSample(total, tagged int) error {
	s.updateConnectionSample(total, tagged)
	if tagged != s.budget.WorkloadTarget {
		return fmt.Errorf(
			"connection_pool injected sessions changed: actual=%d target=%d",
			tagged, s.budget.WorkloadTarget,
		)
	}
	return nil
}

func (s *ConnectionScenario) acceptRampSample(total, tagged int) error {
	if err := s.acceptFrozenSample(total, tagged); err != nil {
		return err
	}
	if total < s.budget.DesiredTotal {
		return fmt.Errorf(
			"connection_pool target was not reached: actual=%d target=%d",
			total, s.budget.DesiredTotal,
		)
	}
	s.targetReached = true
	return nil
}

func connectionTargetRampError(err error, opened, target int) error {
	if errors.Is(err, context.DeadlineExceeded) {
		// Keep a mandatory target miss distinct from normal duration completion.
		return fmt.Errorf(
			"connection_pool target was not reached before --duration: opened=%d target=%d",
			opened, target,
		)
	}
	return err
}
```

Add `targetReached bool` to `ConnectionScenario`. Change `Ramp` to open exactly `budget.WorkloadTarget` connections once, routing open/sample errors through `connectionTargetRampError`, then take one real sample and require `acceptRampSample` before freezing. This prevents a duration deadline during connection creation from being accepted as normal completion. Replace hold-time `reconcile` with sampling plus `acceptFrozenSample`; do not call `openConnection` from Hold. External sessions may later leave without causing top-up or erasing the successful ramp evidence, but losing one of this run's tagged sessions fails immediately. Make `Verify` use the sticky `targetReached` field for `ControlResult.Reached` while retaining the latest percentage as `Actual`. Keep the active error channel and wait until the earlier of the shared context deadline or `rt.Config.Run.Duration`:

```go
func (s *ConnectionScenario) Hold(ctx context.Context, rt *Runtime) error {
	interval := rt.Config.Run.RampInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timer := time.NewTimer(rt.Config.Run.Duration)
	defer timer.Stop()
	for {
		select {
		case err := <-s.activeErrors:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-ticker.C:
			total, tagged, err := s.sampleConnections(ctx, rt)
			if err != nil {
				return err
			}
			if err := s.acceptFrozenSample(total, tagged); err != nil {
				return err
			}
		}
	}
}
```

Retain the existing Stop order: cancel active workers, wait with cleanup context, roll back transactions, close every tagged connection, and clear slices.

- [ ] **Step 8: Run all 401-related tests**

```bash
gofmt -w internal/gsbench/scenario_connections.go internal/gsbench/scenario_connections_test.go
go test ./internal/gsbench -run 'Test(Connection|.*ConnectionScenario)' -count=1
```

Expected: PASS, including `TestConnectionScenarioStopCleansResourcesAfterJoinTimeout`.

- [ ] **Step 9: Commit scenario 401 behavior**

```bash
git add internal/gsbench/scenario_connections.go internal/gsbench/scenario_capacity_test.go internal/gsbench/scenario_connections_test.go
git commit -m "feat(gsbench): freeze connection pool pressure at target"
```

---

### Task 3: Ramp scenario 402 to a real minimum percentage and freeze workers

**Files:**
- Modify: `internal/gsbench/controller.go:15-390`
- Modify: `internal/gsbench/controller_test.go:1-220`
- Modify: `internal/gsbench/scenario_threads.go:1-235`
- Modify: `internal/gsbench/scenario_capacity_test.go:65-150`
- Create: `internal/gsbench/scenario_threads_test.go`
- Modify: `internal/gsbench/runner_test.go:900-960`

**Interfaces:**
- Consumes: `BenchConfig.PoolTargets.ThreadPercent`, `ThreadPoolStatus`, `WorkerGroup` as `Actuator`.
- Produces: `Controller.RunToMinimum(ctx context.Context) ControlResult`, `threadPoolPercent(status ThreadPoolStatus) float64`, and `threadUtilizationCeilingFromBaseline(status ThreadPoolStatus, newSessions int) float64`.
- Produces: synchronous 402 Ramp that freezes `WorkerGroup.Target()` after three consecutive at-or-above-target samples.

- [ ] **Step 1: Write failing controller tests for threshold reach and frozen worker target**

Add to `internal/gsbench/controller_test.go` using the existing test actuator pattern:

```go
func TestControllerRunToMinimumStopsAddingAtTarget(t *testing.T) {
	actuator := &fakeActuator{target: 0}
	values := []float64{80, 86, 90, 91, 90}
	index := 0
	result := (Controller{
		Config: ControllerConfig{
			Target: 90, MinWorkers: 1, MaxWorkers: 20,
			Step: 2, RequiredSamples: 3, Interval: time.Millisecond,
		},
		Actuator: actuator,
		Sample: func(context.Context) Sample {
			value := values[min(index, len(values)-1)]
			index++
			return Sample{Available: true, Value: value}
		},
	}).RunToMinimum(context.Background())
	if !result.Reached || result.Actual < 90 {
		t.Fatalf("result=%+v", result)
	}
	if actuator.Target() != result.Workers {
		t.Fatalf("actuator target=%d result workers=%d", actuator.Target(), result.Workers)
	}
	if index != 5 {
		t.Fatalf("samples=%d want=5", index)
	}
}

func TestControllerRunToMinimumFailsAtCeiling(t *testing.T) {
	actuator := &fakeActuator{}
	result := (Controller{
		Config: ControllerConfig{
			Target: 90, MinWorkers: 1, MaxWorkers: 2,
			Step: 1, RequiredSamples: 2, Interval: time.Millisecond,
		},
		Actuator: actuator,
		Sample: func(context.Context) Sample {
			return Sample{Available: true, Value: 50}
		},
	}).RunToMinimum(context.Background())
	if !result.Ceiling || result.Reached {
		t.Fatalf("result=%+v", result)
	}
}
```

- [ ] **Step 2: Run controller tests and verify RED**

```bash
go test ./internal/gsbench -run 'TestControllerRunToMinimum' -count=1
```

Expected: FAIL because `RunToMinimum` does not exist.

- [ ] **Step 3: Implement a monotonic minimum-target controller mode**

Add `RunToMinimum` to `Controller`. It must:

```go
func (c Controller) RunToMinimum(ctx context.Context) ControlResult {
	cfg := normalizedControllerConfig(c.Config)
	result := ControlResult{}
	if c.Actuator == nil || c.Sample == nil {
		result.Err = fmt.Errorf("controller actuator and sampler are required")
		return result
	}
	if err := c.Actuator.SetTarget(cfg.MinWorkers); err != nil {
		result.Err = err
		return result
	}
	consecutive := 0
	for {
		if err := ctx.Err(); err != nil {
			result.Err = err
			updateControlWorkers(&result, c.Actuator)
			return result
		}
		sample := c.Sample(ctx)
		result.Samples++
		updateControlWorkers(&result, c.Actuator)
		if sample.Err != nil || sample.Errors > 0 || !sample.Available {
			// Return an explicit error; a percentage target requires real evidence.
			result.Err = minimumTargetSampleError(sample)
			return result
		}
		result.Measured = true
		result.Actual = sample.Value
		result.LastSuccessful = sample.Value
		result.ReachableMax = max(result.ReachableMax, sample.Value)
		if sample.Value >= cfg.Target {
			consecutive++
			if consecutive >= cfg.RequiredSamples {
				result.Reached = true
				return result
			}
		} else {
			consecutive = 0
			current := c.Actuator.Target()
			if current >= cfg.MaxWorkers {
				result.Ceiling = true
				return result
			}
			step := actuatorRampAdjustment(cfg, sample, current)
			if err := c.Actuator.SetTarget(min(cfg.MaxWorkers, current+step)); err != nil {
				result.Err = err
				return result
			}
			updateControlWorkers(&result, c.Actuator)
		}
		if err := waitContext(ctx, cfg.Interval); err != nil {
			result.Err = err
			return result
		}
	}
}
```

Extract shared controller defaulting into `normalizedControllerConfig` so existing `Run`/`RunUntil` behavior remains unchanged:

```go
func normalizedControllerConfig(cfg ControllerConfig) ControllerConfig {
	if cfg.MinWorkers <= 0 {
		cfg.MinWorkers = 1
	}
	if cfg.MaxWorkers < cfg.MinWorkers {
		cfg.MaxWorkers = cfg.MinWorkers
	}
	if cfg.Step <= 0 {
		cfg.Step = max(1, (cfg.MaxWorkers-cfg.MinWorkers+9)/10)
	}
	if cfg.RequiredSamples <= 0 {
		cfg.RequiredSamples = 3
	}
	if cfg.RequiredAdjustmentSamples <= 0 {
		cfg.RequiredAdjustmentSamples = 1
	}
	if cfg.RequiredSettlingSamples <= 0 {
		cfg.RequiredSettlingSamples = 1
	}
	if cfg.MaxSamples <= 0 {
		cfg.MaxSamples = max(cfg.RequiredSamples, cfg.MaxWorkers*4)
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.Tolerance <= 0 {
		cfg.Tolerance = 2
	}
	return cfg
}

func minimumTargetSampleError(sample Sample) error {
	switch {
	case sample.Err != nil:
		return sample.Err
	case sample.Errors > 0:
		return fmt.Errorf("workload execution errors=%d", sample.Errors)
	case !sample.Available:
		return fmt.Errorf("minimum target metric is unavailable")
	default:
		return nil
	}
}
```

- [ ] **Step 4: Run controller tests and existing controller suite**

```bash
gofmt -w internal/gsbench/controller.go internal/gsbench/controller_test.go
go test ./internal/gsbench -run 'TestController' -count=1
```

Expected: PASS.

- [ ] **Step 5: Write failing 402 baseline and ceiling tests**

Add to `scenario_capacity_test.go`:

```go
func TestThreadPoolPercentAndCeilingIncludeExistingBusyWorkers(t *testing.T) {
	status := ThreadPoolStatus{Actual: 100, Idle: 20}
	if got := threadPoolPercent(status); got != 80 {
		t.Fatalf("baseline=%v", got)
	}
	if got := threadUtilizationCeilingFromBaseline(status, 10); got != 90 {
		t.Fatalf("ceiling=%v", got)
	}
}

func TestThreadTargetMustExceedBaseline(t *testing.T) {
	status := ThreadPoolStatus{Actual: 100, Idle: 20}
	for _, target := range []int{79, 80} {
		if err := validateThreadTarget(status, target, 20); err == nil {
			t.Fatalf("target %d accepted at 80%% baseline", target)
		}
	}
	if err := validateThreadTarget(status, 90, 5); err == nil {
		t.Fatal("unreachable 90% target was accepted")
	}
}
```

Create `scenario_threads_test.go` with mandatory timeout and frozen-worker checks:

```go
func TestThreadTargetDeadlineRemainsFailureWithoutWrappingDeadline(t *testing.T) {
	err := threadTargetControlError(ControlResult{
		Err: context.DeadlineExceeded, Actual: 89, Workers: 10,
	}, 90)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline target error=%v", err)
	}
}

func TestFrozenThreadWorkersMustRemainEstablished(t *testing.T) {
	if err := validateFrozenWorkerSnapshot(WorkerSnapshot{
		Target: 10, Active: 10,
	}, 10); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []WorkerSnapshot{
		{Target: 10, Active: 9},
		{Target: 9, Active: 9},
	} {
		if err := validateFrozenWorkerSnapshot(snapshot, 10); err == nil {
			t.Fatalf("accepted lost frozen worker: %+v", snapshot)
		}
	}
}
```

- [ ] **Step 6: Run 402 capacity tests and verify RED**

```bash
go test ./internal/gsbench -run 'TestThread(PoolPercentAndCeilingIncludeExistingBusyWorkers|TargetMustExceedBaseline)$' -count=1
```

Expected: FAIL because the capacity, mandatory-error, and frozen-worker helpers do not exist.

- [ ] **Step 7: Implement real-baseline preflight and synchronous frozen ramp**

Implement:

```go
func threadPoolPercent(status ThreadPoolStatus) float64 {
	if status.Actual <= 0 {
		return 0
	}
	return float64(status.Actual-status.Idle) / float64(status.Actual) * 100
}

func threadUtilizationCeilingFromBaseline(status ThreadPoolStatus, newSessions int) float64 {
	if status.Actual <= 0 {
		return 0
	}
	busy := status.Actual - status.Idle
	return float64(min(status.Actual, busy+max(0, newSessions))) /
		float64(status.Actual) * 100
}

func validateThreadTarget(status ThreadPoolStatus, target int, newSessions int) error {
	baseline := threadPoolPercent(status)
	if float64(target) <= baseline {
		return fmt.Errorf("thread_pool target %.1f%% must be above baseline %.1f%%", float64(target), baseline)
	}
	ceiling := threadUtilizationCeilingFromBaseline(status, newSessions)
	if ceiling < float64(target) {
		return fmt.Errorf("thread_pool target %.1f%% is unreachable; ceiling %.1f%%", float64(target), ceiling)
	}
	return nil
}
```

Add helpers used by Ramp and Hold:

```go
func threadTargetControlError(result ControlResult, target float64) error {
	if result.Reached && result.Err == nil {
		return nil
	}
	if errors.Is(result.Err, context.DeadlineExceeded) {
		// Deliberately do not wrap DeadlineExceeded: target miss is a failure,
		// not Runner's normal duration completion.
		return fmt.Errorf(
			"thread_pool target %.1f%% was not reached before --duration; actual=%.1f%% workers=%d",
			target, result.Actual, result.Workers,
		)
	}
	if result.Err != nil {
		return result.Err
	}
	if result.Ceiling {
		return fmt.Errorf(
			"thread_pool target %.1f%% is unreachable; measured peak %.1f%%",
			target, result.ReachableMax,
		)
	}
	return fmt.Errorf(
		"thread_pool target %.1f%% was not reached; actual=%.1f%%",
		target, result.Actual,
	)
}

func validateFrozenWorkerSnapshot(snapshot WorkerSnapshot, frozen int) error {
	if err := workerSnapshotError(snapshot); err != nil {
		return err
	}
	if snapshot.Target != frozen || snapshot.Active != frozen {
		return fmt.Errorf(
			"thread_pool frozen workers changed: target=%d active=%d frozen=%d",
			snapshot.Target, snapshot.Active, frozen,
		)
	}
	return nil
}
```

In `ThreadScenario.Prepare`:

- retain the optional enable-with-restart path;
- require `s.real` after that path, rejecting active-backend fallback as unavailable evidence;
- sample `global_threadpool_status` before constructing the workload;
- read `rt.Config.PoolTargets.ThreadPercent`;
- compute `maxWorkers` from connection/worker safety headroom;
- call `validateThreadTarget` before creating `sqlWorkload`.

Remove the `continuousControl` field and add `frozenWorkers int`. In `Ramp`, run `Controller.RunToMinimum` synchronously, save its result, pass it through `threadTargetControlError`, and on success store the exact returned worker count in `frozenWorkers`. This freezes the current WorkerGroup target. The non-wrapping deadline conversion prevents `Runner.runDurationElapsed` from misclassifying a mandatory target miss as normal completion when validation is disabled. In `Hold`, keep the frozen workers until `ctx` or duration completes and fail immediately when `validateFrozenWorkerSnapshot` sees execution errors or a changed target/active count. In `Stop`, call `s.workload.Stop(ctx)`; no controller goroutine remains to join.

Use this hold loop so frozen workers are observed without changing their target:

```go
func (s *ThreadScenario) Hold(ctx context.Context, rt *Runtime) error {
	interval := rt.Config.Run.RampInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timer := time.NewTimer(rt.Config.Run.Duration)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-ticker.C:
			if err := validateFrozenWorkerSnapshot(
				s.workload.Snapshot(), s.frozenWorkers,
			); err != nil {
				return err
			}
		}
	}
}
```

- [ ] **Step 8: Run all controller, capacity, and thread scenario tests**

Add a Runner regression test showing a target/ramp failure remains local:

```go
func TestRunnerTargetFailureDoesNotCancelOtherScenario(t *testing.T) {
	failed := &fakeScenario{
		name: "pool-target", failPhase: PhaseRamp, outcome: OutcomeSuccess,
	}
	other := &fakeScenario{name: "other", outcome: OutcomeSuccess}
	summary := runTestScenarios(
		t, context.Background(), &Runtime{RunID: "run-1"},
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
```

```bash
gofmt -w internal/gsbench/scenario_threads.go internal/gsbench/scenario_capacity_test.go internal/gsbench/scenario_threads_test.go internal/gsbench/runner_test.go
go test ./internal/gsbench -run 'Test(Controller|Thread|RunnerTargetFailureDoesNotCancelOtherScenario)' -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit scenario 402 behavior**

```bash
git add internal/gsbench/controller.go internal/gsbench/controller_test.go internal/gsbench/scenario_threads.go internal/gsbench/scenario_capacity_test.go internal/gsbench/scenario_threads_test.go internal/gsbench/runner_test.go
git commit -m "feat(gsbench): freeze thread pool pressure at target"
```

---

### Task 4: Allow only action-free pool-only stale recovery failures to warn and continue

**Files:**
- Modify: `internal/gsbench/restore.go:15-35,185-300,470-575`
- Modify: `internal/gsbench/restore_test.go:250-380,780-830`
- Modify: `internal/gsbench/app.go:780-850,2010-2315`
- Modify: `internal/gsbench/app_plan_test.go:340-520`
- Modify: `internal/gsbench/plan_definitions_test.go:410-435`

**Interfaces:**
- Produces: `RestoreRun.ScenarioCodes []ScenarioCode`, `RestoreSummary.Runs []RestoreRun`, proof flags for completed discovery and released restore locking, `restoreRunIDs([]RestoreRun) []string`, and `canContinueAfterPoolOnlyRecoveryFailure(RestoreSummary) bool`.
- Consumes: strict `meta_runs.scenarios` data and merged database/local pending actions.
- Keeps: RestoreCoordinator failure semantics; only `commandRunCore` decides whether a failed stale restore is a non-blocking pool-only warning.

- [ ] **Step 1: Write failing tests for strict stored run identity and summary preservation**

Extend `restore_test.go`:

```go
func TestPrepareRestorePlanPreservesScenarioCodes(t *testing.T) {
	discovery := RestoreDiscovery{Runs: []RestoreRun{{
		RunID: "pool-run", ScenarioCodes: []ScenarioCode{401, 402},
	}}}
	runs, actions, err := prepareRestorePlan(discovery, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 || len(runs) != 1 ||
		!reflect.DeepEqual(runs[0].ScenarioCodes, []ScenarioCode{401, 402}) {
		t.Fatalf("runs=%+v actions=%+v", runs, actions)
	}
}

func TestRestoreSummaryRetainsDiscoveredRunMetadataOnFailure(t *testing.T) {
	backend := &fakeRestoreBackend{
		discovery: RestoreDiscovery{Runs: []RestoreRun{{
			RunID: "pool-run", ScenarioCodes: []ScenarioCode{401},
		}}},
		fail: map[string]error{"stop:pool-run": errors.New("stop failed")},
	}
	summary := NewRestoreCoordinator(backend).Restore(context.Background(), RestoreRequest{})
	if !summary.Failed || len(summary.Runs) != 1 ||
		!summary.DiscoveryComplete || !summary.RestoreLockReleased ||
		!reflect.DeepEqual(summary.Runs[0].ScenarioCodes, []ScenarioCode{401}) {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestParseStoredScenarioCodesIsStrict(t *testing.T) {
	codes, err := parseStoredScenarioCodes("401,402")
	if err != nil || !reflect.DeepEqual(codes, []ScenarioCode{401, 402}) {
		t.Fatalf("codes=%v err=%v", codes, err)
	}
	for _, value := range []string{
		"", "401,401", "401,unknown", "401,999",
		"401,,402", ",401", "401,",
	} {
		if _, err := parseStoredScenarioCodes(value); err == nil {
			t.Fatalf("accepted stored scenarios %q", value)
		}
	}
}
```

- [ ] **Step 2: Run restore metadata tests and verify RED**

```bash
go test ./internal/gsbench -run 'Test(PrepareRestorePlanPreservesScenarioCodes|RestoreSummaryRetainsDiscoveredRunMetadataOnFailure|ParseStoredScenarioCodesIsStrict)$' -count=1
```

Expected: FAIL because restore plans/summaries keep only run IDs.

- [ ] **Step 3: Preserve typed run metadata through restore planning**

Extend:

```go
type RestoreRun struct {
	RunID         string
	StartedAt     time.Time
	ScenarioCodes []ScenarioCode
}

type RestoreSummary struct {
	Runs                []RestoreRun
	RunIDs              []string
	PlannedActions      []Action
	DiscoveryComplete   bool
	RestoreLockReleased bool
	Outcome             Outcome
	Failed              bool
	Err                 error
}
```

Change `prepareRestorePlan` to return `[]RestoreRun` instead of `[]string`, preserving the newest metadata for duplicate IDs. Add:

```go
func restoreRunIDs(runs []RestoreRun) []string {
	ids := make([]string, len(runs))
	for i, run := range runs {
		ids[i] = run.RunID
	}
	return ids
}
```

Use `restoreRunIDs` only at backend calls requiring strings: mark, stop, action grouping, verification, and terminal-outcome loops keep their existing string IDs. Update the offline-control-plane caller in `app.go` in the same way. Populate both `Runs` and `RunIDs` in successful and failed summaries, and change `failedRestoreSummary` to accept `[]RestoreRun`. Runs synthesized only from actions have empty scenario codes, which intentionally makes them unsafe for non-blocking continuation. Set `DiscoveryComplete` only after both `DiscoverRestore` and `prepareRestorePlan` succeed; never infer it from partial discovery. Set `RestoreLockReleased` only when the acquired restore lock reports a successful release. This keeps discovery and locking failures fail-closed.

- [ ] **Step 4: Run restore package tests and verify GREEN**

```bash
gofmt -w internal/gsbench/restore.go internal/gsbench/restore_test.go
go test ./internal/gsbench -run 'Test(PrepareRestorePlan|RestoreSummary|RestoreCoordinator)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Write failing policy tests for the exact non-blocking boundary**

Add to `app_plan_test.go`:

```go
func TestPoolOnlyRecoveryFailureContinuationPolicy(t *testing.T) {
	errStop := errors.New("stop failed")
	tests := []struct {
		name    string
		summary RestoreSummary
		want    bool
	}{
		{
			name: "401 and 402 without actions",
			summary: RestoreSummary{Failed: true, Err: errStop,
				DiscoveryComplete: true, RestoreLockReleased: true,
				Runs: []RestoreRun{
				{RunID: "a", ScenarioCodes: []ScenarioCode{401}},
				{RunID: "b", ScenarioCodes: []ScenarioCode{402}},
			}},
			want: true,
		},
		{name: "success is not an exception", summary: RestoreSummary{Runs: []RestoreRun{{RunID: "a", ScenarioCodes: []ScenarioCode{401}}}}},
		{name: "no discovered runs", summary: RestoreSummary{Failed: true, Err: errStop}},
		{name: "discovery failure", summary: RestoreSummary{Failed: true, Err: errStop, RestoreLockReleased: true, Runs: []RestoreRun{{RunID: "a", ScenarioCodes: []ScenarioCode{401}}}}},
		{name: "restore lock release failure", summary: RestoreSummary{Failed: true, Err: errStop, DiscoveryComplete: true, Runs: []RestoreRun{{RunID: "a", ScenarioCodes: []ScenarioCode{401}}}}},
		{name: "unknown run identity", summary: RestoreSummary{Failed: true, Err: errStop, DiscoveryComplete: true, RestoreLockReleased: true, Runs: []RestoreRun{{RunID: "a"}}}},
		{name: "non-pool scenario", summary: RestoreSummary{Failed: true, Err: errStop, DiscoveryComplete: true, RestoreLockReleased: true, Runs: []RestoreRun{{RunID: "a", ScenarioCodes: []ScenarioCode{401, 301}}}}},
		{name: "database or local action", summary: RestoreSummary{Failed: true, Err: errStop, DiscoveryComplete: true, RestoreLockReleased: true, Runs: []RestoreRun{{RunID: "a", ScenarioCodes: []ScenarioCode{402}}}, PlannedActions: []Action{{RunID: "a", ScenarioCode: 402}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := canContinueAfterPoolOnlyRecoveryFailure(test.summary); got != test.want {
				t.Fatalf("got=%v want=%v summary=%+v", got, test.want, test.summary)
			}
		})
	}
}
```

- [ ] **Step 6: Run policy tests and verify RED**

```bash
go test ./internal/gsbench -run TestPoolOnlyRecoveryFailureContinuationPolicy -count=1
```

Expected: FAIL because the policy function does not exist.

- [ ] **Step 7: Parse `meta_runs.scenarios` strictly and implement the policy**

Add a strict parser reusing catalog lookup:

```go
func parseStoredScenarioCodes(value string) ([]ScenarioCode, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("stored scenario list is empty")
	}
	parts := strings.Split(value, ",")
	codes := make([]ScenarioCode, 0, len(parts))
	seen := map[ScenarioCode]bool{}
	for _, raw := range parts {
		part := strings.TrimSpace(raw)
		if part == "" {
			return nil, fmt.Errorf("stored scenario list contains an empty item")
		}
		n, err := strconv.ParseUint(part, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid stored scenario %q", part)
		}
		code := ScenarioCode(n)
		if _, err := DefaultScenarioCatalog().LookupCode(code); err != nil {
			return nil, err
		}
		if seen[code] {
			return nil, fmt.Errorf("duplicate stored scenario %03d", code)
		}
		seen[code] = true
		codes = append(codes, code)
	}
	return codes, nil
}
```

Change `discoverMetaRuns` and `addPendingRunMetadata` queries to select `scenarios` with `started_at`, parse them, and store `ScenarioCodes`. Any metadata read/parse failure returns an error and remains blocking.

Implement:

```go
func canContinueAfterPoolOnlyRecoveryFailure(summary RestoreSummary) bool {
	if !summary.Failed || summary.Err == nil || !summary.DiscoveryComplete ||
		!summary.RestoreLockReleased || len(summary.Runs) == 0 ||
		len(summary.PlannedActions) != 0 {
		return false
	}
	for _, run := range summary.Runs {
		if len(run.ScenarioCodes) == 0 {
			return false
		}
		for _, code := range run.ScenarioCodes {
			if code != 401 && code != 402 {
				return false
			}
		}
	}
	return true
}
```

- [ ] **Step 8: Make pre-run stale recovery warn and start a new run only for the safe class**

First add a focused orchestration test in `app_plan_test.go`:

```go
func TestContinueAfterPoolOnlyRecoveryFailureStartsNewRun(t *testing.T) {
	var output bytes.Buffer
	log, err := NewRunLog(&output, "", Version)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	starts := 0
	summary := RestoreSummary{
		Failed: true, Err: errors.New("stop stale sessions failed"),
		DiscoveryComplete: true, RestoreLockReleased: true,
		Runs: []RestoreRun{{RunID: "old", ScenarioCodes: []ScenarioCode{401}}},
	}
	if err := continueAfterPoolOnlyRecoveryFailure(summary, log, func() error {
		starts++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if starts != 1 || !strings.Contains(output.String(), "WARN") ||
		!strings.Contains(output.String(), "stop stale sessions failed") {
		t.Fatalf("starts=%d output=%s", starts, output.String())
	}
}
```

Implement the orchestration boundary:

```go
func continueAfterPoolOnlyRecoveryFailure(
	summary RestoreSummary,
	log *RunLog,
	start func() error,
) error {
	if !canContinueAfterPoolOnlyRecoveryFailure(summary) {
		if summary.Err != nil {
			return summary.Err
		}
		return fmt.Errorf("stale recovery failed without a recorded cause")
	}
	log.Warn(
		"stale pool-only recovery FAILED but will not block later tests: runs=%d error=%v",
		len(summary.Runs), summary.Err,
	)
	if start == nil {
		return fmt.Errorf("new run recorder is unavailable")
	}
	return start()
}
```

In `commandRunCore`, first extract the current `RestoreRequest.afterSuccess` body into a `startPreparedRun(ctx)` closure, including the existing plan-change active-run and baseline checks followed by `startRun`. Use that closure both for normal restore success and for the safe exception, so a new 601–606 run cannot bypass its normal baseline preparation. When the returned summary failed:

```go
if staleSummary.Failed {
	if err := continueAfterPoolOnlyRecoveryFailure(staleSummary, log, func() error {
		return startPreparedRun(parent)
	}); err != nil {
		log.Error("recover stale state and record run: %v", err)
		return 1
	}
	return 0
}
```

Do not mutate the old summary or mark the old run successful. If plan baseline preparation or `startRun` fails, block the new run. Any summary with actions—including the 402 enable-thread-pool mutation—fails the policy automatically.

- [ ] **Step 9: Run restore and app orchestration tests**

```bash
gofmt -w internal/gsbench/app.go internal/gsbench/app_plan_test.go
go test ./internal/gsbench -run 'Test(PoolOnlyRecoveryFailureContinuationPolicy|ContinueAfterPoolOnlyRecoveryFailureStartsNewRun|PrepareRestorePlan|RestoreSummary|RestoreCoordinator|.*Stale.*)' -count=1
```

Expected: PASS.

- [ ] **Step 10: Commit the narrow stale recovery exception**

```bash
git add internal/gsbench/restore.go internal/gsbench/restore_test.go internal/gsbench/app.go internal/gsbench/app_plan_test.go internal/gsbench/plan_definitions_test.go
git commit -m "fix(gsbench): continue after action-free pool recovery failures"
```

---

### Task 5: Document commands and run minimum final verification

**Files:**
- Modify: `docs/gsbench/README.md:45-90`
- Modify: `docs/gsbench/CONFIG.md:65-105,188-205,215-225`
- Modify: `configs/gsbench.cfg:105-125`
- Verify: `cmd/gsbench/main_test.go`

**Interfaces:**
- Consumes: `--percent`, typed pool targets, baseline/target evidence, cleanup semantics, and stale recovery warning policy.
- Produces: release documentation for 401/402 commands and precise safety behavior.

- [ ] **Step 1: Update the user documentation and configuration comments**

Add these examples to `docs/gsbench/README.md`:

```bash
# 连接池：从压测前基线补足到 90%，达标后保持到 5m 结束
gsbench run 401 --percent 90 --duration 5m

# 线程池：用真实 thread-pool 指标补足到 90%，达标后冻结 worker 数
gsbench run 402 --percent 90 --duration 5m
```

Document that target must be above baseline, unreachable targets fail, 401 uses `max_connections-reserved`, 402 uses busy/actual workers, cleanup removes injected sessions, and database-owned idle worker destruction is not controlled by gsbench. Document the pool-only/action-free stale warning exception and the continued fail-closed behavior for all mutations.

In `CONFIG.md` and `configs/gsbench.cfg`, keep both default target values at 95 and state that CLI `--percent` overrides only selected 401/402 scenarios.

- [ ] **Step 2: Run focused tests and retain the SIGINT contract**

```bash
go test ./internal/gsbench -run 'Test(ParseCLIArgs.*Pool|Config.*Pool|Connection|ControllerRunToMinimum|Thread|PoolOnlyRecovery|PrepareRestorePlan|RestoreSummary)' -count=1
go test ./cmd/gsbench -run TestCommandContextLeavesInterruptsToOperatingSystem -count=1
git diff --check
```

Expected: PASS. The targeted command test does not open a local listener and proves SIGINT remains delegated to the operating system.

- [ ] **Step 3: Run the full affected package suite**

```bash
go test ./internal/gsbench -count=1
```

Expected: PASS.

Do not use `go test ./cmd/gsbench` without `-run` in this managed sandbox: its process integration test opens `127.0.0.1:0`, which the sandbox rejects independently of this feature.

- [ ] **Step 4: Commit documentation**

```bash
git add docs/gsbench/README.md docs/gsbench/CONFIG.md configs/gsbench.cfg
git commit -m "docs(gsbench): document pool percentage targets"
```

- [ ] **Step 5: Inspect final state**

```bash
git status --short --branch
git log -6 --oneline
```

Expected: clean local `main`; the latest commits cover CLI/config, 401, 402, stale recovery, documentation, plan, and design.
