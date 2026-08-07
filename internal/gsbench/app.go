package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/rand"
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

func configOverridesFromCLI(options CLIOptions) Overrides {
	overrides := Overrides{
		ScenarioCodes: options.ScenarioCodes, Duration: options.Duration,
		Workers: options.Workers, TPWorkers: options.TPWorkers,
		APWorkers: options.APWorkers, WorkMemKB: options.WorkMemKB,
		PoolPercent: options.PoolPercent,
		Sessions:    options.Sessions, ChainDepth: options.ChainDepth,
		Profile: options.Profile, DatasetBytes: options.DatasetBytes,
		DatasetSize: options.DatasetSize,
	}
	if options.DryRun {
		value := true
		overrides.DryRun = &value
	}
	return overrides
}

func runCommandNeedsGeneratedRunID(options CLIOptions) bool {
	if options.Command != "run" {
		return false
	}
	return options.PlanAction == "" || options.PlanAction == PlanRunInit
}

func executeCommand(ctx context.Context, options CLIOptions, stdout, stderr io.Writer) int {
	overrides := configOverridesFromCLI(options)
	cfg, err := LoadConfig(options.ConfigPath, overrides)
	if err != nil {
		fmt.Fprintln(stderr, "load config:", err)
		return 1
	}
	if options.Command == "run" && options.PlanAction == "" &&
		scenarioCodesContainPlanChange(cfg.Run.ScenarioCodes) {
		fmt.Fprintln(
			stderr,
			"plan scenarios 601-606 require init, fault, or recover; "+
				"for example: gsbench run 601 init --worker 10 --duration 1m",
		)
		return 1
	}
	runID := strings.TrimSpace(options.RunID)
	if runID == "" && runCommandNeedsGeneratedRunID(options) {
		runID = newRunID()
	}
	if runID != "" {
		if err := validateTagComponent("run ID", runID); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	logIdentity := runID
	if logIdentity == "" {
		if options.Command == "run" {
			logIdentity = newRunID()
		} else {
			logIdentity = options.Command
		}
	}
	logPath := ""
	if !commandIsReadOnly(options.Command, cfg.Run.DryRun) {
		logPath, err = runLogPath(cfg.ConfigDir, logIdentity)
		if err != nil {
			fmt.Fprintln(stderr, "resolve log path:", err)
			return 1
		}
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
	if options.Command != "init" &&
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
		if options.PlanAction != "" {
			return commandPlanRunAction(
				ctx,
				db,
				cfg,
				environment,
				caps,
				logger,
				options,
				runID,
			)
		}
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
		return commandStatus(ctx, db, cfg, logger, runID)
	case "stop":
		return commandStop(ctx, db, cfg, logger, runID)
	case "restore":
		return commandRestore(ctx, db, cfg, logger, runID)
	case "cleanup":
		return commandCleanup(ctx, db, cfg, logger, runID, options.WithData)
	default:
		logger.Error("unknown command %s", options.Command)
		return 2
	}
}

func commandIsReadOnly(command string, dryRun bool) bool {
	return command == "doctor" ||
		command == "status" ||
		(dryRun && (command == "restore" ||
			command == "stop" ||
			command == "cleanup"))
}

func commandDoctor(ctx context.Context, db *Database, cfg BenchConfig, env Environment, log *RunLog) int {
	for _, line := range doctorEnvironmentReport(env, implementedScenarioDefinitions()) {
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
) (exitCode int) {
	if !caps.Supported {
		log.Error("unsupported target product or topology")
		return 1
	}
	if !cfg.Run.DryRun {
		lockIdentity := planDatabaseLockIdentity(cfg)
		release, err := AcquireDatabaseRunLock(ctx, db, lockIdentity)
		if err != nil {
			log.Error("acquire plan baseline lock for initialization: %v", err)
			return 1
		}
		defer finishInitializationPlanLock(release, log, &exitCode)
	}
	exists, err := datasetExists(ctx, db, cfg.Data.Schema)
	if err != nil {
		log.Error("inspect existing benchmark dataset: %v", err)
		return 1
	}
	if err := validateDatasetReusePolicy(
		cfg.Data.ReuseExisting,
		exists,
		cfg.Data.Schema,
	); err != nil {
		log.Error("initialize dataset: %v", err)
		return 1
	}
	providerDB := databaseJournalDB{db: db}
	externalProviders := DatasetExternalProviders{}
	capacityProvider, providerErr := selectDatasetCapacityProvider(
		cfg, env, providerDB, externalProviders,
	)
	if providerErr != nil {
		if !cfg.Run.DryRun {
			log.Error("select dataset capacity provider: %v", providerErr)
			return 1
		}
		capacityProvider = unavailableDatasetCapacityProvider{err: providerErr}
	}
	capacity, capacityStatus, err := resolveDatasetCapacity(
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
	plan, err := PlanDataset(cfg, capacity, env)
	if err != nil {
		log.Error("plan dataset: %v", err)
		return 1
	}
	var physicalProvider DatasetPhysicalProvider
	if !cfg.Run.DryRun {
		physicalProvider, err = selectDatasetPhysicalProvider(
			cfg, env, providerDB, externalProviders,
		)
		if err != nil {
			log.Error("select dataset physical-size provider: %v", err)
			return 1
		}
	}
	executor := initializationDatasetExecutor{
		dbDatasetExecutor: dbDatasetExecutor{
			db:                 db,
			schema:             cfg.Data.Schema,
			env:                env,
			capacityProvider:   capacityProvider,
			physicalProvider:   physicalProvider,
			minFreeDiskPercent: cfg.Data.MinFreeDiskPercent,
		},
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
	backend := newDatabaseRestoreBackend(
		db,
		cfg,
		log,
		DefaultFaultProviderRegistry(),
	)
	if code := executeInitializationMutation(
		ctx,
		backend,
		log,
		acquireInitializationRestoreLock,
		func() error { return manager.Init(ctx, plan) },
	); code != 0 {
		return code
	}
	log.Info("init SUCCESS")
	return 0
}

type initializationRestoreLockAcquirer func(
	context.Context,
	*databaseRestoreBackend,
) (RestoreLock, error)

// acquireInitializationRestoreLock is deliberately a single-attempt lock
// acquisition. Initialization must never trigger recovery side effects merely
// because the database is unavailable while it is trying to exclude cleanup.
func acquireInitializationRestoreLock(
	ctx context.Context,
	backend *databaseRestoreBackend,
) (RestoreLock, error) {
	if backend == nil {
		return nil, fmt.Errorf("initialization restore backend is unavailable")
	}
	acquireLocal := backend.acquireLocalRestoreLock
	if acquireLocal == nil {
		acquireLocal = acquireLocalRestoreLock
	}
	local, err := acquireLocal(
		ctx,
		backend.cfg.FaultProvider.LedgerPath,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"acquire local initialization mutex: %w",
			err,
		)
	}
	if local == nil {
		return nil, fmt.Errorf(
			"acquire local initialization mutex: no lock returned",
		)
	}
	acquireDatabase := backend.acquireDatabaseRestoreLock
	if acquireDatabase == nil {
		acquireDatabase = func(
			acquireCtx context.Context,
			local RestoreLock,
		) (RestoreLock, error) {
			return backend.acquireDatabaseLockWithPlanRequirement(
				acquireCtx,
				local,
				false,
			)
		}
	}
	lock, lockErr := acquireDatabase(ctx, local)
	if lockErr != nil {
		return nil, errors.Join(
			fmt.Errorf("acquire database initialization mutex: %w", lockErr),
			wrapRestoreError(
				"release local initialization mutex",
				local.Release(),
			),
		)
	}
	if lock == nil {
		return nil, errors.Join(
			fmt.Errorf("acquire database initialization mutex: no lock returned"),
			wrapRestoreError(
				"release local initialization mutex",
				local.Release(),
			),
		)
	}
	return lock, nil
}

func executeInitializationMutation(
	ctx context.Context,
	backend *databaseRestoreBackend,
	log *RunLog,
	acquire initializationRestoreLockAcquirer,
	mutation func() error,
) int {
	if acquire == nil {
		acquire = acquireInitializationRestoreLock
	}
	lock, err := acquire(ctx, backend)
	if err != nil {
		log.Error("acquire cleanup exclusion lock for initialization: %v", err)
		return 1
	}
	if lock == nil {
		log.Error(
			"acquire cleanup exclusion lock for initialization: no lock returned",
		)
		return 1
	}
	var mutationErr error
	if mutation == nil {
		mutationErr = fmt.Errorf("dataset initialization mutation is unavailable")
	} else {
		mutationErr = mutation()
	}
	releaseErr := lock.Release()
	if mutationErr != nil {
		log.Error("initialize dataset: %v", mutationErr)
	}
	if releaseErr != nil {
		log.Error(
			"release cleanup exclusion lock after initialization: %v",
			releaseErr,
		)
	}
	if mutationErr != nil || releaseErr != nil {
		return 1
	}
	return 0
}

func finishInitializationPlanLock(
	release func() error,
	log *RunLog,
	exitCode *int,
) {
	if release == nil {
		log.Error("release plan baseline lock after initialization: release is unavailable")
		if exitCode != nil {
			*exitCode = 1
		}
		return
	}
	if err := release(); err != nil {
		log.Error("release plan baseline lock after initialization: %v", err)
		if exitCode != nil {
			*exitCode = 1
		}
	}
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
	var planLockErr error
	exitCode, runLockErr := withRunExecutionDatabaseLock(
		parent,
		db,
		cfg,
		runID,
		AcquireDatabaseRunLock,
		func() int {
			var code int
			code, planLockErr = withPlanScenarioDatabaseLock(
				parent,
				db,
				cfg,
				AcquireDatabaseRunLock,
				func() int {
					return commandRunCore(
						parent,
						db,
						cfg,
						environment,
						caps,
						allowRisk,
						log,
						runID,
						quotedSchema,
					)
				},
			)
			return code
		},
	)
	if runLockErr != nil {
		log.Error("run execution session lock: %v", runLockErr)
		return 1
	}
	if planLockErr != nil {
		log.Error("plan-change session lock: %v", planLockErr)
		return 1
	}
	return exitCode
}

type databaseRunLockAcquirer func(
	context.Context,
	*Database,
	string,
) (func() error, error)

func withPlanScenarioDatabaseLock(
	ctx context.Context,
	db *Database,
	cfg BenchConfig,
	acquire databaseRunLockAcquirer,
	work func() int,
) (exitCode int, err error) {
	if work == nil {
		return 1, fmt.Errorf("plan-change run callback is unavailable")
	}
	if !scenarioCodesContainPlanChange(cfg.Run.ScenarioCodes) {
		return work(), nil
	}
	return withPlanDatabaseLock(ctx, db, cfg, acquire, work)
}

func withPlanDatabaseLock(
	ctx context.Context,
	db *Database,
	cfg BenchConfig,
	acquire databaseRunLockAcquirer,
	work func() int,
) (exitCode int, err error) {
	if work == nil {
		return 1, fmt.Errorf("plan baseline callback is unavailable")
	}
	if acquire == nil {
		return 1, fmt.Errorf("plan baseline lock acquirer is unavailable")
	}
	identity := planDatabaseLockIdentity(cfg)
	release, err := acquire(ctx, db, identity)
	if err != nil {
		return 1, fmt.Errorf("acquire plan baseline lock: %w", err)
	}
	if release == nil {
		return 1, fmt.Errorf("plan baseline lock release is unavailable")
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			exitCode = 1
			err = fmt.Errorf("release plan baseline lock: %w", releaseErr)
		}
	}()
	return work(), nil
}

func planDatabaseLockIdentity(cfg BenchConfig) string {
	return fmt.Sprintf(
		"gsbench:plan:%s:%s",
		cfg.Database.Database,
		cfg.Data.Schema,
	)
}

func withPlanRunPreparationDatabaseLock(
	ctx context.Context,
	db *Database,
	cfg BenchConfig,
	alreadyHeld bool,
	acquire databaseRunLockAcquirer,
	work func() int,
) (int, error) {
	if alreadyHeld {
		if work == nil {
			return 1, fmt.Errorf("plan baseline callback is unavailable")
		}
		return work(), nil
	}
	return withPlanDatabaseLock(ctx, db, cfg, acquire, work)
}

func commandRunCore(
	parent context.Context,
	db *Database,
	cfg BenchConfig,
	environment Environment,
	caps Capabilities,
	allowRisk RiskLevel,
	log *RunLog,
	runID string,
	quotedSchema string,
) int {
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
	// Run preparation always executes under the outer plan/schema lock.
	// The restore backend must not try to reacquire that session lock.
	backend.requirePlanLock = false
	backend.skipLiveExecutionRuns = true
	startPreparedRun := func(ctx context.Context) error {
		if scenarioCodesContainPlanChange(cfg.Run.ScenarioCodes) {
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
			if err := preparePlanRunBaseline(
				ctx,
				db,
				cfg,
				log,
				RepairPlanBaseline,
				VerifyPlanBaseline,
			); err != nil {
				return err
			}
		}
		return startRun(ctx, db, cfg, runID)
	}
	var staleSummary RestoreSummary
	prepareRun := func() int {
		staleSummary = NewRestoreCoordinatorWithValidation(
			backend,
			cfg.Run.ValidationEnabled,
		).Restore(
			parent,
			RestoreRequest{afterSuccess: func(
				ctx context.Context,
				_ RestoreLock,
			) error {
				return startPreparedRun(ctx)
			}},
		)
		if staleSummary.Failed {
			if err := continueAfterPoolOnlyRecoveryFailure(
				staleSummary,
				log,
				func() error {
					return startPreparedRun(parent)
				},
			); err != nil {
				log.Error("recover stale state and record run: %v", err)
				return 1
			}
			return 0
		}
		if len(staleSummary.RunIDs) != 0 {
			log.Info(
				"stale recovery SUCCESS runs=%d actions=%d",
				len(staleSummary.RunIDs),
				len(staleSummary.PlannedActions),
			)
		}
		return 0
	}
	prepareCode, prepareLockErr := withPlanRunPreparationDatabaseLock(
		parent,
		db,
		cfg,
		scenarioCodesContainPlanChange(cfg.Run.ScenarioCodes),
		AcquireDatabaseRunLock,
		prepareRun,
	)
	if prepareLockErr != nil {
		log.Error("plan baseline lock for stale recovery: %v", prepareLockErr)
		return 1
	}
	if prepareCode != 0 {
		return prepareCode
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go watchStop(ctx, db, cfg.Data.Schema, runID, cancel)
	restoreBackend, closeRestoreBackend, err := newIsolatedRunRestoreBackend(
		ctx,
		cfg,
		log,
		DefaultFaultProviderRegistry(),
		OpenRestoreDatabase,
	)
	if err != nil {
		log.Error("open isolated run restore database: %v", err)
		return 1
	}
	defer func() {
		if err := closeRestoreBackend(); err != nil {
			log.Error("close isolated run restore database: %v", err)
		}
	}()
	// Plan scenarios keep the outer plan/schema lock through Runner restore.
	// A non-plan run cannot journal plan baseline actions, so requiring the
	// plan lock here would only make unrelated cleanup conflict with a plan run.
	restoreBackend.requirePlanLock = false
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

func canContinueAfterPoolOnlyRecoveryFailure(
	summary RestoreSummary,
) bool {
	if !summary.Failed || summary.Err == nil ||
		!summary.DiscoveryComplete || !summary.RestoreLockReleased ||
		summary.AfterSuccessAttempted || len(summary.Runs) == 0 ||
		len(summary.PlannedActions) != 0 {
		return false
	}
	for _, run := range summary.Runs {
		if len(run.ScenarioCodes) == 0 {
			return false
		}
		for _, code := range run.ScenarioCodes {
			if code != 401 && code != 402 {
				return false
			}
		}
	}
	return true
}

func continueAfterPoolOnlyRecoveryFailure(
	summary RestoreSummary,
	log *RunLog,
	start func() error,
) error {
	if !canContinueAfterPoolOnlyRecoveryFailure(summary) {
		if summary.Err != nil {
			return summary.Err
		}
		return fmt.Errorf("stale recovery failed without a recorded cause")
	}
	log.Warn(
		"stale pool-only recovery FAILED but will not block later tests: runs=%d error=%v",
		len(summary.Runs),
		summary.Err,
	)
	if start == nil {
		return fmt.Errorf("new run recorder is unavailable")
	}
	return start()
}

type planBaselineRepairFunc func(
	context.Context,
	*Database,
	string,
) ([]BaselineRepairResult, error)

type planBaselineVerifyFunc func(context.Context, *Database, string) error

func preparePlanRunBaseline(
	ctx context.Context,
	db *Database,
	cfg BenchConfig,
	log *RunLog,
	repair planBaselineRepairFunc,
	verify planBaselineVerifyFunc,
) error {
	if repair == nil {
		return fmt.Errorf("plan baseline repair is unavailable")
	}
	results, err := repair(ctx, db, cfg.Data.Schema)
	for _, result := range results {
		log.Info(
			"pre-run plan baseline target=%s status=%s",
			result.Target,
			result.Status,
		)
	}
	if err != nil {
		return fmt.Errorf("repair pre-run plan baseline: %w", err)
	}
	if !cfg.Run.ValidationEnabled {
		return nil
	}
	if verify == nil {
		return fmt.Errorf("plan baseline verification is unavailable")
	}
	if err := verify(ctx, db, cfg.Data.Schema); err != nil {
		return fmt.Errorf("verify pre-run plan baseline: %w", err)
	}
	return nil
}

func exitCodeForOutcome(outcome Outcome) int {
	switch outcome {
	case OutcomeSuccess, OutcomeCompletedWithWarnings,
		OutcomeUnverified, OutcomeNotApplicable:
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
	return commandRestoreOperation(ctx, db, cfg, log, runID, "stop", true, nil)
}

func commandRestoreOperation(
	ctx context.Context,
	db *Database,
	cfg BenchConfig,
	log *RunLog,
	runID string,
	commandName string,
	requirePlanLock bool,
	afterSuccess func(context.Context, RestoreLock) error,
) int {
	backend := newDatabaseRestoreBackend(
		db,
		cfg,
		log,
		DefaultFaultProviderRegistry(),
	)
	backend.requirePlanLock = requirePlanLock
	if requirePlanLock && !cfg.Run.DryRun {
		requestStop := func(requestCtx context.Context) error {
			return requestRestoreRunsStop(
				requestCtx,
				dbDatasetExecutor{db: db, schema: cfg.Data.Schema},
				cfg.Data.Schema,
				runID,
			)
		}
		backend.requestPlanLockOwnerStop = requestStop
		if err := requestStop(ctx); err != nil {
			log.Info(
				"%s: initial stop request deferred until database recovery: %v",
				commandName,
				err,
			)
		}
	}
	return executeRestoreService(
		ctx,
		NewRestoreCoordinatorWithValidation(
			backend,
			cfg.Run.ValidationEnabled,
		),
		RestoreRequest{
			RunID: runID, DryRun: cfg.Run.DryRun,
			afterSuccess: afterSuccess,
		},
		commandName,
		log,
	)
}

type restoreStopRequestExecutor interface {
	Exec(context.Context, string, ...any) error
}

func requestRestoreRunsStop(
	ctx context.Context,
	executor restoreStopRequestExecutor,
	schema string,
	runID string,
) error {
	if executor == nil {
		return fmt.Errorf("restore stop-request executor is unavailable")
	}
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return fmt.Errorf("unsafe dataset schema %q", schema)
	}
	query := "UPDATE " + quotedSchema +
		".meta_runs SET status='stop_requested'," +
		"detail='stop requested',updated_at=current_timestamp"
	ownershipGate := " EXISTS (SELECT 1 FROM " + quotedSchema +
		".meta_dataset WHERE key='dataset_version'" +
		" AND value IN ('1','2','3','4'))"
	if runID == "" {
		return executor.Exec(
			ctx,
			query+" WHERE status='running' AND"+ownershipGate,
		)
	}
	if err := validateTagComponent("run ID", runID); err != nil {
		return err
	}
	return executor.Exec(
		ctx,
		query+" WHERE run_id=$1 AND status='running' AND"+ownershipGate,
		runID,
	)
}

type databaseRestoreBackend struct {
	db                       *Database
	cfg                      BenchConfig
	log                      *RunLog
	store                    ActionStore
	ledger                   RecoveryLedger
	provider                 FaultProvider
	providerErr              error
	environment              Environment
	executor                 *restoreDispatchExecutor
	actionStore              *coordinatorActionStore
	journal                  *Journal
	health                   restoreHealthVerifier
	requirePlanLock          bool
	requestPlanLockOwnerStop func(context.Context) error
	acquirePlanRunLock       databaseRunLockAcquirer
	acquireLocalRestoreLock  func(context.Context, string) (RestoreLock, error)
	openAdvisorySession      advisoryLockSessionOpener
	runExecutionLeaseHeld    func(context.Context, string) (bool, error)
	skipLiveExecutionRuns    bool

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

func (b *databaseRestoreBackend) ValidateRestoreOwnership(
	ctx context.Context,
) error {
	if b.db == nil {
		// Unit-test restore backends may inject every mutation boundary without
		// constructing a SQL pool. Production backends always carry b.db.
		return nil
	}
	openSession := b.openAdvisorySession
	if openSession == nil {
		openSession = openAdvisoryLockSession
	}
	interval := b.restorePollInterval
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	for {
		session, err := openSession(
			ctx,
			b.db,
			b.cfg.Database.ApplicationName+"/restore-ownership",
		)
		if err != nil {
			err = fmt.Errorf("open restore ownership session: %w", err)
		} else {
			err = validateDatasetOwnership(
				ctx,
				&databaseRestoreLock{session: session},
				b.cfg.Data.Schema,
			)
			if err == nil {
				return wrapRestoreError(
					"close restore ownership session",
					session.Close(),
				)
			}
			err = errors.Join(
				err,
				wrapRestoreError(
					"discard failed restore ownership session",
					session.Discard(),
				),
			)
		}
		if !isRetryableAdvisoryLockError(err) {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

var errRestoreBusy = errors.New("restore is busy")

type planFirstRestoreLock struct {
	once        sync.Once
	restore     RestoreLock
	releasePlan func() error
	err         error
}

func (l *planFirstRestoreLock) Release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		var errs []error
		if l.restore != nil {
			errs = append(errs, l.restore.Release())
			l.restore = nil
		}
		if l.releasePlan != nil {
			errs = append(errs, l.releasePlan())
			l.releasePlan = nil
		}
		l.err = errors.Join(errs...)
	})
	return l.err
}

func (l *planFirstRestoreLock) DatasetVersion(
	ctx context.Context,
	schema string,
) (string, error) {
	executor, ok := l.restore.(cleanupDatasetExecutor)
	if !ok {
		return "", fmt.Errorf(
			"restore lock does not expose its protected database session",
		)
	}
	return executor.DatasetVersion(ctx, schema)
}

func (l *planFirstRestoreLock) Exec(
	ctx context.Context,
	query string,
	args ...any,
) error {
	executor, ok := l.restore.(cleanupDatasetExecutor)
	if !ok {
		return fmt.Errorf(
			"restore lock does not expose its protected database session",
		)
	}
	return executor.Exec(ctx, query, args...)
}

func (b *databaseRestoreBackend) acquirePlanLockForRestore(
	ctx context.Context,
) (func() error, error) {
	acquire := b.acquirePlanRunLock
	if acquire == nil {
		acquire = AcquireDatabaseRunLock
	}
	identity := planDatabaseLockIdentity(b.cfg)
	interval := b.restorePollInterval
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	for {
		if b.requestPlanLockOwnerStop != nil {
			if err := b.requestPlanLockOwnerStop(ctx); err != nil {
				requestErr := fmt.Errorf(
					"request active runs to stop before plan lock: %w",
					err,
				)
				if b.db == nil || b.db.Ping(ctx) != nil {
					return nil, newRestoreDatabaseConnectivityError(requestErr)
				}
				return nil, requestErr
			}
		}
		release, err := acquire(ctx, b.db, identity)
		if err == nil {
			if release == nil {
				return nil, fmt.Errorf("plan restore lock release is unavailable")
			}
			return release, nil
		}
		if ctx.Err() != nil {
			return nil, errors.Join(
				fmt.Errorf("acquire plan restore lock: %w", err),
				ctx.Err(),
			)
		}
		if b.db == nil {
			return nil, newRestoreDatabaseConnectivityError(err)
		}
		if pingErr := b.db.Ping(ctx); pingErr != nil {
			return nil, newRestoreDatabaseConnectivityError(
				errors.Join(err, pingErr),
			)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, errors.Join(
				fmt.Errorf("acquire plan restore lock: %w", err),
				ctx.Err(),
			)
		case <-timer.C:
		}
	}
}

func (b *databaseRestoreBackend) acquirePlanFirstRestoreLock(
	ctx context.Context,
) (RestoreLock, error) {
	releasePlan, err := b.acquirePlanLockForRestore(ctx)
	if err != nil {
		return nil, err
	}
	acquireLocal := b.acquireLocalRestoreLock
	if acquireLocal == nil {
		acquireLocal = acquireLocalRestoreLock
	}
	local, err := acquireLocal(ctx, b.cfg.FaultProvider.LedgerPath)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("acquire local restore mutex: %w", err),
			wrapRestoreError("release plan restore lock", releasePlan()),
		)
	}
	acquireDatabase := b.acquireDatabaseRestoreLock
	if acquireDatabase == nil {
		acquireDatabase = func(
			acquireCtx context.Context,
			local RestoreLock,
		) (RestoreLock, error) {
			return b.acquireDatabaseLockWithPlanRequirement(
				acquireCtx,
				local,
				false,
			)
		}
	}
	restoreLock, err := acquireDatabase(ctx, local)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("acquire database restore lock: %w", err),
			wrapRestoreError("release local restore mutex", local.Release()),
			wrapRestoreError("release plan restore lock", releasePlan()),
		)
	}
	if restoreLock == nil {
		return nil, errors.Join(
			fmt.Errorf("acquire database restore lock: no lock returned"),
			wrapRestoreError("release local restore mutex", local.Release()),
			wrapRestoreError("release plan restore lock", releasePlan()),
		)
	}
	return &planFirstRestoreLock{
		restore: restoreLock, releasePlan: releasePlan,
	}, nil
}

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
		requirePlanLock:     true,
		cancelTagged:        db.CancelTagged,
		terminateTagged:     db.TerminateTagged,
		taggedSessionState:  db.TaggedSessionState,
		restorePollInterval: 200 * time.Millisecond,
		openAdvisorySession: db.openAdvisorySession,
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

type runRestoreDatabaseOpener func(
	context.Context,
	BenchConfig,
) (*Database, error)

func newIsolatedRunRestoreBackend(
	ctx context.Context,
	cfg BenchConfig,
	log *RunLog,
	registry *FaultProviderRegistry,
	open runRestoreDatabaseOpener,
) (*databaseRestoreBackend, func() error, error) {
	if open == nil {
		open = OpenRestoreDatabase
	}
	db, err := open(context.WithoutCancel(ctx), cfg)
	if err != nil {
		return nil, nil, err
	}
	if db == nil {
		return nil, nil, fmt.Errorf(
			"restore database opener returned no database",
		)
	}
	return newDatabaseRestoreBackend(db, cfg, log, registry), db.Close, nil
}

type databaseRestoreLock struct {
	once    sync.Once
	session advisoryLockSession
	keys    []string
	local   RestoreLock
	err     error
}

func (l *databaseRestoreLock) DatasetVersion(
	ctx context.Context,
	schema string,
) (string, error) {
	if l == nil || l.session == nil {
		return "", fmt.Errorf("protected restore database session is unavailable")
	}
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return "", fmt.Errorf("unsafe dataset schema %q", schema)
	}
	var version string
	err := l.session.Scan(
		ctx,
		"SELECT value FROM "+quotedSchema+
			".meta_dataset WHERE key='dataset_version'",
		nil,
		&version,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return version, err
}

func (l *databaseRestoreLock) Exec(
	ctx context.Context,
	query string,
	args ...any,
) error {
	if l == nil || l.session == nil {
		return fmt.Errorf("protected restore database session is unavailable")
	}
	return l.session.Exec(ctx, query, args...)
}

func (l *databaseRestoreLock) Release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		var errs []error
		if l.session != nil {
			releaseErr := releaseDatabaseAdvisoryKeys(l.session, l.keys)
			errs = append(errs, releaseErr)
			if releaseErr != nil {
				errs = append(errs, wrapRestoreError(
					"discard uncertain database advisory-lock session",
					l.session.Discard(),
				))
			} else {
				errs = append(errs, wrapRestoreError(
					"close database restore lock session",
					l.session.Close(),
				))
			}
			l.session = nil
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

func releaseDatabaseAdvisoryKeys(
	session advisoryLockSession,
	keys []string,
) error {
	var errs []error
	for index := len(keys) - 1; index >= 0; index-- {
		key := keys[index]
		unlocked, err := session.Unlock(context.Background(), key)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"release database advisory lock %q: %w",
				key,
				err,
			))
		} else if !unlocked {
			errs = append(errs, fmt.Errorf(
				"database advisory lock %q was not held",
				key,
			))
		}
	}
	return errors.Join(errs...)
}

