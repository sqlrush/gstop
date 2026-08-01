package gsbench

import (
	"context"
	"errors"
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
