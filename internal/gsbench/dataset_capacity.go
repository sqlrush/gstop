package gsbench

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type StorageCapacityFact struct {
	NodeName   string
	TotalBytes int64
	FreeBytes  int64
	Source     string
}

type DatasetCapacityProvider interface {
	CapacityFacts(ctx context.Context, env Environment) ([]StorageCapacityFact, error)
}

type CapacityStatus struct {
	Source    string
	NodeCount int
	Error     string
}

type DatasetExternalProviders struct {
	Capacity DatasetCapacityProvider
	Physical DatasetPhysicalProvider
}

type unavailableDatasetCapacityProvider struct {
	err error
}

func (p unavailableDatasetCapacityProvider) CapacityFacts(
	context.Context,
	Environment,
) ([]StorageCapacityFact, error) {
	return nil, p.err
}

type tablespaceQuotaCapacityProvider struct {
	db journalDatabase
}

func (p tablespaceQuotaCapacityProvider) CapacityFacts(
	ctx context.Context,
	_ Environment,
) ([]StorageCapacityFact, error) {
	var tablespace string
	var quotaText sql.NullString
	var used int64
	err := p.db.Scan(ctx, `WITH selected AS (
			SELECT CASE
				WHEN NULLIF(trim(current_setting('default_tablespace')),'') IS NULL
					THEN d.dattablespace
				ELSE (
					SELECT configured.oid
					FROM pg_catalog.pg_tablespace configured
					WHERE configured.spcname=NULLIF(trim(current_setting('default_tablespace')),'')
				)
			END AS oid
			FROM pg_catalog.pg_database d
			WHERE d.datname=current_database()
		)
		SELECT t.spcname,
			NULLIF(trim(t.spcmaxsize::text),'') AS quota_text,
			pg_catalog.pg_tablespace_size(t.oid) AS used_bytes
		FROM selected
		JOIN pg_catalog.pg_tablespace t ON t.oid=selected.oid`,
		nil, &tablespace, &quotaText, &used,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"query documented default-tablespace quota: %w; provider wiring is not implemented in this build",
			err,
		)
	}
	if !quotaText.Valid {
		return nil, fmt.Errorf(
			"default tablespace %q has no finite spcmaxsize; provider wiring is not implemented in this build",
			tablespace,
		)
	}
	quota, err := parseTablespaceQuota(quotaText.String)
	if err != nil || quota <= 0 {
		return nil, fmt.Errorf(
			"default tablespace %q has invalid spcmaxsize %q: %v; provider wiring is not implemented in this build",
			tablespace, quotaText.String, err,
		)
	}
	if used < 0 || used > quota {
		return nil, fmt.Errorf(
			"invalid default tablespace usage: tablespace=%s quota=%d used=%d",
			tablespace, quota, used,
		)
	}
	return []StorageCapacityFact{{
		NodeName:   tablespace,
		TotalBytes: quota,
		FreeBytes:  quota - used,
		Source:     "tablespace_quota",
	}}, nil
}

func parseTablespaceQuota(value string) (int64, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if value == "" {
		return 0, fmt.Errorf("empty quota")
	}
	index := 0
	for index < len(value) && value[index] >= '0' && value[index] <= '9' {
		index++
	}
	if index == 0 {
		return 0, fmt.Errorf("quota has no numeric value")
	}
	number, err := strconv.ParseInt(value[:index], 10, 64)
	if err != nil {
		return 0, err
	}
	unit := strings.TrimSpace(value[index:])
	multipliers := map[string]int64{
		"": 1, "B": 1,
		"K": 1 << 10, "KB": 1 << 10,
		"M": 1 << 20, "MB": 1 << 20,
		"G": 1 << 30, "GB": 1 << 30,
		"T": 1 << 40, "TB": 1 << 40,
		"P": 1 << 50, "PB": 1 << 50,
	}
	multiplier, ok := multipliers[unit]
	if !ok {
		return 0, fmt.Errorf("unsupported quota unit %q", unit)
	}
	if number <= 0 || number > int64(^uint64(0)>>1)/multiplier {
		return 0, fmt.Errorf("quota is outside int64 range")
	}
	return number * multiplier, nil
}

type storageStat func(path string) (totalBytes, freeBytes int64, err error)

type localDataDirectoryCapacityProvider struct {
	db   journalDatabase
	host string
	stat storageStat
}

func (p localDataDirectoryCapacityProvider) CapacityFacts(
	ctx context.Context,
	_ Environment,
) ([]StorageCapacityFact, error) {
	if !isProvablyLocalDatabaseHost(p.host) {
		return nil, fmt.Errorf(
			"database endpoint %q is not provably local; provider wiring is not implemented in this build",
			p.host,
		)
	}
	var dataDirectory string
	if err := p.db.Scan(
		ctx, "SHOW data_directory", nil, &dataDirectory,
	); err != nil {
		return nil, fmt.Errorf("read server data_directory: %w", err)
	}
	dataDirectory = filepath.Clean(strings.TrimSpace(dataDirectory))
	if !filepath.IsAbs(dataDirectory) {
		return nil, fmt.Errorf(
			"server data_directory %q is not an absolute local path", dataDirectory)
	}
	stat := p.stat
	if stat == nil {
		stat = statLocalStorage
	}
	total, free, err := stat(dataDirectory)
	if err != nil {
		return nil, fmt.Errorf(
			"stat exact server data_directory %q: %w; no client-path fallback is allowed",
			dataDirectory, err,
		)
	}
	return []StorageCapacityFact{{
		NodeName:   "local",
		TotalBytes: total,
		FreeBytes:  free,
		Source:     "local_data_directory",
	}}, nil
}

func isProvablyLocalDatabaseHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" || strings.HasPrefix(host, "/") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func statLocalStorage(path string) (int64, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	if !info.IsDir() {
		return 0, 0, fmt.Errorf("path is not a directory")
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	return int64(stat.Blocks) * int64(stat.Bsize),
		int64(stat.Bavail) * int64(stat.Bsize),
		nil
}

func selectDatasetCapacityProvider(
	cfg BenchConfig,
	env Environment,
	db journalDatabase,
	external DatasetExternalProviders,
) (DatasetCapacityProvider, error) {
	name := defaultDatasetProvider(cfg.Data.CapacityProvider)
	if env.Topology == TopologyDistributed {
		if external.Capacity != nil {
			return external.Capacity, nil
		}
		return nil, fmt.Errorf(
			"distributed capacity requires verified per-primary-DN facts; provider wiring is not implemented in this build",
		)
	}
	switch name {
	case "auto":
		if isProvablyLocalDatabaseHost(cfg.Database.Host) {
			return localDataDirectoryCapacityProvider{
				db: db, host: cfg.Database.Host,
			}, nil
		}
		return tablespaceQuotaCapacityProvider{db: db}, nil
	case "local_data_directory":
		return localDataDirectoryCapacityProvider{
			db: db, host: cfg.Database.Host,
		}, nil
	case "tablespace_quota":
		return tablespaceQuotaCapacityProvider{db: db}, nil
	default:
		return nil, fmt.Errorf("unsupported capacity provider %q", name)
	}
}

func resolveDatasetCapacity(
	ctx context.Context,
	provider DatasetCapacityProvider,
	env Environment,
	dryRun bool,
	targetBytes int64,
	minFreePercent int,
) (Capacity, CapacityStatus, error) {
	facts, err := provider.CapacityFacts(ctx, env)
	if err == nil {
		var capacity Capacity
		facts, err = requiredCapacityFacts(env, facts)
		if err == nil {
			for _, fact := range facts {
				if fact.TotalBytes <= 0 || fact.FreeBytes < 0 || fact.FreeBytes > fact.TotalBytes {
					err = fmt.Errorf("invalid target capacity fact for node %q: total=%d free=%d",
						fact.NodeName, fact.TotalBytes, fact.FreeBytes)
					break
				}
				reservePercent := minFreePercent
				if reservePercent == 0 {
					reservePercent = 20
				}
				reserved := fact.TotalBytes * int64(reservePercent) / 100
				if fact.FreeBytes <= reserved {
					err = fmt.Errorf(
						"target node %q is below disk reserve: free=%d reserved=%d",
						fact.NodeName, fact.FreeBytes, reserved,
					)
					break
				}
				capacity.TotalBytes += fact.TotalBytes
				capacity.FreeBytes += fact.FreeBytes
			}
		}
		if err == nil {
			if env.Topology == TopologyDistributed {
				targetShare := (targetBytes + int64(len(facts)) - 1) /
					int64(len(facts))
				for _, fact := range facts {
					reservePercent := minFreePercent
					if reservePercent == 0 {
						reservePercent = 20
					}
					safe := fact.FreeBytes -
						fact.TotalBytes*int64(reservePercent)/100
					if targetShare > safe {
						err = fmt.Errorf(
							"primary DN %q cannot fit target_share=%d: free=%d reserved=%d safe_available=%d",
							fact.NodeName,
							targetShare,
							fact.FreeBytes,
							fact.TotalBytes*int64(reservePercent)/100,
							safe,
						)
						break
					}
				}
			}
		}
		if err == nil {
			source := facts[0].Source
			if source == "" {
				source = "database-provider"
			}
			return capacity, CapacityStatus{Source: source, NodeCount: len(facts)}, nil
		}
	}
	if !dryRun {
		return Capacity{}, CapacityStatus{}, fmt.Errorf("target database capacity unavailable: %w", err)
	}
	total := int64(4 << 40)
	if targetBytes > total/2 {
		total = targetBytes * 2
	}
	return Capacity{
			TotalBytes: total,
			FreeBytes:  total,
		}, CapacityStatus{
			Source: "unavailable",
			Error:  err.Error(),
		}, nil
}

func requiredCapacityFacts(env Environment, facts []StorageCapacityFact) ([]StorageCapacityFact, error) {
	if len(facts) == 0 {
		return nil, fmt.Errorf("capacity provider returned no target storage facts")
	}
	if env.Topology != TopologyDistributed {
		return facts, nil
	}
	byNode := make(map[string]StorageCapacityFact, len(facts))
	for _, fact := range facts {
		name := strings.TrimSpace(fact.NodeName)
		if name == "" {
			return nil, fmt.Errorf("distributed capacity fact has empty node name")
		}
		if _, duplicate := byNode[name]; duplicate {
			return nil, fmt.Errorf("duplicate distributed capacity fact for node %s", name)
		}
		byNode[name] = fact
	}
	var primaryNames []string
	for _, node := range env.Nodes {
		if node.Role == NodeRoleDNPrimary {
			primaryNames = append(primaryNames, node.Name)
		}
	}
	if len(primaryNames) == 0 {
		return nil, fmt.Errorf("distributed environment has no primary DN inventory")
	}
	sort.Strings(primaryNames)
	required := make([]StorageCapacityFact, 0, len(primaryNames))
	var missing []string
	for _, name := range primaryNames {
		fact, ok := byNode[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		required = append(required, fact)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("capacity provider missing primary DN facts: %s", strings.Join(missing, ","))
	}
	return required, nil
}
