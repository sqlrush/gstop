package gsbench

import (
	"context"
	"fmt"
	"strings"
)

type fakeCapabilityProber struct {
	values map[string]string
	errors map[string]error
}

// validateTopologyQueryContract locks the product boundary: distributed needs
// C plus D/S inventory evidence, while centralized needs no valid C/D/S row
// and a positive PG_NODE_ENV fact.
func validateTopologyQueryContract(query string) error {
	for _, want := range []string{
		"pg_catalog.pgxc_node",
		"node_type = 'C'",
		"node_type IN ('D','S')",
		"NOT EXISTS (SELECT 1 FROM pg_catalog.pgxc_node WHERE node_type IN ('C','D','S'))",
		"pg_catalog.pg_node_env",
	} {
		if !strings.Contains(query, want) {
			return fmt.Errorf("missing topology predicate %q", want)
		}
	}
	return nil
}

func (f fakeCapabilityProber) Probe(_ context.Context, name, _ string) (string, error) {
	if err := f.errors[name]; err != nil {
		return "", err
	}
	return f.values[name], nil
}

type recordingCapabilityProber struct {
	values  map[string]string
	errors  map[string]error
	queries map[string]string
}

func (f *recordingCapabilityProber) Probe(_ context.Context, name, query string) (string, error) {
	if f.queries == nil {
		f.queries = map[string]string{}
	}
	f.queries[name] = query
	if err := f.errors[name]; err != nil {
		return "", err
	}
	return f.values[name], nil
}

func centralizedFixture() Environment {
	return Environment{
		Product: ProductGaussDB, Topology: TopologyCentralized, Supported: true,
		Capabilities: CapabilitySet{},
	}
}

func distributedFixture() Environment {
	return Environment{
		Product: ProductGaussDB, Topology: TopologyDistributed, Supported: true,
		Capabilities: CapabilitySet{},
	}
}
