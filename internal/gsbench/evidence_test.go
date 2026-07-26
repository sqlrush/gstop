package gsbench

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEvidenceEnvelopeCarriesStableScenarioAndRestoreMetadata(
	t *testing.T,
) {
	started := time.Date(2026, 7, 26, 9, 10, 11, 12, time.UTC)
	ended := started.Add(5 * time.Second)
	result := Result{
		ScenarioCode: 702,
		Scenario:     "replication_sync_commit_block",
		Category:     CategoryReplicationCluster,
		Product:      ProductGaussDB,
		Topology:     TopologyDistributed,
		Strategy:     "distributed_dn_remote_apply",
		Targets: []ScenarioTarget{{
			Node: "dn_6001", Role: NodeRoleDNPrimary, Shard: "shard_1",
			Host: "db.internal", Port: 5432,
		}},
		Risk:         RiskA,
		Requirements: []Requirement{RequirementPrimaryStandby},
		Outcome:      OutcomeSuccess,
		Message:      "target reached",
		Evidence: []Evidence{{
			Metric: "commit_p95_ms", Target: 400, Actual: 420,
			Available: true,
		}},
		Restore: RestoreEvidence{
			State: "restored", Outcome: OutcomeSuccess,
			RunIDs: []string{"run-1"}, PlannedActions: 2,
		},
		StartedAt: started,
		EndedAt:   ended,
	}

	first, err := json.Marshal(NewEvidenceEnvelope("run-1", result))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(NewEvidenceEnvelope("run-1", result))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("JSON is unstable:\n%s\n%s", first, second)
	}
	var got struct {
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
		StartedAt    time.Time        `json:"started_at"`
		EndedAt      time.Time        `json:"ended_at"`
	}
	if err := json.Unmarshal(first, &got); err != nil {
		t.Fatal(err)
	}
	if got.RunID != "run-1" ||
		got.ScenarioCode != 702 ||
		got.Scenario != "replication_sync_commit_block" ||
		got.Category != CategoryReplicationCluster ||
		got.Product != ProductGaussDB ||
		got.Topology != TopologyDistributed ||
		got.Strategy != "distributed_dn_remote_apply" ||
		got.Risk != RiskA ||
		got.Outcome != OutcomeSuccess ||
		!got.StartedAt.Equal(started) ||
		!got.EndedAt.Equal(ended) {
		t.Fatalf("envelope=%s", first)
	}
	if len(got.Targets) != 1 ||
		got.Targets[0].Node != "dn_6001" ||
		len(got.Requirements) != 1 ||
		got.Requirements[0] != RequirementPrimaryStandby ||
		len(got.Evidence) != 1 ||
		got.Restore.State != "restored" ||
		got.Restore.PlannedActions != 2 {
		t.Fatalf("envelope=%s", first)
	}
}

func TestEvidenceEnvelopeAndRunLogRedactNestedSecrets(t *testing.T) {
	const secret = "evidence-secret"
	result := Result{
		ScenarioCode: 101,
		Scenario:     "tp_cpu",
		Message:      "password=" + secret,
		Evidence: []Evidence{{
			Metric:    "operations",
			Available: true,
			Details: map[string]any{
				"token": secret,
				"nested": map[string]any{
					"detail": "Authorization: Bearer " + secret,
				},
			},
		}},
		Restore: RestoreEvidence{
			State: "restore_failed",
			Error: "provider password=" + secret,
		},
	}
	envelope := NewEvidenceEnvelope("run-1", result)
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("envelope leaked secret: %s", body)
	}

	var screen bytes.Buffer
	log, err := NewRunLog(&screen, "", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Evidence(envelope); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(screen.String(), secret) {
		t.Fatalf("run log leaked secret: %q", screen.String())
	}
	if !strings.Contains(screen.String(), `"scenario_code":101`) ||
		!strings.Contains(screen.String(), `"evidence"`) {
		t.Fatalf("run log missing evidence envelope: %q", screen.String())
	}
}
