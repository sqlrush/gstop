# gsbench Work-Memory Calibration Fallback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let scenarios 201 and 202 accept non-spilling operator memory from 70% through 97% of requested `work_mem`, and continue with a visible warning when database memory protection prevents reaching that band.

**Architecture:** Keep the bounded calibration search and SQL unchanged. Extend its result with `TargetMet`, select a deterministic best non-spilling fallback instead of returning a percentage error, and expose fallback state through `WARN` output and evidence. Preserve fatal errors for database/probe failures and searches that find no usable non-spilling observation.

**Tech Stack:** Go 1.26.5, standard `testing` package, openGauss 5.0.3 in container og5.

## Global Constraints

- Preferred calibration band is 70%-97%, inclusive.
- Missing the band is non-fatal only when a usable non-spilling observation exists.
- Spill observations are never selected for worker execution.
- SQL, workers, duration, Ctrl+C handling, and cleanup remain unchanged.
- Modify only scenarios 201/202 and the generic log level needed for their warning.

---

### Task 1: Calibrator Accepts 70% and Falls Back Without Error

**Files:**
- Modify: `internal/gsbench/scenario_workmem.go:14-39,555-637`
- Test: `internal/gsbench/scenario_workmem_test.go:244-351`

**Interfaces:**
- Consumes: `workMemProbe func(context.Context, int64) (workMemObservation, error)`.
- Produces: `workMemCalibration{RangeEnd int64, Attempts int, Observation workMemObservation, TargetMet bool}` while retaining the existing `calibrateWorkMemRange` signature.

- [ ] **Step 1: Write failing calibration regression tests**

Add tests for the new lower bound, the reproduced 64 MB cliff, a 256 MB fallback capped around 47 MB, and the fatal all-spill case:

```go
func TestCalibrateWorkMemRangeAcceptsOpenGaussEarlySpillCliff(t *testing.T) {
    calibration, err := calibrateWorkMemRange(
        context.Background(), 64*1024, workMemSort,
        func(_ context.Context, rangeEnd int64) (workMemObservation, error) {
            if rangeEnd > 3455 {
                return workMemObservation{OperatorCount: 1, Spilled: true}, nil
            }
            return workMemObservation{UsedKB: 47095, TotalUsedKB: 47095, OperatorCount: 1}, nil
        },
    )
    if err != nil { t.Fatal(err) }
    if !calibration.TargetMet || calibration.Observation.Spilled {
        t.Fatalf("calibration=%+v want accepted non-spilling 70%% result", calibration)
    }
}

func TestCalibrateWorkMemRangeContinuesWithBestNonSpillingFallback(t *testing.T) {
    calibration, err := calibrateWorkMemRange(
        context.Background(), 256*1024, workMemSort,
        func(_ context.Context, rangeEnd int64) (workMemObservation, error) {
            if rangeEnd > 3455 {
                return workMemObservation{OperatorCount: 1, Spilled: true}, nil
            }
            return workMemObservation{UsedKB: 47095, TotalUsedKB: 47095, OperatorCount: 1}, nil
        },
    )
    if err != nil { t.Fatal(err) }
    if calibration.TargetMet || calibration.Observation.Spilled ||
        calibration.Observation.UsedKB != 47095 {
        t.Fatalf("calibration=%+v want unmet non-spilling fallback", calibration)
    }
}

func TestCalibrateWorkMemRangeFailsWithoutNonSpillingObservation(t *testing.T) {
    _, err := calibrateWorkMemRange(
        context.Background(), 64*1024, workMemSort,
        func(context.Context, int64) (workMemObservation, error) {
            return workMemObservation{OperatorCount: 1, Spilled: true}, nil
        },
    )
    if err == nil || !strings.Contains(err.Error(), "no usable non-spilling") {
        t.Fatalf("error=%v want no usable non-spilling error", err)
    }
}
```

Rename the existing 90%-97% test to 70%-97%, change its lower assertion to `targetKB*70/100`, require `TargetMet=true`, and change the existing unreachable-target test to require a non-error fallback with `TargetMet=false`.

- [ ] **Step 2: Run focused tests and verify RED**

```bash
go test ./internal/gsbench -run 'TestCalibrateWorkMemRange' -count=1
```

Expected: compilation fails because `TargetMet` is absent, proving the regression tests exercise the missing behavior.

- [ ] **Step 3: Implement the minimum bounded fallback**

Add exact band constants and state:

```go
const (
    workMemCalibrationLowerPercent = int64(70)
    workMemCalibrationUpperPercent = int64(97)
)

type workMemCalibration struct {
    RangeEnd    int64
    Attempts    int
    Observation workMemObservation
    TargetMet   bool
}
```

Compute `lowerKB := (targetKB*70+99)/100` and `upperKB := targetKB*97/100`. Mark an in-band result as met. Track the largest non-spilling result below the band and the smallest non-spilling result above it. When the bounded search ends, return the below-band candidate first, otherwise the above-band candidate, with `TargetMet=false` and no error. Return an error containing `no usable non-spilling` only when neither candidate exists.

- [ ] **Step 4: Run focused and package tests and verify GREEN**

```bash
gofmt -w internal/gsbench/scenario_workmem.go internal/gsbench/scenario_workmem_test.go
go test ./internal/gsbench -run 'TestCalibrateWorkMemRange' -count=1
go test ./internal/gsbench -count=1
```

Expected: all calibration and package tests pass.

- [ ] **Step 5: Commit calibration behavior**

```bash
git add internal/gsbench/scenario_workmem.go internal/gsbench/scenario_workmem_test.go
git commit -m "fix(gsbench): tolerate workmem calibration caps"
```

