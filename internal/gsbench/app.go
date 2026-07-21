package gsbench

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func executeCommand(ctx context.Context, options CLIOptions, stdout, stderr io.Writer) int {
	overrides := Overrides{Scenarios: options.Scenarios, Duration: options.Duration, Profile: options.Profile}
	if options.DryRun {
		value := true
		overrides.DryRun = &value
	}
	cfg, err := LoadConfig(options.ConfigPath, overrides)
	if err != nil {
		fmt.Fprintln(stderr, "load config:", err)
		return 1
	}
	runID := options.RunID
	if runID == "" {
		runID = newRunID()
	}
	logPath := filepath.Join("logs", "gsbench_"+runID+".log")
	logger, err := NewRunLog(stdout, logPath, Version)
	if err != nil {
		fmt.Fprintln(stderr, "open log:", err)
		return 1
	}
	defer logger.Close()
	logger.Info("command=%s run_id=%s config=%s", options.Command, runID, cfg.Path)
	logger.Info("database=%s", cfg.Redacted())
	db, err := OpenDatabase(ctx, cfg)
	if err != nil {
		logger.Error("connect database: %v", err)
		return 1
	}
	defer db.Close()
	caps := DetectCapabilities(ctx, db)
	if options.Command != "init" && options.Command != "doctor" {
		if ok, checkErr := datasetExists(ctx, db, cfg.Data.Schema); checkErr != nil || !ok {
			logger.Error("benchmark schema is not initialized; run gsbench init first")
			return 1
		}
	}
	switch options.Command {
	case "doctor":
		return commandDoctor(ctx, db, cfg, caps, logger)
	case "init":
		return commandInit(ctx, db, cfg, caps, logger)
	case "run":
		return commandRun(ctx, db, cfg, caps, logger, runID)
	case "status":
		return commandStatus(ctx, db, cfg, logger, options.RunID)
	case "stop":
		return commandStop(ctx, db, cfg, logger, options.RunID)
	case "cleanup":
		return commandCleanup(ctx, db, cfg, logger, options.RunID, options.WithData)
	default:
		logger.Error("unknown command %s", options.Command)
		return 2
	}
}

func commandDoctor(ctx context.Context, db *Database, cfg BenchConfig, caps Capabilities, log *RunLog) int {
	log.Info("product=%s version=%s supported=%v centralized=%v admin=%v", caps.Product, caps.Version, caps.Supported, caps.Centralized, caps.Admin)
	log.Info("thread_pool_enabled=%v thread_pool_view=%v dynamic_memory_view=%v database_cpu=%v statement_history=%v vacuum_stats=%v", caps.ThreadPoolEnabled, caps.ThreadPoolView, caps.DynamicMemoryView, caps.DatabaseCPU, caps.StatementHistory, caps.VacuumStats)
	for _, warning := range caps.Warnings {
		log.Info("capability fallback: %s", warning)
	}
	if !caps.Supported {
		log.Error("target is not a supported centralized openGauss/GaussDB instance")
		return 1
	}
	if exists, _ := datasetExists(ctx, db, cfg.Data.Schema); exists {
		if err := NewSQLJournal(db, cfg.Data.Schema).RecoverStale(ctx); err != nil {
			log.Error("stale recovery: %v", err)
			return 1
		}
	}
	log.Info("doctor SUCCESS")
	return 0
}

func commandInit(ctx context.Context, db *Database, cfg BenchConfig, caps Capabilities, log *RunLog) int {
	if !caps.Supported {
		log.Error("unsupported target product or topology")
		return 1
	}
	capacity, source, capacityPath, err := detectCapacity(ctx, db)
	if err != nil {
		log.Error("detect disk capacity: %v", err)
		return 1
	}
	plan, err := PlanDataset(cfg, capacity)
	if err != nil {
		log.Error("plan dataset: %v", err)
		return 1
	}
	log.Info("dataset profile=%s estimated_bytes=%d capacity_source=%s reserved_free_bytes=%d", plan.Profile, plan.EstimatedBytes, source, plan.ReservedFreeBytes)
	if cfg.Run.DryRun {
		for _, ddl := range plan.DDL {
			log.Info("DRY-RUN %s", ddl)
		}
		log.Info("init SUCCESS (dry run)")
		return 0
	}
	manager := NewDatasetManager(dbDatasetExecutor{db: db, schema: cfg.Data.Schema, capacityPath: capacityPath, reservedFreeBytes: plan.ReservedFreeBytes})
	if err := manager.Init(ctx, plan); err != nil {
		log.Error("initialize dataset: %v", err)
		return 1
	}
	log.Info("init SUCCESS")
	return 0
}

