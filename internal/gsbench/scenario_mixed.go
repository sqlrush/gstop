package gsbench

import (
	"context"
)

func MixedWorkerTargets(total, tpPercent int) (tp, ap int) {
	if total <= 0 {
		return 0, 0
	}
	if tpPercent < 0 {
		tpPercent = 0
	}
	if tpPercent > 100 {
		tpPercent = 100
	}
	tp = total * tpPercent / 100
	if total > 1 {
		if tp == 0 {
			tp = 1
		}
		if tp == total {
			tp = total - 1
		}
	}
	ap = total - tp
	return tp, ap
}

type mixedActuator struct {
	tp, ap    *sqlWorkload
	tpPercent int
	target    int
}

func (a *mixedActuator) Target() int { return a.target }
func (a *mixedActuator) SetTarget(total int) error {
	tp, ap := MixedWorkerTargets(total, a.tpPercent)
	if err := a.tp.SetTarget(tp); err != nil {
		return err
	}
	if err := a.ap.SetTarget(ap); err != nil {
		return err
	}
	a.target = total
	return nil
}

type MixedScenario struct {
	tp, ap    *sqlWorkload
	actuator  *mixedActuator
	control   ControlResult
	available bool
	target    float64
}

func NewMixedScenario() *MixedScenario { return &MixedScenario{} }
func (s *MixedScenario) Name() string  { return "mixed_cpu" }
func (s *MixedScenario) Prepare(ctx context.Context, rt *Runtime) error {
	s.tp = buildTPWorkload(ctx, rt, "mixed_cpu_tp")
	s.ap = buildAPWorkload(ctx, rt, "mixed_cpu_ap")
	tpPercent := rt.Config.Raw.GetInt("scenario.mixed_cpu.tp_percent", 80)
	s.actuator = &mixedActuator{tp: s.tp, ap: s.ap, tpPercent: tpPercent}
	s.available = rt.CPU != nil && rt.Capabilities.DatabaseCPU
	return nil
}
func (s *MixedScenario) Ramp(ctx context.Context, rt *Runtime) error {
	s.target = float64(rt.Config.Safety.CPUTargetPercent)
	c := Controller{
		Config:   ControllerConfig{Target: s.target, Tolerance: 3, MinWorkers: 1, MaxWorkers: rt.Config.Safety.MaxWorkers, Step: 1, RequiredSamples: 3, Interval: rt.Config.Run.RampInterval},
		Actuator: s.actuator,
		Sample: func(ctx context.Context) Sample {
			errors := s.tp.Snapshot().Errors + s.ap.Snapshot().Errors
			if s.available {
				if value, ok := rt.CPU.SampleCPU(ctx); ok {
					return Sample{Available: true, Value: value, Errors: errors}
				}
			}
			return Sample{Errors: errors}
		},
	}
	s.control = c.Run(ctx)
	return s.control.Err
}
func (s *MixedScenario) Hold(ctx context.Context, rt *Runtime) error {
	return waitContext(ctx, rt.Config.Run.Duration)
}
func (s *MixedScenario) Verify(context.Context, *Runtime) (Result, error) {
	tp, ap := s.tp.Snapshot(), s.ap.Snapshot()
	combined := WorkerSnapshot{Operations: tp.Operations + ap.Operations, Errors: tp.Errors + ap.Errors, Active: tp.Active + ap.Active}
	return verifyCPUResult(s.Name(), s.target, s.available, s.control, combined), nil
}
func (s *MixedScenario) Stop(ctx context.Context, _ *Runtime) error {
	err := s.tp.Stop(ctx)
	if apErr := s.ap.Stop(ctx); err == nil {
		err = apErr
	}
	return err
}
func (s *MixedScenario) Restore(context.Context, *Runtime) error { return nil }
