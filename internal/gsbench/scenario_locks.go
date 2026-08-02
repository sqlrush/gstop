package gsbench

import (
	"context"
	"fmt"
)

// LockScenario adapts a declarative lock definition to the common scenario
// lifecycle. Definitions are resolved during Prepare because the schema and
// run ID are runtime values, never user supplied SQL fragments.
type LockScenario struct {
	definition LockDefinition
	engine     *LockEngine
}

func NewLockScenario(definition LockDefinition) Scenario {
	return &LockScenario{definition: definition}
}

func (s *LockScenario) Code() ScenarioCode { return s.definition.Code }
func (s *LockScenario) Name() string       { return s.definition.Name }
func (s *LockScenario) Strategy() string   { return "transaction_safe_lock" }

func (s *LockScenario) configureDefinition(rt *Runtime) error {
	if rt == nil {
		return fmt.Errorf("lock runtime is unavailable")
	}
	definition, ok := lockDefinitionForCode(
		s.definition.Code, rt.Config.Data.Schema, rt.RunID,
	)
	if !ok {
		return errLockDefinitionUnavailable(s.definition.Code)
	}
	if s.definition.Code >= 501 && s.definition.Code <= 503 {
		configured, err := configureLockDefinition(
			definition,
			rt.Config.LockWorkloads,
			rt.Config.Data.Schema,
			rt.RunID,
		)
		if err != nil {
			return fmt.Errorf("configure lock workload: %w", err)
		}
		definition = configured
	}
	s.definition = definition
	return nil
}

func (s *LockScenario) Prepare(ctx context.Context, rt *Runtime) error {
	if err := s.configureDefinition(rt); err != nil {
		return err
	}
	s.engine = NewLockEngine(s.definition)
	return s.engine.Prepare(ctx, rt)
}

func (s *LockScenario) Ramp(ctx context.Context, rt *Runtime) error {
	return s.engine.Ramp(ctx, rt)
}

func (s *LockScenario) Hold(ctx context.Context, rt *Runtime) error {
	return s.engine.Hold(ctx, rt)
}

func (s *LockScenario) Verify(ctx context.Context, rt *Runtime) (Result, error) {
	return s.engine.Verify(ctx, rt)
}

func (s *LockScenario) Stop(ctx context.Context, rt *Runtime) error {
	return s.engine.Stop(ctx, rt)
}

func (s *LockScenario) Restore(ctx context.Context, rt *Runtime) error {
	return s.engine.Restore(ctx, rt)
}