func commandRun(parent context.Context, db *Database, cfg BenchConfig, caps Capabilities, log *RunLog, runID string) int {
	if cfg.Run.DryRun {
		for _, name := range cfg.Run.Scenarios {
			log.Info("DRY-RUN scenario=%s lifecycle=prepare,ramp,hold,verify,stop,restore", name)
		}
		log.Info("run SUCCESS (dry run, no database mutations)")
		return 0
	}
	journal := NewSQLJournal(db, cfg.Data.Schema)
	if err := journal.RecoverStale(parent); err != nil {
		log.Error("recover stale mutations: %v", err)
		return 1
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if err := startRun(ctx, db, cfg, runID); err != nil {
		log.Error("record run: %v", err)
		return 1
	}
	go watchStop(ctx, db, cfg.Data.Schema, runID, cancel)
	runtime := &Runtime{Config: cfg, Database: db, Capabilities: caps, Journal: journal, Log: log, RunID: runID}
	runtime.ReportPhase = func(phaseCtx context.Context, scenario string, phase Phase) {
		_, _ = db.Exec(phaseCtx, "UPDATE "+cfg.Data.Schema+".meta_runs SET phase=$1,detail=$2,updated_at=current_timestamp WHERE run_id=$3", string(phase), scenario+":"+string(phase), runID)
	}
	if caps.DatabaseCPU {
		runtime.CPU = NewDatabaseCPUSampler(db)
	}
	scenarios := []Scenario{NewTPScenario(), NewAPScenario(), NewMixedScenario(), NewConnectionScenario(), NewThreadScenario(), NewMemoryScenario(), NewPlanScenario(), NewLockScenario(), NewVacuumScenario()}
	summary := NewRunner(runtime, scenarios).Run(ctx, cfg.Run.Scenarios)
	for _, result := range summary.Results {
		log.Info("scenario=%s outcome=%s message=%s", result.Scenario, result.Outcome, result.Message)
	}
	_ = finishRun(context.Background(), db, cfg.Data.Schema, runID, summary.Outcome, "complete")
	switch summary.Outcome {
	case OutcomeSuccess:
		return 0
	case OutcomeDegraded:
		return 3
	default:
		return 1
	}
}

func commandStatus(ctx context.Context, db *Database, cfg BenchConfig, log *RunLog, runID string) int {
	query := "SELECT run_id,scenarios,phase,status,started_at,updated_at,COALESCE(detail,'') FROM " + cfg.Data.Schema + ".meta_runs"
	var args []any
	if runID != "" {
		query += " WHERE run_id=$1"
		args = []any{runID}
	}
	query += " ORDER BY started_at DESC LIMIT 50"
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		log.Error("status query: %v", err)
		return 1
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id, scenarios, phase, status, detail string
		var started, updated time.Time
		if err := rows.Scan(&id, &scenarios, &phase, &status, &started, &updated, &detail); err != nil {
			log.Error("status scan: %v", err)
			return 1
		}
		log.Info("run_id=%s scenarios=%s phase=%s status=%s started=%s updated=%s detail=%s", id, scenarios, phase, status, started.Format(time.RFC3339), updated.Format(time.RFC3339), detail)
		count++
	}
	log.Info("status rows=%d", count)
	return 0
}

