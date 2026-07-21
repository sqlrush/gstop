package gsbench

import (
	"context"
	"errors"
	"testing"
)

type fakeCapabilityProber struct {
	values map[string]string
	errors map[string]error
}

func (f fakeCapabilityProber) Probe(_ context.Context, name, _ string) (string, error) {
	if err := f.errors[name]; err != nil {
		return "", err
	}
	return f.values[name], nil
}

func TestCapabilitiesSelectEnhancedFeaturesWhenAvailable(t *testing.T) {
	p := fakeCapabilityProber{values: map[string]string{
		"version": "openGauss 7.0.0", "distributed_nodes": "1", "admin": "true",
		"thread_pool_enabled": "on", "thread_pool_view": "1", "dynamic_memory": "1",
		"database_cpu": "1", "statement_history": "1", "vacuum_stats": "1",
	}}
	c := DetectCapabilities(context.Background(), p)
	if c.Product != "openGauss" || !c.Centralized || !c.Admin || !c.ThreadPoolEnabled || !c.ThreadPoolView {
		t.Fatalf("capabilities = %+v", c)
	}
	if !c.DynamicMemoryView || !c.DatabaseCPU || !c.StatementHistory || !c.VacuumStats {
		t.Fatalf("enhanced probes missing: %+v", c)
	}
}

func TestCapabilitiesTreatPermissionErrorsAsFallbackFacts(t *testing.T) {
	denied := errors.New("permission denied")
	p := fakeCapabilityProber{
		values: map[string]string{"version": "GaussDB Kernel V500", "distributed_nodes": "1"},
		errors: map[string]error{
			"admin": denied, "thread_pool_enabled": denied, "thread_pool_view": denied,
			"dynamic_memory": denied, "database_cpu": denied, "statement_history": denied,
			"vacuum_stats": denied,
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

func TestCapabilitiesRejectDistributedTopology(t *testing.T) {
	p := fakeCapabilityProber{values: map[string]string{
		"version": "openGauss", "distributed_nodes": "3",
	}}
	c := DetectCapabilities(context.Background(), p)
	if c.Centralized || c.Supported {
		t.Fatalf("distributed instance marked supported: %+v", c)
	}
}
