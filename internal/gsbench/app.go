package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type restoreService interface {
	Restore(context.Context, RestoreRequest) RestoreSummary
}

func executeRestoreService(
	ctx context.Context,
	service restoreService,
	request RestoreRequest,
	commandName string,
	log *RunLog,
) int {
	if service == nil {
		log.Error("%s: restore coordinator is unavailable", commandName)
		return 1
	}
	summary := service.Restore(ctx, request)
	if summary.Failed {
		log.Error("%s: %v", commandName, summary.Err)
		return 1
	}
	if request.DryRun {
		log.Info(
			"%s SUCCESS (dry run) runs=%d actions=%d",
			commandName,
			len(summary.RunIDs),
			len(summary.PlannedActions),
		)
		return 0
	}
	log.Info("%s SUCCESS runs=%d", commandName, len(summary.RunIDs))
	return 0
}

func executeCommand(ctx context.Context, options CLIOptions, stdout, stderr io.Writer) int {
	overrides := Overrides{
		ScenarioCodes: options.ScenarioCodes, Duration: options.Duration,
		Profile: options.Profile, DatasetBytes: options.DatasetBytes,
		DatasetSize: options.DatasetSize,
	}
	if options.DryRun {
		value := true
		overrides.DryRun = &value
	}
	cfg, err := LoadConfig(options.ConfigPath, overrides)
	if err != nil {
		fmt.Fprintln(stderr, "load config:", err)
		return 1
	}
	runID := strings.TrimSpace(options.RunID)
	if runID == "" && options.Command == "run" {
		runID = newRunID()
	}
	logIdentity := runID
	if logIdentity == "" {
		logIdentity = options.Command
	}
	logPath := ""
	if !commandIsReadOnly(options.Command, cfg.Run.DryRun) {
		logPath = filepath.Join("logs", "gsbench_"+logIdentity+".log")
	}
	logger, err := NewRunLog(stdout, logPath, Version)
	if err != nil {
		fmt.Fprintln(stderr, "open log:", err)
		return 1
	}
	defer logger.Close()
	if runID == "" {
		logger.Info("command=%s requested_run_id=all config=%s", options.Command, cfg.Path)
	} else {
		logger.Info("command=%s run_id=%s config=%s", options.Command, runID, cfg.Path)
	}
	logger.Info("database=%s", cfg.Redacted())
	logger.Info("runtime_validation_enabled=%v", cfg.Run.ValidationEnabled)
	openDatabase := OpenDatabase
	if options.Command == "restore" || options.Command == "stop" {
		openDatabase = OpenRestoreDatabase
	}
	db, err := openDatabase(ctx, cfg)
	if err != nil {
		logger.Error("connect database: %v", err)
		return 1
	}
	defer db.Close()
	environment := Environment{}
	caps := Capabilities{}
	if options.Command != "restore" && options.Command != "stop" {
		environment = DetectEnvironment(ctx, db)
		db.setTargetProduct(environment.Product)
		caps = capabilitiesFor(environment)
	}
	if cfg.Run.ValidationEnabled &&
		options.Command != "init" &&
		options.Command != "doctor" &&
		options.Command != "restore" &&
		options.Command != "stop" {
		if ok, checkErr := datasetExists(ctx, db, cfg.Data.Schema); checkErr != nil || !ok {
			logger.Error("benchmark schema is not initialized; run gsbench init first")
			return 1
		}
	}
	switch options.Command {
	case "doctor":
		return commandDoctor(ctx, db, cfg, environment, logger)
	case "init":
		return commandInit(ctx, db, cfg, environment, caps, logger)
	case "run":
		return commandRun(
			ctx,
			db,
			cfg,
			environment,
			caps,
			options.AllowRisk,
			logger,
			runID,
		)
	case "status":
		return commandStatus(ctx, db, cfg, logger, options.RunID)
	case "stop":
		return commandStop(ctx, db, cfg, logger, options.RunID)
	case "restore":
		return commandRestore(ctx, db, cfg, logger, options.RunID)
	case "cleanup":
		return commandCleanup(ctx, db, cfg, logger, options.RunID, options.WithData)
	default:
		logger.Error("unknown command %s", options.Command)
		return 2
	}
}

func commandIsReadOnly(command string, dryRun bool) bool {
	return command == "doctor" ||
		command == "status" ||
		(dryRun && (command == "restore" || command == "stop"))
}

func commandDoctor(ctx context.Context, db *Database, cfg BenchConfig, env Environment, log *RunLog) int {
	for _, line := range doctorEnvironmentReport(env, DefaultScenarioCatalog().Definitions()) {
		log.Info("%s", line)
	}
	if !env.Supported {
		log.Error("target is not a supported openGauss/GaussDB instance")
		return 1
	}
	if exists, _ := datasetExists(ctx, db, cfg.Data.Schema); exists {
		backend := newDatabaseRestoreBackend(
			db,
			cfg,
			log,
			DefaultFaultProviderRegistry(),
		)
		summary := NewRestoreCoordinatorWithValidation(
			backend,
			cfg.Run.ValidationEnabled,
		).Restore(
			ctx,
			RestoreRequest{DryRun: true},
		)
		log.Info(
			"stale recovery runs=%d pending_actions=%d action=report_only",
			len(summary.RunIDs),
			len(summary.PlannedActions),
		)
		for _, runID := range summary.RunIDs {
			log.Info("stale run_id=%s action=report_only", runID)
		}
		if summary.Failed {
			log.Error("read stale recovery state: %v", summary.Err)
			return 1
		}
	}
	log.Info("doctor SUCCESS")
	return 0
}

func doctorEnvironmentReport(env Environment, definitions []ScenarioDefinition) []string {
	lines := []string{
		fmt.Sprintf("product=%s version=%s topology=%s supported=%v", env.Product, env.Version, env.Topology, env.Supported),
		fmt.Sprintf("nodes=%d", len(env.Nodes)),
	}
	for _, node := range env.Nodes {
		lines = append(lines, fmt.Sprintf("node=%s role=%s shard=%s host=%s port=%d", node.Name, node.Role, node.Shard, node.Host, node.Port))
	}
	capabilities := make([]Capability, 0, len(knownCapabilities))
	for _, capability := range knownCapabilities {
		capabilities = append(capabilities, capability)
	}
	sort.Slice(capabilities, func(i, j int) bool { return capabilities[i] < capabilities[j] })
	for _, capability := range capabilities {
		lines = append(lines, fmt.Sprintf("capability=%s supported=%v", capability, env.Capabilities[capability]))
	}
	for _, definition := range definitions {
		decision, detail := doctorScenarioDecision(env, definition)
		line := fmt.Sprintf("scenario=%03d name=%s decision=%s", definition.Code, definition.Name, decision)
		if detail != "" {
			line += " detail=" + detail
		}
		lines = append(lines, line)
	}
	for _, warning := range env.Warnings {
		lines = append(lines, "capability warning="+warning)
	}
	return lines
}