func (b *databaseRestoreBackend) AcquireRestoreLock(
	ctx context.Context,
) (RestoreLock, error) {
	if b.requirePlanLock {
		return b.acquireRestoreLockPlanFirst(ctx)
	}
	return b.acquireRestoreLockLocalFirst(ctx)
}

func (b *databaseRestoreBackend) acquireRestoreLockPlanFirst(
	ctx context.Context,
) (RestoreLock, error) {
	lock, initialErr := b.acquirePlanFirstRestoreLock(ctx)
	if initialErr == nil {
		b.mutating = true
		return lock, nil
	}
	if !isRestoreDatabaseConnectivityError(initialErr) {
		return nil, initialErr
	}

	acquireLocal := b.acquireLocalRestoreLock
	if acquireLocal == nil {
		acquireLocal = acquireLocalRestoreLock
	}
	local, localAcquireErr := acquireLocal(
		ctx,
		b.cfg.FaultProvider.LedgerPath,
	)
	if localAcquireErr != nil {
		return nil, errors.Join(
			fmt.Errorf("initial plan restore lock: %w", initialErr),
			fmt.Errorf(
				"acquire local control-plane recovery mutex: %w",
				localAcquireErr,
			),
		)
	}
	localErr := b.restoreLocalControlPlane(ctx)
	releaseLocalErr := local.Release()

	waitForDatabase := b.waitForDatabaseFn
	if waitForDatabase == nil {
		waitForDatabase = b.waitForDatabase
	}
	waitErr := waitForDatabase(ctx)
	var retryErr error
	if waitErr == nil {
		lock, retryErr = b.acquirePlanFirstRestoreLock(ctx)
		if retryErr == nil {
			b.mutating = true
			return lock, nil
		}
	}
	return nil, errors.Join(
		fmt.Errorf("initial plan restore lock: %w", initialErr),
		wrapRestoreError("restore local control-plane actions", localErr),
		wrapRestoreError("release local control-plane recovery mutex", releaseLocalErr),
		wrapRestoreError("reconnect restore database", waitErr),
		wrapRestoreError("retry plan-first restore lock", retryErr),
	)
}