### Task 2: Emit WARN and Calibration Evidence

**Files:**
- Modify: `internal/gsbench/runlog.go:114-122`
- Test: `internal/gsbench/runlog_test.go:12-51`
- Modify: `internal/gsbench/scenario_workmem.go:95-134,221-238`
- Test: `internal/gsbench/scenario_workmem_test.go`

**Interfaces:**
- Consumes: `workMemCalibration.TargetMet`, requested kB, and the selected observation.
- Produces: `func (*RunLog) Warn(string, ...any)` and evidence details `target_met`, `observed_percent`, `target_lower_percent`, and `target_upper_percent`.

- [ ] **Step 1: Write failing WARN and evidence tests**

```go
func TestRunLogWritesWarnLevel(t *testing.T) {
    var screen bytes.Buffer
    logger, err := NewRunLog(&screen, "", "dev")
    if err != nil { t.Fatal(err) }
    logger.Warn("calibration target missed")
    if !strings.Contains(screen.String(), "WARN calibration target missed") {
        t.Fatalf("screen=%q want WARN record", screen.String())
    }
}

func TestWorkMemCalibrationEvidenceReportsTargetMiss(t *testing.T) {
    scenario := &workMemScenario{
        kind: workMemSort, targetKB: 256 * 1024,
        calibrated: workMemCalibration{
            RangeEnd: 3455, Attempts: 13,
            Observation: workMemObservation{
                UsedKB: 47095, TotalUsedKB: 47095, OperatorCount: 1,
            },
        },
    }
    details := scenario.calibrationEvidence()[0].Details
    if details["target_met"] != false {
        t.Fatalf("details=%v want target_met=false", details)
    }
    if got := details["observed_percent"].(float64); got < 17.9 || got > 18.0 {
        t.Fatalf("observed_percent=%v want about 17.97", got)
    }
}
```

- [ ] **Step 2: Run focused tests and verify RED**

```bash
go test ./internal/gsbench -run 'TestRunLogWritesWarnLevel|TestWorkMemCalibrationEvidenceReportsTargetMiss' -count=1
```

Expected: compilation fails because `RunLog.Warn` is missing; after that is introduced, evidence still fails until its new fields exist.

- [ ] **Step 3: Implement WARN output and evidence fields**

Add the logger method:

```go
func (l *RunLog) Warn(format string, args ...any) {
    l.write("WARN", format, args...)
}
```

Use a zero-safe percentage helper for both output and evidence. In `Prepare`, after calibration and before worker creation:

```go
if rt.Log != nil && !calibrated.TargetMet {
    rt.Log.Warn(
        "scenario=%s work_mem calibration target=70%%..97%% not reached requested=%dkB observed=%dkB observed_percent=%.2f calibrated_range=1..%d attempts=%d continuing_with_best_non_spilling_range=true",
        s.Name(), s.targetKB, calibrated.Observation.UsedKB,
        workMemObservedPercent(calibrated.Observation.UsedKB, s.targetKB),
        calibrated.RangeEnd, calibrated.Attempts,
    )
}
```

Add `target_met`, `observed_percent`, `target_lower_percent`, and `target_upper_percent` to `calibrationEvidence`.

- [ ] **Step 4: Run package tests and verify GREEN**

```bash
gofmt -w internal/gsbench/runlog.go internal/gsbench/runlog_test.go internal/gsbench/scenario_workmem.go internal/gsbench/scenario_workmem_test.go
go test ./internal/gsbench -count=1
go test ./cmd/gsbench -count=1
```

Expected: all tests pass with clean output.

- [ ] **Step 5: Commit reporting behavior**

```bash
git add internal/gsbench/runlog.go internal/gsbench/runlog_test.go internal/gsbench/scenario_workmem.go internal/gsbench/scenario_workmem_test.go
git commit -m "feat(gsbench): report workmem calibration fallback"
```

### Task 3: Build, Deploy, and Exercise og5

**Files:**
- Build: `cmd/gsbench`
- Deploy: `/Users/sqlrush/gstop/gsbench-local/bin/gsbench`
- Verify config: `/Users/sqlrush/gstop/gsbench-local/gsbench.cfg`

**Interfaces:**
- Consumes: local `gsbench` and og5 on `127.0.0.1:5433`.
- Produces: a deployed executable that starts scenario 201 instead of failing in calibration.

- [ ] **Step 1: Run minimum repository verification and build**

```bash
go test ./internal/gsbench ./cmd/gsbench -count=1
go build -trimpath -o /tmp/gsbench-workmem-fallback ./cmd/gsbench
```

Expected: both commands exit 0.

- [ ] **Step 2: Back up and deploy the binary**

```bash
cp -p /Users/sqlrush/gstop/gsbench-local/bin/gsbench /Users/sqlrush/gstop/gsbench-local/bin/gsbench.before-workmem-fallback
install -m 0755 /tmp/gsbench-workmem-fallback /Users/sqlrush/gstop/gsbench-local/bin/gsbench
```

Expected: `gsbench version` resolves to the deployed binary and succeeds.

- [ ] **Step 3: Run the reproduced 201 workload against og5**

```bash
gsbench run 201 --workers 5 --work-mem 64MB --duration 5s
```

Expected: calibration reaches at least 70%, five workers start, the five-second hold completes, and evidence succeeds. The command must not return `could not calibrate Sort memory` during `Prepare`.

- [ ] **Step 4: Inspect the run log**

Verify that the new `gsbench-local/logs/gsbench_*.log` contains requested kB, observed kB, selected range, attempts, and target status. A target below 70% must produce exactly one `WARN` before worker startup.
