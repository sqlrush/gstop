package gsbench

import (
	"context"
	"errors"
	"testing"
)

func TestCapabilitiesSelectEnhancedFeaturesWhenAvailable(t *testing.T) {
	p := fakeCapabilityProber{values: map[string]string{
		"version": "openGauss 7.0.0", "replication_deployment": "standalone", "local_node": "primary|127.0.0.1|5432", "admin": "true",
		"thread_pool_enabled": "on", "thread_pool_view": "1", "dynamic_memory": "1",
		"database_cpu": "1", "statement_history_relation": "true", "statement_history_identity_columns": "true", "statement_history_n_hard_parse": "true", "vacuum_stats": "1",
		"plan_index_unusable": "1", "plan_column_stats": "1",
		"plan_ndistinct": "1", "plan_extended_stats": "1",
	}}
	c := DetectCapabilities(context.Background(), p)
	if c.Product != "openGauss" || !c.Centralized || !c.Admin || !c.ThreadPoolEnabled || !c.ThreadPoolView {
		t.Fatalf("capabilities = %+v", c)
	}
	if !c.DynamicMemoryView || !c.DatabaseCPU || !c.StatementHistory || !c.VacuumStats {
		t.Fatalf("enhanced probes missing: %+v", c)
	}
	if !c.PlanIndexUnusable || !c.PlanColumnStats || !c.PlanNDistinct || !c.PlanExtendedStats {
		t.Fatalf("plan capabilities missing: %+v", c)
	}
}

func TestCapabilitiesTreatPermissionErrorsAsFallbackFacts(t *testing.T) {
	denied := errors.New("permission denied")
	p := fakeCapabilityProber{
		values: gaussDBValues("centralized", "cn_1|C|10.0.0.1|5432"),
		errors: map[string]error{
			"admin": denied, "thread_pool_enabled": denied, "thread_pool_view": denied,
			"dynamic_memory": denied, "database_cpu": denied, "statement_history_relation": denied, "statement_history_identity_columns": denied, "statement_history_n_hard_parse": denied,
			"vacuum_stats": denied, "plan_index_unusable": denied,
			"plan_column_stats": denied, "plan_ndistinct": denied, "plan_extended_stats": denied,
		},
	}
	c := DetectCapabilities(context.Background(), p)
	if c.Product != "GaussDB" || !c.Centralized || c.Admin || c.ThreadPoolView || c.DynamicMemoryView {
		t.Fatalf("capabilities = %+v", c)
	}
	if len(c.Warnings) == 0 {
		t.Fatal("permission fallbacks should be reported")
	}
}

func TestCapabilitiesAcceptDistributedTopology(t *testing.T) {
	p := fakeCapabilityProber{values: gaussDBValues("distributed", "cn_1|C|10.0.0.1|5432\ndn_1|D|10.0.0.2|15400")}
	c := DetectCapabilities(context.Background(), p)
	if c.Centralized || !c.Supported {
		t.Fatalf("distributed instance rejected: %+v", c)
	}
}
