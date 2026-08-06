package gsbench

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type ScenarioDefinition struct {
	Code                 ScenarioCode
	Name                 string
	Category             Category
	Risk                 RiskLevel
	AppliesTo            []EnvironmentClass
	Requires             []Requirement
	FallbackRequirements []Requirement
}

type ScenarioCatalog struct {
	byCode  map[ScenarioCode]ScenarioDefinition
	byName  map[string]ScenarioDefinition
	aliases map[string]ScenarioDefinition
	codes   []ScenarioCode
}

func NewScenarioCatalog(definitions []ScenarioDefinition) (*ScenarioCatalog, error) {
	catalog := &ScenarioCatalog{
		byCode:  make(map[ScenarioCode]ScenarioDefinition, len(definitions)),
		byName:  make(map[string]ScenarioDefinition, len(definitions)),
		aliases: make(map[string]ScenarioDefinition),
		codes:   make([]ScenarioCode, 0, len(definitions)),
	}
	for _, definition := range definitions {
		if definition.Code < 100 || definition.Code > 999 {
			return nil, fmt.Errorf("scenario code %d must be three digits", definition.Code)
		}
		if Category(definition.Code/100) != definition.Category {
			return nil, fmt.Errorf("scenario code %d does not match category %d", definition.Code, definition.Category)
		}
		definition.Name = strings.TrimSpace(definition.Name)
		if definition.Name == "" {
			return nil, fmt.Errorf("scenario code %d has an empty name", definition.Code)
		}
		if _, exists := catalog.byCode[definition.Code]; exists {
			return nil, fmt.Errorf("duplicate scenario code %d", definition.Code)
		}
		if _, exists := catalog.byName[definition.Name]; exists {
			return nil, fmt.Errorf("duplicate scenario name %q", definition.Name)
		}
		definition.AppliesTo = append([]EnvironmentClass(nil), definition.AppliesTo...)
		definition.Requires = append([]Requirement(nil), definition.Requires...)
		definition.FallbackRequirements = append(
			[]Requirement(nil),
			definition.FallbackRequirements...,
		)
		catalog.byCode[definition.Code] = definition
		catalog.byName[definition.Name] = definition
		catalog.codes = append(catalog.codes, definition.Code)
	}
	sort.Slice(catalog.codes, func(i, j int) bool { return catalog.codes[i] < catalog.codes[j] })
	return catalog, nil
}

func (c *ScenarioCatalog) Codes() []ScenarioCode {
	return append([]ScenarioCode(nil), c.codes...)
}

func (c *ScenarioCatalog) Definitions() []ScenarioDefinition {
	definitions := make([]ScenarioDefinition, 0, len(c.codes))
	for _, code := range c.codes {
		definitions = append(definitions, cloneScenarioDefinition(c.byCode[code]))
	}
	return definitions
}

func (c *ScenarioCatalog) Resolve(input string) (ScenarioDefinition, error) {
	input = strings.TrimSpace(input)
	if len(input) == 3 {
		if code, err := strconv.ParseUint(input, 10, 16); err == nil {
			if definition, ok := c.byCode[ScenarioCode(code)]; ok {
				return cloneScenarioDefinition(definition), nil
			}
		}
	}
	if definition, ok := c.byName[input]; ok {
		return cloneScenarioDefinition(definition), nil
	}
	if definition, ok := c.aliases[input]; ok {
		return cloneScenarioDefinition(definition), nil
	}
	return ScenarioDefinition{}, fmt.Errorf("unknown scenario %q", input)
}

func (c *ScenarioCatalog) MustCode(code ScenarioCode) ScenarioDefinition {
	definition, err := c.LookupCode(code)
	if err != nil {
		panic(fmt.Sprintf("unknown scenario code %d", code))
	}
	return definition
}

func (c *ScenarioCatalog) LookupCode(code ScenarioCode) (ScenarioDefinition, error) {
	definition, ok := c.byCode[code]
	if !ok {
		return ScenarioDefinition{}, fmt.Errorf("unknown scenario code %d", code)
	}
	return cloneScenarioDefinition(definition), nil
}

func cloneScenarioDefinition(definition ScenarioDefinition) ScenarioDefinition {
	definition.AppliesTo = append([]EnvironmentClass(nil), definition.AppliesTo...)
	definition.Requires = append([]Requirement(nil), definition.Requires...)
	definition.FallbackRequirements = append(
		[]Requirement(nil),
		definition.FallbackRequirements...,
	)
	return definition
}

func DesignedScenarioCodes() []ScenarioCode {
	return DefaultScenarioCatalog().Codes()
}

func DefaultScenarioCatalog() *ScenarioCatalog {
	catalog, err := NewScenarioCatalog(defaultScenarioDefinitions())
	if err != nil {
		panic(err)
	}
	catalog.aliases["planchange_index_unusable"] = catalog.byCode[602]
	return catalog
}

