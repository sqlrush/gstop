package gsbench

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeActuator struct {
	target  int
	max     int
	history []int
}

func (a *fakeActuator) Target() int { return a.target }
func (a *fakeActuator) SetTarget(n int) error {
	a.target = n
	a.history = append(a.history, n)
	return nil
}

func TestControllerRampsGraduallyAndHoldsTarget(t *testing.T) {
	a := &fakeActuator{max: 8}
	c := Controller{
		Config:   ControllerConfig{Target: 60, Tolerance: 2, MinWorkers: 1, MaxWorkers: 8, Step: 1, RequiredSamples: 3, Interval: time.Millisecond},
		Actuator: a,
		Sample: func(context.Context) Sample {
			return Sample{Available: true, Value: float64(a.target * 20)}
		},
	}
	result := c.Run(context.Background())
	if !result.Reached || result.Actual != 60 || a.target != 3 {
		t.Fatalf("result=%+v history=%v", result, a.history)
	}
	for i := 1; i < len(a.history); i++ {
		if a.history[i]-a.history[i-1] > 1 {
			t.Fatalf("non-gradual ramp: %v", a.history)
		}
	}
}

func TestControllerRunToMinimumStopsAddingAtTarget(t *testing.T) {
	actuator := &fakeActuator{}
	values := []float64{80, 86, 90, 91, 90}
	index := 0
	result := (Controller{
		Config: ControllerConfig{
			Target: 90, MinWorkers: 1, MaxWorkers: 20,
			Step: 2, RequiredSamples: 3, Interval: time.Millisecond,
		},
		Actuator: actuator,
		Sample: func(context.Context) Sample {
			valueIndex := min(index, len(values)-1)
			index++
			return Sample{Available: true, Value: values[valueIndex]}
		},
	}).RunToMinimum(context.Background())
	if !result.Reached || result.Actual < 90 {
		t.Fatalf("result=%+v", result)
	}
	if actuator.Target() != result.Workers {
		t.Fatalf(
			"actuator target=%d result workers=%d",
			actuator.Target(),
			result.Workers,
		)
	}
	if index != 5 {
		t.Fatalf("samples=%d want=5", index)
	}
}

func TestControllerRunToMinimumFailsAtCeiling(t *testing.T) {
	actuator := &fakeActuator{}
	result := (Controller{
		Config: ControllerConfig{
			Target: 90, MinWorkers: 1, MaxWorkers: 2,
			Step: 1, RequiredSamples: 2, Interval: time.Millisecond,
		},
		Actuator: actuator,
		Sample: func(context.Context) Sample {
			return Sample{Available: true, Value: 50}
		},
	}).RunToMinimum(context.Background())
	if !result.Ceiling || result.Reached {
		t.Fatalf("result=%+v", result)
	}
}

func TestControllerRunToMinimumRequiresAvailableMetric(t *testing.T) {
	result := (Controller{
		Config: ControllerConfig{
			Target: 90, MinWorkers: 1, MaxWorkers: 2,
			Interval: time.Millisecond,
		},
		Actuator: &fakeActuator{},
		Sample: func(context.Context) Sample {
			return Sample{Available: false}
		},
	}).RunToMinimum(context.Background())
	if result.Err == nil || result.Samples != 1 {
		t.Fatalf("result=%+v", result)
	}
}

func TestControllerDoesNotReplaceLastSampleOnTimeout(t *testing.T) {
	a := &fakeActuator{}
	calls := 0
	c := Controller{
		Config:   ControllerConfig{Target: 90, Tolerance: 2, MinWorkers: 1, MaxWorkers: 2, Step: 1, RequiredSamples: 2, Interval: time.Millisecond},
		Actuator: a,
		Sample: func(context.Context) Sample {
			calls++
			if calls == 1 {
				return Sample{Available: true, Value: 40}
			}
			return Sample{Available: false}
		},
	}
	result := c.Run(context.Background())
	if result.LastSuccessful != 40 || !result.Ceiling {
		t.Fatalf("result=%+v", result)
	}
}

