package gsbench

import (
	"strings"
	"testing"
)

func TestPrecheckWarningRendersStableLogLine(t *testing.T) {
	warning := PrecheckWarning{
		ScenarioCode: 401,
		Scenario:     "connection_pool",
		Check:        "capacity",
		Object:       "max_connections",
		Actual:       "82.9%",
		Expected:     "90%",
		Impact:       "target_may_not_be_reached",
	}
	want := "PRECHECK_WARN scenario=401 name=connection_pool check=capacity " +
		"object=max_connections actual=82.9% expected=90% " +
		"impact=target_may_not_be_reached"
	if got := warning.LogLine(); got != want {
		t.Fatalf("warning line=%q want=%q", got, want)
	}
}

func TestPrecheckWarningRedactsCredentialMaterial(t *testing.T) {
	warning := PrecheckWarning{
		ScenarioCode: 601,
		Scenario:     "planchange_index_drop",
		Check:        "probe_error",
		Object:       "plan_data_lookup_idx",
		Actual:       "password=customer-secret\nsecond line",
		Expected:     "readable catalog state",
		Impact:       "inspection_unavailable",
	}
	line := warning.LogLine()
	if strings.Contains(line, "customer-secret") || strings.Contains(line, "\n") {
		t.Fatalf("warning exposed unsafe value %q", line)
	}
	if !strings.Contains(line, "actual=redacted") {
		t.Fatalf("warning did not identify redaction %q", line)
	}
}

func TestAdvisoryCollectorKeepsScenarioWarningsInReportOrder(t *testing.T) {
	collector := &AdvisoryCollector{}
	collector.Report(PrecheckWarning{ScenarioCode: 402, Check: "first"})
	collector.Report(PrecheckWarning{ScenarioCode: 401, Check: "other"})
	collector.Report(PrecheckWarning{ScenarioCode: 402, Check: "second"})

	warnings := collector.Scenario(402)
	if len(warnings) != 2 || warnings[0].Check != "first" ||
		warnings[1].Check != "second" {
		t.Fatalf("scenario warnings=%+v", warnings)
	}
	warnings[0].Check = "modified-copy"
	if got := collector.Scenario(402)[0].Check; got != "first" {
		t.Fatalf("collector returned mutable storage, got=%q", got)
	}
}

func TestPrecheckWarningProducesStructuredEvidence(t *testing.T) {
	warning := PrecheckWarning{
		ScenarioCode: 401,
		Scenario:     "connection_pool",
		Check:        "capacity",
		Object:       "max_connections",
		Actual:       "82.9%",
		Expected:     "90%",
		Impact:       "target_may_not_be_reached",
	}
	evidence := warning.Evidence()
	if evidence.Metric != "precheck_warning" || evidence.Actual != 1 ||
		!evidence.Available {
		t.Fatalf("warning evidence=%+v", evidence)
	}
	for key, want := range map[string]any{
		"check":    "capacity",
		"object":   "max_connections",
		"actual":   "82.9%",
		"expected": "90%",
		"impact":   "target_may_not_be_reached",
	} {
		if got := evidence.Details[key]; got != want {
			t.Errorf("warning detail %s=%v want=%v", key, got, want)
		}
	}
}

func TestCompletedWithWarningsHasZeroExitWithoutHidingFailure(t *testing.T) {
	if got := exitCodeForOutcome(OutcomeCompletedWithWarnings); got != 0 {
		t.Fatalf("warning exit code=%d want=0", got)
	}
	if got := worseOutcome(OutcomeCompletedWithWarnings, OutcomeFailed); got != OutcomeFailed {
		t.Fatalf("worse warning/failure outcome=%s want=%s", got, OutcomeFailed)
	}
}
