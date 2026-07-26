package gsbench

import "time"

type Phase string

const (
	PhasePrepare Phase = "prepare"
	PhaseRamp    Phase = "ramp"
	PhaseHold    Phase = "hold"
	PhaseVerify  Phase = "verify"
	PhaseStop    Phase = "stop"
	PhaseRestore Phase = "restore"
)

type Outcome string

const (
	OutcomeSuccess  Outcome = "SUCCESS"
	OutcomeDegraded Outcome = "DEGRADED"
	OutcomeFailed   Outcome = "FAILED"
)

type Evidence struct {
	Metric    string
	Target    float64
	Actual    float64
	Available bool
	Details   map[string]any
}

type Result struct {
	Scenario  string
	Outcome   Outcome
	Message   string
	Evidence  []Evidence
	StartedAt time.Time
	EndedAt   time.Time
}