func TestControllerStopsAfterFiniteSamplesWhenMetricOscillates(t *testing.T) {
	a := &fakeActuator{}
	calls := 0
	c := Controller{
		Config:   ControllerConfig{Target: 50, Tolerance: 1, MinWorkers: 1, MaxWorkers: 8, Step: 1, RequiredSamples: 3, MaxSamples: 4, Interval: time.Millisecond},
		Actuator: a,
		Sample: func(context.Context) Sample {
			calls++
			if calls%2 == 0 {
				return Sample{Available: true, Value: 100}
			}
			return Sample{Available: true, Value: 0}
		},
	}
	result := c.Run(context.Background())
	if result.Samples != 4 || result.Reached || result.Err != nil {
		t.Fatalf("result=%+v", result)
	}
}

func TestControllerDefaultStepTraversesLargeRangeInAtMostTenAdjustments(t *testing.T) {
	a := &fakeActuator{}
	c := Controller{
		Config: ControllerConfig{
			Target: 100, MinWorkers: 1, MaxWorkers: 640,
			RequiredSamples: 1, Interval: time.Nanosecond,
		},
		Actuator: a,
		Sample: func(context.Context) Sample {
			return Sample{Available: true, Value: 0}
		},
	}
	result := c.Run(context.Background())
	if !result.Ceiling {
		t.Fatalf("result=%+v history=%v", result, a.history)
	}
	if len(a.history) < 2 || a.history[1]-a.history[0] <= 1 {
		t.Fatalf("large range did not use a proportional first adjustment: %v", a.history)
	}
	if upwardAdjustments := len(a.history) - 1; upwardAdjustments > 10 {
		t.Fatalf("large range required %d upward adjustments: %v", upwardAdjustments, a.history)
	}
}

func TestControllerDefaultStepRemainsOneForSmallRange(t *testing.T) {
	a := &fakeActuator{}
	c := Controller{
		Config: ControllerConfig{
			Target: 100, MinWorkers: 1, MaxWorkers: 8,
			RequiredSamples: 1, Interval: time.Nanosecond,
		},
		Actuator: a,
		Sample: func(context.Context) Sample {
			return Sample{Available: true, Value: 0}
		},
	}
	_ = c.Run(context.Background())
	for i := 1; i < len(a.history); i++ {
		if delta := a.history[i] - a.history[i-1]; delta != 1 {
			t.Fatalf("small range adjustment=%d history=%v", delta, a.history)
		}
	}
}

func TestControllerNarrowsWorkerAdjustmentNearTarget(t *testing.T) {
	a := &fakeActuator{}
	c := Controller{
		Config: ControllerConfig{
			Target: 95, MinWorkers: 1, MaxWorkers: 100,
			Step: 8, RequiredSamples: 1, Interval: time.Nanosecond,
		},
		Actuator: a,
		Sample: func(context.Context) Sample {
			switch a.target {
			case 1:
				return Sample{Available: true, Value: 0}
			case 9:
				return Sample{Available: true, Value: 90}
			default:
				return Sample{Available: true, Value: 95}
			}
		},
	}
	result := c.Run(context.Background())
	if !result.Reached {
		t.Fatalf("result=%+v history=%v", result, a.history)
	}
	want := []int{1, 9, 10}
	if len(a.history) != len(want) {
		t.Fatalf("history=%v want=%v", a.history, want)
	}
	for i := range want {
		if a.history[i] != want[i] {
			t.Fatalf("history=%v want=%v", a.history, want)
		}
	}
}

func TestControllerFineGrainedRampStartsSmallAndBracketsOvershoot(t *testing.T) {
	a := &fakeActuator{}
	calls := 0
	result := (Controller{
		Config: ControllerConfig{
			Target: 95, Tolerance: 2, MinWorkers: 1, MaxWorkers: 64_000,
			Step: 800, ExplorationStep: 100, RequiredSamples: 1,
			RequiredAdjustmentSamples: 1, Interval: time.Nanosecond,
		},
		Actuator: a,
		Sample: func(context.Context) Sample {
			calls++
			if calls == 1 {
				return Sample{}
			}
			switch {
			case a.target < 160:
				return Sample{Available: true, Value: 60}
			case a.target > 180:
				return Sample{Available: true, Value: 100}
			default:
				return Sample{Available: true, Value: 95}
			}
		},
	}).Run(context.Background())

	if !result.Reached || result.Workers < 160 || result.Workers > 180 {
		t.Fatalf("fine-grained ramp did not converge inside the bracket: result=%+v history=%v", result, a.history)
	}
	if len(a.history) < 2 || a.history[1] != 101 {
		t.Fatalf("unavailable first sample made a coarse jump: history=%v", a.history)
	}
	for _, target := range a.history {
		if target > 201 {
			t.Fatalf("ramp walked onto a saturated plateau: history=%v", a.history)
		}
	}
}

