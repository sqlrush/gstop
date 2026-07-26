package gsbench

import (
	"encoding/json"
	"strings"
	"time"
)

type EvidenceEnvelope struct {
	RunID        string           `json:"run_id"`
	ScenarioCode ScenarioCode     `json:"scenario_code"`
	Scenario     string           `json:"scenario"`
	Category     Category         `json:"category"`
	Product      Product          `json:"product"`
	Topology     Topology         `json:"topology"`
	Strategy     string           `json:"strategy"`
	Targets      []ScenarioTarget `json:"targets"`
	Risk         RiskLevel        `json:"risk"`
	Requirements []Requirement    `json:"requirements"`
	Evidence     []Evidence       `json:"evidence"`
	Restore      RestoreEvidence  `json:"restore"`
	Outcome      Outcome          `json:"outcome"`
	Message      string           `json:"message,omitempty"`
	StartedAt    time.Time        `json:"started_at"`
	EndedAt      time.Time        `json:"ended_at"`
}

func NewEvidenceEnvelope(runID string, result Result) EvidenceEnvelope {
	result = sanitizeResult(result)
	return EvidenceEnvelope{
		RunID:        safeEvidenceText(runID),
		ScenarioCode: result.ScenarioCode,
		Scenario:     result.Scenario,
		Category:     result.Category,
		Product:      result.Product,
		Topology:     result.Topology,
		Strategy:     result.Strategy,
		Targets:      append([]ScenarioTarget{}, result.Targets...),
		Risk:         result.Risk,
		Requirements: append([]Requirement{}, result.Requirements...),
		Evidence:     cloneEvidence(result.Evidence),
		Restore:      cloneRestoreEvidence(result.Restore),
		Outcome:      result.Outcome,
		Message:      result.Message,
		StartedAt:    result.StartedAt,
		EndedAt:      result.EndedAt,
	}
}

func (e EvidenceEnvelope) MarshalJSON() ([]byte, error) {
	type envelopeAlias EvidenceEnvelope
	safe := NewEvidenceEnvelope(e.RunID, Result{
		ScenarioCode: e.ScenarioCode,
		Scenario:     e.Scenario,
		Category:     e.Category,
		Product:      e.Product,
		Topology:     e.Topology,
		Strategy:     e.Strategy,
		Targets:      e.Targets,
		Risk:         e.Risk,
		Requirements: e.Requirements,
		Evidence:     e.Evidence,
		Restore:      e.Restore,
		Outcome:      e.Outcome,
		Message:      e.Message,
		StartedAt:    e.StartedAt,
		EndedAt:      e.EndedAt,
	})
	return json.Marshal(envelopeAlias(safe))
}

func (r Result) MarshalJSON() ([]byte, error) {
	type resultAlias Result
	return json.Marshal(resultAlias(sanitizeResult(r)))
}

func sanitizeResult(result Result) Result {
	result.Scenario = safeEvidenceText(result.Scenario)
	result.Strategy = safeEvidenceText(result.Strategy)
	result.Message = safeEvidenceText(result.Message)
	result.Targets = append([]ScenarioTarget{}, result.Targets...)
	for index := range result.Targets {
		result.Targets[index].Node = safeEvidenceText(
			result.Targets[index].Node,
		)
		result.Targets[index].Shard = safeEvidenceText(
			result.Targets[index].Shard,
		)
		result.Targets[index].Host = safeEvidenceText(
			result.Targets[index].Host,
		)
	}
	result.Requirements = append(
		[]Requirement{},
		result.Requirements...,
	)
	result.Evidence = cloneEvidence(result.Evidence)
	result.Restore = cloneRestoreEvidence(result.Restore)
	return result
}

func cloneEvidence(values []Evidence) []Evidence {
	if values == nil {
		return []Evidence{}
	}
	cloned := make([]Evidence, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].Metric = safeEvidenceText(value.Metric)
		if value.Details != nil {
			cloned[index].Details = sanitizeEvidenceMap(value.Details)
		}
	}
	return cloned
}

func cloneRestoreEvidence(value RestoreEvidence) RestoreEvidence {
	value.State = safeEvidenceText(value.State)
	value.Error = safeEvidenceText(value.Error)
	value.RunIDs = append([]string{}, value.RunIDs...)
	for index := range value.RunIDs {
		value.RunIDs[index] = safeEvidenceText(value.RunIDs[index])
	}
	return value
}

func sanitizeEvidenceMap(value map[string]any) map[string]any {
	cloned := make(map[string]any, len(value))
	for key, child := range value {
		if obviousSecretKey(key) {
			cloned[key] = "<redacted>"
			continue
		}
		cloned[key] = sanitizeEvidenceValue(child)
	}
	return cloned
}

func sanitizeEvidenceValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return sanitizeEvidenceMap(typed)
	case map[string]string:
		cloned := make(map[string]any, len(typed))
		for key, child := range typed {
			if obviousSecretKey(key) {
				cloned[key] = "<redacted>"
			} else {
				cloned[key] = safeEvidenceText(child)
			}
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for index, child := range typed {
			cloned[index] = sanitizeEvidenceValue(child)
		}
		return cloned
	case []string:
		cloned := make([]string, len(typed))
		for index, child := range typed {
			cloned[index] = safeEvidenceText(child)
		}
		return cloned
	case string:
		return safeEvidenceText(typed)
	default:
		return value
	}
}

func safeEvidenceText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if journalStringContainsCredentialMaterial(value) {
		return "<redacted>"
	}
	return value
}
