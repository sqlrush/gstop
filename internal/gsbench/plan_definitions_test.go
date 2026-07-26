package gsbench

import (
	"regexp"
	"strings"
	"testing"
)

func TestPlanDefinitionsCoverSixScenariosWithLiteralSQL(t *testing.T) {
	defs, err := PlanScenarioDefinitions("gsbench")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"plan_stats_target": false, "plan_index_unusable": false,
		"plan_stats_ndistinct": false, "plan_stats_extended": false,
		"plan_index_drop": false, "plan_index_shape": false,
	}
	bind := regexp.MustCompile(`\$[0-9]+|\?`)
	for _, def := range defs {
		if _, ok := want[def.Name]; !ok {
			t.Fatalf("unexpected definition %s", def.Name)
		}
		want[def.Name] = true
		if len(def.Candidates) < 2 {
			t.Fatalf("%s candidates=%d", def.Name, len(def.Candidates))
		}
		for _, query := range def.Candidates {
			if bind.MatchString(query) {
				t.Fatalf("%s uses bind placeholder: %s", def.Name, query)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing definition %s", name)
		}
	}
}

func TestEveryPlanMutationHasInverseAndRestoreVerification(t *testing.T) {
	for _, name := range []string{
		"plan_stats_target", "plan_index_unusable", "plan_stats_ndistinct",
		"plan_stats_extended", "plan_index_drop", "plan_index_shape",
	} {
		mutations, err := PlanMutationSet("run-1", "gsbench", name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "plan_index_shape" && len(mutations) != 2 {
			t.Fatalf("shape mutations=%d want=2", len(mutations))
		}
		for _, mutation := range mutations {
			if mutation.Scenario != name || mutation.ForwardSQL == "" ||
				mutation.InverseSQL == "" || mutation.VerifySQL == "" {
				t.Fatalf("%s mutation=%+v", name, mutation)
			}
			if strings.Contains(mutation.ForwardSQL, ";") || strings.Contains(mutation.InverseSQL, ";") {
				t.Fatalf("%s uses a multi-command mutation unsupported by openGauss: %+v", name, mutation)
			}
		}
	}

	shape, _ := PlanMutationSet("run-1", "gsbench", "plan_index_shape")
	if !strings.Contains(shape[0].ForwardSQL, "DROP INDEX") ||
		!strings.Contains(shape[1].ForwardSQL, "index_shape_tail,index_shape_lead") {
		t.Fatalf("shape mutations=%+v", shape)
	}
	unusable, _ := PlanMutationSet("run-1", "gsbench", "plan_index_unusable")
	if !strings.Contains(unusable[0].ForwardSQL, "UNUSABLE") ||
		!strings.Contains(unusable[0].InverseSQL, "REBUILD") {
		t.Fatalf("unusable mutation=%+v", unusable)
	}
}
