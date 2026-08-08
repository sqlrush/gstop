package gsbench

import (
	"context"
	"fmt"
	"math"
	"sort"
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
	Target                    float64
	Tolerance                 float64
	MinWorkers                int
	MaxWorkers                int
	Step                      int
	ExplorationStep           int
	SettledStep               int
	RequiredSamples           int
	RequiredAdjustmentSamples int
	RequiredSettlingSamples   int
	AdjustmentCooldownSamples int
	MaxSamples                int
	Interval                  time.Duration
}

type ControlResult struct {
	Reached         bool
	Ceiling         bool
	Measured        bool
	Actual          float64
	LastSuccessful  float64
	ReachableMax    float64
	Workers         int
	ResidentWorkers int
	Samples         int
	Err             error
}

type Controller struct {
	Config   ControllerConfig
	Actuator Actuator
	Sample   func(context.Context) Sample
}

func workerRampAdjustment(cfg ControllerConfig, sample Sample) int {
	return actuatorRampAdjustment(cfg, sample, max(1, cfg.Step))
}

func actuatorRampAdjustment(
	cfg ControllerConfig,
	sample Sample,
	current int,
) int {
	step := max(1, cfg.Step)
	if !sample.Available || cfg.Target <= 0 || sample.Value <= 0 {
		initial := cfg.ExplorationStep
		if initial <= 0 {
			initial = step
		}
		return max(1, min(step, initial))
	}
	deviation := math.Abs(cfg.Target-sample.Value) / cfg.Target
	base := min(step, max(1, current))
	return max(1, min(step, int(math.Ceil(float64(base)*deviation))))
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

// RunToMinimum monotonically increases pressure until consecutive real
// samples prove the requested lower bound. It never reduces the actuator, so
// its returned worker target can be frozen for the hold phase.
func (c Controller) RunToMinimum(ctx context.Context) ControlResult {
	cfg := normalizedControllerConfig(c.Config)
	result := ControlResult{}
	if c.Actuator == nil || c.Sample == nil {
		result.Err = fmt.Errorf("controller actuator and sampler are required")
		return result
	}
	if err := c.Actuator.SetTarget(cfg.MinWorkers); err != nil {
		result.Err = err
		return result
	}
	updateControlWorkers(&result, c.Actuator)
	consecutive := 0
	for {
		if err := ctx.Err(); err != nil {
			result.Err = err
			updateControlWorkers(&result, c.Actuator)
			return result
		}
		sample := c.Sample(ctx)
		result.Samples++
		updateControlWorkers(&result, c.Actuator)
		if sample.Err != nil || sample.Errors > 0 || !sample.Available {
			result.Err = minimumTargetSampleError(sample)
			return result
		}
		result.Measured = true
		result.Actual = sample.Value
		result.LastSuccessful = sample.Value
		result.ReachableMax = max(result.ReachableMax, sample.Value)
		if sample.Value >= cfg.Target {
			consecutive++
			if consecutive >= cfg.RequiredSamples {
				result.Reached = true
				return result
			}
		} else {
			consecutive = 0
			current := c.Actuator.Target()
			if current >= cfg.MaxWorkers {
				result.Ceiling = true
				return result
			}
			step := actuatorRampAdjustment(cfg, sample, current)
			if err := c.Actuator.SetTarget(
				min(cfg.MaxWorkers, current+step),
			); err != nil {
				result.Err = err
				return result
			}
			updateControlWorkers(&result, c.Actuator)
		}
		if err := waitContext(ctx, cfg.Interval); err != nil {
			result.Err = err
			updateControlWorkers(&result, c.Actuator)
			return result
		}
	}
}

func minimumTargetSampleError(sample Sample) error {
	switch {
	case sample.Err != nil:
		return sample.Err
	case sample.Errors > 0:
		return fmt.Errorf("workload execution errors=%d", sample.Errors)
	case !sample.Available:
		return fmt.Errorf("minimum target metric is unavailable")
	default:
		return nil
	}
}

func normalizedControllerConfig(cfg ControllerConfig) ControllerConfig {
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
	if cfg.RequiredAdjustmentSamples <= 0 {
		cfg.RequiredAdjustmentSamples = 1
	}
	if cfg.RequiredSettlingSamples <= 0 {
		cfg.RequiredSettlingSamples = 1
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
	return cfg
}

func (c Controller) run(ctx context.Context, continuous bool) ControlResult {
	cfg := normalizedControllerConfig(c.Config)
	if c.Actuator == nil {
		return ControlResult{Err: fmt.Errorf("controller actuator is unavailable")}
	}
	if c.Sample == nil {
		return ControlResult{Err: fmt.Errorf("controller sampler is unavailable")}
	}
	if err := c.Actuator.SetTarget(cfg.MinWorkers); err != nil {
		return ControlResult{Err: err}
	}
	result := ControlResult{}
	updateControlWorkers(&result, c.Actuator)
	var inBand int
	var ceilingSamples int
	var haveReachable bool
	var lowerTarget, upperTarget int
	var haveLowerTarget, haveUpperTarget bool
	var settled bool
	var settledValues []float64
	var bandObservations int
	var adjustmentCooldown int
	var pendingDirection int
	var pendingSamples int
	resetPendingAdjustment := func() {
		pendingDirection = 0
		pendingSamples = 0
	}
	adjustmentConfirmed := func(direction int) bool {
		if direction != pendingDirection {
			pendingDirection = direction
			pendingSamples = 0
		}
		pendingSamples++
		if pendingSamples < cfg.RequiredAdjustmentSamples {
			return false
		}
		resetPendingAdjustment()
		return true
	}
	for {
		if err := ctx.Err(); err != nil {
			if !continuous {
				result.Err = err
			}
			updateControlWorkers(&result, c.Actuator)
			return result
		}
		sample := c.Sample(ctx)
		result.Samples++
		current := c.Actuator.Target()
		updateControlWorkers(&result, c.Actuator)
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
			bandObservations = 0
			resetPendingAdjustment()
			settledValues = nil
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
				adjustment := actuatorRampAdjustment(cfg, sample, current)
				if err := c.Actuator.SetTarget(min(cfg.MaxWorkers, current+adjustment)); err != nil {
					result.Err = err
					return result
				}
				bandObservations = 0
				updateControlWorkers(&result, c.Actuator)
			}
		} else {
			result.Measured = true
			result.Actual = sample.Value
			result.LastSuccessful = sample.Value
			if !haveReachable || sample.Value > result.ReachableMax {
				result.ReachableMax = sample.Value
				haveReachable = true
			}
			controlSample := sample
			if settled {
				settledValues = append(settledValues, sample.Value)
				if len(settledValues) > 3 {
					settledValues = settledValues[len(settledValues)-3:]
				}
				controlSample.Value = medianControlValue(settledValues)
			}
			coolingDown := settled && adjustmentCooldown > 0
			if coolingDown {
				adjustmentCooldown--
			}
			if math.Abs(controlSample.Value-cfg.Target) <= cfg.Tolerance {
				inBand++
				ceilingSamples = 0
				resetPendingAdjustment()
				if cfg.SettledStep > 0 && !settled {
					bandObservations++
					if bandObservations >= cfg.RequiredSettlingSamples {
						settled = true
						settledValues = []float64{sample.Value}
						haveLowerTarget = false
						haveUpperTarget = false
					}
				}
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
				case controlSample.Value < cfg.Target:
					if settled {
						if coolingDown {
							resetPendingAdjustment()
							ceilingSamples = 0
							break
						}
						if current >= cfg.MaxWorkers {
							resetPendingAdjustment()
							ceilingSamples++
							if ceilingSamples >= cfg.RequiredSamples {
								result.Ceiling = true
								return result
							}
						} else if adjustmentConfirmed(1) {
							ceilingSamples = 0
							adjustment := min(
								cfg.SettledStep,
								actuatorRampAdjustment(cfg, controlSample, current),
							)
							if err := c.Actuator.SetTarget(min(cfg.MaxWorkers, current+max(1, adjustment))); err != nil {
								result.Err = err
								return result
							}
							adjustmentCooldown = cfg.AdjustmentCooldownSamples
							updateControlWorkers(&result, c.Actuator)
						}
						break
					}
					sameUpperTarget := haveUpperTarget && current == upperTarget
					if haveUpperTarget && current > upperTarget {
						haveUpperTarget = false
					}
					if !haveLowerTarget || current > lowerTarget {
						lowerTarget, haveLowerTarget = current, true
					}
					if current >= cfg.MaxWorkers {
						resetPendingAdjustment()
						ceilingSamples++
						if ceilingSamples >= cfg.RequiredSamples {
							result.Ceiling = true
							return result
						}
					} else if adjustmentConfirmed(1) {
						ceilingSamples = 0
						next := 0
						if haveUpperTarget && upperTarget > current {
							next = current + max(1, (upperTarget-current)/2)
						} else {
							adjustment := actuatorRampAdjustment(cfg, controlSample, current)
							if sameUpperTarget {
								haveUpperTarget = false
							} else if cfg.ExplorationStep > 0 {
								adjustment = max(adjustment, min(cfg.Step, cfg.ExplorationStep))
							}
							next = min(cfg.MaxWorkers, current+adjustment)
						}
						if err := c.Actuator.SetTarget(next); err != nil {
							result.Err = err
							return result
						}
						bandObservations = 0
						updateControlWorkers(&result, c.Actuator)
					}
				case controlSample.Value > cfg.Target && current > cfg.MinWorkers:
					if settled {
						ceilingSamples = 0
						if coolingDown {
							resetPendingAdjustment()
							break
						}
						if adjustmentConfirmed(-1) {
							adjustment := min(
								cfg.SettledStep,
								actuatorRampAdjustment(cfg, controlSample, current),
							)
							if err := c.Actuator.SetTarget(max(cfg.MinWorkers, current-max(1, adjustment))); err != nil {
								result.Err = err
								return result
							}
							adjustmentCooldown = cfg.AdjustmentCooldownSamples
							updateControlWorkers(&result, c.Actuator)
						}
						break
					}
					sameLowerTarget := haveLowerTarget && current == lowerTarget
					if haveLowerTarget && current < lowerTarget {
						haveLowerTarget = false
					}
					if !haveUpperTarget || current < upperTarget {
						upperTarget, haveUpperTarget = current, true
					}
					ceilingSamples = 0
					if !adjustmentConfirmed(-1) {
						break
					}
					next := 0
					if haveLowerTarget && lowerTarget < current {
						next = lowerTarget + (current-lowerTarget)/2
					} else if sameLowerTarget {
						haveLowerTarget = false
						adjustment := actuatorRampAdjustment(cfg, controlSample, current)
						next = max(cfg.MinWorkers, current-adjustment)
					} else {
						next = max(cfg.MinWorkers, current/2)
					}
					if err := c.Actuator.SetTarget(next); err != nil {
						result.Err = err
						return result
					}
					bandObservations = 0
					updateControlWorkers(&result, c.Actuator)
				default:
					ceilingSamples = 0
					resetPendingAdjustment()
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
			updateControlWorkers(&result, c.Actuator)
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

func medianControlValue(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	middle := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[middle]
	}
	return (ordered[middle-1] + ordered[middle]) / 2
}

type residentWorkerActuator interface {
	ResidentWorkers() int
}

func updateControlWorkers(result *ControlResult, actuator Actuator) {
	result.Workers = actuator.Target()
	result.ResidentWorkers = result.Workers
	if resident, ok := actuator.(residentWorkerActuator); ok {
		result.ResidentWorkers = resident.ResidentWorkers()
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
