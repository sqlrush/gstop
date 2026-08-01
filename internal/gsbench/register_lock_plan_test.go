package gsbench

import "testing"

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
		planScenario, ok := scenario.(*PlanChangeScenario)
		if !ok {
			t.Fatalf("factory %d returned %T", code, scenario)
		}
		if shared == nil {
			shared = planScenario.coordinator
			continue
		}
		if planScenario.coordinator != shared {
			t.Fatalf(
				"factory %d coordinator=%p want shared=%p",
				code,
				planScenario.coordinator,
				shared,
			)
		}
	}
}
