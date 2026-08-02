package gsbench

import (
	"context"
	"fmt"
	"sync"
)

type planMutationHoldCoordinator struct {
	permit chan struct{}
}

func newPlanMutationHoldCoordinator() *planMutationHoldCoordinator {
	coordinator := &planMutationHoldCoordinator{
		permit: make(chan struct{}, 1),
	}
	coordinator.permit <- struct{}{}
	return coordinator
}

func (c *planMutationHoldCoordinator) acquire(
	ctx context.Context,
) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.permit:
	}
	var once sync.Once
	return func() {
		once.Do(func() { c.permit <- struct{}{} })
	}, nil
}

type coordinatedPlanChangeScenario struct {
	*PlanChangeScenario
	mutationHold *planMutationHoldCoordinator
	phaseMu      sync.Mutex
	phaseRelease func()
}

func (s *coordinatedPlanChangeScenario) Ramp(
	ctx context.Context,
	rt *Runtime,
) error {
	release, err := s.mutationHold.acquire(ctx)
	if err != nil {
		return err
	}
	s.phaseMu.Lock()
	if s.phaseRelease != nil {
		s.phaseMu.Unlock()
		release()
		return fmt.Errorf("plan mutation/hold phase is already active")
	}
	s.phaseRelease = release
	s.phaseMu.Unlock()
	if err := s.PlanChangeScenario.Ramp(ctx, rt); err != nil {
		s.releaseMutationHold()
		return err
	}
	return nil
}

func (s *coordinatedPlanChangeScenario) Hold(
	ctx context.Context,
	rt *Runtime,
) error {
	defer s.releaseMutationHold()
	return s.PlanChangeScenario.Hold(ctx, rt)
}

func (s *coordinatedPlanChangeScenario) Stop(
	ctx context.Context,
	rt *Runtime,
) error {
	defer s.releaseMutationHold()
	return s.PlanChangeScenario.Stop(ctx, rt)
}

func (s *coordinatedPlanChangeScenario) releaseMutationHold() {
	s.phaseMu.Lock()
	release := s.phaseRelease
	s.phaseRelease = nil
	s.phaseMu.Unlock()
	if release != nil {
		release()
	}
}

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
	mutationHold := newPlanMutationHoldCoordinator()
	for code := ScenarioCode(601); code <= 606; code++ {
		code := code
		factories[code] = func(definition ScenarioDefinition, _ Environment) (Scenario, error) {
			return &coordinatedPlanChangeScenario{
				PlanChangeScenario: NewPlanChangeScenario(
					PlanScenarioDefinition{Code: code, Name: definition.Name},
					planCoordinator,
				),
				mutationHold: mutationHold,
			}, nil
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