func TestControllerRunUntilDoesNotOvercorrectSmallCPUOvershoot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &fakeActuator{}
	calls := 0
	c := Controller{
		Config: ControllerConfig{
			Target: 95, Tolerance: 3, MinWorkers: 1, MaxWorkers: 640,
			Step: 8, RequiredSamples: 2, Interval: time.Nanosecond,
		},
		Actuator: a,
		Sample: func(context.Context) Sample {
			calls++
			switch calls {
			case 1:
				return Sample{Available: true, Value: 0}
			case 2:
				return Sample{Available: true, Value: 60}
			case 3:
				return Sample{Available: true, Value: 80}
			case 4:
				return Sample{Available: true, Value: 90}
			case 5:
				// The final 101 trace peaked at 99.26%.  Removing the full
				// eight-worker ramp step then drove CPU down to 70.71%.
				return Sample{Available: true, Value: 99.26}
			default:
				if calls == 7 {
					cancel()
				}
				switch a.target {
				case 14:
					return Sample{Available: true, Value: 95}
				case 7:
					return Sample{Available: true, Value: 70}
				default:
					return Sample{Available: true, Value: 80}
				}
			}
		},
	}

	result := c.RunUntil(ctx)
	if !result.Reached || result.Actual != 95 || result.Workers != 14 {
		t.Fatalf("small overshoot destabilized controller: result=%+v history=%v", result, a.history)
	}
	for i := 1; i < len(a.history); i++ {
		if drop := a.history[i-1] - a.history[i]; drop > 1 {
			t.Fatalf("small overshoot removed %d workers at once: history=%v", drop, a.history)
		}
	}
}

func TestControllerDoesNotHalveOnConflictingSampleAtLowerBound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &fakeActuator{}
	calls := 0
	result := (Controller{
		Config: ControllerConfig{
			Target: 95, Tolerance: 3, MinWorkers: 100, MaxWorkers: 2_000,
			Step: 800, ExplorationStep: 800, RequiredSamples: 3,
			RequiredAdjustmentSamples: 2, Interval: time.Nanosecond,
		},
		Actuator: a,
		Sample: func(context.Context) Sample {
			calls++
			switch calls {
			case 1, 2:
				return Sample{Available: true, Value: 60}
			case 3, 4:
				return Sample{Available: true, Value: 90}
			case 5, 6:
				return Sample{Available: true, Value: 100}
			case 7, 8, 9:
				return Sample{Available: true, Value: 90}
			case 10:
				return Sample{Available: true, Value: 100}
			default:
				cancel()
				return Sample{Available: true, Value: 100}
			}
		},
	}).RunUntil(ctx)

	if result.Err != nil {
		t.Fatalf("controller returned error: %+v", result)
	}
	if result.Workers < 1_400 {
		t.Fatalf("conflicting sample halved load: result=%+v history=%v", result, a.history)
	}
	if len(a.history) < 2 {
		t.Fatalf("controller did not adjust: history=%v", a.history)
	}
	if drop := a.history[len(a.history)-2] - a.history[len(a.history)-1]; drop > 100 {
		t.Fatalf("conflicting sample dropped %d load units: history=%v", drop, a.history)
	}
}