func commandStop(ctx context.Context, db *Database, cfg BenchConfig, log *RunLog, runID string) int {
	runs, err := activeRunIDs(ctx, db, cfg.Data.Schema, runID)
	if err != nil {
		log.Error("list active runs: %v", err)
		return 1
	}
	journal := NewSQLJournal(db, cfg.Data.Schema)
	var failed bool
	for _, id := range runs {
		_, _ = db.Exec(ctx, "UPDATE "+cfg.Data.Schema+".meta_runs SET status='stop_requested',updated_at=current_timestamp WHERE run_id=$1", id)
		query, arg, _ := StopTaggedSQL(id)
		if _, err := db.Exec(ctx, query, arg); err != nil {
			log.Error("terminate run %s: %v", id, err)
			failed = true
		}
		waitRunStopped(ctx, db, cfg.Data.Schema, id, 5*time.Second)
		if err := journal.RestoreRun(ctx, id); err != nil {
			log.Error("restore run %s: %v", id, err)
			failed = true
		} else {
			_ = finishRun(ctx, db, cfg.Data.Schema, id, OutcomeFailed, "stopped")
		}
	}
	if failed {
		return 1
	}
	log.Info("stop SUCCESS runs=%d", len(runs))
	return 0
}

func waitRunStopped(ctx context.Context, db *Database, schema, runID string, maximum time.Duration) {
	deadline := time.NewTimer(maximum)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
			var status string
			if db.Scan(ctx, "SELECT status FROM "+schema+".meta_runs WHERE run_id=$1", []any{runID}, &status) == nil && status != "running" && status != "stop_requested" {
				return
			}
		}
	}
}

func commandCleanup(ctx context.Context, db *Database, cfg BenchConfig, log *RunLog, runID string, withData bool) int {
	if code := commandStop(ctx, db, cfg, log, runID); code != 0 {
		return code
	}
	if withData {
		if _, err := db.Exec(ctx, "DROP SCHEMA "+cfg.Data.Schema+" CASCADE"); err != nil {
			log.Error("drop benchmark schema: %v", err)
			return 1
		}
		log.Info("removed schema=%s (not recoverable except by gsbench init)", cfg.Data.Schema)
	}
	log.Info("cleanup SUCCESS")
	return 0
}

func datasetExists(ctx context.Context, db *Database, schema string) (bool, error) {
	var count int
	err := db.Scan(ctx, "SELECT count(*) FROM pg_catalog.pg_tables WHERE schemaname=$1 AND tablename='meta_runs'", []any{schema}, &count)
	return count == 1, err
}
func startRun(ctx context.Context, db *Database, cfg BenchConfig, runID string) error {
	_, err := db.Exec(ctx, "INSERT INTO "+cfg.Data.Schema+".meta_runs(run_id,scenarios,phase,status,owner_name,started_at,updated_at) VALUES($1,$2,$3,$4,current_user,current_timestamp,current_timestamp)", runID, strings.Join(cfg.Run.Scenarios, ","), string(PhasePrepare), "running")
	return err
}
func finishRun(ctx context.Context, db *Database, schema, runID string, outcome Outcome, detail string) error {
	_, err := db.Exec(ctx, "UPDATE "+schema+".meta_runs SET phase=$1,status=$2,detail=$3,updated_at=current_timestamp WHERE run_id=$4", string(PhaseRestore), string(outcome), detail, runID)
	return err
}
func watchStop(ctx context.Context, db *Database, schema, runID string, cancel context.CancelFunc) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var status string
			if db.Scan(ctx, "SELECT status FROM "+schema+".meta_runs WHERE run_id=$1", []any{runID}, &status) == nil && status == "stop_requested" {
				cancel()
				return
			}
		}
	}
}
func activeRunIDs(ctx context.Context, db *Database, schema, requested string) ([]string, error) {
	if requested != "" {
		return []string{requested}, nil
	}
	rows, err := db.Query(ctx, "SELECT run_id FROM "+schema+".meta_runs WHERE status IN ('running','stop_requested') ORDER BY started_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
func newRunID() string {
	return time.Now().Format("20060102T150405") + "-" + strconv.FormatInt(rand.Int63n(1<<24), 36)
}

func detectCapacity(ctx context.Context, db *Database) (Capacity, string, string, error) {
	path := "."
	source := "client-filesystem"
	if dataDir, err := db.Probe(ctx, "data_directory", "SHOW data_directory"); err == nil {
		if info, statErr := os.Stat(dataDir); statErr == nil && info.IsDir() {
			path = dataDir
			source = "database-data-directory"
		}
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return Capacity{}, source, path, err
	}
	return Capacity{TotalBytes: int64(stat.Blocks) * int64(stat.Bsize), FreeBytes: int64(stat.Bavail) * int64(stat.Bsize)}, source, path, nil
}
