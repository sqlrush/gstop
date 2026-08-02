package gsbench

import (
	"context"
	"errors"
	"testing"
	"time"
)

type planSerialTestStore struct {
	nextSequence int64
}

func (s *planSerialTestStore) InsertPlanned(
	_ context.Context,
	action Action,
) (Action, error) {
	s.nextSequence++
	action.Sequence = s.nextSequence
	action.State = MutationPlanned
	return action, nil
}

func (*planSerialTestStore) SetState(
	context.Context,
	string,
	int64,
	MutationState,
	string,
) error {
	return nil
}

func (*planSerialTestStore) Pending(
	context.Context,
	string,
) ([]Action, error) {
	return nil, nil
}

func (*planSerialTestStore) StaleRuns(context.Context) ([]string, error) {
	return nil, nil
}

type planSerialTestExecutor struct {
	onApply func(Action)
}

func (*planSerialTestExecutor) Preflight(context.Context, Action) error {
	return nil
}

func (e *planSerialTestExecutor) Apply(
	_ context.Context,
	action Action,
) error {
	if e.onApply != nil {
		e.onApply(action)
	}
	return nil
}

func (*planSerialTestExecutor) Restore(context.Context, Action) error {
	return nil
}

func (*planSerialTestExecutor) VerifyRestored(context.Context, Action) error {
	return nil
}

func TestLockScenarioFactoriesRegisterImplementedCodesOnly(t *testing.T) {
	factories := LockScenarioFactories()
	for code := ScenarioCode(501); code <= 510; code++ {
		if code == 507 {
			continue
		}
		if factories[code] == nil {
			t.Fatalf("missing factory for %d", code)
		}
	}
	for code := ScenarioCode(520); code <= 540; code++ {
		if factories[code] == nil {
			t.Fatalf("missing factory for %d", code)
		}
	}
	for _, code := range []ScenarioCode{507, 511, 512} {
		if factories[code] != nil {
			t.Fatalf("deferred code %d is registered", code)
		}
	}
}

func TestPlanChangeFactoriesShareOneCoordinator(t *testing.T) {
	factories := LockScenarioFactories()
	var shared *PlanCoordinator
	for code := ScenarioCode(601); code <= 606; code++ {
		definition := DefaultScenarioCatalog().MustCode(code)
		scenario, err := factories[code](definition, Environment{})
		if err != nil {
			t.Fatalf("factory %d: %v", code, err)
		}
		planScenario, ok := scenario.(*coordinatedPlanChangeScenario)
		if !ok {
			t.Fatalf("factory %d returned %T", code, scenario)
		}
		if shared == nil {
			shared = planScenario.PlanChangeScenario.coordinator
			continue
		}
		if planScenario.PlanChangeScenario.coordinator != shared {
			t.Fatalf(
				"factory %d coordinator=%p want shared=%p",
				code,
				planScenario.PlanChangeScenario.coordinator,
				shared,
			)
		}
	}
}

func TestPlanChangeFactoriesSerializeMutationThroughHold(t *testing.T) {
	catalog := DefaultScenarioCatalog()
	for holderCode := ScenarioCode(601); holderCode <= 606; holderCode++ {
		holderDefinition := catalog.MustCode(holderCode)
		t.Run(holderDefinition.Name, func(t *testing.T) {
			factories := LockScenarioFactories()
			contenderCode := ScenarioCode(
				601 + (int(holderCode)-601+1)%6,
			)
			contenderDefinition := catalog.MustCode(contenderCode)
			holder, err := factories[holderCode](
				holderDefinition,
				Environment{},
			)
			if err != nil {
				t.Fatalf("holder factory %d: %v", holderCode, err)
			}
			contender, err := factories[contenderCode](
				contenderDefinition,
				Environment{},
			)
			if err != nil {
				t.Fatalf("contender factory %d: %v", contenderCode, err)
			}

			holderApplies := 0
			holderRuntime := planSerialTestRuntime(
				"holder-run",
				func(Action) { holderApplies++ },
			)
			if err := holder.Ramp(
				context.Background(),
				holderRuntime,
			); err != nil {
				t.Fatalf("holder ramp: %v", err)
			}
			if holderApplies == 0 {
				t.Fatal("holder ramp applied no plan mutation")
			}

			contenderApplies := 0
			contenderRuntime := planSerialTestRuntime(
				"contender-run",
				func(Action) { contenderApplies++ },
			)
			blocked, cancel := context.WithTimeout(
				context.Background(),
				100*time.Millisecond,
			)
			err = contender.Ramp(blocked, contenderRuntime)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("overlapping ramp error=%v want deadline exceeded", err)
			}
			if contenderApplies != 0 {
				t.Fatalf(
					"overlapping ramp applied %d mutations want=0",
					contenderApplies,
				)
			}

			// Empty baselines make the test Hold fail quickly after exercising
			// the real phase boundary. The serial gate must still be released.
			_ = holder.Hold(context.Background(), holderRuntime)
			if err := contender.Ramp(
				context.Background(),
				contenderRuntime,
			); err != nil {
				t.Fatalf("contender ramp after holder hold: %v", err)
			}
			_ = contender.Hold(context.Background(), contenderRuntime)
		})
	}
}

func planSerialTestRuntime(
	runID string,
	onApply func(Action),
) *Runtime {
	return &Runtime{
		Config: BenchConfig{Data: DataConfig{Schema: "Bench"}},
		RunID:  runID,
		Journal: NewJournal(
			&planSerialTestStore{},
			&planSerialTestExecutor{onApply: onApply},
			ProductGaussDB,
		),
	}
}