func TestControllerDoesNotFastRampOnConflictingSampleAtUpperBound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &fakeActuator{}
	calls := 0
	result := (Controller{
		Config: ControllerConfig{
			Target: 70, Tolerance: 3, MinWorkers: 100, MaxWorkers: 2_000,
			Step: 800, ExplorationStep: 800, RequiredSamples: 3,
			RequiredAdjustmentSamples: 2, Interval: time.Nanosecond,
		},
		Actuator: a,
		Sample: func(context.Context) Sample {
			calls++
			switch calls {
			case 1, 2:
				return Sample{Available: true, Value: 50}
			case 3:
				return Sample{Available: true, Value: 80}
			case 4:
				return Sample{Available: true, Value: 60}
			default:
				cancel()
				return Sample{Available: true, Value: 60}
			}
		},
	}).RunUntil(ctx)

	if result.Err != nil {
		t.Fatalf("controller returned error: %+v", result)
	}
	if result.Workers > 1_050 {
		t.Fatalf("conflicting sample used fast exploration step: result=%+v history=%v", result, a.history)
	}
	wantPrefix := []int{100, 900}
	if len(a.history) < len(wantPrefix)+1 || !reflect.DeepEqual(a.history[:2], wantPrefix) {
		t.Fatalf("unexpected controller history=%v", a.history)
	}
}

func TestControllerSettledModeIgnoresSingleSampleSpike(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &fakeActuator{}
	calls := 0
	result := (Controller{
		Config: ControllerConfig{
			Target: 50, Tolerance: 2, MinWorkers: 100, MaxWorkers: 2_000,
			Step: 800, ExplorationStep: 800, SettledStep: 25,
			RequiredSamples: 3, RequiredAdjustmentSamples: 2,
			RequiredSettlingSamples: 1,
			Interval:                time.Nanosecond,
		},
		Actuator: a,
		Sample: func(context.Context) Sample {
			calls++
			switch calls {
			case 1, 2, 3, 5:
				return Sample{Available: true, Value: 50}
			case 4:
				return Sample{Available: true, Value: 10}
			default:
				cancel()
				return Sample{Available: true, Value: 50}
			}
		},
	}).RunUntil(ctx)

	if result.Err != nil || len(a.history) != 1 || a.history[0] != 100 {
		t.Fatalf("single spike changed settled load: result=%+v history=%v", result, a.history)
	}
}

func TestControllerSettledModeCapsSustainedCorrections(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &fakeActuator{}
	calls := 0
	result := (Controller{
		Config: ControllerConfig{
			Target: 50, Tolerance: 2, MinWorkers: 100, MaxWorkers: 2_000,
			Step: 800, ExplorationStep: 800, SettledStep: 25,
			RequiredSamples: 3, RequiredAdjustmentSamples: 2,
			RequiredSettlingSamples: 1, AdjustmentCooldownSamples: 2,
			Interval: time.Nanosecond,
		},
		Actuator: a,
		Sample: func(context.Context) Sample {
			calls++
			switch {
			case calls <= 3:
				return Sample{Available: true, Value: 50}
			case calls <= 10:
				return Sample{Available: true, Value: 30}
			case calls <= 16:
				return Sample{Available: true, Value: 70}
			default:
				cancel()
				return Sample{Available: true, Value: 50}
			}
		},
	}).RunUntil(ctx)

	if result.Err != nil || len(a.history) < 3 {
		t.Fatalf("settled controller did not regulate: result=%+v history=%v", result, a.history)
	}
	if len(a.history) != 4 {
		t.Fatalf("settled cooldown allowed too many adjustments: history=%v", a.history)
	}
	for i := 1; i < len(a.history); i++ {
		if delta := a.history[i] - a.history[i-1]; delta < -25 || delta > 25 {
			t.Fatalf("settled correction exceeded 25 units: history=%v", a.history)
		}
	}
}

func TestControllerSettlesAfterFirstTargetBandObservation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &fakeActuator{}
	calls := 0
	result := (Controller{
		Config: ControllerConfig{
			Target: 50, Tolerance: 2, MinWorkers: 100, MaxWorkers: 2_000,
			Step: 800, ExplorationStep: 800, SettledStep: 25,
			RequiredSamples: 3, RequiredAdjustmentSamples: 2,
			RequiredSettlingSamples: 2,
			Interval:                time.Nanosecond,
		},
		Actuator: a,
		Sample: func(context.Context) Sample {
			calls++
			switch calls {
			case 1, 3:
				return Sample{Available: true, Value: 50}
			case 2:
				return Sample{Available: true, Value: 70}
			case 4, 5:
				return Sample{Available: true, Value: 30}
			default:
				cancel()
				return Sample{Available: true, Value: 30}
			}
		},
	}).RunUntil(ctx)

	if result.Err != nil {
		t.Fatalf("controller returned error: %+v", result)
	}
	if len(a.history) != 2 || a.history[0] != 100 || a.history[1] != 125 {
		t.Fatalf("first band observation did not enter settled mode: history=%v", a.history)
	}
}

