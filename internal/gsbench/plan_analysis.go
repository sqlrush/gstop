package gsbench

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var planCostSuffixRE = regexp.MustCompile(`\s+\(cost=[^)]*\)(?:\s+\(actual[^)]*\))?\s*$`)

type PlanObservation struct {
	SQL                string
	PlanHash           string
	PlanText           string
	StructureSignature string
	ResultFingerprint  string
	Median             time.Duration
}

func NormalizePlanStructure(plan string) string {
	var structural []string
	for _, raw := range strings.Split(plan, "\n") {
		if !strings.Contains(raw, "(cost=") {
			continue
		}
		depth := (len(raw) - len(strings.TrimLeft(raw, " "))) / 2
		node := strings.TrimSpace(planCostSuffixRE.ReplaceAllString(raw, ""))
		node = strings.TrimSpace(strings.TrimPrefix(node, "->"))
		node = strings.Join(strings.Fields(node), " ")
		structural = append(structural, fmt.Sprintf("%02d:%s", depth, node))
	}
	return strings.Join(structural, "\n")
}

func NewPlanObservation(sqlText, planText, fingerprint string, durations []time.Duration) PlanObservation {
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	median := time.Duration(0)
	if len(sorted) > 0 {
		median = sorted[len(sorted)/2]
	}
	return PlanObservation{
		SQL:                sqlText,
		PlanText:           planText,
		PlanHash:           fmt.Sprintf("%x", sha256.Sum256([]byte(planText))),
		StructureSignature: NormalizePlanStructure(planText),
		ResultFingerprint:  fingerprint,
		Median:             median,
	}
}

func EvaluatePlanChange(name string, baseline, changed PlanObservation, minimumSlowdown float64) Result {
	result := Result{Scenario: name, Outcome: OutcomeFailed}
	if baseline.StructureSignature == changed.StructureSignature {
		result.Message = "execution plan structure did not change"
		return result
	}
	if baseline.ResultFingerprint != changed.ResultFingerprint {
		result.Message = "changed plan returned a different result fingerprint"
		return result
	}
	if baseline.Median <= 0 {
		result.Message = "baseline median elapsed time is unavailable"
		return result
	}
	slowdown := float64(changed.Median) / float64(baseline.Median)
	if slowdown < minimumSlowdown {
		result.Message = fmt.Sprintf("plan changed but slowdown was %.2fx, below %.2fx", slowdown, minimumSlowdown)
		return result
	}
	result.Outcome = OutcomeSuccess
	result.Message = fmt.Sprintf("plan structure changed and median elapsed time regressed %.2fx", slowdown)
	result.Evidence = []Evidence{{
		Metric: "slowdown_factor", Target: minimumSlowdown, Actual: slowdown, Available: true,
		Details: map[string]any{
			"sql":                  baseline.SQL,
			"baseline_plan":        baseline.PlanText,
			"changed_plan":         changed.PlanText,
			"baseline_signature":   baseline.StructureSignature,
			"changed_signature":    changed.StructureSignature,
			"baseline_median":      baseline.Median.String(),
			"changed_median":       changed.Median.String(),
			"baseline_fingerprint": baseline.ResultFingerprint,
			"changed_fingerprint":  changed.ResultFingerprint,
		},
	}}
	return result
}

func SelectChangedCandidate(baselines []PlanObservation, changedPlans map[string]string) (PlanObservation, string, error) {
	for _, baseline := range baselines {
		changedPlan := changedPlans[baseline.SQL]
		if changedPlan == "" {
			continue
		}
		if NormalizePlanStructure(changedPlan) != baseline.StructureSignature {
			return baseline, changedPlan, nil
		}
	}
	return PlanObservation{}, "", fmt.Errorf("no literal SQL candidate produced a structural plan change")
}
