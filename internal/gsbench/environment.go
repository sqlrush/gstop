package gsbench

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// DetectEnvironment discovers the target without changing it. Optional probe
// failures are recorded as warnings so one unavailable view cannot hide the
// capabilities that were successfully observed.
func DetectEnvironment(ctx context.Context, p CapabilityProber) Environment {
	env := Environment{Capabilities: make(CapabilitySet)}
	value, err := p.Probe(ctx, "version", "SELECT version()")
	if err != nil {
		env.warn("version", err)
	} else {
		env.Version = value
		env.Product = productForVersion(value)
	}
	if env.Product == "" {
		env.Product = ProductUnknown
	}

	replication := probeReplicationDeployment(ctx, p, &env)
	switch env.Product {
	case ProductOpenGauss:
		env.Topology = openGaussTopology(replication)
		probeLocalNode(ctx, p, &env, replication)
	case ProductGaussDB:
		env.Topology = probeGaussDBTopology(ctx, p, &env)
		if env.Topology == TopologyDistributed {
			if !probeNodes(ctx, p, &env) || !hasRole(env.Nodes, NodeRoleCN) {
				env.warn("gaussdb_topology", fmt.Errorf("no valid CN evidence"))
				env.Topology = TopologyUnknown
			}
		} else if env.Topology == TopologyCentralized {
			probeLocalNode(ctx, p, &env, replication)
		}
	}
	probeCapabilities(ctx, p, &env)
	env.Supported = (env.Product == ProductOpenGauss || env.Product == ProductGaussDB) && env.Topology != TopologyUnknown
	return env
}

func productForVersion(version string) Product {
	switch lower := strings.ToLower(version); {
	case strings.Contains(lower, "opengauss"):
		return ProductOpenGauss
	case strings.Contains(lower, "gaussdb"):
		return ProductGaussDB
	default:
		return ProductUnknown
	}
}

func probeReplicationDeployment(ctx context.Context, p CapabilityProber, env *Environment) string {
	value, err := p.Probe(ctx, "replication_deployment", "SELECT CASE WHEN pg_is_in_recovery() THEN 'standby' WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_settings WHERE name LIKE 'replconninfo%' AND setting <> '') THEN 'primary' ELSE 'standalone' END")
	if err != nil {
		env.warn("replication_deployment", err)
		return ""
	}
	switch strings.TrimSpace(value) {
	case "standalone":
		return "standalone"
	case "primary", "standby":
		env.Capabilities[capabilityPrimaryStandby] = true
		return strings.TrimSpace(value)
	default:
		env.warn("replication_deployment", fmt.Errorf("invalid replication deployment %q", value))
		return ""
	}
}

func openGaussTopology(replication string) Topology {
	if replication == "standalone" {
		return TopologyStandalone
	}
	if replication == "primary" || replication == "standby" {
		return TopologyPrimaryStandby
	}
	return TopologyUnknown
}

func probeGaussDBTopology(ctx context.Context, p CapabilityProber, env *Environment) Topology {
	value, err := p.Probe(ctx, "gaussdb_topology", "SELECT CASE WHEN EXISTS (SELECT 1 FROM pg_catalog.pgxc_node WHERE node_type = 'C') AND EXISTS (SELECT 1 FROM pg_catalog.pgxc_node WHERE node_type IN ('D','S')) THEN 'distributed' WHEN NOT EXISTS (SELECT 1 FROM pg_catalog.pgxc_node WHERE node_type IN ('C','D','S')) AND EXISTS (SELECT 1 FROM pg_catalog.pg_node_env) THEN 'centralized' ELSE 'unknown' END")
	if err != nil {
		env.warn("gaussdb_topology", err)
		return TopologyUnknown
	}
	switch strings.TrimSpace(value) {
	case "centralized":
		return TopologyCentralized
	case "distributed":
		return TopologyDistributed
	default:
		env.warn("gaussdb_topology", fmt.Errorf("invalid topology discriminator %q", value))
		return TopologyUnknown
	}
}

func probeNodes(ctx context.Context, p CapabilityProber, env *Environment) bool {
	value, err := p.Probe(ctx, "nodes", "SELECT node_name || '|' || node_type || '|' || node_host || '|' || node_port::text FROM pg_catalog.pgxc_node ORDER BY node_name")
	if err != nil {
		env.warn("nodes", err)
		return false
	} else {
		nodes, ok := parseNodes(value)
		if !ok {
			env.warn("nodes", fmt.Errorf("invalid pgxc_node rows"))
			return false
		}
		env.Nodes = nodes
	}
	return true
}

