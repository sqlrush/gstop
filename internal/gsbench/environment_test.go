package gsbench

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDetectEnvironmentDistinguishesSupportedTopologies(t *testing.T) {
	tests := []struct {
		name     string
		values   map[string]string
		product  Product
		topology Topology
	}{
		{"openGauss standalone", map[string]string{"version": "openGauss 7.0", "replication_deployment": "standalone", "local_node": "primary|127.0.0.1|5432"}, ProductOpenGauss, TopologyStandalone},
		{"openGauss primary standby", map[string]string{"version": "openGauss 7.0", "replication_deployment": "primary", "local_node": "primary|127.0.0.1|5432"}, ProductOpenGauss, TopologyPrimaryStandby},
		{"GaussDB centralized", gaussDBValues("centralized", ""), ProductGaussDB, TopologyCentralized},
		{"GaussDB distributed", gaussDBValues("distributed", "cn_1|C|10.0.0.1|5432\ndn_1|D|10.0.0.2|15400"), ProductGaussDB, TopologyDistributed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectEnvironment(context.Background(), fakeCapabilityProber{values: tt.values})
			if got.Product != tt.product || got.Topology != tt.topology || !got.Supported {
				t.Fatalf("got=%+v", got)
			}
		})
	}
}

func TestDetectEnvironmentKeepsDisconnectedPrimaryInPrimaryStandbyDeployment(t *testing.T) {
	prober := recordingCapabilityProber{values: map[string]string{
		"version": "openGauss 7.0", "replication_deployment": "primary", "local_node": "primary|127.0.0.1|5432",
	}}
	env := DetectEnvironment(context.Background(), &prober)
	if env.Topology != TopologyPrimaryStandby || !env.Supported {
		t.Fatalf("environment=%+v", env)
	}
	query := prober.queries["replication_deployment"]
	if !strings.Contains(query, "replconninfo") || strings.Contains(query, "pg_stat_replication") {
		t.Fatalf("deployment query=%s", query)
	}
}

func TestCentralizedGaussDBUsesPGNodeEnvAndReplicationFact(t *testing.T) {
	values := gaussDBValues("centralized", "")
	values["replication_deployment"] = "standby"
	values["local_node"] = "dn_1|10.0.0.1|5432"
	env := DetectEnvironment(context.Background(), fakeCapabilityProber{values: values})
	if env.Topology != TopologyCentralized || !env.Supported {
		t.Fatalf("environment=%+v", env)
	}
	if missing := env.Missing([]Requirement{RequirementPrimaryStandby}); len(missing) != 0 {
		t.Fatalf("centralized primary/standby missing=%v", missing)
	}
}

func TestDetectEnvironmentFailsClosedForInconclusiveGaussDBTopology(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
		errors map[string]error
	}{
		{"probe error", gaussDBValues("centralized", "cn_1|C|10.0.0.1|5432"), map[string]error{"gaussdb_topology": errors.New("permission denied")}},
		{"invalid discriminator", gaussDBValues("not-a-topology", "cn_1|C|10.0.0.1|5432"), nil},
		{"DN only", gaussDBValues("distributed", "dn_1|D|10.0.0.2|15400"), nil},
		{"unknown discriminator", gaussDBValues("unknown", ""), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := DetectEnvironment(context.Background(), fakeCapabilityProber{values: tt.values, errors: tt.errors})
			if env.Topology != TopologyUnknown || env.Supported {
				t.Fatalf("environment=%+v", env)
			}
		})
	}
}

func TestUnknownProductIsUnsupported(t *testing.T) {
	env := DetectEnvironment(context.Background(), fakeCapabilityProber{values: map[string]string{"version": "PostgreSQL 16"}})
	if env.Product != ProductUnknown || env.Supported {
		t.Fatalf("environment=%+v", env)
	}
}

func TestDetectEnvironmentKeepsOptionalProbeFailureAsWarning(t *testing.T) {
	values := gaussDBValues("centralized", "cn_1|C|10.0.0.1|5432")
	values["thread_pool_enabled"] = "on"
	values["thread_pool_view"] = "true"
	env := DetectEnvironment(context.Background(), fakeCapabilityProber{values: values, errors: map[string]error{"global_lock_views": errors.New("permission denied")}})
	if !env.Supported || !env.Capabilities[CapabilityThreadPool] {
		t.Fatalf("environment=%+v", env)
	}
	if env.Capabilities[CapabilityGlobalLockViews] || len(env.Warnings) == 0 {
		t.Fatalf("optional probe failure was not isolated: %+v", env)
	}
}