func defaultScenarioDefinitions() []ScenarioDefinition {
	definitions := make([]ScenarioDefinition, 0, 90)
	add := func(code ScenarioCode, name string) {
		metadata := scenarioMetadataFor(code)
		definitions = append(definitions, ScenarioDefinition{
			Code:                 code,
			Name:                 name,
			Category:             Category(code / 100),
			Risk:                 metadata.Risk,
			AppliesTo:            metadata.AppliesTo,
			Requires:             metadata.Requires,
			FallbackRequirements: metadata.FallbackRequirements,
		})
	}
	for _, definition := range []struct {
		code ScenarioCode
		name string
	}{
		{101, "tp_cpu"}, {102, "ap_cpu"}, {103, "mixed_cpu"},
		{201, "memory_workmem_sort"}, {202, "memory_workmem_hash"}, {203, "memory_sharedbuffer_churn"},
		{204, "memory_plancache_growth"}, {205, "memory_session_context_growth"}, {206, "memory_global_cache_pressure"},
		{207, "memory_total_pressure"}, {208, "memory_retention"}, {209, "memory_oom_guarded"},
		{301, "io_sequential_read"}, {302, "io_random_read"}, {303, "io_wal_write"}, {304, "io_temp_spill"}, {305, "io_checkpoint_flush"},
		{321, "network_client_egress"}, {322, "network_client_ingress"}, {331, "network_cn_dn_stream"},
		{332, "network_distributed_shuffle"}, {333, "network_distributed_broadcast"},
		{341, "network_latency_injection"}, {342, "network_packet_loss"}, {343, "network_partition"},
		{401, "connection_pool"}, {402, "thread_pool"}, {403, "connection_churn"}, {404, "threadpool_queue"}, {405, "pooler_cn_dn_pressure"},
		{501, "lock_row_chain"}, {502, "lock_table_exclusive"}, {503, "lock_ddl_wait"}, {504, "lock_deadlock"},
		{505, "lock_ddl_blocks_dml"}, {506, "lock_select_blocks_ddl"}, {507, "lock_vacuum_blocks_ddl"},
		{508, "lock_ddl_blocks_vacuum"}, {509, "lock_createindex_blocks_dml"}, {510, "lock_dml_blocks_createindex"},
		{511, "lock_distributed_ddl_global"}, {512, "lock_distributed_txn_chain"},
		{601, "planchange_stats_target"}, {602, "planchange_stats_lookup"}, {603, "planchange_stats_ndistinct"},
		{604, "planchange_stats_extended"}, {605, "planchange_index_drop"}, {606, "planchange_index_shape"},
		{621, "hardparse_literal_flood"}, {622, "hardparse_unprepared"}, {623, "hardparse_force_custom"},
		{624, "hardparse_session_churn"}, {625, "hardparse_ddl_invalidation"}, {626, "hardparse_gpc_bypass"},
		{701, "replication_wal_pressure"}, {702, "replication_sync_commit_block"}, {703, "replication_replay_delay"},
		{704, "replication_standby_read_conflict"}, {705, "replication_network_delay"}, {706, "replication_network_partition"},
		{721, "cluster_data_skew"}, {722, "cluster_cn_hotspot"}, {723, "cluster_dn_hotspot"}, {724, "cluster_cross_shard_txn"},
		{725, "cluster_gtm_pressure"}, {726, "cluster_partial_node_slow"}, {727, "cluster_node_failure"}, {728, "cluster_switchover"},
		{801, "vacuum_pressure"},
	} {
		add(definition.code, definition.name)
	}
	for _, definition := range TableLockConflictDefinitions() {
		add(definition.Code, definition.Name)
	}
	return definitions
}

type scenarioMetadata struct {
	Risk                 RiskLevel
	AppliesTo            []EnvironmentClass
	Requires             []Requirement
	FallbackRequirements []Requirement
}

var allEnvironmentClasses = []EnvironmentClass{
	EnvironmentOpenGauss,
	EnvironmentCentralizedGaussDB,
	EnvironmentDistributedGaussDB,
}

