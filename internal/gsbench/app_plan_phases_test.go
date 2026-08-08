package gsbench

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestPlanHeartbeatIgnoresShutdownCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := planHeartbeatError(ctx, context.Canceled); err != nil {
		t.Fatalf("shutdown heartbeat error=%v", err)
	}
	active := context.Background()
	want := errors.New("metadata update failed")
	if got := planHeartbeatError(active, want); !errors.Is(got, want) {
		t.Fatalf("active heartbeat error=%v want=%v", got, want)
	}
}

func TestPlanFinalizationWaitsForTransientControlLock(t *testing.T) {
	attempts := 0
	released := false
	release, err := acquirePlanFinalizationLock(
		context.Background(),
		func(context.Context) (func() error, error) {
			attempts++
			if attempts < 3 {
				return nil, errDatabaseRunLockBusy
			}
			return func() error {
				released = true
				return nil
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts=%d want=3", attempts)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("control lock release was not called")
	}
}

type planActionBackendTest struct {
	events              []string
	workload            planRunRecord
	workloadErr         error
	workloadAlive       bool
	workloadLiveErr     error
	inspection          PlanFaultInspection
	inspectionErr       error
	recordStartErr      error
	applyErr            error
	verifyErr           error
	recordFinishErr     error
	recordFinishCtxErr  error
	recordFinishOutcome Outcome
	recordFinishDetail  string
}

func (b *planActionBackendTest) Lock(context.Context) (func() error, error) {
	b.events = append(b.events, "lock")
	return func() error {
		b.events = append(b.events, "unlock")
		return nil
	}, nil
}

func (b *planActionBackendTest) ResolveWorkload(
	context.Context,
	ScenarioCode,
) (planRunRecord, error) {
	b.events = append(b.events, "resolve-workload")
	return b.workload, b.workloadErr
}

func (b *planActionBackendTest) WorkloadAlive(context.Context) (bool, error) {
	b.events = append(b.events, "workload-alive")
	return b.workloadAlive, b.workloadLiveErr
}

func (b *planActionBackendTest) InspectFaultState(
	_ context.Context,
	code ScenarioCode,
) (PlanFaultInspection, error) {
	b.events = append(b.events, fmt.Sprintf("inspect-state:%03d", code))
	return b.inspection, b.inspectionErr
}

func (b *planActionBackendTest) RecordFaultStart(
	_ context.Context,
	runID string,
	_ ScenarioCode,
) error {
	b.events = append(b.events, "record-start:"+runID)
	return b.recordStartErr
}

func (b *planActionBackendTest) ApplyFault(
	_ context.Context,
	runID string,
	_ ScenarioCode,
) error {
	b.events = append(b.events, "apply-fault:"+runID)
	return b.applyErr
}

func (b *planActionBackendTest) VerifyFault(
	_ context.Context,
	code ScenarioCode,
) error {
	b.events = append(b.events, fmt.Sprintf("verify-fault:%03d", code))
	return b.verifyErr
}

func (b *planActionBackendTest) RecordFaultFinish(
	ctx context.Context,
	runID string,
	outcome Outcome,
	detail string,
) error {
	b.recordFinishCtxErr = ctx.Err()
	b.recordFinishOutcome = outcome
	b.recordFinishDetail = detail
	b.events = append(b.events, "record-finish:"+runID+":"+string(outcome))
	return b.recordFinishErr
}

type planFaultReporterTest struct {
	states   []PlanFaultInspection
	warnings []PrecheckWarning
}

func (r *planFaultReporterTest) reporters() planFaultReporters {
	return planFaultReporters{
		State: func(inspection PlanFaultInspection) {
			r.states = append(r.states, inspection)
		},
		Warning: func(warning PrecheckWarning) {
			r.warnings = append(r.warnings, warning)
		},
	}
}

func TestExecutePlanFaultActionAlwaysContinuesAfterLiveStateWarning(t *testing.T) {
	for _, state := range []PlanFaultLiveState{
		PlanFaultRestored,
		PlanFaultPresent,
		PlanFaultDrifted,
		PlanFaultUnavailable,
	} {
		t.Run(string(state), func(t *testing.T) {
			backend := &planActionBackendTest{
				workload:      planRunRecord{RunID: "workload-1", Code: 601},
				workloadAlive: true,
				inspection: PlanFaultInspection{
					Code: 601, State: state,
					Object: `"gsbench".plan_data_lookup_idx`,
				},
			}
			reporter := &planFaultReporterTest{}
			runID, err := executePlanFaultAction(
				context.Background(), 601, backend,
				func() string { return "fault-1" }, reporter.reporters(),
			)
			if err != nil {
				t.Fatalf("state=%s runID=%q error=%v", state, runID, err)
			}
			want := []string{
				"lock",
				"resolve-workload",
				"workload-alive",
				"inspect-state:601",
				"record-start:fault-1",
				"apply-fault:fault-1",
				"verify-fault:601",
			}
			if state == PlanFaultRestored {
				want = append(want, "record-finish:fault-1:SUCCESS")
			} else {
				want = append(want, "record-finish:fault-1:COMPLETED_WITH_WARNINGS")
			}
			want = append(want, "unlock")
			if !reflect.DeepEqual(backend.events, want) {
				t.Fatalf("state=%s events=%v want=%v", state, backend.events, want)
			}
			if len(reporter.states) != 1 || reporter.states[0].State != state {
				t.Fatalf("reported states=%+v", reporter.states)
			}
			wantWarnings := 0
			if state != PlanFaultRestored {
				wantWarnings = 1
			}
			if len(reporter.warnings) != wantWarnings {
				t.Fatalf("state=%s warnings=%+v want=%d", state, reporter.warnings, wantWarnings)
			}
		})
	}
}

func TestExecutePlanFaultActionContinuesWhenLiveInspectionErrors(t *testing.T) {
	backend := &planActionBackendTest{
		workload:      planRunRecord{RunID: "workload-602", Code: 602},
		workloadAlive: true,
		inspectionErr: errors.New("catalog connection failed"),
	}
	reporter := &planFaultReporterTest{}
	_, err := executePlanFaultAction(
		context.Background(), 602, backend,
		func() string { return "fault-602" }, reporter.reporters(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !containsEventPrefix(backend.events, "apply-fault:fault-602") {
		t.Fatalf("events=%v", backend.events)
	}
	if len(reporter.states) != 1 || reporter.states[0].State != PlanFaultUnavailable {
		t.Fatalf("states=%+v", reporter.states)
	}
	if len(reporter.warnings) != 1 || reporter.warnings[0].Check != "fault_state" {
		t.Fatalf("warnings=%+v", reporter.warnings)
	}
}

func TestExecutePlanFaultActionIgnoresAuditWriteFailures(t *testing.T) {
	backend := &planActionBackendTest{
		workload:        planRunRecord{RunID: "workload-601", Code: 601},
		workloadAlive:   true,
		inspection:      PlanFaultInspection{Code: 601, State: PlanFaultRestored},
		recordStartErr:  errors.New("audit start failed"),
		recordFinishErr: errors.New("audit finish failed"),
	}
	reporter := &planFaultReporterTest{}
	runID, err := executePlanFaultAction(
		context.Background(), 601, backend,
		func() string { return "fault-1" }, reporter.reporters(),
	)
	if err != nil || runID != "fault-1" {
		t.Fatalf("runID=%q error=%v", runID, err)
	}
	if !containsEventPrefix(backend.events, "apply-fault:fault-1") {
		t.Fatalf("events=%v", backend.events)
	}
	if len(reporter.warnings) != 2 {
		t.Fatalf("warnings=%+v", reporter.warnings)
	}
	if backend.recordFinishOutcome != OutcomeCompletedWithWarnings {
		t.Fatalf("finish outcome=%s", backend.recordFinishOutcome)
	}
}

func TestExecutePlanFaultActionRejectsStoppedWorkload(t *testing.T) {
	backend := &planActionBackendTest{
		workload:      planRunRecord{RunID: "workload-1", Code: 601},
		workloadAlive: false,
	}
	_, err := executePlanFaultAction(
		context.Background(), 601, backend,
		func() string { return "fault-1" }, planFaultReporters{},
	)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("error=%v", err)
	}
	if containsEventPrefix(backend.events, "record-start") {
		t.Fatalf("events=%v", backend.events)
	}
}

func TestExecutePlanFaultActionFailsOnlyOnRealApplyError(t *testing.T) {
	backend := &planActionBackendTest{
		workload:        planRunRecord{RunID: "workload-601", Code: 601},
		workloadAlive:   true,
		inspection:      PlanFaultInspection{Code: 601, State: PlanFaultPresent},
		applyErr:        errors.New("DROP INDEX failed"),
		recordFinishErr: errors.New("audit finish failed"),
	}
	reporter := &planFaultReporterTest{}
	_, err := executePlanFaultAction(
		context.Background(), 601, backend,
		func() string { return "fault-1" }, reporter.reporters(),
	)
	if err == nil || !strings.Contains(err.Error(), "DROP INDEX failed") ||
		strings.Contains(err.Error(), "audit finish failed") {
		t.Fatalf("error=%v", err)
	}
	if backend.recordFinishOutcome != OutcomeFailed ||
		!containsEventPrefix(backend.events, "record-finish:fault-1:FAILED") {
		t.Fatalf("events=%v", backend.events)
	}
	if len(reporter.warnings) != 2 {
		t.Fatalf("warnings=%+v", reporter.warnings)
	}
}

func TestExecutePlanFaultActionKeepsUnchanged602PlanWithWarning(t *testing.T) {
	backend := &planActionBackendTest{
		workload:      planRunRecord{RunID: "workload-602", Code: 602},
		workloadAlive: true,
		inspection:    PlanFaultInspection{Code: 602, State: PlanFaultRestored},
		verifyErr:     errors.New("fault plan candidate 2 still uses index"),
	}
	reporter := &planFaultReporterTest{}
	_, err := executePlanFaultAction(
		context.Background(), 602, backend,
		func() string { return "fault-602" }, reporter.reporters(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(reporter.warnings) != 1 || reporter.warnings[0].Check != "fault_effect" {
		t.Fatalf("warnings=%+v", reporter.warnings)
	}
	if backend.recordFinishOutcome != OutcomeCompletedWithWarnings {
		t.Fatalf("finish outcome=%s", backend.recordFinishOutcome)
	}
}

func TestRecordPlanFaultFailureFinalizesAfterCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	backend := &planActionBackendTest{}
	reporter := &planFaultReporterTest{}

	err := recordPlanFaultFailure(
		ctx, backend, "fault-601", 601,
		errors.New("apply failed"), reporter.reporters(),
	)
	if err == nil || !strings.Contains(err.Error(), "apply failed") {
		t.Fatalf("error=%v", err)
	}
	if backend.recordFinishCtxErr != nil {
		t.Fatalf("finalization context error=%v", backend.recordFinishCtxErr)
	}
}

func containsEventPrefix(events []string, prefix string) bool {
	for _, event := range events {
		if strings.HasPrefix(event, prefix) {
			return true
		}
	}
	return false
}

func TestRunCommandNeedsGeneratedRunIDOnlyForWorkloadOwners(t *testing.T) {
	for _, test := range []struct {
		options CLIOptions
		want    bool
	}{
		{options: CLIOptions{Command: "run"}, want: true},
		{options: CLIOptions{Command: "run", PlanAction: PlanRunInit}, want: true},
		{options: CLIOptions{Command: "run", PlanAction: PlanRunFault}, want: false},
		{options: CLIOptions{Command: "run", PlanAction: PlanRunRecover}, want: false},
		{options: CLIOptions{Command: "status"}, want: false},
	} {
		if got := runCommandNeedsGeneratedRunID(test.options); got != test.want {
			t.Fatalf("options=%+v got=%v want=%v", test.options, got, test.want)
		}
	}
}

func TestStrictPlanInitVerificationIsMandatoryOnlyFor602(t *testing.T) {
	if !strictPlanInitVerificationRequired(false, 602) {
		t.Fatal("602 init skipped strict plan verification")
	}
	if strictPlanInitVerificationRequired(false, 601) {
		t.Fatal("601 validation behavior changed")
	}
	if strictPlanInitVerificationRequired(true, 602) {
		t.Fatal("602 duplicated general plan verification")
	}
}