func TestDetectEnvironmentNormalizesRealPGXCNodeRoles(t *testing.T) {
	env := DetectEnvironment(context.Background(), fakeCapabilityProber{values: gaussDBValues("distributed", "cn_1|C|10.0.0.1|5432\ndn_primary|D|10.0.0.2|15400\ndn_standby|S|10.0.0.3|15401")})
	want := []Node{
		{Name: "cn_1", Role: NodeRoleCN, Host: "10.0.0.1", Port: 5432},
		{Name: "dn_primary", Role: NodeRoleDNPrimary, Host: "10.0.0.2", Port: 15400},
		{Name: "dn_standby", Role: NodeRoleDNStandby, Host: "10.0.0.3", Port: 15401},
	}
	if !reflect.DeepEqual(env.Nodes, want) {
		t.Fatalf("nodes=%+v want=%+v", env.Nodes, want)
	}
	for _, node := range env.Nodes {
		if node.Shard != "" {
			t.Fatalf("node_id was used as shard: %+v", node)
		}
	}
}

func TestDetectEnvironmentRejectsUnknownPGXCNodeRole(t *testing.T) {
	env := DetectEnvironment(context.Background(), fakeCapabilityProber{values: gaussDBValues("distributed", "cn_1|X|10.0.0.1|5432")})
	if env.Topology != TopologyUnknown || env.Supported {
		t.Fatalf("environment=%+v", env)
	}
}

func TestDetectEnvironmentProbesGTMAsSeparateFact(t *testing.T) {
	values := gaussDBValues("distributed", "cn_1|C|10.0.0.1|5432\ndn_1|D|10.0.0.2|15400")
	values["gtm"] = "true"
	env := DetectEnvironment(context.Background(), fakeCapabilityProber{values: values})
	if missing := env.Missing([]Requirement{RequirementGTM}); len(missing) != 0 {
		t.Fatalf("GTM requirement missing=%v", missing)
	}
}

func TestTopologyAndGTMProbesUseOfficialCatalogBoundaries(t *testing.T) {
	prober := recordingCapabilityProber{values: gaussDBValues("distributed", "cn_1|C|10.0.0.1|5432\ndn_1|D|10.0.0.2|15400")}
	_ = DetectEnvironment(context.Background(), &prober)
	for name, want := range map[string]string{
		"gaussdb_topology": "pg_node_env",
		"gtm":              "pgxc_gtm_snapshot_status",
	} {
		if query := prober.queries[name]; !strings.Contains(query, want) {
			t.Fatalf("%s query=%s, missing %q", name, query, want)
		}
	}
	if query := prober.queries["gtm"]; strings.Contains(query, "'gtm'") {
		t.Fatalf("GTM query uses imprecise function: %s", query)
	}
}

func TestTopologyRecordingContractRejectsIncompleteCentralizedDiscriminator(t *testing.T) {
	// This resembles the old recording expectation: it recognizes the local
	// node view but omits both distributed and empty-PGXC_NODE predicates.
	incomplete := "SELECT CASE WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_node_env) THEN 'centralized' END"
	if err := validateTopologyQueryContract(incomplete); err == nil {
		t.Fatalf("incomplete topology discriminator was accepted: %s", incomplete)
	}
}

func TestRecordingQueriesSpecifyTopologyAndNodeBoundaries(t *testing.T) {
	central := recordingCapabilityProber{values: gaussDBValues("centralized", "")}
	_ = DetectEnvironment(context.Background(), &central)
	if err := validateTopologyQueryContract(central.queries["gaussdb_topology"]); err != nil {
		t.Fatal(err)
	}
	local := central.queries["local_node"]
	for _, want := range []string{"pg_catalog.pg_node_env", "node_name", "host", "port"} {
		if !strings.Contains(local, want) {
			t.Fatalf("local-node query missing %q: %s", want, local)
		}
	}
	if strings.Contains(local, "pgxc_node_name()") {
		t.Fatalf("local-node query uses unsupported function: %s", local)
	}

	distributed := recordingCapabilityProber{values: gaussDBValues("distributed", "cn_1|C|10.0.0.1|5432\ndn_1|D|10.0.0.2|15400")}
	_ = DetectEnvironment(context.Background(), &distributed)
	inventory := distributed.queries["nodes"]
	if !strings.Contains(inventory, "pg_catalog.pgxc_node") || strings.Contains(inventory, "pg_node_env") {
		t.Fatalf("distributed inventory query=%s", inventory)
	}
}