func doctorScenarioDecision(env Environment, definition ScenarioDefinition) (string, string) {
	if !env.Applicable(definition) {
		return "NOT_APPLICABLE", "topology or product does not apply"
	}
	missing := env.Missing(definition.Requires)
	if len(missing) == 0 {
		return "SUPPORTED", ""
	}
	if definitionHasFallback(definition, missing) {
		return "DEGRADED", "fallback for missing " + joinRequirements(missing)
	}
	return "UNSUPPORTED", "preflight missing " + joinRequirements(missing)
}

func definitionHasFallback(definition ScenarioDefinition, missing []Requirement) bool {
	if len(missing) == 0 ||
		len(definition.FallbackRequirements) == 0 {
		return false
	}
	allowed := make(map[Requirement]struct{}, len(
		definition.FallbackRequirements,
	))
	for _, requirement := range definition.FallbackRequirements {
		allowed[requirement] = struct{}{}
	}
	for _, requirement := range missing {
		if _, ok := allowed[requirement]; !ok {
			return false
		}
	}
	return true
}

func joinRequirements(requirements []Requirement) string {
	values := make([]string, len(requirements))
	for i, requirement := range requirements {
		values[i] = string(requirement)
	}
	return strings.Join(values, ",")
}

var knownCapabilities = []Capability{
	CapabilityAdmin,
	CapabilityStatementHistory,
	CapabilityHardParseCounters,
	CapabilityGlobalPlanCache,
	CapabilityGlobalLockViews,
	CapabilityThreadPool,
	CapabilityPoolerViews,
	CapabilityMemoryNodeViews,
	CapabilityWALSenderViews,
	CapabilityStandbyControl,
	CapabilityExternalFaultProvider,
}

func commandInit(
	ctx context.Context,
	db *Database,
	cfg BenchConfig,
	env Environment,
	caps Capabilities,
	log *RunLog,
) int {
	if cfg.Run.ValidationEnabled && !caps.Supported {
		log.Error("unsupported target product or topology")
		return 1
	}
	providerDB := databaseJournalDB{db: db}
	externalProviders := DatasetExternalProviders{}
	var capacityProvider DatasetCapacityProvider
	capacity := Capacity{}
	capacityStatus := CapacityStatus{Source: "validation_disabled"}
	if cfg.Run.ValidationEnabled {
		var providerErr error
		capacityProvider, providerErr = selectDatasetCapacityProvider(
			cfg, env, providerDB, externalProviders,
		)
		if providerErr != nil {
			if !cfg.Run.DryRun {
				log.Error("select dataset capacity provider: %v", providerErr)
				return 1
			}
			capacityProvider = unavailableDatasetCapacityProvider{err: providerErr}
		}
		var err error
		capacity, capacityStatus, err = resolveDatasetCapacity(
			ctx,
			capacityProvider,
			env,
			cfg.Run.DryRun,
			cfg.Data.TargetBytes,
			cfg.Data.MinFreeDiskPercent,
		)
		if err != nil {
			log.Error("detect disk capacity: %v", err)
			return 1
		}
	}
	plan, err := PlanDataset(cfg, capacity, env)
	if err != nil {
		log.Error("plan dataset: %v", err)
		return 1
	}
	var physicalProvider DatasetPhysicalProvider
	if !cfg.Run.DryRun && cfg.Run.ValidationEnabled {
		physicalProvider, err = selectDatasetPhysicalProvider(
			cfg, env, providerDB, externalProviders,
		)
		if err != nil {
			log.Error("select dataset physical-size provider: %v", err)
			return 1
		}
	}
	executor := dbDatasetExecutor{
		db:                 db,
		schema:             cfg.Data.Schema,
		env:                env,
		capacityProvider:   capacityProvider,
		physicalProvider:   physicalProvider,
		minFreeDiskPercent: cfg.Data.MinFreeDiskPercent,
	}
	if cfg.Run.DryRun {
		if err := LoadDatasetHighWater(ctx, executor, &plan); err != nil {
			log.Error("read resumable dataset work: %v", err)
			return 1
		}
	}
	logDatasetPlan(log, cfg, plan, capacityStatus.Source)
	if capacityStatus.Error != "" {
		log.Info("dataset capacity_status=unavailable detail=%s", capacityStatus.Error)
	}
	if cfg.Run.DryRun {
		log.Info("DRY-RUN CHECK pg_catalog.pg_namespace FOR schema=%s", plan.Schema)
		log.Info("DRY-RUN IF MISSING CREATE SCHEMA %s", plan.Schema)
		for _, ddl := range plan.DDL {
			log.Info("DRY-RUN %s", ddl)
		}
		for _, column := range plan.Columns {
			log.Info("DRY-RUN IF MISSING ALTER TABLE %s.%s ADD COLUMN %s %s",
				plan.Schema, column.Table, column.Name, column.Declaration)
		}
		for _, ddl := range plan.PostMigrationDDL {
			log.Info("DRY-RUN AFTER MIGRATION %s", ddl)
		}
		log.Info("init SUCCESS (dry run)")
		return 0
	}
	manager := NewDatasetManagerWithValidation(
		executor,
		cfg.Run.ValidationEnabled,
		log.Info,
	)
	if err := manager.Init(ctx, plan); err != nil {
		log.Error("initialize dataset: %v", err)
		return 1
	}
	results, err := RepairPlanBaseline(ctx, db, cfg.Data.Schema)
	for _, result := range results {
		log.Info("plan baseline target=%s status=%s", result.Target, result.Status)
	}
	if err != nil {
		log.Error("repair plan baseline: %v", err)
		return 1
	}
	if cfg.Run.ValidationEnabled {
		if err := VerifyPlanBaseline(ctx, db, cfg.Data.Schema); err != nil {
			log.Error("verify plan baseline: %v", err)
			return 1
		}
	}
	log.Info("init SUCCESS")
	return 0
}