func probeLocalNode(ctx context.Context, p CapabilityProber, env *Environment, replication string) {
	value, err := p.Probe(ctx, "local_node", "SELECT node_name || '|' || host || '|' || port::text FROM pg_catalog.pg_node_env")
	if err != nil {
		env.warn("local_node", err)
		return
	}
	fields := strings.Split(strings.TrimSpace(value), "|")
	if len(fields) != 3 {
		env.warn("local_node", fmt.Errorf("invalid pg_node_env row"))
		return
	}
	port, err := strconv.Atoi(fields[2])
	if err != nil {
		env.warn("local_node", fmt.Errorf("invalid pg_node_env port"))
		return
	}
	role := NodeRoleDNPrimary
	if replication == "standby" {
		role = NodeRoleDNStandby
	}
	env.Nodes = []Node{{Name: fields[0], Role: role, Host: fields[1], Port: port}}
}

func parseNodes(value string) ([]Node, bool) {
	var nodes []Node
	for _, line := range strings.FieldsFunc(value, func(r rune) bool { return r == '\n' || r == ';' }) {
		fields := strings.Split(line, "|")
		if len(fields) != 4 || strings.TrimSpace(fields[0]) == "" {
			return nil, false
		}
		port, err := strconv.Atoi(strings.TrimSpace(fields[3]))
		if err != nil {
			return nil, false
		}
		role, ok := nodeRole(strings.TrimSpace(fields[1]))
		if !ok {
			return nil, false
		}
		nodes = append(nodes, Node{
			Name: strings.TrimSpace(fields[0]), Role: role,
			Host: strings.TrimSpace(fields[2]), Port: port,
		})
	}
	return nodes, true
}

func nodeRole(raw string) (NodeRole, bool) {
	switch raw {
	case "C":
		return NodeRoleCN, true
	case "D":
		return NodeRoleDNPrimary, true
	case "S":
		return NodeRoleDNStandby, true
	default:
		return "", false
	}
}

func hasNode(nodes []Node, name string) bool {
	for _, node := range nodes {
		if node.Name == name {
			return true
		}
	}
	return false
}

func hasRole(nodes []Node, role NodeRole) bool {
	for _, node := range nodes {
		if node.Role == role {
			return true
		}
	}
	return false
}

func probeCapabilities(ctx context.Context, p CapabilityProber, env *Environment) {
	probes := []struct {
		name  string
		query string
		set   func(string)
	}{
		{"admin", "SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname = current_user", func(v string) { env.Capabilities[CapabilityAdmin] = truthy(v) }},
		{"thread_pool_enabled", "SHOW enable_thread_pool", func(v string) { env.Capabilities[capabilityThreadPoolEnabled] = truthy(v) }},
		{"thread_pool_view", catalogRelationProbe("dbe_perf", "global_threadpool_status"), func(v string) { env.Capabilities[capabilityThreadPoolView] = truthy(v) }},
		{"global_plan_cache", catalogRelationProbe("dbe_perf", "global_plancache_status"), func(v string) { env.Capabilities[CapabilityGlobalPlanCache] = truthy(v) }},
		{"statement_history_relation", catalogRelationProbe("dbe_perf", "statement_history"), func(v string) { env.Capabilities[capabilityStatementHistoryRelation] = truthy(v) }},
		{"statement_history_identity_columns", statementHistoryColumnProbe("unique_sql_id"), func(v string) { env.Capabilities[capabilityStatementHistoryIdentity] = truthy(v) }},
		{"statement_history_n_hard_parse", statementHistoryColumnProbe("n_hard_parse"), func(v string) { env.Capabilities[CapabilityHardParseCounters] = truthy(v) }},
		{"global_lock_views", catalogRelationProbe("dbe_perf", "global_locks"), func(v string) { env.Capabilities[CapabilityGlobalLockViews] = truthy(v) }},
		{"memory_node_views", catalogRelationProbe("dbe_perf", "memory_node_detail"), func(v string) { env.Capabilities[CapabilityMemoryNodeViews] = truthy(v) }},
		{"wal_sender_views", catalogFunctionProbe("pg_stat_get_wal_senders"), func(v string) { env.Capabilities[CapabilityWALSenderViews] = truthy(v) }},
		{"standby_control", catalogFunctionProbe("pg_promote"), func(v string) { env.Capabilities[CapabilityStandbyControl] = truthy(v) }},
		{"distributed_stream", catalogRelationProbe("dbe_perf", "global_session_stat"), func(v string) { env.Capabilities[CapabilityPoolerViews] = truthy(v) }},
		{"gtm", catalogFunctionProbe("pgxc_gtm_snapshot_status"), func(v string) { env.Capabilities[capabilityGTM] = truthy(v) }},
		{"dynamic_memory", catalogFunctionProbe("pv_total_memory_detail"), func(v string) { env.Capabilities[capabilityDynamicMemory] = truthy(v) }},
		{"database_cpu", catalogRelationProbe("dbe_perf", "os_runtime"), func(v string) { env.Capabilities[capabilityDatabaseCPU] = truthy(v) }},
		{"vacuum_stats", catalogRelationProbe("pg_catalog", "pg_stat_all_tables"), func(v string) { env.Capabilities[capabilityVacuumStats] = truthy(v) }},
		{"plan_index_unusable", "SELECT count(*) FROM pg_attribute WHERE attrelid='pg_catalog.pg_index'::regclass AND attname='indisusable'", func(v string) { env.Capabilities[capabilityPlanIndexUnusable] = nonzero(v) }},
		{"plan_column_stats", "SELECT count(*) FROM pg_attribute WHERE attrelid='pg_catalog.pg_attribute'::regclass AND attname='attstattarget'", func(v string) { env.Capabilities[capabilityPlanColumnStats] = nonzero(v) }},
		{"plan_ndistinct", "SELECT count(*) FROM pg_attribute WHERE attrelid='pg_catalog.pg_attribute'::regclass AND attname='attoptions'", func(v string) { env.Capabilities[capabilityPlanNDistinct] = nonzero(v) }},
		{"plan_extended_stats", "SELECT count(*) FROM pg_class WHERE relnamespace=(SELECT oid FROM pg_namespace WHERE nspname='pg_catalog') AND relname='pg_statistic_ext'", func(v string) { env.Capabilities[capabilityPlanExtendedStats] = nonzero(v) }},
	}
	for _, probe := range probes {
		value, err := p.Probe(ctx, probe.name, probe.query)
		if err != nil {
			env.warn(probe.name, err)
			continue
		}
		probe.set(value)
	}
	env.Capabilities[CapabilityStatementHistory] = env.Capabilities[capabilityStatementHistoryRelation] && env.Capabilities[capabilityStatementHistoryIdentity]
	env.Capabilities[CapabilityHardParseCounters] = env.Capabilities[CapabilityStatementHistory] && env.Capabilities[CapabilityHardParseCounters]
	env.Capabilities[CapabilityThreadPool] = env.Capabilities[capabilityThreadPoolEnabled] && env.Capabilities[capabilityThreadPoolView]
}

