package gsbench

import (
	"bytes"
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

func TestDeprecatedConfigWarningsIdentifyNonEnforcingKeys(t *testing.T) {
	cfg := BenchConfig{
		Run:  RunConfig{ValidationEnabled: true},
		Data: DataConfig{MaxSizeGB: 7, MinFreeDiskPercent: 8},
		Safety: SafetyConfig{
			RestoreOnExit:       false,
			RestoreOriginalRole: true,
			MaxWorkers:          4,
			MaxConnections:      5,
			ProfileCapGB:        6,
		},
	}
	warnings := deprecatedConfigWarnings(cfg)
	want := []string{
		"run.validation_enabled",
		"safety.restore_on_exit",
		"safety.restore_original_role",
		"safety.max_workers",
		"safety.max_connections",
		"safety.profile_cap_gb",
		"data.max_size_gb",
		"data.min_free_disk_percent",
	}
	if len(warnings) != len(want) {
		t.Fatalf("deprecated warnings=%+v", warnings)
	}
	for index, object := range want {
		warning := warnings[index]
		if warning.Check != "deprecated_config" || warning.Object != object ||
			warning.Impact != "accepted_but_not_enforced" {
			t.Errorf("warning[%d]=%+v", index, warning)
		}
	}
}

func TestLogDeprecatedConfigWarningsWritesEachWarningOnce(t *testing.T) {
	var output bytes.Buffer
	log, err := NewRunLog(&output, "", Version)
	if err != nil {
		t.Fatal(err)
	}
	logDeprecatedConfigWarnings(BenchConfig{}, log)
	if got := strings.Count(output.String(), "PRECHECK_WARN"); got != 8 {
		t.Fatalf("deprecated warning lines=%d output=%q", got, output.String())
	}
	if !strings.Contains(output.String(), "object=safety.max_connections") ||
		!strings.Contains(output.String(), "impact=accepted_but_not_enforced") {
		t.Fatalf("deprecated warnings missing details: %q", output.String())
	}
}