func logDatasetPlan(log *RunLog, cfg BenchConfig, plan DatasetPlan, source string) {
	requestedSize := cfg.Data.RequestedSize
	if requestedSize == "" {
		requestedSize = "configuration/profile"
	}
	log.Info(
		"dataset profile=%s product=%s topology=%s requested_size=%s target_bytes=%d estimated_bytes=%d capacity_provider=%s physical_size_provider=%s capacity_source=%s reserved_free_bytes=%d available_for_data_bytes=%d",
		plan.Profile, plan.Product, plan.Topology, requestedSize, cfg.Data.TargetBytes, plan.EstimatedBytes,
		defaultDatasetProvider(cfg.Data.CapacityProvider),
		defaultDatasetProvider(cfg.Data.PhysicalSizeProvider),
		source, plan.ReservedFreeBytes, plan.AvailableForData,
	)
	if !cfg.Run.DryRun {
		return
	}
	for _, table := range plan.Batches {
		high := plan.HighWater[table.Table]
		remaining := table.Rows - high
		if remaining < 0 {
			remaining = 0
		}
		batchCount := (remaining + table.BatchSize - 1) / table.BatchSize
		log.Info(
			"DRY-RUN dataset table=%s weight_percent=%d estimated_row_bytes=%d target_rows=%d batch_size=%d batch_count=%d current_high_water=%d remaining_rows=%d estimated_new_bytes=%d",
			table.Table, table.WeightPercent, table.EstimatedRowBytes, table.Rows,
			table.BatchSize, batchCount, high, remaining,
			remaining*table.EstimatedRowBytes,
		)
	}
}

func commandRun(
	parent context.Context,
	db *Database,
	cfg BenchConfig,
	environment Environment,
	caps Capabilities,
	allowRisk RiskLevel,
	log *RunLog,
	runID string,
) int {
	quotedSchema, schemaOK := quoteDatasetSchema(cfg.Data.Schema)
	if !schemaOK {
		log.Error("unsafe dataset schema %q", cfg.Data.Schema)
		return 1
	}
	if cfg.Run.DryRun {
		for _, code := range cfg.Run.ScenarioCodes {
			definition := DefaultScenarioCatalog().MustCode(code)
			lifecycle := "preflight,prepare,ramp,hold,stop,restore"
			if cfg.Run.ValidationEnabled {
				lifecycle = "preflight,prepare,ramp,hold,verify,stop,restore,verify_restore"
			}
			log.Info(
				"DRY-RUN scenario=%03d name=%s lifecycle=%s",
				code,
				definition.Name,
				lifecycle,
			)
		}
		log.Info("run SUCCESS (dry run, no database mutations)")
		return 0
	}
	journal := NewSQLJournalWithValidation(
		db,
		cfg.Data.Schema,
		cfg.Run.ValidationEnabled,
	)
	backend := newDatabaseRestoreBackend(
		db,
		cfg,
		log,
		DefaultFaultProviderRegistry(),
	)
	staleSummary := NewRestoreCoordinatorWithValidation(
		backend,
		cfg.Run.ValidationEnabled,
	).Restore(
		parent,
		RestoreRequest{afterSuccess: func(ctx context.Context) error {
			if cfg.Run.ValidationEnabled && scenarioCodesContainPlanChange(
				cfg.Run.ScenarioCodes,
			) {
				activeRunID, err := findActivePlanRun(
					ctx,
					db,
					cfg.Data.Schema,
				)
				if err != nil {
					return fmt.Errorf(
						"check active plan-change run: %w",
						err,
					)
				}
				if activeRunID != "" {
					return fmt.Errorf(
						"plan-change run %s is already active; "+
							"stop or restore it first",
						activeRunID,
					)
				}
			}
			return startRun(ctx, db, cfg, runID)
		}},
	)
	if staleSummary.Failed {
		log.Error("recover stale state and record run: %v", staleSummary.Err)
		return 1
	}
	if len(staleSummary.RunIDs) != 0 {
		log.Info(
			"stale recovery SUCCESS runs=%d actions=%d",
			len(staleSummary.RunIDs),
			len(staleSummary.PlannedActions),
		)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go watchStop(ctx, db, cfg.Data.Schema, runID, cancel)
	restoreBackend := newDatabaseRestoreBackend(
		db,
		cfg,
		log,
		DefaultFaultProviderRegistry(),
	)
	runtime := &Runtime{
		Config: cfg, Database: db, Capabilities: caps, Journal: journal,
		Environment: environment, Catalog: DefaultScenarioCatalog(),
		Provider: restoreBackend.provider, Ledger: restoreBackend.ledger,
		Log: log, RunID: runID, AllowRisk: allowRisk,
	}
	runtime.RestoreService = NewRestoreCoordinatorWithValidation(
		restoreBackend,
		cfg.Run.ValidationEnabled,
	)
	if cfg.Run.ValidationEnabled {
		runtime.PlanPreflight = func(
			preflightCtx context.Context,
			scenario string,
			statements []string,
		) error {
			return EnsureWorkloadPlans(preflightCtx, runtime, scenario, statements)
		}
	}
	runtime.ReportPhase = func(phaseCtx context.Context, scenario string, phase Phase) {
		_, _ = db.Exec(phaseCtx, "UPDATE "+quotedSchema+".meta_runs SET phase=$1,detail=$2,updated_at=current_timestamp WHERE run_id=$3", string(phase), scenario+":"+string(phase), runID)
	}
	if caps.DatabaseCPU {
		runtime.CPU = NewDatabaseCPUSampler(db)
	}
	summary := NewRunner(
		runtime,
		runtime.Catalog,
		DefaultScenarioFactories(),
	).Run(ctx, cfg.Run.ScenarioCodes)
	for _, result := range summary.Results {
		if err := log.Evidence(
			NewEvidenceEnvelope(summary.RunID, result),
		); err != nil {
			log.Error(
				"write scenario %03d evidence: %v",
				result.ScenarioCode,
				err,
			)
			return 1
		}
	}
	return exitCodeForOutcome(summary.Outcome)
}

func exitCodeForOutcome(outcome Outcome) int {
	switch outcome {
	case OutcomeSuccess, OutcomeNotApplicable:
		return 0
	case OutcomeDegraded:
		return 3
	default:
		return 1
	}
}

func scenarioCodesContainPlanChange(codes []ScenarioCode) bool {
	for _, code := range codes {
		if isPlanChangeCode(code) {
			return true
		}
	}
	return false
}

func findActivePlanRun(ctx context.Context, db *Database, schema string) (string, error) {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return "", fmt.Errorf("unsafe dataset schema %q", schema)
	}
	rows, err := db.Query(ctx,
		"SELECT run_id,scenarios FROM "+quotedSchema+".meta_runs "+
			"WHERE status IN ('running','stop_requested') ORDER BY started_at")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var runID, scenarios string
		if err := rows.Scan(&runID, &scenarios); err != nil {
			return "", err
		}
		if storedScenarioCodesContainPlanChange(scenarios) {
			return runID, nil
		}
	}
	return "", rows.Err()
}

