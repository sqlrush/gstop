package gsbench

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const restoreReplicationGapLimit int64 = 16 << 20

type replicationHealthSample struct {
	InRecovery        bool
	RequiredStandbys  int
	StreamingStandbys int
	ReceiverConnected bool
	ReceiveLocation   string
	ReplayLocation    string
	ReplayGapBytes    int64
}

func evaluateReplicationHealth(
	first replicationHealthSample,
	second replicationHealthSample,
) error {
	if first.InRecovery != second.InRecovery {
		return fmt.Errorf("replication role changed during health sampling")
	}
	gapHealthy := second.ReplayGapBytes >= 0 &&
		(second.ReplayGapBytes <= restoreReplicationGapLimit ||
			(first.ReplayGapBytes > second.ReplayGapBytes))
	if first.InRecovery {
		if !first.ReceiverConnected || !second.ReceiverConnected {
			return fmt.Errorf("standby WAL receiver is not streaming")
		}
		if strings.TrimSpace(first.ReceiveLocation) == "" ||
			strings.TrimSpace(first.ReplayLocation) == "" ||
			strings.TrimSpace(second.ReceiveLocation) == "" ||
			strings.TrimSpace(second.ReplayLocation) == "" {
			return fmt.Errorf("standby receive/replay locations are unavailable")
		}
		if !gapHealthy {
			return fmt.Errorf(
				"standby receive/replay gap is neither bounded nor converging",
			)
		}
		return nil
	}
	if first.RequiredStandbys <= 0 ||
		second.RequiredStandbys != first.RequiredStandbys {
		return fmt.Errorf("primary required-standby count is unavailable")
	}
	if first.StreamingStandbys < first.RequiredStandbys ||
		second.StreamingStandbys < second.RequiredStandbys {
		return fmt.Errorf(
			"primary streaming standbys=%d/%d then %d/%d",
			first.StreamingStandbys,
			first.RequiredStandbys,
			second.StreamingStandbys,
			second.RequiredStandbys,
		)
	}
	if !gapHealthy {
		return fmt.Errorf(
			"primary sender/replay gap is neither bounded nor converging",
		)
	}
	return nil
}

type distributedHealthSample struct {
	CatalogCN   int
	CatalogDN   int
	RuntimeCN   int
	RuntimeDN   int
	GTMMode     string
	GTMSnapshot *gtmSnapshot
}

type gtmSnapshot struct {
	XMin             string
	XMax             string
	CSN              int64
	OldestXMin       string
	TransactionCount int64
	RunningXIDs      string
}

type gtmHealthEvidence struct {
	Mode     string
	Snapshot *gtmSnapshot
}

