package gsbench

import "time"

type Phase string

const (
	PhasePreflight     Phase = "preflight"
	PhasePrepare       Phase = "prepare"
	PhaseRamp          Phase = "ramp"
	PhaseHold          Phase = "hold"
	PhaseVerify        Phase = "verify"
	PhaseStop          Phase = "stop"
	PhaseRestore       Phase = "restore"
	PhaseVerifyRestore Phase = "verify_restore"
)

type Outcome string

const (
	OutcomeSuccess               Outcome = "SUCCESS"
	OutcomeCompletedWithWarnings Outcome = "COMPLETED_WITH_WARNINGS"
	OutcomeUnverified            Outcome = "UNVERIFIED"
	OutcomeNotApplicable         Outcome = "NOT_APPLICABLE"
	OutcomeDegraded              Outcome = "DEGRADED"
	OutcomeNotImplemented        Outcome = "NOT_IMPLEMENTED"
	OutcomeFailed                Outcome = "FAILED"
	OutcomeRestoreFailed         Outcome = "RESTORE_FAILED"
)

var outcomeRank = map[Outcome]int{
	OutcomeSuccess:               0,
	OutcomeCompletedWithWarnings: 1,
	OutcomeNotApplicable:         0,
	OutcomeUnverified:            1,
	OutcomeDegraded:              2,
	OutcomeNotImplemented:        3,
	OutcomeFailed:                4,
	OutcomeRestoreFailed:         5,
}

type Evidence struct {
	Metric    string         `json:"metric"`
	Target    float64        `json:"target"`
	Actual    float64        `json:"actual"`
	Available bool           `json:"available"`
	Details   map[string]any `json:"details,omitempty"`
}

type ScenarioTarget struct {
	Node  string   `json:"node,omitempty"`
	Role  NodeRole `json:"role,omitempty"`
	Shard string   `json:"shard,omitempty"`
	Host  string   `json:"host,omitempty"`
	Port  int      `json:"port,omitempty"`
}

type RestoreEvidence struct {
	State          string   `json:"state"`
	Outcome        Outcome  `json:"outcome"`
	RunIDs         []string `json:"run_ids,omitempty"`
	PlannedActions int      `json:"planned_actions"`
	Failed         bool     `json:"failed"`
	Error          string   `json:"error,omitempty"`
}

type Result struct {
	ScenarioCode ScenarioCode     `json:"scenario_code"`
	Scenario     string           `json:"scenario"`
	Category     Category         `json:"category"`
	Product      Product          `json:"product"`
	Topology     Topology         `json:"topology"`
	Strategy     string           `json:"strategy"`
	Targets      []ScenarioTarget `json:"targets"`
	Risk         RiskLevel        `json:"risk"`
	Requirements []Requirement    `json:"requirements"`
	Outcome      Outcome          `json:"outcome"`
	Message      string           `json:"message,omitempty"`
	Evidence     []Evidence       `json:"evidence"`
	Restore      RestoreEvidence  `json:"restore"`
	StartedAt    time.Time        `json:"started_at"`
	EndedAt      time.Time        `json:"ended_at"`
}