func storedScenarioCodesContainPlanChange(value string) bool {
	for _, raw := range splitList(value) {
		code, err := strconv.ParseUint(raw, 10, 16)
		if err == nil && isPlanChangeCode(ScenarioCode(code)) {
			return true
		}
	}
	return false
}

func commandStatus(ctx context.Context, db *Database, cfg BenchConfig, log *RunLog, runID string) int {
	quotedSchema, ok := quoteDatasetSchema(cfg.Data.Schema)
	if !ok {
		log.Error("unsafe dataset schema %q", cfg.Data.Schema)
		return 1
	}
	query := "SELECT run_id,scenarios,phase,status,started_at,updated_at,COALESCE(detail,'') FROM " + quotedSchema + ".meta_runs"
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
	stale, staleErr := ReadStaleRecoveryStatus(
		ctx,
		NewSQLJournal(db, cfg.Data.Schema),
		NewFileRecoveryLedger(cfg.FaultProvider.LedgerPath),
	)
	log.Info(
		"stale recovery runs=%d database_runs=%d local_actions=%d",
		len(stale.RunIDs),
		stale.DatabaseRunCount,
		stale.LocalActionCount,
	)
	for _, staleRunID := range stale.RunIDs {
		log.Info("stale run_id=%s", staleRunID)
	}
	if staleErr != nil {
		log.Error("read stale recovery state: %v", staleErr)
		return 1
	}
	return 0
}

func commandStop(ctx context.Context, db *Database, cfg BenchConfig, log *RunLog, runID string) int {
	backend := newDatabaseRestoreBackend(
		db,
		cfg,
		log,
		DefaultFaultProviderRegistry(),
	)
	return executeRestoreService(
		ctx,
		NewRestoreCoordinatorWithValidation(
			backend,
			cfg.Run.ValidationEnabled,
		),
		RestoreRequest{RunID: runID, DryRun: cfg.Run.DryRun},
		"stop",
		log,
	)
}

type databaseRestoreBackend struct {
	db          *Database
	cfg         BenchConfig
	log         *RunLog
	store       ActionStore
	ledger      RecoveryLedger
	provider    FaultProvider
	providerErr error
	environment Environment
	executor    *restoreDispatchExecutor
	actionStore *coordinatorActionStore
	journal     *Journal
	health      restoreHealthVerifier

	databaseActions []Action
	localActions    []Action

	cancelTagged        func(context.Context, string) error
	terminateTagged     func(context.Context, string) error
	taggedSessionState  func(context.Context, string) (int, int, error)
	restorePollInterval time.Duration

	acquireDatabaseRestoreLock func(
		context.Context,
		RestoreLock,
	) (RestoreLock, error)
	waitForDatabaseFn func(context.Context) error
	offlineRestored   []Action
	mutating          bool
	finishRunFn       func(
		context.Context,
		string,
		string,
		Outcome,
		string,
	) error
}

var errRestoreBusy = errors.New("restore is busy")

type restoreDatabaseConnectivityError struct {
	err error
}

func (e *restoreDatabaseConnectivityError) Error() string {
	return e.err.Error()
}

func (e *restoreDatabaseConnectivityError) Unwrap() error {
	return e.err
}

func newRestoreDatabaseConnectivityError(err error) error {
	if err == nil {
		err = errors.New("database restore session is unavailable")
	}
	return &restoreDatabaseConnectivityError{err: err}
}

func newRestoreBusyError(scope string) error {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return errRestoreBusy
	}
	return fmt.Errorf("%w for %s", errRestoreBusy, scope)
}

func isRestoreDatabaseConnectivityError(err error) bool {
	var connectivityErr *restoreDatabaseConnectivityError
	return errors.As(err, &connectivityErr)
}

type restoreDatabasePinger interface {
	Ping(context.Context) error
}

func (b *databaseRestoreBackend) RestoreTimeout() time.Duration {
	return b.cfg.Safety.RestoreTimeout
}

func (b *databaseRestoreBackend) RestoreFinalizationTimeout() time.Duration {
	return b.cfg.Safety.QueryTimeout
}

func waitForRestoreDatabase(
	ctx context.Context,
	pinger restoreDatabasePinger,
	maximum time.Duration,
	interval time.Duration,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if pinger == nil {
		return fmt.Errorf("restore database pinger is unavailable")
	}
	if maximum <= 0 {
		maximum = 10 * time.Minute
	}
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	waitCtx := ctx
	cancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		waitCtx, cancel = context.WithTimeout(ctx, maximum)
	}
	defer cancel()
	var lastErr error
	for {
		if err := waitCtx.Err(); err != nil {
			return errors.Join(
				fmt.Errorf("wait for restore database: %w", err),
				lastErr,
			)
		}
		if err := pinger.Ping(waitCtx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return errors.Join(
				fmt.Errorf(
					"wait for restore database: %w",
					waitCtx.Err(),
				),
				lastErr,
			)
		case <-timer.C:
		}
	}
}

func (b *databaseRestoreBackend) waitForDatabase(
	ctx context.Context,
) error {
	return waitForRestoreDatabase(
		ctx,
		b.db,
		b.cfg.Safety.RestoreTimeout,
		200*time.Millisecond,
	)
}

