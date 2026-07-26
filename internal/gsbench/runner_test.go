package gsbench

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

type fakeScenario struct {
	name      string
	mu        sync.Mutex
	phases    []Phase
	failPhase Phase
	outcome   Outcome
}

func (s *fakeScenario) Name() string { return s.name }
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

func TestRunnerUsesExactLifecycleOrder(t *testing.T) {
	s := &fakeScenario{name: "one", outcome: OutcomeSuccess}
	runner := NewRunner(&Runtime{}, []Scenario{s})
	summary := runner.Run(context.Background(), []string{"one"})
	want := []Phase{PhasePrepare, PhaseRamp, PhaseHold, PhaseVerify, PhaseStop, PhaseRestore}
	if !reflect.DeepEqual(s.phases, want) {
		t.Fatalf("phases=%v want=%v", s.phases, want)
	}
	if summary.Outcome != OutcomeSuccess {
		t.Fatalf("summary=%+v", summary)
	}
	if summary.Results[0].StartedAt.IsZero() || summary.Results[0].EndedAt.IsZero() {
		t.Fatalf("runner timestamps missing: %+v", summary.Results[0])
	}
}

func TestRunnerRestoresAfterRampFailure(t *testing.T) {
	s := &fakeScenario{name: "one", failPhase: PhaseRamp, outcome: OutcomeSuccess}
	summary := NewRunner(&Runtime{}, []Scenario{s}).Run(context.Background(), []string{"one"})
	want := []Phase{PhasePrepare, PhaseRamp, PhaseStop, PhaseRestore}
	if !reflect.DeepEqual(s.phases, want) {
		t.Fatalf("phases=%v want=%v", s.phases, want)
	}
	if summary.Outcome != OutcomeFailed {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestRunnerAggregatesWorstOutcome(t *testing.T) {
	scenarios := []Scenario{
		&fakeScenario{name: "ok", outcome: OutcomeSuccess},
		&fakeScenario{name: "degraded", outcome: OutcomeDegraded},
	}
	summary := NewRunner(&Runtime{}, scenarios).Run(context.Background(), []string{"ok", "degraded"})
	if summary.Outcome != OutcomeDegraded {
		t.Fatalf("summary=%+v", summary)
	}
}