func (b *databaseRestoreBackend) acquireRestoreLockLocalFirst(
	ctx context.Context,
) (RestoreLock, error) {
	acquireLocal := b.acquireLocalRestoreLock
	if acquireLocal == nil {
		acquireLocal = acquireLocalRestoreLock
	}
	local, err := acquireLocal(ctx, b.cfg.FaultProvider.LedgerPath)
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
	return b.acquireDatabaseLockWithPlanRequirement(
		ctx,
		local,
		b.requirePlanLock,
	)
}

func (b *databaseRestoreBackend) acquireDatabaseLockWithPlanRequirement(
	ctx context.Context,
	local RestoreLock,
	requirePlanLock bool,
) (RestoreLock, error) {
	if b.db == nil || b.db.pool == nil {
		return nil, newRestoreDatabaseConnectivityError(
			errors.New("database restore connection is unavailable"),
		)
	}
	if requirePlanLock && b.requestPlanLockOwnerStop != nil {
		if err := b.requestPlanLockOwnerStop(ctx); err != nil {
			requestErr := fmt.Errorf(
				"request active runs to stop before plan lock: %w",
				err,
			)
			if pingErr := b.db.Ping(ctx); pingErr != nil {
				return nil, newRestoreDatabaseConnectivityError(
					errors.Join(requestErr, pingErr),
				)
			}
			return nil, requestErr
		}
	}
	openSession := b.openAdvisorySession
	if openSession == nil {
		openSession = openAdvisoryLockSession
	}
	session, err := openSession(
		ctx,
		b.db,
		b.cfg.Database.ApplicationName+"/restore-lock",
	)
	if err != nil {
		sessionErr := fmt.Errorf(
			"open database restore lock session: %w",
			err,
		)
		if ctx.Err() != nil {
			return nil, sessionErr
		}
		if isRetryableAdvisoryLockError(err) {
			return nil, newRestoreDatabaseConnectivityError(sessionErr)
		}
		return nil, sessionErr
	}
	keys := restoreDatabaseAdvisoryKeys(b.cfg, requirePlanLock)
	acquiredKeys := make([]string, 0, len(keys))
	cleanupSession := func(discard bool) error {
		releaseErr := releaseDatabaseAdvisoryKeys(session, acquiredKeys)
		if releaseErr != nil {
			discard = true
		}
		var finishErr error
		if discard {
			finishErr = session.Discard()
		} else {
			finishErr = session.Close()
		}
		return errors.Join(
			wrapRestoreError(
				"release partially acquired database advisory locks",
				releaseErr,
			),
			wrapRestoreError(
				"finish database restore lock session",
				finishErr,
			),
		)
	}
	for _, key := range keys {
		for {
			var acquired bool
			acquired, err = session.TryLock(ctx, key)
			if err != nil {
				acquireErr := fmt.Errorf(
					"acquire database advisory lock %q: %w",
					key,
					err,
				)
				resultErr := errors.Join(
					acquireErr,
					cleanupSession(true),
				)
				if isRetryableAdvisoryLockError(err) {
					return nil, newRestoreDatabaseConnectivityError(resultErr)
				}
				return nil, resultErr
			}
			if acquired {
				break
			}
			busyErr := newRestoreBusyError(
				fmt.Sprintf(
					"database %s schema %s lock %s",
					b.cfg.Database.Database,
					b.cfg.Data.Schema,
					key,
				),
			)
			if !requirePlanLock || key != planDatabaseLockIdentity(b.cfg) {
				return nil, errors.Join(
					busyErr,
					cleanupSession(false),
				)
			}
			// The plan owner can insert meta_runs after the initial stop request
			// but before its first mutation. Repeat the request on every busy
			// observation so that a late row is still seen by watchStop.
			if b.requestPlanLockOwnerStop != nil {
				if requestErr := b.requestPlanLockOwnerStop(ctx); requestErr != nil {
					return nil, errors.Join(
						fmt.Errorf(
							"repeat stop request while plan lock is busy: %w",
							requestErr,
						),
						cleanupSession(false),
					)
				}
			}
			interval := b.restorePollInterval
			if interval <= 0 {
				interval = 200 * time.Millisecond
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return nil, errors.Join(
					busyErr,
					ctx.Err(),
					cleanupSession(false),
				)
			case <-timer.C:
			}
		}
		acquiredKeys = append(acquiredKeys, key)
	}
	return &databaseRestoreLock{
		session: session, keys: acquiredKeys, local: local,
	}, nil
}

