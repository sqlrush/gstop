package gsbench

func LockScenarioFactories() map[ScenarioCode]ScenarioFactory {
	factories := make(map[ScenarioCode]ScenarioFactory, 43)
	register := func(code ScenarioCode) {
		factories[code] = func(definition ScenarioDefinition, _ Environment) (Scenario, error) {
			return NewLockScenario(LockDefinition{Code: definition.Code, Name: definition.Name}), nil
		}
	}
	for code := ScenarioCode(501); code <= 510; code++ {
		// A short nontransactional VACUUM cannot reliably prove it still owns
		// ShareUpdateExclusive before the DDL waiter starts. Keep 507 deferred
		// until that heavier orchestration is available rather than report it.
		if code == 507 {
			continue
		}
		register(code)
	}
	for code := ScenarioCode(520); code <= 540; code++ {
		register(code)
	}
	planCoordinator := &PlanCoordinator{}
	for code := ScenarioCode(601); code <= 606; code++ {
		code := code
		factories[code] = func(definition ScenarioDefinition, _ Environment) (Scenario, error) {
			return NewPlanChangeScenario(
				PlanScenarioDefinition{Code: code, Name: definition.Name},
				planCoordinator,
			), nil
		}
	}
	for code := ScenarioCode(621); code <= 625; code++ {
		code := code
		factories[code] = func(definition ScenarioDefinition, _ Environment) (Scenario, error) {
			return NewHardParseScenario(code, definition.Name, nil), nil
		}
	}
	return factories
}