func TestStatementHistoryAndHardParseCountersAreIndependentlyDetected(t *testing.T) {
	withoutHardParse := gaussDBValues("centralized", "cn_1|C|10.0.0.1|5432")
	withoutHardParse["statement_history_relation"] = "true"
	withoutHardParse["statement_history_identity_columns"] = "true"
	withoutHardParse["statement_history_n_hard_parse"] = "false"
	env := DetectEnvironment(context.Background(), fakeCapabilityProber{values: withoutHardParse})
	if !env.Capabilities[CapabilityStatementHistory] || env.Capabilities[CapabilityHardParseCounters] {
		t.Fatalf("capabilities=%+v", env.Capabilities)
	}
	withHardParse := gaussDBValues("centralized", "cn_1|C|10.0.0.1|5432")
	withHardParse["statement_history_relation"] = "true"
	withHardParse["statement_history_identity_columns"] = "true"
	withHardParse["statement_history_n_hard_parse"] = "true"
	env = DetectEnvironment(context.Background(), fakeCapabilityProber{values: withHardParse})
	if !env.Capabilities[CapabilityHardParseCounters] {
		t.Fatalf("capabilities=%+v", env.Capabilities)
	}
}

func TestCapabilityProbesAreCatalogChecksAndNeverInvokeStandbyPromotion(t *testing.T) {
	prober := recordingCapabilityProber{values: gaussDBValues("centralized", "cn_1|C|10.0.0.1|5432")}
	_ = DetectEnvironment(context.Background(), &prober)
	for _, name := range []string{"thread_pool_view", "global_plan_cache", "global_lock_views", "memory_node_views", "wal_sender_views", "standby_control", "distributed_stream"} {
		query := prober.queries[name]
		if !strings.Contains(query, "pg_catalog") || !strings.Contains(query, "has_") || strings.Contains(query, "LIMIT 1") {
			t.Fatalf("%s is not a catalog/privilege probe: %s", name, query)
		}
	}
	if query := prober.queries["standby_control"]; strings.Contains(strings.ToLower(query), "pg_promote(") {
		t.Fatalf("standby probe invokes promotion: %s", query)
	}
}

func TestEnvironmentApplicableAndMissingCoverEveryRequirement(t *testing.T) {
	env := distributedFixture()
	env.Nodes = []Node{{Name: "dn_standby", Role: NodeRoleDNStandby}}
	for _, capability := range requirementCapability {
		env.Capabilities[capability] = true
	}
	env.Capabilities[capabilityGTM] = true
	requirements := []Requirement{RequirementStatementHistory, RequirementHardParseCounters, RequirementGlobalPlanCache, RequirementGlobalLockViews, RequirementThreadPool, RequirementPoolerViews, RequirementMemoryNodeViews, RequirementWALSenderViews, RequirementStandbyControl, RequirementExternalFaultProvider, RequirementPrimaryStandby, RequirementGTM}
	if got := env.Missing(requirements); len(got) != 0 {
		t.Fatalf("missing=%v", got)
	}
	distributedOnly := ScenarioDefinition{AppliesTo: []EnvironmentClass{EnvironmentDistributedGaussDB}}
	if !env.Applicable(distributedOnly) {
		t.Fatal("distributed definition is not applicable to distributed GaussDB")
	}
	if centralizedFixture().Applicable(distributedOnly) {
		t.Fatal("distributed definition is applicable to centralized environment")
	}
}

func gaussDBValues(topology, nodes string) map[string]string {
	return map[string]string{"version": "GaussDB Kernel V500", "gaussdb_topology": topology, "nodes": nodes, "local_node": "dn_1|10.0.0.1|5432"}
}