func evaluateDistributedHealth(sample distributedHealthSample) error {
	if sample.CatalogCN <= 0 || sample.CatalogDN <= 0 {
		return fmt.Errorf("distributed catalog has no required CN/DN members")
	}
	if sample.RuntimeCN != sample.CatalogCN ||
		sample.RuntimeDN != sample.CatalogDN {
		return fmt.Errorf(
			"distributed runtime members CN=%d/%d DN=%d/%d",
			sample.RuntimeCN,
			sample.CatalogCN,
			sample.RuntimeDN,
			sample.CatalogDN,
		)
	}
	mode := strings.ToLower(strings.TrimSpace(sample.GTMMode))
	switch mode {
	case "gtm-free":
		return nil
	case "gtm-lite":
		return fmt.Errorf(
			"NOT_SUPPORTED: GTM-LITE live runtime health evidence is unavailable",
		)
	case "gtm":
		if sample.GTMSnapshot == nil {
			return fmt.Errorf("classic GTM snapshot is unavailable")
		}
		if err := validateGTMSnapshot(*sample.GTMSnapshot); err != nil {
			return fmt.Errorf("classic GTM snapshot is invalid: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("NOT_SUPPORTED: unknown GTM mode %q", mode)
	}
}

func validateGTMSnapshot(snapshot gtmSnapshot) error {
	for name, value := range map[string]string{
		"xmin":       snapshot.XMin,
		"xmax":       snapshot.XMax,
		"oldestxmin": snapshot.OldestXMin,
	} {
		if _, err := strconv.ParseUint(
			strings.TrimSpace(value),
			10,
			64,
		); err != nil {
			return fmt.Errorf("%s is not an xid: %w", name, err)
		}
	}
	if snapshot.CSN < 0 {
		return fmt.Errorf("csn is negative")
	}
	if snapshot.TransactionCount < 0 {
		return fmt.Errorf("xcnt is negative")
	}
	return nil
}

func primaryReplicationHealthSQL() string {
	return "SELECT false," +
		"(SELECT count(*) FROM pg_catalog.pg_settings " +
		"WHERE name LIKE 'replconninfo%' AND setting<>'')," +
		"(SELECT count(*) FROM pg_catalog.pg_stat_replication " +
		"WHERE lower(state)='streaming'),false,'',''," +
		"COALESCE((SELECT max(abs(pg_xlog_location_diff(" +
		"sender_sent_location,receiver_replay_location)))::bigint " +
		"FROM pg_catalog.pg_stat_replication " +
		"WHERE lower(state)='streaming'),-1)"
}

func standbyReplicationHealthSQL() string {
	return "SELECT pg_is_in_recovery(),0,0," +
		"EXISTS (SELECT 1 FROM pg_stat_get_wal_receiver() r " +
		"WHERE lower(r.status)='streaming')," +
		"COALESCE(pg_last_xlog_receive_location(),'')," +
		"COALESCE(pg_last_xlog_replay_location(),'')," +
		"COALESCE(pg_xlog_location_diff(" +
		"pg_last_xlog_receive_location()," +
		"pg_last_xlog_replay_location()),-1)::bigint"
}

func distributedRuntimeHealthSQL(group string) string {
	return "EXECUTE DIRECT ON " + group + " 'SELECT 1'"
}

func classicGTMSnapshotSQL() string {
	return "SELECT xmin::text,xmax::text,csn::bigint," +
		"oldestxmin::text,xcnt::bigint," +
		"COALESCE(running_xids,'')::text " +
		"FROM pgxc_gtm_snapshot_status()"
}

type restoreHealthVerifier interface {
	Verify(context.Context, Environment) error
}

type databaseRestoreHealthVerifier struct {
	db             *Database
	sampleInterval time.Duration
	probeValue     func(context.Context, string, string) (string, error)
	scanValue      func(context.Context, string, []any, ...any) error
}

func (v databaseRestoreHealthVerifier) Verify(
	ctx context.Context,
	environment Environment,
) error {
	if !environment.Supported {
		return fmt.Errorf(
			"NOT_SUPPORTED: product=%s topology=%s",
			environment.Product,
			environment.Topology,
		)
	}
	switch environment.Topology {
	case TopologyStandalone:
		if environment.Product != ProductOpenGauss {
			return fmt.Errorf(
				"NOT_SUPPORTED: standalone product=%s",
				environment.Product,
			)
		}
		return nil
	case TopologyPrimaryStandby:
		if environment.Product != ProductOpenGauss &&
			environment.Product != ProductGaussDB {
			return fmt.Errorf(
				"NOT_SUPPORTED: primary/standby product=%s",
				environment.Product,
			)
		}
		return v.verifyReplication(ctx)
	case TopologyCentralized:
		if environment.Product != ProductGaussDB {
			return fmt.Errorf(
				"NOT_SUPPORTED: centralized product=%s",
				environment.Product,
			)
		}
		if environment.Capabilities[capabilityPrimaryStandby] {
			return v.verifyReplication(ctx)
		}
		return nil
	case TopologyDistributed:
		if environment.Product != ProductGaussDB {
			return fmt.Errorf(
				"NOT_SUPPORTED: distributed product=%s",
				environment.Product,
			)
		}
		return v.verifyDistributed(ctx, environment)
	default:
		return fmt.Errorf(
			"NOT_SUPPORTED: product=%s topology=%s",
			environment.Product,
			environment.Topology,
		)
	}
}

func (v databaseRestoreHealthVerifier) verifyReplication(
	ctx context.Context,
) error {
	first, err := v.sampleReplication(ctx)
	if err != nil {
		return err
	}
	interval := v.sampleInterval
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	timer := time.NewTimer(interval)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		return ctx.Err()
	case <-timer.C:
	}
	second, err := v.sampleReplication(ctx)
	if err != nil {
		return err
	}
	return evaluateReplicationHealth(first, second)
}

func (v databaseRestoreHealthVerifier) sampleReplication(
	ctx context.Context,
) (replicationHealthSample, error) {
	if v.db == nil {
		return replicationHealthSample{}, fmt.Errorf(
			"replication database boundary is unavailable",
		)
	}
	var inRecovery bool
	if err := v.db.Scan(
		ctx,
		"SELECT pg_is_in_recovery()",
		nil,
		&inRecovery,
	); err != nil {
		return replicationHealthSample{}, fmt.Errorf(
			"detect replication role: %w",
			err,
		)
	}
	query := primaryReplicationHealthSQL()
	if inRecovery {
		query = standbyReplicationHealthSQL()
	}
	var sample replicationHealthSample
	if err := v.db.Scan(
		ctx,
		query,
		nil,
		&sample.InRecovery,
		&sample.RequiredStandbys,
		&sample.StreamingStandbys,
		&sample.ReceiverConnected,
		&sample.ReceiveLocation,
		&sample.ReplayLocation,
		&sample.ReplayGapBytes,
	); err != nil {
		return replicationHealthSample{}, fmt.Errorf(
			"NOT_SUPPORTED: sample replication health: %w",
			err,
		)
	}
	return sample, nil
}

func (v databaseRestoreHealthVerifier) verifyDistributed(
	ctx context.Context,
	environment Environment,
) error {
	sample := distributedHealthSample{}
	for _, node := range environment.Nodes {
		switch node.Role {
		case NodeRoleCN:
			sample.CatalogCN++
		case NodeRoleDNPrimary:
			sample.CatalogDN++
		}
	}
	var err error
	sample.RuntimeCN, err = v.runtimeNodeCount(ctx, "COORDINATORS")
	if err != nil {
		return fmt.Errorf("NOT_SUPPORTED: probe coordinator runtime: %w", err)
	}
	sample.RuntimeDN, err = v.runtimeNodeCount(ctx, "DATANODES")
	if err != nil {
		return fmt.Errorf("NOT_SUPPORTED: probe datanode runtime: %w", err)
	}
	gtm, err := v.sampleGTMHealth(ctx)
	if err != nil {
		return err
	}
	sample.GTMMode = gtm.Mode
	sample.GTMSnapshot = gtm.Snapshot
	return evaluateDistributedHealth(sample)
}

func (v databaseRestoreHealthVerifier) sampleGTMHealth(
	ctx context.Context,
) (gtmHealthEvidence, error) {
	free, err := v.probe(
		ctx,
		"gtm_free_mode",
		"SELECT current_setting('enable_gtm_free',true)",
	)
	if err != nil {
		return gtmHealthEvidence{}, fmt.Errorf(
			"NOT_SUPPORTED: detect GTM-FREE mode: %w",
			err,
		)
	}
	if truthy(free) {
		return gtmHealthEvidence{Mode: "gtm-free"}, nil
	}
	lite, err := v.probe(
		ctx,
		"gtm_lite_mode",
		"SELECT current_setting('enable_gtm_lite',true)",
	)
	if err != nil {
		return gtmHealthEvidence{}, fmt.Errorf(
			"NOT_SUPPORTED: detect GTM-LITE mode: %w",
			err,
		)
	}
	if truthy(lite) {
		return gtmHealthEvidence{}, fmt.Errorf(
			"NOT_SUPPORTED: GTM-LITE live runtime health evidence " +
				"is unavailable; configuration alone is insufficient",
		)
	}
	snapshot := &gtmSnapshot{}
	if err := v.scan(
		ctx,
		classicGTMSnapshotSQL(),
		nil,
		&snapshot.XMin,
		&snapshot.XMax,
		&snapshot.CSN,
		&snapshot.OldestXMin,
		&snapshot.TransactionCount,
		&snapshot.RunningXIDs,
	); err != nil {
		return gtmHealthEvidence{}, fmt.Errorf(
			"NOT_SUPPORTED: read classic GTM snapshot record: %w",
			err,
		)
	}
	if err := validateGTMSnapshot(*snapshot); err != nil {
		return gtmHealthEvidence{}, fmt.Errorf(
			"classic GTM snapshot record is invalid: %w",
			err,
		)
	}
	return gtmHealthEvidence{Mode: "gtm", Snapshot: snapshot}, nil
}

func (v databaseRestoreHealthVerifier) probe(
	ctx context.Context,
	name string,
	query string,
) (string, error) {
	if v.probeValue != nil {
		return v.probeValue(ctx, name, query)
	}
	if v.db == nil {
		return "", fmt.Errorf("distributed database boundary is unavailable")
	}
	return v.db.Probe(ctx, name, query)
}

func (v databaseRestoreHealthVerifier) scan(
	ctx context.Context,
	query string,
	args []any,
	dest ...any,
) error {
	if v.scanValue != nil {
		return v.scanValue(ctx, query, args, dest...)
	}
	if v.db == nil {
		return fmt.Errorf("distributed database boundary is unavailable")
	}
	return v.db.Scan(ctx, query, args, dest...)
}

func (v databaseRestoreHealthVerifier) runtimeNodeCount(
	ctx context.Context,
	group string,
) (int, error) {
	if group != "COORDINATORS" && group != "DATANODES" {
		return 0, fmt.Errorf("unsafe distributed node group %q", group)
	}
	rows, err := v.db.Query(ctx, distributedRuntimeHealthSQL(group))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var marker int
		if err := rows.Scan(&marker); err != nil {
			return 0, err
		}
		if marker != 1 {
			return 0, fmt.Errorf("node returned marker %d", marker)
		}
		count++
	}
	return count, rows.Err()
}