func newDatabaseRestoreBackend(
	db *Database,
	cfg BenchConfig,
	log *RunLog,
	registry *FaultProviderRegistry,
) *databaseRestoreBackend {
	store := newSQLJournalStore(databaseJournalDB{db: db}, cfg.Data.Schema)
	provider, providerErr := NewFaultProvider(registry, cfg.FaultProvider)
	environment := Environment{
		Product:      ProductUnknown,
		Topology:     TopologyUnknown,
		Capabilities: make(CapabilitySet),
	}
	executor := newRestoreDispatchExecutor(
		dbActionExecutor{db: db},
		provider,
		environment,
	)
	executor.providerErr = providerErr
	return &databaseRestoreBackend{
		db: db, cfg: cfg, log: log, store: store,
		ledger:   NewFileRecoveryLedger(cfg.FaultProvider.LedgerPath),
		provider: provider, providerErr: providerErr,
		environment: environment, executor: executor,
		cancelTagged:        db.CancelTagged,
		terminateTagged:     db.TerminateTagged,
		taggedSessionState:  db.TaggedSessionState,
		restorePollInterval: 200 * time.Millisecond,
		waitForDatabaseFn: func(ctx context.Context) error {
			return waitForRestoreDatabase(
				ctx,
				db,
				cfg.Safety.RestoreTimeout,
				200*time.Millisecond,
			)
		},
		health: databaseRestoreHealthVerifier{db: db},
	}
}

type databaseRestoreLock struct {
	once  sync.Once
	db    *Database
	conn  *sql.Conn
	key   string
	local RestoreLock
	err   error
}

func (l *databaseRestoreLock) Release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		var errs []error
		if l.conn != nil {
			ctx, cancel := l.db.operationContext(context.Background())
			var unlocked bool
			err := l.conn.QueryRowContext(
				ctx,
				"SELECT pg_advisory_unlock(hashtext($1))",
				l.key,
			).Scan(&unlocked)
			cancel()
			if err != nil {
				errs = append(errs, fmt.Errorf(
					"release database advisory lock: %w", err,
				))
			} else if !unlocked {
				errs = append(errs, fmt.Errorf(
					"database advisory lock was not held",
				))
			}
			if err := l.conn.Close(); err != nil {
				errs = append(errs, fmt.Errorf(
					"close restore lock connection: %w", err,
				))
			}
			l.conn = nil
		}
		if l.local != nil {
			if err := l.local.Release(); err != nil {
				errs = append(errs, err)
			}
			l.local = nil
		}
		l.err = errors.Join(errs...)
	})
	return l.err
}

func (b *databaseRestoreBackend) AcquireRestoreLock(
	ctx context.Context,
) (RestoreLock, error) {
	local, err := acquireLocalRestoreLock(
		ctx,
		b.cfg.FaultProvider.LedgerPath,
	)
	if err != nil {
		return nil, err
	}
	acquireDatabase := b.acquireDatabaseRestoreLock
	if acquireDatabase == nil {
		acquireDatabase = b.acquireDatabaseLock
	}
	lock, databaseErr := acquireDatabase(ctx, local)
	if databaseErr == nil {
		b.mutating = true
		return lock, nil
	}
	if errors.Is(databaseErr, errRestoreBusy) ||
		!isRestoreDatabaseConnectivityError(databaseErr) {
		releaseErr := local.Release()
		return nil, errors.Join(
			fmt.Errorf("database restore lock: %w", databaseErr),
			wrapRestoreError("release local restore mutex", releaseErr),
		)
	}
	initialDatabaseErr := databaseErr

	localErr := b.restoreLocalControlPlane(ctx)
	waitForDatabase := b.waitForDatabaseFn
	if waitForDatabase == nil {
		waitForDatabase = b.waitForDatabase
	}
	waitErr := waitForDatabase(ctx)
	var retryErr error
	if waitErr == nil {
		lock, databaseErr = acquireDatabase(ctx, local)
		if databaseErr == nil {
			b.mutating = true
			return lock, nil
		}
		retryErr = databaseErr
	}
	releaseErr := local.Release()
	return nil, errors.Join(
		fmt.Errorf("initial database restore lock: %w", initialDatabaseErr),
		wrapRestoreError("restore local control-plane actions", localErr),
		wrapRestoreError("reconnect restore database", waitErr),
		wrapRestoreError("retry database restore lock", retryErr),
		wrapRestoreError("release local restore mutex", releaseErr),
	)
}

func (b *databaseRestoreBackend) acquireDatabaseLock(
	ctx context.Context,
	local RestoreLock,
) (RestoreLock, error) {
	if b.db == nil || b.db.pool == nil {
		return nil, newRestoreDatabaseConnectivityError(
			errors.New("database restore connection is unavailable"),
		)
	}
	opCtx, cancel := b.db.operationContext(ctx)
	conn, err := b.db.pool.Conn(opCtx)
	if err != nil {
		cancel()
		sessionErr := fmt.Errorf(
			"open database restore lock session: %w",
			err,
		)
		if ctx.Err() != nil {
			return nil, sessionErr
		}
		return nil, newRestoreDatabaseConnectivityError(sessionErr)
	}
	key := "gsbench/restore/" + b.cfg.Database.Database + "/" +
		b.cfg.Data.Schema
	var acquired bool
	err = conn.QueryRowContext(
		opCtx,
		"SELECT pg_try_advisory_lock(hashtext($1))",
		key,
	).Scan(&acquired)
	cancel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("acquire database advisory lock: %w", err)
	}
	if !acquired {
		_ = conn.Close()
		return nil, newRestoreBusyError(
			fmt.Sprintf(
				"database %s schema %s",
				b.cfg.Database.Database,
				b.cfg.Data.Schema,
			),
		)
	}
	return &databaseRestoreLock{
		db: b.db, conn: conn, key: key, local: local,
	}, nil
}

