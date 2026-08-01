package gsbench

import (
	"context"
	"fmt"
)

const (
	defaultMixedMaximum   = 20
	defaultMixedAPMaximum = 4
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

func MixedWorkerTargetsCapped(total, tpPercent, apMaximum int) (tp, ap int) {
	tp, ap = MixedWorkerTargets(total, tpPercent)
	if apMaximum >= 0 && ap > apMaximum {
		ap = apMaximum
		tp = total - ap
	}
	return tp, ap
}

type mixedActuator struct {
	tp, ap    *sqlWorkload
	tpPercent int
	apMaximum int
	target    int
}

func (a *mixedActuator) Target() int { return a.target }
func (a *mixedActuator) SetTarget(total int) error {
	tp, ap := MixedWorkerTargetsCapped(total, a.tpPercent, a.apMaximum)
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
	loop      continuousControl
	available bool
	target    float64
	maximum   int
	apMaximum int
	scanRows  int
}

func NewMixedScenario() *MixedScenario { return &MixedScenario{} }
func (s *MixedScenario) Code() ScenarioCode {
	return 103
}
func (s *MixedScenario) Name() string     { return "mixed_cpu" }
func (s *MixedScenario) Strategy() string { return "mixed_tp_ap_feedback" }
func (s *MixedScenario) Prepare(ctx context.Context, rt *Runtime) error {
	policy, err := LoadAPSafety(rt, "scenario.mixed_cpu", APSafety{
		CPUTargetPercent: 70, MaxWorkers: defaultMixedMaximum, ScanRows: 1_000_000,
	})
	if err != nil {
		return err
	}
	apMaximum := runtimeInt(rt, "scenario.mixed_cpu.max_ap_workers", defaultMixedAPMaximum)
	if apMaximum <= 0 {
		return fmt.Errorf("scenario.mixed_cpu.max_ap_workers must be positive")
	}
	apMaximum = min(apMaximum, policy.MaxWorkers)
	s.tp = buildTPWorkload(ctx, rt, "mixed_cpu_tp")
	s.ap, err = buildAPWorkload(ctx, rt, "mixed_cpu_ap", apMaximum, policy.ScanRows)
	if err != nil {
		return err
	}
	tpPercent := runtimeInt(rt, "scenario.mixed_cpu.tp_percent", 80)
	s.actuator = &mixedActuator{tp: s.tp, ap: s.ap, tpPercent: tpPercent, apMaximum: apMaximum}
	s.available = rt.CPU != nil && rt.Capabilities.DatabaseCPU
	s.target = float64(policy.CPUTargetPercent)
	s.maximum = policy.MaxWorkers
	s.apMaximum = apMaximum
	s.scanRows = policy.ScanRows
	if rt.Log != nil {
		rt.Log.Info("scenario=%s cpu_target=%d max_workers=%d max_ap_workers=%d scan_rows=%d ap_query_timeout=disabled",
			s.Name(), policy.CPUTargetPercent, policy.MaxWorkers, apMaximum, policy.ScanRows)
	}
	return nil
}
func (s *MixedScenario) Ramp(ctx context.Context, rt *Runtime) error {
	c := Controller{
		Config:   ControllerConfig{Target: s.target, Tolerance: 3, MinWorkers: 1, MaxWorkers: s.maximum, RequiredSamples: 3, Interval: rt.Config.Run.RampInterval},
		Actuator: s.actuator,
		Sample: func(ctx context.Context) Sample {
			snapshot := s.ExecutionSnapshot()
			if !s.available {
				return sampleCPU(ctx, nil, snapshot)
			}
			return sampleCPU(ctx, rt.CPU, snapshot)
		},
	}
	s.loop.Start(ctx, c)
	return nil
}
func (s *MixedScenario) Hold(ctx context.Context, rt *Runtime) error {
	var err error
	s.control, err = s.loop.Wait(ctx, rt.Config.Run.Duration)
	return err
}
func (s *MixedScenario) Verify(context.Context, *Runtime) (Result, error) {
	return verifyCPUResult(s.Name(), s.target, s.available, s.control, s.ExecutionSnapshot()), nil
}
func (s *MixedScenario) ExecutionSnapshot() WorkerSnapshot {
	if s.tp == nil || s.ap == nil {
		return WorkerSnapshot{}
	}
	tp, ap := s.tp.Snapshot(), s.ap.Snapshot()
	firstError := tp.FirstError
	if firstError == "" {
		firstError = ap.FirstError
	}
	return WorkerSnapshot{
		Target: tp.Target + ap.Target, Active: tp.Active + ap.Active,
		Operations: tp.Operations + ap.Operations, Errors: tp.Errors + ap.Errors,
		FirstError: firstError, TotalLatency: tp.TotalLatency + ap.TotalLatency,
	}
}
func (s *MixedScenario) Stop(ctx context.Context, _ *Runtime) error {
	s.control = s.loop.Stop()
	if s.tp == nil || s.ap == nil {
		return nil
	}
	err := s.tp.Stop(ctx)
	if apErr := s.ap.Stop(ctx); err == nil {
		err = apErr
	}
	return err
}
func (s *MixedScenario) Restore(context.Context, *Runtime) error { return nil }