func TestControllerRunUntilReadjustsAfterDroppingOutOfBand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &fakeActuator{}
	calls := 0
	c := Controller{
		Config: ControllerConfig{
			Target: 50, Tolerance: 1, MinWorkers: 1, MaxWorkers: 8,
			Step: 1, RequiredSamples: 2, Interval: time.Nanosecond,
		},
		Actuator: a,
		Sample: func(context.Context) Sample {
			calls++
			switch calls {
			case 1, 2:
				return Sample{Available: true, Value: 50}
			case 3:
				return Sample{Available: true, Value: 10}
			case 4:
				return Sample{Available: true, Value: 50}
			default:
				cancel()
				return Sample{Available: true, Value: 50}
			}
		},
	}
	result := c.RunUntil(ctx)
	if !result.Reached || result.Err != nil {
		t.Fatalf("result=%+v history=%v", result, a.history)
	}
	if a.target <= 1 {
		t.Fatalf("controller did not increase after the drop: %v", a.history)
	}
}

func TestControllerRunUntilPreservesPeakReachableValue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &fakeActuator{}
	calls := 0
	c := Controller{
		Config: ControllerConfig{
			Target: 90, Tolerance: 1, MinWorkers: 1, MaxWorkers: 8,
			Step: 1, RequiredSamples: 3, Interval: time.Nanosecond,
		},
		Actuator: a,
		Sample: func(context.Context) Sample {
			calls++
			if calls == 1 {
				return Sample{Available: true, Value: 80}
			}
			cancel()
			return Sample{Available: true, Value: 40}
		},
	}
	result := c.RunUntil(ctx)
	if result.Actual != 40 || result.ReachableMax != 80 || result.Err != nil {
		t.Fatalf("result=%+v", result)
	}
}

func TestControllerRunUntilClearsReachedAfterFinalDrop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &fakeActuator{}
	calls := 0
	result := (Controller{
		Config: ControllerConfig{
			Target: 50, Tolerance: 1, MinWorkers: 1, MaxWorkers: 8,
			Step: 1, RequiredSamples: 2, Interval: time.Nanosecond,
		},
		Actuator: a,
		Sample: func(context.Context) Sample {
			calls++
			if calls <= 2 {
				return Sample{Available: true, Value: 50}
			}
			cancel()
			return Sample{Available: true, Value: 10}
		},
	}).RunUntil(ctx)
	if result.Reached || result.Err != nil {
		t.Fatalf("stale target-band result=%+v", result)
	}
}

func TestControllerRunUntilPropagatesSamplerAndWorkerErrors(t *testing.T) {
	t.Run("sampler", func(t *testing.T) {
		sentinel := errors.New("sample failed")
		a := &fakeActuator{}
		result := (Controller{
			Config:   ControllerConfig{Target: 50, MinWorkers: 1, MaxWorkers: 2, Interval: time.Nanosecond},
			Actuator: a,
			Sample: func(context.Context) Sample {
				return Sample{Err: sentinel}
			},
		}).RunUntil(context.Background())
		if !errors.Is(result.Err, sentinel) {
			t.Fatalf("result=%+v", result)
		}
	})

	t.Run("worker", func(t *testing.T) {
		a := &fakeActuator{}
		result := (Controller{
			Config:   ControllerConfig{Target: 50, MinWorkers: 1, MaxWorkers: 2, Interval: time.Nanosecond},
			Actuator: a,
			Sample: func(context.Context) Sample {
				return Sample{Errors: 1}
			},
		}).RunUntil(context.Background())
		if result.Err == nil || !strings.Contains(result.Err.Error(), "workload execution errors") {
			t.Fatalf("result=%+v", result)
		}
	})
}