func (b *databaseRestoreBackend) restoreLocalControlPlane(
	ctx context.Context,
) error {
	if b.ledger == nil {
		return fmt.Errorf("local recovery ledger is unavailable")
	}
	if b.executor == nil {
		return fmt.Errorf("restore action executor is unavailable")
	}
	localActions, err := b.ledger.Pending(ctx, "")
	if err != nil {
		return fmt.Errorf("discover local recovery actions: %w", err)
	}
	var controlActions []Action
	for _, action := range localActions {
		stage, _ := restoreActionOrder(action)
		if stage == restoreStageExternalControl {
			controlActions = append(controlActions, action)
		}
	}
	runs, actions, planErr := prepareRestorePlan(
		RestoreDiscovery{LocalActions: controlActions},
		"",
	)
	if planErr != nil {
		return planErr
	}
	store := newCoordinatorActionStore(
		nil,
		b.ledger,
		nil,
		actions,
	)
	journal := NewJournalWithValidation(
		store,
		b.executor,
		b.cfg.Run.ValidationEnabled,
	)
	var errs []error
	for _, runID := range runs {
		group := restoreActionGroup(
			actions,
			runID,
			restoreStageExternalControl,
		)
		if len(group) == 0 {
			continue
		}
		if err := journal.restoreCoordinatorActions(ctx, group); err != nil {
			errs = append(errs, fmt.Errorf(
				"restore local run %s control-plane actions: %w",
				runID,
				err,
			))
			continue
		}
		b.offlineRestored = append(b.offlineRestored, group...)
	}
	return errors.Join(errs...)
}

func (b *databaseRestoreBackend) DiscoverRestore(
	ctx context.Context,
	requested string,
) (RestoreDiscovery, error) {
	var discovery RestoreDiscovery
	allLocal, localErr := b.localActionSnapshot(ctx, requested)
	if localErr != nil {
		return discovery, fmt.Errorf(
			"discover local recovery actions: %w",
			localErr,
		)
	}
	for _, action := range allLocal {
		if action.State != MutationRestored {
			discovery.LocalActions = append(discovery.LocalActions, action)
		}
	}
	runIDs := []string{requested}
	if requested == "" {
		staleRuns, err := b.store.StaleRuns(ctx)
		if err != nil {
			return discovery, errors.Join(
				fmt.Errorf("discover pending database runs: %w", err),
				wrapRestoreError(
					"restore local control-plane actions",
					b.restoreLocalControlPlane(ctx),
				),
			)
		}
		runIDs = staleRuns
	}
	for _, runID := range runIDs {
		if runID == "" {
			continue
		}
		actions, err := b.store.Pending(ctx, runID)
		if err != nil {
			return discovery, errors.Join(
				fmt.Errorf(
					"discover database actions for run %s: %w",
					runID,
					err,
				),
				wrapRestoreError(
					"restore local control-plane actions",
					b.restoreLocalControlPlane(ctx),
				),
			)
		}
		discovery.DatabaseActions = append(
			discovery.DatabaseActions,
			actions...,
		)
	}
	if b.mutating {
		var err error
		discovery.DatabaseActions, err = b.syncRestoredLocalMirrors(
			ctx,
			discovery.DatabaseActions,
			allLocal,
		)
		if err != nil {
			return discovery, err
		}
	}

	runs, err := b.discoverMetaRuns(ctx, requested)
	if err != nil {
		return discovery, err
	}
	runs, err = b.addPendingRunMetadata(
		ctx,
		runs,
		append(
			append([]Action(nil), discovery.DatabaseActions...),
			discovery.LocalActions...,
		),
	)
	if err != nil {
		return discovery, err
	}
	discovery.Runs = runs
	environment := DetectEnvironment(ctx, b.db)
	b.environment = environment
	b.executor.environment = environment
	b.databaseActions = append([]Action(nil), discovery.DatabaseActions...)
	b.localActions = append([]Action(nil), discovery.LocalActions...)
	actionStore := newCoordinatorActionStore(
		b.store,
		b.ledger,
		b.databaseActions,
		b.localActions,
	)
	b.actionStore = actionStore
	b.journal = NewJournalWithValidation(
		actionStore,
		b.executor,
		b.cfg.Run.ValidationEnabled,
	)
	return discovery, nil
}

type recoveryLedgerSnapshotter interface {
	Snapshot(context.Context, string) ([]Action, error)
}

func (b *databaseRestoreBackend) localActionSnapshot(
	ctx context.Context,
	runID string,
) ([]Action, error) {
	if b.ledger == nil {
		return nil, fmt.Errorf("local recovery ledger is unavailable")
	}
	if snapshotter, ok := b.ledger.(recoveryLedgerSnapshotter); ok {
		return snapshotter.Snapshot(ctx, runID)
	}
	return b.ledger.Pending(ctx, runID)
}

func (b *databaseRestoreBackend) syncRestoredLocalMirrors(
	ctx context.Context,
	databaseActions []Action,
	localActions []Action,
) ([]Action, error) {
	if b.store == nil {
		return nil, fmt.Errorf("database journal store is unavailable")
	}
	restored := make(map[restoreActionIdentity]Action)
	for _, action := range localActions {
		if action.State == MutationRestored {
			restored[restoreIdentity(action)] = action
		}
	}
	pending := make([]Action, 0, len(databaseActions))
	var errs []error
	for _, databaseAction := range databaseActions {
		localAction, ok := restored[restoreIdentity(databaseAction)]
		if !ok {
			pending = append(pending, databaseAction)
			continue
		}
		if !sameRestoreAction(databaseAction, localAction) {
			errs = append(errs, fmt.Errorf(
				"conflicting restored local mirror for run %s action %d",
				databaseAction.RunID,
				databaseAction.Sequence,
			))
			pending = append(pending, databaseAction)
			continue
		}
		if err := b.store.SetState(
			ctx,
			databaseAction.RunID,
			databaseAction.Sequence,
			MutationRestored,
			"",
		); err != nil {
			errs = append(errs, fmt.Errorf(
				"synchronize offline-restored database mirror %s/%d: %w",
				databaseAction.RunID,
				databaseAction.Sequence,
				err,
			))
			pending = append(pending, databaseAction)
		}
	}
	return pending, errors.Join(errs...)
}

