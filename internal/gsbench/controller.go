package gsbench

import (
	"context"
	"math"
	"time"
)

type Sample struct {
	Value      float64
	Available  bool
	Errors     int64
	Throughput float64
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
	Actual         float64
	LastSuccessful float64
	Workers        int
	Samples        int
	Err            error
}

type Controller struct {
	Config   ControllerConfig
	Actuator Actuator
	Sample   func(context.Context) Sample
}

func (c Controller) Run(ctx context.Context) ControlResult {
	cfg := c.Config
	if cfg.Step <= 0 {
		cfg.Step = 1
	}
	if cfg.MinWorkers <= 0 {
		cfg.MinWorkers = 1
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
	if err := c.Actuator.SetTarget(cfg.MinWorkers); err != nil {
		return ControlResult{Err: err}
	}
	var result ControlResult
	var inBand int
	var previousErrors int64
	for {
		if err := waitContext(ctx, cfg.Interval); err != nil {
			result.Err = err
			result.Workers = c.Actuator.Target()
			return result
		}
		sample := c.Sample(ctx)
		result.Samples++
		current := c.Actuator.Target()
		if result.Samples >= cfg.MaxSamples && !(sample.Available && math.Abs(sample.Value-cfg.Target) <= cfg.Tolerance && inBand+1 >= cfg.RequiredSamples) {
			result.Workers = current
			return result
		}
		if !sample.Available {
			if current >= cfg.MaxWorkers {
				result.Ceiling = true
				result.Workers = current
				return result
			}
			if err := c.Actuator.SetTarget(min(cfg.MaxWorkers, current+cfg.Step)); err != nil {
				result.Err = err
				return result
			}
			continue
		}
		result.Actual = sample.Value
		result.LastSuccessful = sample.Value
		if sample.Errors > previousErrors && current > cfg.MinWorkers {
			_ = c.Actuator.SetTarget(max(cfg.MinWorkers, current-cfg.Step))
			previousErrors = sample.Errors
			inBand = 0
			continue
		}
		previousErrors = sample.Errors
		if math.Abs(sample.Value-cfg.Target) <= cfg.Tolerance {
			inBand++
			if inBand >= cfg.RequiredSamples {
				result.Reached = true
				result.Workers = current
				return result
			}
			continue
		}
		inBand = 0
		switch {
		case sample.Value < cfg.Target:
			if current >= cfg.MaxWorkers {
				result.Ceiling = true
				result.Workers = current
				return result
			}
			if err := c.Actuator.SetTarget(min(cfg.MaxWorkers, current+cfg.Step)); err != nil {
				result.Err = err
				return result
			}
		case sample.Value > cfg.Target && current > cfg.MinWorkers:
			if err := c.Actuator.SetTarget(max(cfg.MinWorkers, current-cfg.Step)); err != nil {
				result.Err = err
				return result
			}
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
