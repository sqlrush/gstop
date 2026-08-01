package gsbench

import (
	"context"
	"fmt"
	"math"
	"time"
)

type Sample struct {
	Value      float64
	Available  bool
	Errors     int64
	Throughput float64
	Err        error
}

type Actuator interface {
	Target() int
	SetTarget(int) error
}

type ControllerConfig struct {
	Target          float64
	Tolerance       float64
	MinWorkers      int
	MaxWorkers      int
	Step            int
	RequiredSamples int
	MaxSamples      int
	Interval        time.Duration
}

type ControlResult struct {
	Reached        bool
	Ceiling        bool
	Measured       bool
	Actual         float64
	LastSuccessful float64
	ReachableMax   float64
	Workers        int
	Samples        int
	Err            error
}

type Controller struct {
	Config   ControllerConfig
	Actuator Actuator
	Sample   func(context.Context) Sample
}

func workerRampAdjustment(cfg ControllerConfig, sample Sample) int {
	step := max(1, cfg.Step)
	if !sample.Available || cfg.Target <= 0 || sample.Value <= 0 ||
		sample.Value >= cfg.Target {
		return step
	}
	remaining := (cfg.Target - sample.Value) / cfg.Target
	return max(1, min(step, int(math.Ceil(float64(step)*remaining))))
}

func (c Controller) Run(ctx context.Context) ControlResult {
	return c.run(ctx, false)
}

// RunUntil continuously regulates the actuator until the context completes,
// an execution dependency fails, or repeated samples confirm that the worker
// ceiling cannot reach the target. Entering the target band is retained as
// evidence but does not stop regulation.
func (c Controller) RunUntil(ctx context.Context) ControlResult {
	return c.run(ctx, true)
}

func (c Controller) run(ctx context.Context, continuous bool) ControlResult {
	cfg := c.Config
	if cfg.MinWorkers <= 0 {
		cfg.MinWorkers = 1
	}
	if cfg.MaxWorkers < cfg.MinWorkers {
		cfg.MaxWorkers = cfg.MinWorkers
	}
	if cfg.Step <= 0 {
		cfg.Step = max(1, (cfg.MaxWorkers-cfg.MinWorkers+9)/10)
	}
	if cfg.RequiredSamples <= 0 {
		cfg.RequiredSamples = 3
	}
	if cfg.MaxSamples <= 0 {
		cfg.MaxSamples = max(cfg.RequiredSamples, cfg.MaxWorkers*4)
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.Tolerance <= 0 {
		cfg.Tolerance = 2
	}
	if c.Actuator == nil {
		return ControlResult{Err: fmt.Errorf("controller actuator is unavailable")}
	}
	if c.Sample == nil {
		return ControlResult{Err: fmt.Errorf("controller sampler is unavailable")}
	}
	if err := c.Actuator.SetTarget(cfg.MinWorkers); err != nil {
		return ControlResult{Err: err}
	}
	result := ControlResult{Workers: c.Actuator.Target()}
	var inBand int
	var ceilingSamples int
	var haveReachable bool
	for {
		if err := ctx.Err(); err != nil {
			if !continuous {
				result.Err = err
			}
			result.Workers = c.Actuator.Target()
			return result
		}
		sample := c.Sample(ctx)
		result.Samples++
		current := c.Actuator.Target()
		result.Workers = current
		if sample.Err != nil {
			result.Err = sample.Err
			return result
		}
		if sample.Errors > 0 {
			result.Err = fmt.Errorf("workload execution errors=%d", sample.Errors)
			return result
		}
		if !sample.Available {
			inBand = 0
			if continuous {
				result.Reached = false
			}
			if current >= cfg.MaxWorkers {
				ceilingSamples++
				if ceilingSamples >= cfg.RequiredSamples {
					result.Ceiling = true
					return result
				}
			} else {
				ceilingSamples = 0
				adjustment := workerRampAdjustment(cfg, sample)
				if err := c.Actuator.SetTarget(min(cfg.MaxWorkers, current+adjustment)); err != nil {
					result.Err = err
					return result
				}
				result.Workers = c.Actuator.Target()
			}
		} else {
			result.Measured = true
			result.Actual = sample.Value
			result.LastSuccessful = sample.Value
			if !haveReachable || sample.Value > result.ReachableMax {
				result.ReachableMax = sample.Value
				haveReachable = true
			}
			if math.Abs(sample.Value-cfg.Target) <= cfg.Tolerance {
				inBand++
				ceilingSamples = 0
				if inBand >= cfg.RequiredSamples {
					result.Reached = true
					if !continuous {
						return result
					}
				}
			} else {
				inBand = 0
				if continuous {
					result.Reached = false
				}
				switch {
				case sample.Value < cfg.Target:
					if current >= cfg.MaxWorkers {
						ceilingSamples++
						if ceilingSamples >= cfg.RequiredSamples {
							result.Ceiling = true
							return result
						}
					} else {
						ceilingSamples = 0
						adjustment := workerRampAdjustment(cfg, sample)
						if err := c.Actuator.SetTarget(min(cfg.MaxWorkers, current+adjustment)); err != nil {
							result.Err = err
							return result
						}
						result.Workers = c.Actuator.Target()
					}
				case sample.Value > cfg.Target && current > cfg.MinWorkers:
					ceilingSamples = 0
					if err := c.Actuator.SetTarget(max(cfg.MinWorkers, current-cfg.Step)); err != nil {
						result.Err = err
						return result
					}
					result.Workers = c.Actuator.Target()
				default:
					ceilingSamples = 0
				}
			}
		}
		if !continuous && result.Samples >= cfg.MaxSamples {
			return result
		}
		if err := ctx.Err(); err != nil {
			if !continuous {
				result.Err = err
			}
			result.Workers = c.Actuator.Target()
			return result
		}
		if err := waitContext(ctx, cfg.Interval); err != nil {
			if !continuous {
				result.Err = err
			}
			result.Workers = c.Actuator.Target()
			return result
		}
	}
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