func (b *databaseRestoreBackend) addPendingRunMetadata(
	ctx context.Context,
	runs []RestoreRun,
	actions []Action,
) ([]RestoreRun, error) {
	quotedSchema, ok := quoteDatasetSchema(b.cfg.Data.Schema)
	if !ok {
		return nil, fmt.Errorf("unsafe dataset schema %q", b.cfg.Data.Schema)
	}
	seen := make(map[string]bool, len(runs))
	for _, run := range runs {
		seen[run.RunID] = true
	}
	for _, action := range actions {
		if seen[action.RunID] {
			continue
		}
		seen[action.RunID] = true
		var startedAt time.Time
		err := b.db.Scan(
			ctx,
			"SELECT started_at FROM "+quotedSchema+
				".meta_runs WHERE run_id=$1",
			[]any{action.RunID},
			&startedAt,
		)
		if errors.Is(err, sql.ErrNoRows) {
			runs = append(runs, RestoreRun{RunID: action.RunID})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf(
				"discover pending run %s metadata: %w",
				action.RunID,
				err,
			)
		}
		runs = append(runs, RestoreRun{
			RunID: action.RunID, StartedAt: startedAt,
		})
	}
	return runs, nil
}

func (b *databaseRestoreBackend) discoverMetaRuns(
	ctx context.Context,
	requested string,
) ([]RestoreRun, error) {
	quotedSchema, ok := quoteDatasetSchema(b.cfg.Data.Schema)
	if !ok {
		return nil, fmt.Errorf("unsafe dataset schema %q", b.cfg.Data.Schema)
	}
	if requested != "" {
		var startedAt time.Time
		err := b.db.Scan(
			ctx,
			"SELECT started_at FROM "+quotedSchema+
				".meta_runs WHERE run_id=$1",
			[]any{requested},
			&startedAt,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return []RestoreRun{{RunID: requested}}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("discover requested meta run: %w", err)
		}
		return []RestoreRun{{RunID: requested, StartedAt: startedAt}}, nil
	}
	rows, err := b.db.Query(
		ctx,
		"SELECT run_id,started_at FROM "+quotedSchema+
			".meta_runs WHERE status IN ("+
			"'running','stop_requested','restore_requested',"+
			"'restore_failed','RESTORE_FAILED') "+
			"ORDER BY started_at DESC,run_id DESC",
	)
	if err != nil {
		return nil, fmt.Errorf("discover active meta runs: %w", err)
	}
	defer rows.Close()
	var runs []RestoreRun
	for rows.Next() {
		var run RestoreRun
		if err := rows.Scan(&run.RunID, &run.StartedAt); err != nil {
			return nil, fmt.Errorf("scan active meta run: %w", err)
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (b *databaseRestoreBackend) MarkRestoreRequested(
	ctx context.Context,
	runID string,
) error {
	quotedSchema, ok := quoteDatasetSchema(b.cfg.Data.Schema)
	if !ok {
		return fmt.Errorf("unsafe dataset schema %q", b.cfg.Data.Schema)
	}
	_, err := b.db.Exec(
		ctx,
		"UPDATE "+quotedSchema+
			".meta_runs SET phase=$1,status='restore_requested',"+
			"detail='restore requested',updated_at=current_timestamp "+
			"WHERE run_id=$2",
		string(PhaseRestore),
		runID,
	)
	return err
}

func (b *databaseRestoreBackend) StopTaggedSessions(
	ctx context.Context,
	runID string,
) error {
	var errs []error
	if b.cancelTagged == nil || b.terminateTagged == nil ||
		b.taggedSessionState == nil {
		return fmt.Errorf("tagged-session recovery boundary is unavailable")
	}
	if err := b.cancelTagged(ctx, runID); err != nil {
		errs = append(errs, fmt.Errorf("cancel tagged sessions: %w", err))
	}
	if err := b.terminateTagged(ctx, runID); err != nil {
		errs = append(errs, fmt.Errorf("terminate tagged sessions: %w", err))
	}
	interval := b.restorePollInterval
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	for {
		sessions, locks, err := b.taggedSessionState(ctx, runID)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"inspect tagged-session quiescence: %w", err,
			))
		} else if sessions == 0 && locks == 0 {
			break
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			errs = append(errs, fmt.Errorf(
				"wait for run %s session/lock quiescence: %w",
				runID,
				ctx.Err(),
			))
			return errors.Join(errs...)
		case <-timer.C:
		}
	}
	return errors.Join(errs...)
}

func (b *databaseRestoreBackend) RestoreActionGroup(
	ctx context.Context,
	actions []Action,
) error {
	if b.journal == nil {
		return fmt.Errorf("restore action journal was not initialized by discovery")
	}
	for _, action := range actions {
		if action.Kind == ActionSQLMutation ||
			action.Kind == ActionDataBaseline {
			if err := b.waitForDatabase(ctx); err != nil {
				return err
			}
			break
		}
	}
	journalErr := b.journal.restoreCoordinatorActions(ctx, actions)
	return errors.Join(journalErr, b.actionStore.DrainErrors())
}

func (b *databaseRestoreBackend) RepairBaseline(ctx context.Context) error {
	if err := b.waitForDatabase(ctx); err != nil {
		return err
	}
	exists, err := b.planBaselineExists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		b.log.Info("restore baseline status=NOT_APPLICABLE detail=plan_data unavailable")
		return nil
	}
	results, err := RepairPlanBaseline(ctx, b.db, b.cfg.Data.Schema)
	for _, result := range results {
		b.log.Info("restore target=%s status=%s", result.Target, result.Status)
	}
	return err
}

func (b *databaseRestoreBackend) RedetectTopology(ctx context.Context) error {
	if err := b.waitForDatabase(ctx); err != nil {
		return err
	}
	environment := DetectEnvironment(ctx, b.db)
	b.environment = environment
	b.executor.environment = environment
	if !environment.Supported {
		return fmt.Errorf(
			"target topology is not healthy or supported: product=%s topology=%s",
			environment.Product,
			environment.Topology,
		)
	}
	if b.health == nil {
		return fmt.Errorf("restore topology health verifier is unavailable")
	}
	return b.health.Verify(ctx, environment)
}

