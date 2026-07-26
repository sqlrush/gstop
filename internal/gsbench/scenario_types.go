package gsbench

type ScenarioCode uint16

type Category uint8

const (
	CategoryCPU                Category = 1
	CategoryMemory             Category = 2
	CategoryIONetwork          Category = 3
	CategoryConnectionThread   Category = 4
	CategoryLockConcurrency    Category = 5
	CategoryPlan               Category = 6
	CategoryReplicationCluster Category = 7
	CategoryMaintenance        Category = 8
)

type RiskLevel string

const (
	RiskA RiskLevel = "A"
	RiskB RiskLevel = "B"
	RiskC RiskLevel = "C"
)

type EnvironmentClass string

const (
	EnvironmentOpenGauss          EnvironmentClass = "openGauss"
	EnvironmentCentralizedGaussDB EnvironmentClass = "centralized_gaussdb"
	EnvironmentDistributedGaussDB EnvironmentClass = "distributed_gaussdb"
)

type Requirement string

const (
	RequirementStatementHistory      Requirement = "statement_history"
	RequirementHardParseCounters     Requirement = "hard_parse_counters"
	RequirementGlobalPlanCache       Requirement = "global_plan_cache"
	RequirementGlobalLockViews       Requirement = "global_lock_views"
	RequirementThreadPool            Requirement = "thread_pool"
	RequirementPoolerViews           Requirement = "pooler_views"
	RequirementMemoryNodeViews       Requirement = "memory_node_views"
	RequirementWALSenderViews        Requirement = "wal_sender_views"
	RequirementStandbyControl        Requirement = "standby_control"
	RequirementExternalFaultProvider Requirement = "external_fault_provider"
	RequirementPrimaryStandby        Requirement = "primary_standby"
	RequirementGTM                   Requirement = "gtm"
)

type Product string

const (
	ProductUnknown   Product = "unknown"
	ProductOpenGauss Product = "openGauss"
	ProductGaussDB   Product = "GaussDB"
)

type Topology string

const (
	TopologyUnknown        Topology = "unknown"
	TopologyStandalone     Topology = "standalone"
	TopologyPrimaryStandby Topology = "primary_standby"
	TopologyCentralized    Topology = "centralized_gaussdb"
	TopologyDistributed    Topology = "distributed_gaussdb"
)

type NodeRole string

const (
	NodeRoleCN        NodeRole = "CN"
	NodeRoleDNPrimary NodeRole = "DN_PRIMARY"
	NodeRoleDNStandby NodeRole = "DN_STANDBY"
	NodeRoleGTM       NodeRole = "GTM"
	NodeRoleCM        NodeRole = "CM"
	NodeRoleETCD      NodeRole = "ETCD"
)

type Capability string

const (
	CapabilityStatementHistory      Capability = "statement_history"
	CapabilityHardParseCounters     Capability = "hard_parse_counters"
	CapabilityGlobalPlanCache       Capability = "global_plan_cache"
	CapabilityGlobalLockViews       Capability = "global_lock_views"
	CapabilityThreadPool            Capability = "thread_pool"
	CapabilityPoolerViews           Capability = "pooler_views"
	CapabilityMemoryNodeViews       Capability = "memory_node_views"
	CapabilityWALSenderViews        Capability = "wal_sender_views"
	CapabilityStandbyControl        Capability = "standby_control"
	CapabilityExternalFaultProvider Capability = "external_fault_provider"
	CapabilityAdmin                 Capability = "admin"
)

type CapabilitySet map[Capability]bool

type Node struct {
	Name  string
	Role  NodeRole
	Shard string
	Host  string
	Port  int
}

type Environment struct {
	Product      Product
	Version      string
	Topology     Topology
	Nodes        []Node
	Capabilities CapabilitySet
	Warnings     []string
	Supported    bool
}
