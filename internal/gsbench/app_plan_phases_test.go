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
	events          []string
	workload        planRunRecord
	workloadErr     error
	workloadAlive   bool
	workloadLiveErr error
	fault           planRunRecord
	faultErr        error
	applyErr        error
	verifyErr       error
	markActiveErr   error
	restoreErr      error
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

func (b *planActionBackendTest) ResolveFault(
	context.Context,
	ScenarioCode,
) (planRunRecord, error) {
	b.events = append(b.events, "resolve-fault")
	return b.fault, b.faultErr
}

func (b *planActionBackendTest) StartFault(
	_ context.Context,
	runID string,
	_ ScenarioCode,
) error {
	b.events = append(b.events, "start-fault:"+runID)
	return nil
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

func (b *planActionBackendTest) MarkFaultActive(
	_ context.Context,
	runID string,
) error {
	b.events = append(b.events, "mark-active:"+runID)
	return b.markActiveErr
}

func (b *planActionBackendTest) MarkFaultFailed(
	_ context.Context,
	runID string,
	err error,
	restored bool,
) error {
	b.events = append(
		b.events,
		fmt.Sprintf(
			"mark-failed:%s:restored=%t:%s",
			runID,
			restored,
			err.Error(),
		),
	)
	return nil
}

func (b *planActionBackendTest) RestoreFault(
	_ context.Context,
	runID string,
) error {
	b.events = append(b.events, "restore-fault:"+runID)
	return b.restoreErr
}

func TestExecutePlanFaultActionUsesLiveWorkloadAndOneShotFaultRun(t *testing.T) {
	backend := &planActionBackendTest{
		workload:      planRunRecord{RunID: "workload-1", Code: 601},
		workloadAlive: true,
		faultErr:      errPlanFaultNotFound,
	}
	runID, err := executePlanFaultAction(
		context.Background(),
		601,
		backend,
		func() string { return "fault-1" },
	)
	if err != nil {
		t.Fatal(err)
	}
	if runID != "fault-1" {
		t.Fatalf("runID=%q", runID)
	}
	want := []string{
		"lock",
		"resolve-workload",
		"workload-alive",
		"resolve-fault",
		"start-fault:fault-1",
		"apply-fault:fault-1",
		"verify-fault:601",
		"mark-active:fault-1",
		"unlock",
	}
	if !reflect.DeepEqual(backend.events, want) {
		t.Fatalf("events=%v want=%v", backend.events, want)
	}
}

func TestExecutePlanFaultActionRejectsUnchanged602PlanAndRestores(t *testing.T) {
	backend := &planActionBackendTest{
		workload:      planRunRecord{RunID: "workload-602", Code: 602},
		workloadAlive: true,
		faultErr:      errPlanFaultNotFound,
		verifyErr:     errors.New("fault plan candidate 2 still uses index"),
	}
	runID, err := executePlanFaultAction(
		context.Background(),
		602,
		backend,
		func() string { return "fault-602" },
	)
	if err == nil || !strings.Contains(err.Error(), "fault plan") {
		t.Fatalf("runID=%q error=%v", runID, err)
	}
	want := []string{
		"lock",
		"resolve-workload",
		"workload-alive",
		"resolve-fault",
		"start-fault:fault-602",
		"apply-fault:fault-602",
		"verify-fault:602",
		"restore-fault:fault-602",
		"mark-failed:fault-602:restored=true:fault plan candidate 2 still uses index",
		"unlock",
	}
	if !reflect.DeepEqual(backend.events, want) {
		t.Fatalf("events=%v want=%v", backend.events, want)
	}
	if containsEventPrefix(backend.events, "mark-active") {
		t.Fatalf("unverified fault became active: %v", backend.events)
	}
}

func TestExecutePlanFaultActionRestoresWhenActivationCannotBeRecorded(
	t *testing.T,
) {
	backend := &planActionBackendTest{
		workload:      planRunRecord{RunID: "workload-602", Code: 602},
		workloadAlive: true,
		faultErr:      errPlanFaultNotFound,
		markActiveErr: errors.New("record active phase failed"),
	}
	_, err := executePlanFaultAction(
		context.Background(),
		602,
		backend,
		func() string { return "fault-602" },
	)
	if err == nil || !strings.Contains(err.Error(), "record active phase failed") {
		t.Fatalf("error=%v", err)
	}
	if !containsEventPrefix(backend.events, "restore-fault:fault-602") ||
		!containsEventPrefix(
			backend.events,
			"mark-failed:fault-602:restored=true",
		) {
		t.Fatalf("events=%v", backend.events)
	}
}

func TestExecutePlanFaultActionRejectsStoppedWorkload(t *testing.T) {
	backend := &planActionBackendTest{
		workload:      planRunRecord{RunID: "workload-1", Code: 601},
		workloadAlive: false,
	}
	_, err := executePlanFaultAction(
		context.Background(),
		601,
		backend,
		func() string { return "fault-1" },
	)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("error=%v", err)
	}
	if containsEventPrefix(backend.events, "start-fault") {
		t.Fatalf("events=%v", backend.events)
	}
}

func TestExecutePlanFaultActionPersistsFailureForRecovery(t *testing.T) {
	backend := &planActionBackendTest{
		workload:      planRunRecord{RunID: "workload-1", Code: 606},
		workloadAlive: true,
		faultErr:      errPlanFaultNotFound,
		applyErr:      errors.New("create bad index failed"),
		restoreErr:    errors.New("automatic restore failed"),
	}
	_, err := executePlanFaultAction(
		context.Background(),
		606,
		backend,
		func() string { return "fault-606" },
	)
	if err == nil || !strings.Contains(err.Error(), "create bad index failed") {
		t.Fatalf("error=%v", err)
	}
	if !containsEventPrefix(
		backend.events,
		"mark-failed:fault-606:restored=false",
	) || !containsEventPrefix(backend.events, "restore-fault:fault-606") {
		t.Fatalf("events=%v", backend.events)
	}
}

func TestExecutePlanRecoverActionIsOneShotAndIdempotent(t *testing.T) {
	backend := &planActionBackendTest{
		fault: planRunRecord{RunID: "fault-605", Code: 605},
	}
	runID, restored, err := executePlanRecoverAction(
		context.Background(), 605, backend,
	)
	if err != nil || !restored || runID != "fault-605" {
		t.Fatalf("runID=%q restored=%v err=%v", runID, restored, err)
	}
	want := []string{
		"lock", "resolve-fault", "restore-fault:fault-605", "unlock",
	}
	if !reflect.DeepEqual(backend.events, want) {
		t.Fatalf("events=%v want=%v", backend.events, want)
	}

	backend = &planActionBackendTest{faultErr: errPlanFaultNotFound}
	runID, restored, err = executePlanRecoverAction(
		context.Background(), 605, backend,
	)
	if err != nil || restored || runID != "" {
		t.Fatalf("idempotent runID=%q restored=%v err=%v", runID, restored, err)
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