func restoreDatabaseAdvisoryKeys(
	cfg BenchConfig,
	requirePlanLock bool,
) []string {
	keys := make([]string, 0, 2)
	if requirePlanLock {
		keys = append(keys, planDatabaseLockIdentity(cfg))
	}
	return append(
		keys,
		"gsbench/restore/"+cfg.Database.Database+"/"+cfg.Data.Schema,
	)
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
	for _, runID := range restoreRunIDs(runs) {
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
	readOnly bool,
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
			if readOnly {
				return discovery, fmt.Errorf(
					"discover pending database runs: %w",
					err,
				)
			}
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
			if readOnly {
				return discovery, fmt.Errorf(
					"discover database actions for run %s: %w",
					runID,
					err,
				)
			}
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
	if requested == "" && b.skipLiveExecutionRuns {
		probeDiscovery := discovery
		probeDiscovery.LocalActions = allLocal
		probeDiscovery, err = b.excludeLiveExecutionRuns(
			ctx,
			probeDiscovery,
		)
		if err != nil {
			return discovery, err
		}
		allLocal = probeDiscovery.LocalActions
		discovery.Runs = probeDiscovery.Runs
		discovery.DatabaseActions = probeDiscovery.DatabaseActions
		discovery.LocalActions = discovery.LocalActions[:0]
		for _, action := range allLocal {
			if action.State != MutationRestored {
				discovery.LocalActions = append(
					discovery.LocalActions,
					action,
				)
			}
		}
	}
	if b.mutating && !readOnly {
		discovery.DatabaseActions, err = b.syncRestoredLocalMirrors(
			ctx,
			discovery.DatabaseActions,
			allLocal,
		)
		if err != nil {
			return discovery, err
		}
	}
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

func (b *databaseRestoreBackend) excludeLiveExecutionRuns(
	ctx context.Context,
	discovery RestoreDiscovery,
) (RestoreDiscovery, error) {
	runIDs := make([]string, 0, len(discovery.Runs))
	seen := make(map[string]bool)
	addRunID := func(runID string) {
		runID = strings.TrimSpace(runID)
		if runID == "" || seen[runID] {
			return
		}
		seen[runID] = true
		runIDs = append(runIDs, runID)
	}
	for _, run := range discovery.Runs {
		addRunID(run.RunID)
	}
	for _, action := range discovery.DatabaseActions {
		addRunID(action.RunID)
	}
	for _, action := range discovery.LocalActions {
		addRunID(action.RunID)
	}

	live := make(map[string]bool)
	leaseHeld := b.runExecutionLeaseHeld
	if leaseHeld == nil {
		leaseHeld = func(
			probeCtx context.Context,
			runID string,
		) (bool, error) {
			identity, err := runExecutionLockIdentity(b.cfg, runID)
			if err != nil {
				return false, err
			}
			return DatabaseRunLockHeld(probeCtx, b.db, identity)
		}
	}
	for _, runID := range runIDs {
		held, err := leaseHeld(ctx, runID)
		if err != nil {
			return discovery, fmt.Errorf(
				"check run %s execution lease: %w",
				runID,
				err,
			)
		}
		live[runID] = held
	}

	filtered := RestoreDiscovery{}
	for _, run := range discovery.Runs {
		if !live[run.RunID] {
			filtered.Runs = append(filtered.Runs, run)
		}
	}
	for _, action := range discovery.DatabaseActions {
		if !live[action.RunID] {
			filtered.DatabaseActions = append(
				filtered.DatabaseActions,
				action,
			)
		}
	}
	for _, action := range discovery.LocalActions {
		if !live[action.RunID] {
			filtered.LocalActions = append(filtered.LocalActions, action)
		}
	}
	return filtered, nil
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
		var scenarios string
		var startedAt time.Time
		err := b.db.Scan(
			ctx,
			"SELECT scenarios,started_at FROM "+quotedSchema+
				".meta_runs WHERE run_id=$1",
			[]any{action.RunID},
			&scenarios,
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
		codes, err := parseStoredScenarioCodes(scenarios)
		if err != nil {
			return nil, fmt.Errorf(
				"discover pending run %s scenarios: %w",
				action.RunID,
				err,
			)
		}
		runs = append(runs, RestoreRun{
			RunID:         action.RunID,
			StartedAt:     startedAt,
			ScenarioCodes: codes,
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
		var scenarios string
		var startedAt time.Time
		err := b.db.Scan(
			ctx,
			"SELECT scenarios,started_at FROM "+quotedSchema+
				".meta_runs WHERE run_id=$1",
			[]any{requested},
			&scenarios,
			&startedAt,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return []RestoreRun{{RunID: requested}}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("discover requested meta run: %w", err)
		}
		codes, err := parseStoredScenarioCodes(scenarios)
		if err != nil {
			return nil, fmt.Errorf(
				"discover requested meta run scenarios: %w",
				err,
			)
		}
		return []RestoreRun{{
			RunID:         requested,
			StartedAt:     startedAt,
			ScenarioCodes: codes,
		}}, nil
	}
	rows, err := b.db.Query(
		ctx,
		"SELECT run_id,scenarios,started_at FROM "+quotedSchema+
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
		var scenarios string
		if err := rows.Scan(
			&run.RunID,
			&scenarios,
			&run.StartedAt,
		); err != nil {
			return nil, fmt.Errorf("scan active meta run: %w", err)
		}
		codes, err := parseStoredScenarioCodes(scenarios)
		if err != nil {
			return nil, fmt.Errorf(
				"parse active meta run %s scenarios: %w",
				run.RunID,
				err,
			)
		}
		run.ScenarioCodes = codes
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func parseStoredScenarioCodes(value string) ([]ScenarioCode, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("stored scenario list is empty")
	}
	parts := strings.Split(value, ",")
	codes := make([]ScenarioCode, 0, len(parts))
	seen := make(map[ScenarioCode]bool, len(parts))
	for _, raw := range parts {
		part := strings.TrimSpace(raw)
		if part == "" {
			return nil, fmt.Errorf(
				"stored scenario list contains an empty item",
			)
		}
		number, err := strconv.ParseUint(part, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid stored scenario %q", part)
		}
		code := ScenarioCode(number)
		if _, err := DefaultScenarioCatalog().LookupCode(code); err != nil {
			return nil, err
		}
		if seen[code] {
			return nil, fmt.Errorf("duplicate stored scenario %03d", code)
		}
		seen[code] = true
		codes = append(codes, code)
	}
	return codes, nil
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
			return nil
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

func (b *databaseRestoreBackend) ReconcileRestoredActions(
	ctx context.Context,
	actions []Action,
) error {
	if b.actionStore == nil || b.executor == nil {
		return fmt.Errorf("restore action reconciliation was not initialized")
	}
	waitForDatabase := b.waitForDatabaseFn
	if waitForDatabase == nil {
		waitForDatabase = b.waitForDatabase
	}
	if err := waitForDatabase(ctx); err != nil {
		return err
	}
	var errs []error
	for _, action := range actions {
		if action.State == MutationRestored ||
			!actionSkipsPlannedInverseWhenRestored(action) {
			continue
		}
		if err := b.executor.VerifyRestored(ctx, action); err != nil {
			continue
		}
		if err := b.actionStore.SetActionState(
			ctx,
			action,
			MutationRestored,
			"",
		); err != nil {
			errs = append(errs, fmt.Errorf(
				"mark reconciled action %s/%d restored: %w",
				action.RunID,
				action.Sequence,
				err,
			))
			continue
		}
		if b.log != nil {
			b.log.Info(
				"restore reconcile target=%s status=RESTORED_AFTER_BASELINE",
				action.Target,
			)
		}
	}
	return errors.Join(errors.Join(errs...), b.actionStore.DrainErrors())
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
	if err := b.verifyPlanBaselineForActions(ctx, actions); err != nil {
		errs = append(errs, err)
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

func (b *databaseRestoreBackend) verifyPlanBaselineForActions(
	ctx context.Context,
	actions []Action,
) error {
	if !restorePlanVerificationRequired(
		b.cfg.Run.ValidationEnabled,
		actions,
	) {
		return nil
	}
	exists, err := b.planBaselineExists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	codes := make([]ScenarioCode, 0, len(actions))
	seen := make(map[ScenarioCode]struct{}, len(actions))
	for _, action := range actions {
		if !isPlanChangeCode(action.ScenarioCode) {
			continue
		}
		if _, ok := seen[action.ScenarioCode]; ok {
			continue
		}
		seen[action.ScenarioCode] = struct{}{}
		codes = append(codes, action.ScenarioCode)
	}
	if err := VerifyPlanBaselineScenarios(
		ctx,
		b.db,
		b.cfg.Data.Schema,
		codes,
	); err != nil {
		return fmt.Errorf("verify benchmark baseline: %w", err)
	}
	return nil
}

func restorePlanVerificationRequired(
	validationEnabled bool,
	actions []Action,
) bool {
	if !restoreActionsContainPlanChange(actions) {
		return false
	}
	if validationEnabled {
		return true
	}
	for _, action := range actions {
		if action.ScenarioCode == 602 {
			return true
		}
	}
	return false
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
	return commandRestoreOperation(
		ctx, db, cfg, log, runID, "restore", true, nil,
	)
}

func commandCleanup(ctx context.Context, db *Database, cfg BenchConfig, log *RunLog, runID string, withData bool) int {
	if withData && strings.TrimSpace(runID) != "" {
		log.Error("cleanup --data cannot be combined with --run-id")
		return 1
	}
	if !withData {
		if code := commandStop(ctx, db, cfg, log, runID); code != 0 {
			return code
		}
		log.Info("cleanup SUCCESS")
		return 0
	}
	if cfg.Run.DryRun {
		if code := commandRestoreOperation(
			ctx, db, cfg, log, runID, "stop", true, nil,
		); code != 0 {
			return code
		}
		if code := cleanupData(ctx, db, cfg, log); code != 0 {
			return code
		}
		log.Info("cleanup SUCCESS")
		return 0
	}
	// RestoreCoordinator invokes afterSuccess before releasing restore/local/
	// plan locks. Keeping ownership verification and DROP in this callback
	// closes the gap in which a new run could otherwise start.
	afterSuccess := func(
		callbackCtx context.Context,
		lock RestoreLock,
	) error {
		if err := ensureNoPlanWorkload(callbackCtx, db, cfg); err != nil {
			return err
		}
		return cleanupDataAfterRestore(callbackCtx, lock, cfg, log)
	}
	code := commandRestoreOperation(
		ctx, db, cfg, log, runID, "stop", true, afterSuccess,
	)
	if code != 0 {
		return code
	}
	log.Info("cleanup SUCCESS")
	return 0
}

func ensureNoPlanWorkload(
	ctx context.Context,
	db *Database,
	cfg BenchConfig,
) error {
	held, err := DatabaseRunLockHeld(
		ctx,
		db,
		planActivityLockIdentity(cfg),
	)
	if err != nil {
		return fmt.Errorf("inspect active plan workload: %w", err)
	}
	if held {
		return fmt.Errorf(
			"plan workload is running; stop its init process before cleanup --data",
		)
	}
	return nil
}

func cleanupDataAfterRestore(
	ctx context.Context,
	lock RestoreLock,
	cfg BenchConfig,
	log *RunLog,
) error {
	executor, ok := lock.(cleanupDatasetExecutor)
	if !ok {
		return fmt.Errorf(
			"protected restore lock does not expose its database session",
		)
	}
	if code := cleanupDataWithExecutor(ctx, executor, cfg, log); code != 0 {
		return fmt.Errorf("drop benchmark schema after restore")
	}
	return nil
}

func cleanupData(
	ctx context.Context,
	db *Database,
	cfg BenchConfig,
	log *RunLog,
) int {
	return cleanupDataWithExecutor(
		ctx,
		dbDatasetExecutor{db: db, schema: cfg.Data.Schema},
		cfg,
		log,
	)
}

type cleanupDatasetExecutor interface {
	DatasetVersion(context.Context, string) (string, error)
	Exec(context.Context, string, ...any) error
}

type datasetOwnershipCatalog interface {
	DatasetVersion(context.Context, string) (string, error)
}

func validateDatasetOwnership(
	ctx context.Context,
	catalog datasetOwnershipCatalog,
	schema string,
) error {
	if catalog == nil {
		return fmt.Errorf("dataset ownership catalog is unavailable")
	}
	version, err := catalog.DatasetVersion(ctx, schema)
	if err != nil {
		return fmt.Errorf("read dataset_version ownership marker: %w", err)
	}
	switch version {
	case "1", "2", "3", datasetVersion:
		return nil
	case "":
		return fmt.Errorf("missing gsbench dataset_version ownership marker")
	default:
		return fmt.Errorf(
			"unsupported gsbench dataset_version %q",
			version,
		)
	}
}

func cleanupDataWithExecutor(
	ctx context.Context,
	executor cleanupDatasetExecutor,
	cfg BenchConfig,
	log *RunLog,
) int {
	quotedSchema, ok := quoteDatasetSchema(cfg.Data.Schema)
	if !ok {
		log.Error("unsafe dataset schema %q", cfg.Data.Schema)
		return 1
	}
	if cfg.Run.DryRun {
		log.Info("DRY-RUN DROP SCHEMA %s CASCADE", quotedSchema)
		return 0
	}
	if executor == nil {
		log.Error("verify benchmark schema ownership: dataset executor is unavailable")
		return 1
	}
	if err := validateDatasetOwnership(
		ctx,
		executor,
		cfg.Data.Schema,
	); err != nil {
		log.Error(
			"refuse to drop schema=%s: %v",
			cfg.Data.Schema,
			err,
		)
		return 1
	}
	if err := executor.Exec(ctx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
		log.Error("drop benchmark schema: %v", err)
		return 1
	}
	log.Info(
		"removed schema=%s (not recoverable except by gsbench init)",
		cfg.Data.Schema,
	)
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
