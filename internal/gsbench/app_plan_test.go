package gsbench

import (
	"strings"
	"testing"
)

func TestExitCodeForOutcome(t *testing.T) {
	for _, test := range []struct {
		outcome Outcome
		want    int
	}{
		{OutcomeSuccess, 0},
		{OutcomeNotApplicable, 0},
		{OutcomeDegraded, 3},
		{OutcomeNotImplemented, 1},
		{OutcomeFailed, 1},
		{OutcomeRestoreFailed, 1},
	} {
		if got := exitCodeForOutcome(test.outcome); got != test.want {
			t.Fatalf("outcome=%s exit code=%d want=%d", test.outcome, got, test.want)
		}
	}
}

func TestStoredScenarioCodesContainPlanChangeRejectsLegacyNames(
	t *testing.T,
) {
	tests := []struct {
		value string
		want  bool
	}{
		{"101,501", false},
		{"101,605", true},
		{"tp_cpu,plan_index_drop", false},
		{"plan_regression", false},
	}
	for _, test := range tests {
		if got := storedScenarioCodesContainPlanChange(
			test.value,
		); got != test.want {
			t.Fatalf("%q got=%v want=%v", test.value, got, test.want)
		}
	}
}

func TestDoctorEnvironmentReportIncludesTopologyCapabilitiesAndDecisions(t *testing.T) {
	env := Environment{
		Product: ProductGaussDB, Version: "GaussDB Kernel V500", Topology: TopologyCentralized,
		Supported:    true,
		Nodes:        []Node{{Name: "cn_1", Role: NodeRoleCN, Host: "127.0.0.1", Port: 5432}},
		Capabilities: CapabilitySet{CapabilityStatementHistory: true},
	}
	report := strings.Join(doctorEnvironmentReport(env, []ScenarioDefinition{
		{Code: 101, Name: "cpu", AppliesTo: []EnvironmentClass{EnvironmentCentralizedGaussDB}},
		{Code: 405, Name: "pooler", AppliesTo: []EnvironmentClass{EnvironmentDistributedGaussDB}},
		{Code: 621, Name: "hard_parse", AppliesTo: []EnvironmentClass{EnvironmentCentralizedGaussDB}, Requires: []Requirement{RequirementHardParseCounters}},
	}), "\n")
	for _, want := range []string{
		"product=GaussDB version=GaussDB Kernel V500 topology=centralized_gaussdb",
		"nodes=1",
		"node=cn_1 role=CN shard= host=127.0.0.1 port=5432",
		"capability=statement_history supported=true",
		"scenario=101 name=cpu decision=SUPPORTED",
		"scenario=405 name=pooler decision=NOT_APPLICABLE",
		"scenario=621 name=hard_parse decision=UNSUPPORTED",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}

func TestDoctorUsesDeclaredFallbackMetadata(t *testing.T) {
	env := Environment{
		Product: ProductOpenGauss, Topology: TopologyStandalone,
		Capabilities: make(CapabilitySet), Supported: true,
	}
	withFallback := ScenarioDefinition{
		Code:      621,
		Name:      "declared_fallback",
		Category:  CategoryPlan,
		Risk:      RiskA,
		AppliesTo: []EnvironmentClass{EnvironmentOpenGauss},
		Requires:  []Requirement{RequirementHardParseCounters},
		FallbackRequirements: []Requirement{
			RequirementHardParseCounters,
		},
	}
	withoutFallback := withFallback
	withoutFallback.Name = "no_fallback"
	withoutFallback.FallbackRequirements = nil

	decision, _ := doctorScenarioDecision(env, withFallback)
	if decision != "DEGRADED" {
		t.Fatalf("declared fallback decision=%s", decision)
	}
	decision, _ = doctorScenarioDecision(env, withoutFallback)
	if decision != "UNSUPPORTED" {
		t.Fatalf("undeclared fallback decision=%s", decision)
	}
}