func (b *databaseRestoreBackend) VerifyRestore(
	ctx context.Context,
	runIDs []string,
	actions []Action,
) error {
	var errs []error
	if err := b.waitForDatabase(ctx); err != nil {
		return err
	}
	for _, runID := range runIDs {
		predicate, args, err := TaggedSessionPredicate(runID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var sessionCount int
		if err := b.db.Scan(
			ctx,
			"SELECT count(*) FROM pg_stat_activity WHERE "+predicate,
			args,
			&sessionCount,
		); err != nil {
			errs = append(errs, fmt.Errorf(
				"verify sessions for run %s: %w", runID, err,
			))
		} else if sessionCount != 0 {
			errs = append(errs, fmt.Errorf(
				"run %s still has %d tagged sessions",
				runID,
				sessionCount,
			))
		}
		pending, err := b.store.Pending(ctx, runID)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"verify database actions for run %s: %w", runID, err,
			))
		} else if len(pending) != 0 {
			errs = append(errs, fmt.Errorf(
				"run %s still has %d pending database actions",
				runID,
				len(pending),
			))
		}
		localPending, err := b.ledger.Pending(ctx, runID)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"verify local actions for run %s: %w", runID, err,
			))
		} else if len(localPending) != 0 {
			errs = append(errs, fmt.Errorf(
				"run %s still has %d pending local actions",
				runID,
				len(localPending),
			))
		}
	}
	for _, action := range actions {
		if !externalPersistentActionKind(action.Kind) {
			continue
		}
		if b.providerErr != nil {
			errs = append(errs, fmt.Errorf(
				"verify provider state for %s: %w",
				action.Target,
				b.providerErr,
			))
			continue
		}
		if b.provider == nil {
			errs = append(errs, fmt.Errorf(
				"verify provider state for %s: provider unavailable",
				action.Target,
			))
			continue
		}
		if err := b.provider.VerifyRestored(ctx, action); err != nil {
			errs = append(errs, fmt.Errorf(
				"verify provider state for %s: %w", action.Target, err,
			))
		}
	}
	exists, err := b.planBaselineExists(ctx)
	if err != nil {
		errs = append(errs, err)
	} else if exists {
		if err := VerifyPlanBaseline(ctx, b.db, b.cfg.Data.Schema); err != nil {
			errs = append(errs, fmt.Errorf(
				"verify benchmark baseline: %w", err,
			))
		}
	}
	if !b.environment.Supported {
		errs = append(errs, fmt.Errorf(
			"verify topology: unsupported product=%s topology=%s",
			b.environment.Product,
			b.environment.Topology,
		))
	}
	if b.health == nil {
		errs = append(errs, fmt.Errorf(
			"restore topology health verifier is unavailable",
		))
	} else if err := b.health.Verify(ctx, b.environment); err != nil {
		errs = append(errs, fmt.Errorf("verify topology health: %w", err))
	}
	return errors.Join(errs...)
}

func (b *databaseRestoreBackend) planBaselineExists(
	ctx context.Context,
) (bool, error) {
	var count int
	err := b.db.Scan(
		ctx,
		"SELECT count(*) FROM pg_catalog.pg_tables "+
			"WHERE schemaname=$1 AND tablename='plan_data'",
		[]any{b.cfg.Data.Schema},
		&count,
	)
	if err != nil {
		return false, fmt.Errorf("detect plan baseline: %w", err)
	}
	return count == 1, nil
}

func (b *databaseRestoreBackend) MarkRestoreOutcome(
	ctx context.Context,
	runID string,
	outcome Outcome,
) error {
	detail := "restored"
	if outcome == OutcomeRestoreFailed {
		detail = "restore failed; retry gsbench restore"
	} else if outcome != OutcomeSuccess {
		detail = "benchmark complete; restore successful"
	}
	finish := b.finishRunFn
	if finish == nil {
		finish = func(
			ctx context.Context,
			schema string,
			runID string,
			outcome Outcome,
			detail string,
		) error {
			return finishRun(
				ctx,
				b.db,
				schema,
				runID,
				outcome,
				detail,
			)
		}
	}
	return finish(
		ctx,
		b.cfg.Data.Schema,
		runID,
		outcome,
		detail,
	)
}

func commandRestore(ctx context.Context, db *Database, cfg BenchConfig, log *RunLog, runID string) int {
	backend := newDatabaseRestoreBackend(
		db,
		cfg,
		log,
		DefaultFaultProviderRegistry(),
	)
	return executeRestoreService(
		ctx,
		NewRestoreCoordinatorWithValidation(
			backend,
			cfg.Run.ValidationEnabled,
		),
		RestoreRequest{RunID: runID, DryRun: cfg.Run.DryRun},
		"restore",
		log,
	)
}

func commandCleanup(ctx context.Context, db *Database, cfg BenchConfig, log *RunLog, runID string, withData bool) int {
	if code := commandStop(ctx, db, cfg, log, runID); code != 0 {
		return code
	}
	if withData {
		quotedSchema, ok := quoteDatasetSchema(cfg.Data.Schema)
		if !ok {
			log.Error("unsafe dataset schema %q", cfg.Data.Schema)
			return 1
		}
		if _, err := db.Exec(ctx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
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
	quotedSchema, ok := quoteDatasetSchema(cfg.Data.Schema)
	if !ok {
		return fmt.Errorf("unsafe dataset schema %q", cfg.Data.Schema)
	}
	_, err := db.Exec(ctx, "INSERT INTO "+quotedSchema+".meta_runs(run_id,scenarios,phase,status,owner_name,started_at,updated_at) VALUES($1,$2,$3,$4,current_user,current_timestamp,current_timestamp)", runID, formatScenarioCodes(cfg.Run.ScenarioCodes), string(PhasePreflight), "running")
	return err
}

func formatScenarioCodes(codes []ScenarioCode) string {
	values := make([]string, len(codes))
	for index, code := range codes {
		values[index] = fmt.Sprintf("%03d", code)
	}
	return strings.Join(values, ",")
}
func finishRun(ctx context.Context, db *Database, schema, runID string, outcome Outcome, detail string) error {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return fmt.Errorf("unsafe dataset schema %q", schema)
	}
	_, err := db.Exec(ctx, "UPDATE "+quotedSchema+".meta_runs SET phase=$1,status=$2,detail=$3,updated_at=current_timestamp WHERE run_id=$4", string(PhaseRestore), string(outcome), detail, runID)
	return err
}
func watchStop(ctx context.Context, db *Database, schema, runID string, cancel context.CancelFunc) {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		cancel()
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var status string
			if db.Scan(ctx, "SELECT status FROM "+quotedSchema+".meta_runs WHERE run_id=$1", []any{runID}, &status) == nil && status == "stop_requested" {
				cancel()
				return
			}
		}
	}
}
func newRunID() string {
	return time.Now().Format("20060102T150405") + "-" + strconv.FormatInt(rand.Int63n(1<<24), 36)
}
