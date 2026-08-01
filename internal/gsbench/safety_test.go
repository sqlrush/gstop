package gsbench

import (
	"reflect"
	"testing"
)

func TestAuthorizeScenarioFailsClosedByRiskAndGate(t *testing.T) {
	provider := Environment{Capabilities: CapabilitySet{CapabilityExternalFaultProvider: true}}
	admin := BenchConfig{Safety: SafetyConfig{AllowAdminMutation: true}}
	infrastructure := BenchConfig{Safety: SafetyConfig{AllowInfrastructureFault: true}}
	tests := []struct {
		name    string
		def     ScenarioDefinition
		cfg     BenchConfig
		options CLIOptions
		env     Environment
		wantErr bool
	}{
		{"risk A needs no extra authorization", ScenarioDefinition{Code: 101, Risk: RiskA}, BenchConfig{}, CLIOptions{}, Environment{}, false},
		{"empty risk is rejected", ScenarioDefinition{Code: 101}, BenchConfig{}, CLIOptions{}, Environment{}, true},
		{"unknown risk is rejected", ScenarioDefinition{Code: 101, Risk: RiskLevel("D")}, BenchConfig{}, CLIOptions{}, Environment{}, true},
		{"risk B requires admin mutation", ScenarioDefinition{Code: 209, Risk: RiskB}, BenchConfig{}, CLIOptions{AllowRisk: RiskB}, Environment{}, true},
		{"risk B requires CLI authorization", ScenarioDefinition{Code: 209, Risk: RiskB}, admin, CLIOptions{AllowRisk: RiskA}, Environment{}, true},
		{"risk B accepts CLI C", ScenarioDefinition{Code: 209, Risk: RiskB}, admin, CLIOptions{AllowRisk: RiskC}, Environment{}, false},
		{"risk C requires infrastructure fault authorization", ScenarioDefinition{Code: 343, Risk: RiskC}, BenchConfig{}, CLIOptions{AllowRisk: RiskC}, provider, true},
		{"risk C requires CLI C", ScenarioDefinition{Code: 343, Risk: RiskC}, infrastructure, CLIOptions{AllowRisk: RiskB}, provider, true},
		{"risk C requires provider", ScenarioDefinition{Code: 343, Risk: RiskC}, infrastructure, CLIOptions{AllowRisk: RiskC}, Environment{}, true},
		{"risk C accepts every gate", ScenarioDefinition{Code: 343, Risk: RiskC}, infrastructure, CLIOptions{AllowRisk: RiskC}, provider, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := AuthorizeScenario(test.def, test.cfg, test.options, test.env)
			if (err != nil) != test.wantErr {
				t.Fatalf("AuthorizeScenario() error = %v, want error=%t", err, test.wantErr)
			}
		})
	}
}

func TestAuthorizeRiskCRequiresConfigCLIAndProvider(t *testing.T) {
	def := ScenarioDefinition{Code: 343, Risk: RiskC}
	cfg := BenchConfig{Safety: SafetyConfig{
		AllowInfrastructureFault: true,
	}}
	env := Environment{Capabilities: CapabilitySet{CapabilityExternalFaultProvider: true}}
	if err := AuthorizeScenario(def, cfg, CLIOptions{}, env); err == nil {
		t.Fatal("risk C accepted without CLI authorization")
	}
	if err := AuthorizeScenario(def, cfg, CLIOptions{AllowRisk: RiskC}, env); err != nil {
		t.Fatal(err)
	}
}

func TestOutcomeRankOrdersResultStates(t *testing.T) {
	want := map[Outcome]int{
		OutcomeSuccess:        0,
		OutcomeNotApplicable:  0,
		OutcomeUnverified:     1,
		OutcomeDegraded:       2,
		OutcomeNotImplemented: 3,
		OutcomeFailed:         4,
		OutcomeRestoreFailed:  5,
	}
	if !reflect.DeepEqual(outcomeRank, want) {
		t.Fatalf("outcome rank = %#v, want %#v", outcomeRank, want)
	}
}
