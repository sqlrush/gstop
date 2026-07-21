package gsbench

import (
	"context"
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
