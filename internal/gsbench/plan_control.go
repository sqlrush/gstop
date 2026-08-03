package gsbench

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	planWorkloadRunningStatus = "workload_running"
	planWorkloadPhase         = "plan_baseline"
)

var (
	errPlanWorkloadNotFound = errors.New("active plan workload not found")
	errPlanFaultNotFound    = errors.New("active plan fault not found")
)

type planRunRecord struct {
	RunID     string
	Code      ScenarioCode
	Phase     string
	Status    string
	StartedAt time.Time
}

type planControlStore struct {
	db     journalDatabase
	schema string
}

func newPlanControlStore(
	db journalDatabase,
	schema string,
) (*planControlStore, error) {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return nil, fmt.Errorf("unsafe dataset schema %q", schema)
	}
	if db == nil {
		return nil, fmt.Errorf("plan control database is required")
	}
	return &planControlStore{db: db, schema: quotedSchema}, nil
}

func planActivityLockIdentity(cfg BenchConfig) string {
	return fmt.Sprintf(
		"gsbench:plan-workload:%s:%s",
		cfg.Database.Database,
		cfg.Data.Schema,
	)
}

func (s *planControlStore) StartWorkload(
	ctx context.Context,
	runID string,
	code ScenarioCode,
	workers int,
) error {
	if strings.TrimSpace(runID) == "" || !isPlanChangeCode(code) || workers <= 0 {
		return fmt.Errorf("plan workload requires run ID, scenario 601-606, and positive workers")
	}
	_, err := s.db.Exec(
		ctx,
		"INSERT INTO "+s.schema+
			".meta_runs(run_id,scenarios,phase,status,owner_name,"+
			"started_at,updated_at,detail) "+
			"VALUES($1,$2,$3,$4,current_user,current_timestamp,"+
			"current_timestamp,$5)",
		runID,
		fmt.Sprintf("%03d", code),
		planWorkloadPhase,
		planWorkloadRunningStatus,
		fmt.Sprintf("three_phase workers=%d", workers),
	)
	return err
}

func (s *planControlStore) HeartbeatWorkload(
	ctx context.Context,
	runID string,
) error {
	result, err := s.db.Exec(
		ctx,
		"UPDATE "+s.schema+
			".meta_runs SET updated_at=current_timestamp "+
			"WHERE run_id=$1 AND status=$2",
		runID,
		planWorkloadRunningStatus,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("plan workload %s is no longer active", runID)
	}
	return nil
}

func (s *planControlStore) FinishWorkload(
	ctx context.Context,
	runID string,
	outcome Outcome,
	detail string,
) error {
	_, err := s.db.Exec(
		ctx,
		"UPDATE "+s.schema+
			".meta_runs SET status=$1,detail=$2,updated_at=current_timestamp "+
			"WHERE run_id=$3 AND status=$4",
		string(outcome),
		detail,
		runID,
		planWorkloadRunningStatus,
	)
	return err
}

func (s *planControlStore) StartFault(
	ctx context.Context,
	runID string,
	code ScenarioCode,
) error {
	if strings.TrimSpace(runID) == "" || !isPlanChangeCode(code) {
		return fmt.Errorf("plan fault requires run ID and scenario 601-606")
	}
	_, err := s.db.Exec(
		ctx,
		"INSERT INTO "+s.schema+
			".meta_runs(run_id,scenarios,phase,status,owner_name,"+
			"started_at,updated_at,detail) "+
			"VALUES($1,$2,$3,'running',current_user,current_timestamp,"+
			"current_timestamp,'three_phase fault applying')",
		runID,
		fmt.Sprintf("%03d", code),
		string(PhaseRamp),
	)
	return err
}

func (s *planControlStore) SetFaultPhase(
	ctx context.Context,
	runID string,
	phase Phase,
	detail string,
) error {
	_, err := s.db.Exec(
		ctx,
		"UPDATE "+s.schema+
			".meta_runs SET phase=$1,detail=$2,updated_at=current_timestamp "+
			"WHERE run_id=$3",
		string(phase),
		detail,
		runID,
	)
	return err
}

func (s *planControlStore) ResolveWorkload(
	ctx context.Context,
	code ScenarioCode,
) (planRunRecord, error) {
	runs, err := s.queryRuns(
		ctx,
		"SELECT run_id,scenarios,phase,status,started_at FROM "+s.schema+
			".meta_runs WHERE status=$1 ORDER BY started_at DESC,run_id DESC",
		planWorkloadRunningStatus,
	)
	if err != nil {
		return planRunRecord{}, err
	}
	if len(runs) == 0 {
		return planRunRecord{}, errPlanWorkloadNotFound
	}
	if len(runs) != 1 {
		return planRunRecord{}, fmt.Errorf("multiple active plan workloads found: %d", len(runs))
	}
	if runs[0].Code != code {
		return planRunRecord{}, fmt.Errorf(
			"active plan workload is scenario %03d, not %03d",
			runs[0].Code,
			code,
		)
	}
	return runs[0], nil
}

func (s *planControlStore) ResolveFault(
	ctx context.Context,
	code ScenarioCode,
) (planRunRecord, error) {
	runs, err := s.queryRuns(
		ctx,
		"SELECT run_id,scenarios,phase,status,started_at FROM "+s.schema+
			".meta_runs WHERE scenarios=$1 AND status IN ("+
			"'running','stop_requested','restore_requested',"+
			"'restore_failed','RESTORE_FAILED') "+
			"ORDER BY started_at DESC,run_id DESC",
		fmt.Sprintf("%03d", code),
	)
	if err != nil {
		return planRunRecord{}, err
	}
	if len(runs) == 0 {
		return planRunRecord{}, errPlanFaultNotFound
	}
	if len(runs) != 1 {
		return planRunRecord{}, fmt.Errorf(
			"multiple active plan faults found for scenario %03d: %d",
			code,
			len(runs),
		)
	}
	return runs[0], nil
}

func (s *planControlStore) queryRuns(
	ctx context.Context,
	query string,
	args ...any,
) ([]planRunRecord, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []planRunRecord
	for rows.Next() {
		var run planRunRecord
		var scenario string
		if err := rows.Scan(
			&run.RunID,
			&scenario,
			&run.Phase,
			&run.Status,
			&run.StartedAt,
		); err != nil {
			return nil, err
		}
		value, err := strconv.Atoi(strings.TrimSpace(scenario))
		if err != nil || !isPlanChangeCode(ScenarioCode(value)) {
			return nil, fmt.Errorf("invalid plan run scenario %q", scenario)
		}
		run.Code = ScenarioCode(value)
		runs = append(runs, run)
	}
	return runs, rows.Err()
}
