package gsbench

import (
	"reflect"
	"testing"
)

func TestDefaultScenarioCatalogIsCompleteAndUnique(t *testing.T) {
	catalog := DefaultScenarioCatalog()
	if got, want := catalog.Codes(), DesignedScenarioCodes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("codes=%v want=%v", got, want)
	}
	for _, definition := range catalog.Definitions() {
		if int(definition.Code)/100 != int(definition.Category) {
			t.Fatalf("code=%d category=%d", definition.Code, definition.Category)
		}
	}
}

func TestCatalogResolvesCodeAndCanonicalName(t *testing.T) {
	catalog := DefaultScenarioCatalog()
	for _, input := range []string{"601", "planchange_stats_target"} {
		got, err := catalog.Resolve(input)
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 601 || got.Name != "planchange_stats_target" {
			t.Fatalf("definition=%+v", got)
		}
	}
}

func TestCatalogRejectsLegacyNumericAlias(t *testing.T) {
	if _, err := DefaultScenarioCatalog().Resolve("1"); err == nil {
		t.Fatal("legacy numeric alias accepted")
	}
}

func TestCatalogDoesNotExposeMutableDefinitionSlices(t *testing.T) {
	catalog, err := NewScenarioCatalog([]ScenarioDefinition{{
		Code: 101, Name: "tp_cpu", Category: CategoryCPU,
		AppliesTo: []EnvironmentClass{EnvironmentOpenGauss},
		Requires:  []Requirement{RequirementStatementHistory},
	}})
	if err != nil {
		t.Fatal(err)
	}

	definition := catalog.MustCode(101)
	definition.AppliesTo[0] = EnvironmentDistributedGaussDB
	definition.Requires[0] = RequirementGlobalPlanCache

	got := catalog.MustCode(101)
	if got.AppliesTo[0] != EnvironmentOpenGauss || got.Requires[0] != RequirementStatementHistory {
		t.Fatalf("catalog definition mutated: %+v", got)
	}
}

func TestCatalogCarriesCanonicalRiskApplicabilityAndRequirements(t *testing.T) {
	catalog := DefaultScenarioCatalog()
	tests := []struct {
		code      ScenarioCode
		risk      RiskLevel
		appliesTo []EnvironmentClass
		requires  []Requirement
	}{
		{209, RiskB, allEnvironmentClasses, nil},
		{305, RiskB, allEnvironmentClasses, nil},
		{343, RiskC, allEnvironmentClasses, []Requirement{RequirementExternalFaultProvider}},
		{402, RiskA, allEnvironmentClasses, []Requirement{RequirementThreadPool}},
		{405, RiskA, []EnvironmentClass{EnvironmentDistributedGaussDB}, []Requirement{RequirementPoolerViews}},
		{511, RiskA, []EnvironmentClass{EnvironmentDistributedGaussDB}, []Requirement{RequirementGlobalLockViews}},
		{626, RiskA, allEnvironmentClasses, []Requirement{RequirementGlobalPlanCache, RequirementHardParseCounters}},
		{703, RiskB, allEnvironmentClasses, []Requirement{RequirementPrimaryStandby, RequirementStandbyControl}},
		{706, RiskC, allEnvironmentClasses, []Requirement{RequirementPrimaryStandby, RequirementExternalFaultProvider}},
		{728, RiskC, []EnvironmentClass{EnvironmentDistributedGaussDB}, []Requirement{RequirementExternalFaultProvider}},
	}
	for _, test := range tests {
		t.Run(string(rune(test.code)), func(t *testing.T) {
			got := catalog.MustCode(test.code)
			if got.Risk != test.risk || !reflect.DeepEqual(got.AppliesTo, test.appliesTo) || !reflect.DeepEqual(got.Requires, test.requires) {
				t.Fatalf("definition=%+v", got)
			}
		})
	}
}

func TestHardParseCatalogEntriesRequireHardParseCounters(t *testing.T) {
	catalog := DefaultScenarioCatalog()
	for code := ScenarioCode(621); code <= 626; code++ {
		definition := catalog.MustCode(code)
		if !containsRequirement(definition.Requires, RequirementHardParseCounters) {
			t.Fatalf("code=%d requirements=%v", code, definition.Requires)
		}
		if code == 626 && !containsRequirement(definition.Requires, RequirementGlobalPlanCache) {
			t.Fatalf("code=%d requirements=%v", code, definition.Requires)
		}
	}
}

func containsRequirement(requirements []Requirement, expected Requirement) bool {
	for _, requirement := range requirements {
		if requirement == expected {
			return true
		}
	}
	return false
}

func TestCatalogDeclaresHardParseFallbackRequirements(t *testing.T) {
	catalog := DefaultScenarioCatalog()
	for code := ScenarioCode(621); code <= 626; code++ {
		definition := catalog.MustCode(code)
		if !reflect.DeepEqual(
			definition.FallbackRequirements,
			[]Requirement{RequirementHardParseCounters},
		) {
			t.Fatalf(
				"scenario %03d fallback requirements=%v",
				code,
				definition.FallbackRequirements,
			)
		}
	}
	if got := catalog.MustCode(601).FallbackRequirements; len(got) != 0 {
		t.Fatalf("scenario 601 has undeclared fallback=%v", got)
	}
}

func TestTableLockConflictDefinitionsDeclareEveryCanonicalPair(t *testing.T) {
	definitions := TableLockConflictDefinitions()
	if len(definitions) != 21 {
		t.Fatalf("lock conflict count=%d", len(definitions))
	}
	want := []struct {
		code                 ScenarioCode
		name, holder, waiter string
	}{
		{520, "lockmode_accessshare_accessexclusive", "AS", "AX"},
		{521, "lockmode_rowshare_exclusive", "RS", "X"},
		{522, "lockmode_rowshare_accessexclusive", "RS", "AX"},
		{523, "lockmode_rowexclusive_share", "RX", "S"},
		{524, "lockmode_rowexclusive_sharerowexclusive", "RX", "SRX"},
		{525, "lockmode_rowexclusive_exclusive", "RX", "X"},
		{526, "lockmode_rowexclusive_accessexclusive", "RX", "AX"},
		{527, "lockmode_shareupdateexclusive_self", "SUE", "SUE"},
		{528, "lockmode_shareupdateexclusive_share", "SUE", "S"},
		{529, "lockmode_shareupdateexclusive_sharerowexclusive", "SUE", "SRX"},
		{530, "lockmode_shareupdateexclusive_exclusive", "SUE", "X"},
		{531, "lockmode_shareupdateexclusive_accessexclusive", "SUE", "AX"},
		{532, "lockmode_share_sharerowexclusive", "S", "SRX"},
		{533, "lockmode_share_exclusive", "S", "X"},
		{534, "lockmode_share_accessexclusive", "S", "AX"},
		{535, "lockmode_sharerowexclusive_self", "SRX", "SRX"},
		{536, "lockmode_sharerowexclusive_exclusive", "SRX", "X"},
		{537, "lockmode_sharerowexclusive_accessexclusive", "SRX", "AX"},
		{538, "lockmode_exclusive_self", "X", "X"},
		{539, "lockmode_exclusive_accessexclusive", "X", "AX"},
		{540, "lockmode_accessexclusive_self", "AX", "AX"},
	}
	for i, expected := range want {
		got := definitions[i]
		if got.Code != expected.code || got.Name != expected.name || string(got.Holder) != expected.holder || string(got.Waiter) != expected.waiter {
			t.Fatalf("definition[%d]=%+v", i, got)
		}
	}
}
