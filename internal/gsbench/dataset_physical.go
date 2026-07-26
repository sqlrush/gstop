package gsbench

import (
	"context"
	"fmt"
)

type DatasetPhysicalProvider interface {
	DatasetSize(ctx context.Context, schema string) (DatasetSizeSample, error)
	ValidateDatasetLayout(ctx context.Context, plan DatasetPlan) error
}

type catalogDatasetPhysicalProvider struct {
	db journalDatabase
}

type verifiedDistributedPhysicalProvider struct {
	provider DatasetPhysicalProvider
	env      Environment
}

func (e dbDatasetExecutor) DatasetSize(
	ctx context.Context,
	schema string,
) (DatasetSizeSample, error) {
	if e.physicalProvider == nil {
		return DatasetSizeSample{}, fmt.Errorf(
			"physical-size provider is unavailable")
	}
	return e.physicalProvider.DatasetSize(ctx, schema)
}

func (e dbDatasetExecutor) ValidateDatasetLayout(
	ctx context.Context,
	plan DatasetPlan,
) error {
	if e.physicalProvider == nil {
		return fmt.Errorf("physical-size provider is unavailable")
	}
	return e.physicalProvider.ValidateDatasetLayout(ctx, plan)
}

func (p catalogDatasetPhysicalProvider) DatasetSize(
	ctx context.Context,
	schema string,
) (DatasetSizeSample, error) {
	return readCentralDatasetSize(ctx, p.db, schema)
}

func (catalogDatasetPhysicalProvider) ValidateDatasetLayout(
	context.Context,
	DatasetPlan,
) error {
	return nil
}

func selectDatasetPhysicalProvider(
	cfg BenchConfig,
	env Environment,
	db journalDatabase,
	external DatasetExternalProviders,
) (DatasetPhysicalProvider, error) {
	name := defaultDatasetProvider(cfg.Data.PhysicalSizeProvider)
	if env.Topology == TopologyDistributed {
		if external.Physical != nil {
			return verifiedDistributedPhysicalProvider{
				provider: external.Physical,
				env:      env,
			}, nil
		}
		return nil, fmt.Errorf(
			"distributed physical sizing requires a documented CN-global function or verified provider; provider wiring is not implemented in this build",
		)
	}
	switch name {
	case "auto", "catalog":
		return catalogDatasetPhysicalProvider{db: db}, nil
	default:
		return nil, fmt.Errorf("unsupported physical-size provider %q", name)
	}
}

func (p verifiedDistributedPhysicalProvider) DatasetSize(
	ctx context.Context,
	schema string,
) (DatasetSizeSample, error) {
	sample, err := p.provider.DatasetSize(ctx, schema)
	if err != nil {
		return DatasetSizeSample{}, err
	}
	primaryCount := 0
	for _, node := range p.env.Nodes {
		if node.Role == NodeRoleDNPrimary {
			primaryCount++
		}
	}
	if primaryCount == 0 || sample.NodeCount != primaryCount {
		return DatasetSizeSample{}, fmt.Errorf(
			"distributed physical provider primary DN coverage=%d, expected=%d",
			sample.NodeCount, primaryCount,
		)
	}
	if sample.TotalBytes < 0 || sample.Source == "" {
		return DatasetSizeSample{}, fmt.Errorf(
			"distributed physical provider returned invalid sample: %+v", sample)
	}
	return sample, nil
}

func (p verifiedDistributedPhysicalProvider) ValidateDatasetLayout(
	ctx context.Context,
	plan DatasetPlan,
) error {
	return p.provider.ValidateDatasetLayout(ctx, plan)
}

func (e dbDatasetExecutor) CheckCapacity(ctx context.Context) error {
	if e.capacityProvider == nil {
		return fmt.Errorf("target capacity provider is unavailable")
	}
	facts, err := e.capacityProvider.CapacityFacts(ctx, e.env)
	if err != nil {
		return fmt.Errorf("refresh target capacity: %w", err)
	}
	facts, err = requiredCapacityFacts(e.env, facts)
	if err != nil {
		return err
	}
	reservePercent := e.minFreeDiskPercent
	if reservePercent == 0 {
		reservePercent = 20
	}
	for _, fact := range facts {
		if fact.TotalBytes <= 0 || fact.FreeBytes < 0 ||
			fact.FreeBytes > fact.TotalBytes {
			return fmt.Errorf(
				"invalid target capacity fact for node %q: total=%d free=%d",
				fact.NodeName, fact.TotalBytes, fact.FreeBytes,
			)
		}
		reserved := fact.TotalBytes * int64(reservePercent) / 100
		if fact.FreeBytes <= reserved {
			return fmt.Errorf(
				"target node %q crossed disk reserve: free=%d reserved=%d",
				fact.NodeName, fact.FreeBytes, reserved,
			)
		}
	}
	return nil
}

func readCentralDatasetSize(
	ctx context.Context,
	db journalDatabase,
	schema string,
) (DatasetSizeSample, error) {
	rows, err := db.Query(ctx, `SELECT c.relname,
		pg_catalog.pg_relation_size(c.oid) AS heap_bytes,
		pg_catalog.pg_indexes_size(c.oid) AS index_bytes
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relkind='r'
		ORDER BY c.relname`, schema)
	if err != nil {
		return DatasetSizeSample{}, fmt.Errorf("query catalog relation sizes: %w", err)
	}
	defer rows.Close()
	sample := DatasetSizeSample{Source: "database-catalog", NodeCount: 1}
	for rows.Next() {
		var relation string
		var heapBytes, indexBytes int64
		if err := rows.Scan(&relation, &heapBytes, &indexBytes); err != nil {
			return DatasetSizeSample{}, fmt.Errorf("scan catalog relation size: %w", err)
		}
		if heapBytes < 0 || indexBytes < 0 {
			return DatasetSizeSample{}, fmt.Errorf(
				"negative size for relation %s: heap=%d index=%d",
				relation, heapBytes, indexBytes,
			)
		}
		sample.TotalBytes += heapBytes + indexBytes
	}
	if err := rows.Err(); err != nil {
		return DatasetSizeSample{}, fmt.Errorf("read catalog relation sizes: %w", err)
	}
	return sample, nil
}

func validateDistributedFactSalesBalance(
	ctx context.Context,
	db journalDatabase,
	schema string,
) error {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return fmt.Errorf("unsafe dataset schema %q", schema)
	}
	rows, err := db.Query(ctx, `SELECT xc_node_id,count(*)::bigint AS rows
		FROM `+quotedSchema+`.fact_sales
		GROUP BY xc_node_id
		ORDER BY xc_node_id`)
	if err != nil {
		return fmt.Errorf("query fact_sales DN row balance: %w", err)
	}
	defer rows.Close()
	var minRows, maxRows int64
	nodeCount := 0
	for rows.Next() {
		var node string
		var count int64
		if err := rows.Scan(&node, &count); err != nil {
			return fmt.Errorf("scan fact_sales DN row balance: %w", err)
		}
		if nodeCount == 0 || count < minRows {
			minRows = count
		}
		if count > maxRows {
			maxRows = count
		}
		nodeCount++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read fact_sales DN row balance: %w", err)
	}
	if nodeCount < 2 {
		return fmt.Errorf(
			"fact_sales balance requires at least two primary DN row counts")
	}
	if minRows == 0 || (maxRows-minRows)*100 > minRows*10 {
		return fmt.Errorf(
			"fact_sales DN row imbalance exceeds 10%%: min=%d max=%d",
			minRows, maxRows,
		)
	}
	return nil
}
