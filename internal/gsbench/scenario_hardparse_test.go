package gsbench

import (
	"strings"
	"testing"
)

func TestHardParseStatementsUseOnlyFixedBenchmarkObjects(t *testing.T) {
	for _, code := range []ScenarioCode{621, 622, 623, 624, 625} {
		statement, err := HardParseStatement(code, "gsbench", 42)
		if err != nil {
			t.Fatalf("code %d: %v", code, err)
		}
		if !strings.Contains(statement, "fact_sales") && !strings.Contains(statement, "hardparse_targets") {
			t.Fatalf("code %d statement=%q", code, statement)
		}
	}
}

func TestHardParseFactoriesRegisterAllImplementedCodes(t *testing.T) {
	factories := LockScenarioFactories()
	for code := ScenarioCode(601); code <= 606; code++ {
		if factories[code] == nil {
			t.Fatalf("missing planchange factory %d", code)
		}
	}
	for code := ScenarioCode(621); code <= 625; code++ {
		if factories[code] == nil {
			t.Fatalf("missing hardparse factory %d", code)
		}
	}
	if factories[626] != nil {
		t.Fatal("626 must stay unregistered until product-specific GPC evidence is available")
	}
}

func TestHardParseSuccessRequiresCounterDelta(t *testing.T) {
	result := EvaluateHardParse("hardparse_literal_flood", hardParseDelta(
		HardParseSample{Available: true, Hard: 1, Soft: 1},
		HardParseSample{Available: true, Hard: 5, Soft: 2},
	))
	if result.Outcome != OutcomeSuccess {
		t.Fatalf("result=%+v", result)
	}
	result = EvaluateHardParse("hardparse_literal_flood", HardParseDelta{})
	if result.Outcome == OutcomeSuccess {
		t.Fatalf("missing counters returned success: %+v", result)
	}
}

func TestHardParseProtocolsKeepDistinctCauses(t *testing.T) {
	literal, err := HardParseProtocolFor(621, "gsbench", "tag621")
	if err != nil {
		t.Fatal(err)
	}
	if literal.MaxLiterals < 2 || literal.SimpleSQL == "" || literal.ControlSQL != "" {
		t.Fatalf("literal protocol=%+v", literal)
	}
	unprepared, err := HardParseProtocolFor(622, "gsbench", "tag622")
	if err != nil {
		t.Fatal(err)
	}
	if unprepared.SimpleSQL == "" || strings.Contains(unprepared.SimpleSQL, "PREPARE") || !strings.Contains(unprepared.ControlSQL, "PREPARE") {
		t.Fatalf("unprepared protocol=%+v", unprepared)
	}
	if unprepared.SimpleSQL == literal.SimpleSQL {
		t.Fatal("622 must not alias the literal-flood SQL")
	}
	custom, err := HardParseProtocolFor(623, "gsbench", "tag623")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(custom.SetupSQL, "force_custom_plan") || !strings.Contains(custom.PrepareSQL, "$1") || !strings.Contains(custom.ExecuteSQL, "EXECUTE") || !strings.Contains(custom.CleanupSQL, "DEALLOCATE") {
		t.Fatalf("custom protocol=%+v", custom)
	}
	invalidation, err := HardParseProtocolFor(625, "gsbench", "tag625")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(invalidation.PrepareSQL, "hardparse_targets") || !strings.Contains(invalidation.ExecuteSQL, "EXECUTE") {
		t.Fatalf("invalidation protocol=%+v", invalidation)
	}
}

func TestHardParseLiteralFloodStopsAtBoundInsteadOfRepeating(t *testing.T) {
	protocol, err := HardParseProtocolFor(621, "gsbench", "tag621")
	if err != nil {
		t.Fatal(err)
	}
	first, err := protocol.LiteralSQL(1)
	if err != nil {
		t.Fatal(err)
	}
	last, err := protocol.LiteralSQL(protocol.MaxLiterals)
	if err != nil || first == last {
		t.Fatalf("first=%q last=%q err=%v", first, last, err)
	}
	if _, err := protocol.LiteralSQL(protocol.MaxLiterals + 1); err == nil {
		t.Fatal("literal flood must stop at its unique bound")
	}
}

func TestHardParseInvalidationUsesJournaledFixedIndexActions(t *testing.T) {
	mutations, err := HardParseInvalidationMutations("run-1", "gsbench")
	if err != nil {
		t.Fatal(err)
	}
	if len(mutations) != 2 {
		t.Fatalf("mutations=%+v", mutations)
	}
	for _, mutation := range mutations {
		if mutation.ScenarioCode != 625 || mutation.ForwardSQL == "" || mutation.InverseSQL == "" || mutation.VerifySQL == "" {
			t.Fatalf("incomplete journal mutation=%+v", mutation)
		}
		if !strings.Contains(mutation.Target, "hardparse_invalidation_idx") || strings.Contains(mutation.ForwardSQL, "plan_") {
			t.Fatalf("mutation escapes fixed invalidation index=%+v", mutation)
		}
	}
}