func scenarioMetadataFor(code ScenarioCode) scenarioMetadata {
	metadata := scenarioMetadata{Risk: RiskA, AppliesTo: allEnvironmentClasses}
	switch code {
	case 209, 305:
		metadata.Risk = RiskB
	case 341, 342, 343:
		metadata.Risk = RiskC
		metadata.Requires = []Requirement{RequirementExternalFaultProvider}
	case 402, 404:
		metadata.Requires = []Requirement{RequirementThreadPool}
	case 405:
		metadata.AppliesTo = []EnvironmentClass{EnvironmentDistributedGaussDB}
		metadata.Requires = []Requirement{RequirementPoolerViews}
	case 511, 512:
		metadata.AppliesTo = []EnvironmentClass{EnvironmentDistributedGaussDB}
		metadata.Requires = []Requirement{RequirementGlobalLockViews}
	case 621, 622, 623, 624, 625:
		metadata.Requires = []Requirement{RequirementHardParseCounters}
		metadata.FallbackRequirements = []Requirement{
			RequirementHardParseCounters,
		}
	case 626:
		metadata.Requires = []Requirement{RequirementGlobalPlanCache, RequirementHardParseCounters}
		metadata.FallbackRequirements = []Requirement{
			RequirementHardParseCounters,
		}
	case 701, 702:
		metadata.Requires = []Requirement{RequirementPrimaryStandby, RequirementWALSenderViews}
	case 703:
		metadata.Risk = RiskB
		metadata.Requires = []Requirement{RequirementPrimaryStandby, RequirementStandbyControl}
	case 704:
		metadata.Requires = []Requirement{RequirementPrimaryStandby, RequirementWALSenderViews}
	case 705, 706:
		metadata.Risk = RiskC
		metadata.Requires = []Requirement{RequirementPrimaryStandby, RequirementExternalFaultProvider}
	case 721, 722, 723, 724:
		metadata.AppliesTo = []EnvironmentClass{EnvironmentDistributedGaussDB}
	case 725:
		metadata.AppliesTo = []EnvironmentClass{EnvironmentDistributedGaussDB}
		metadata.Requires = []Requirement{RequirementGTM}
	case 726, 727, 728:
		metadata.Risk = RiskC
		metadata.AppliesTo = []EnvironmentClass{EnvironmentDistributedGaussDB}
		metadata.Requires = []Requirement{RequirementExternalFaultProvider}
	case 331, 332, 333:
		metadata.AppliesTo = []EnvironmentClass{EnvironmentDistributedGaussDB}
	}
	return metadata
}

type LockMode string

const (
	LockModeAccessShare          LockMode = "AS"
	LockModeRowShare             LockMode = "RS"
	LockModeRowExclusive         LockMode = "RX"
	LockModeShareUpdateExclusive LockMode = "SUE"
	LockModeShare                LockMode = "S"
	LockModeShareRowExclusive    LockMode = "SRX"
	LockModeExclusive            LockMode = "X"
	LockModeAccessExclusive      LockMode = "AX"
)

type LockConflictDefinition struct {
	Code   ScenarioCode
	Name   string
	Holder LockMode
	Waiter LockMode
}

func TableLockConflictDefinitions() []LockConflictDefinition {
	return append([]LockConflictDefinition(nil), tableLockConflictDefinitions...)
}

var tableLockConflictDefinitions = []LockConflictDefinition{
	{520, "lockmode_accessshare_accessexclusive", LockModeAccessShare, LockModeAccessExclusive},
	{521, "lockmode_rowshare_exclusive", LockModeRowShare, LockModeExclusive},
	{522, "lockmode_rowshare_accessexclusive", LockModeRowShare, LockModeAccessExclusive},
	{523, "lockmode_rowexclusive_share", LockModeRowExclusive, LockModeShare},
	{524, "lockmode_rowexclusive_sharerowexclusive", LockModeRowExclusive, LockModeShareRowExclusive},
	{525, "lockmode_rowexclusive_exclusive", LockModeRowExclusive, LockModeExclusive},
	{526, "lockmode_rowexclusive_accessexclusive", LockModeRowExclusive, LockModeAccessExclusive},
	{527, "lockmode_shareupdateexclusive_self", LockModeShareUpdateExclusive, LockModeShareUpdateExclusive},
	{528, "lockmode_shareupdateexclusive_share", LockModeShareUpdateExclusive, LockModeShare},
	{529, "lockmode_shareupdateexclusive_sharerowexclusive", LockModeShareUpdateExclusive, LockModeShareRowExclusive},
	{530, "lockmode_shareupdateexclusive_exclusive", LockModeShareUpdateExclusive, LockModeExclusive},
	{531, "lockmode_shareupdateexclusive_accessexclusive", LockModeShareUpdateExclusive, LockModeAccessExclusive},
	{532, "lockmode_share_sharerowexclusive", LockModeShare, LockModeShareRowExclusive},
	{533, "lockmode_share_exclusive", LockModeShare, LockModeExclusive},
	{534, "lockmode_share_accessexclusive", LockModeShare, LockModeAccessExclusive},
	{535, "lockmode_sharerowexclusive_self", LockModeShareRowExclusive, LockModeShareRowExclusive},
	{536, "lockmode_sharerowexclusive_exclusive", LockModeShareRowExclusive, LockModeExclusive},
	{537, "lockmode_sharerowexclusive_accessexclusive", LockModeShareRowExclusive, LockModeAccessExclusive},
	{538, "lockmode_exclusive_self", LockModeExclusive, LockModeExclusive},
	{539, "lockmode_exclusive_accessexclusive", LockModeExclusive, LockModeAccessExclusive},
	{540, "lockmode_accessexclusive_self", LockModeAccessExclusive, LockModeAccessExclusive},
}