func catalogRelationProbe(schema, relation string) string {
	return "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = '" + schema + "' AND c.relname = '" + relation + "' AND has_table_privilege(c.oid, 'SELECT'))"
}
func catalogFunctionProbe(name string) string {
	return "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_proc p WHERE p.proname = '" + name + "' AND has_function_privilege(p.oid, 'EXECUTE'))"
}
func statementHistoryColumnProbe(column string) string {
	return "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid = a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'dbe_perf' AND c.relname = 'statement_history' AND a.attname = '" + column + "' AND has_table_privilege(c.oid, 'SELECT'))"
}

func (e *Environment) warn(name string, err error) {
	e.Warnings = append(e.Warnings, fmt.Sprintf("%s: %v", name, err))
}

func present(value string) bool { return strings.TrimSpace(value) != "" }
func nonzero(value string) bool { return strings.TrimSpace(value) != "0" && present(value) }

func (e Environment) Applicable(def ScenarioDefinition) bool {
	if !e.Supported {
		return false
	}
	class := e.class()
	for _, appliesTo := range def.AppliesTo {
		if appliesTo == class {
			return true
		}
	}
	return false
}

func (e Environment) Missing(requirements []Requirement) []Requirement {
	missing := make([]Requirement, 0, len(requirements))
	for _, requirement := range requirements {
		if !e.hasRequirement(requirement) {
			missing = append(missing, requirement)
		}
	}
	return missing
}

func (e Environment) class() EnvironmentClass {
	switch {
	case e.Product == ProductOpenGauss:
		return EnvironmentOpenGauss
	case e.Product == ProductGaussDB && e.Topology == TopologyDistributed:
		return EnvironmentDistributedGaussDB
	case e.Product == ProductGaussDB && e.Topology == TopologyCentralized:
		return EnvironmentCentralizedGaussDB
	default:
		return ""
	}
}

func (e Environment) hasRequirement(requirement Requirement) bool {
	if requirement == RequirementPrimaryStandby {
		return e.Topology == TopologyPrimaryStandby || e.Capabilities[capabilityPrimaryStandby] || hasRole(e.Nodes, NodeRoleDNStandby)
	}
	capability, ok := requirementCapability[requirement]
	return ok && e.Capabilities[capability]
}

var requirementCapability = map[Requirement]Capability{
	RequirementStatementHistory:      CapabilityStatementHistory,
	RequirementHardParseCounters:     CapabilityHardParseCounters,
	RequirementGlobalPlanCache:       CapabilityGlobalPlanCache,
	RequirementGlobalLockViews:       CapabilityGlobalLockViews,
	RequirementThreadPool:            CapabilityThreadPool,
	RequirementPoolerViews:           CapabilityPoolerViews,
	RequirementMemoryNodeViews:       CapabilityMemoryNodeViews,
	RequirementWALSenderViews:        CapabilityWALSenderViews,
	RequirementStandbyControl:        CapabilityStandbyControl,
	RequirementExternalFaultProvider: CapabilityExternalFaultProvider,
	RequirementGTM:                   capabilityGTM,
}
