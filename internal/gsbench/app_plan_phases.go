package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type planActionBackend interface {
	Lock(context.Context) (func() error, error)
	ResolveWorkload(context.Context, ScenarioCode) (planRunRecord, error)
	WorkloadAlive(context.Context) (bool, error)
	ResolveFault(context.Context, ScenarioCode) (planRunRecord, error)
	StartFault(context.Context, string, ScenarioCode) error
	ApplyFault(context.Context, string, ScenarioCode) error
	VerifyFault(context.Context, ScenarioCode) error
	MarkFaultActive(context.Context, string) error
	MarkFaultFailed(context.Context, string, error, bool) error
}

func executePlanFaultAction(
	ctx context.Context,
	code ScenarioCode,
	backend planActionBackend,
	newID func() string,
	reportWarning ...func(PrecheckWarning),
) (runID string, err error) {
	if backend == nil || newID == nil {
		return "", fmt.Errorf("plan fault backend and run ID generator are required")
	}
	release, err := backend.Lock(ctx)
	if err != nil {
		return "", err
	}
	if release == nil {
		return "", fmt.Errorf("plan fault control lock release is unavailable")
	}
	defer func() { err = errors.Join(err, release()) }()

	if _, err := backend.ResolveWorkload(ctx, code); err != nil {
		return "", fmt.Errorf("resolve plan workload: %w", err)
	}
	alive, err := backend.WorkloadAlive(ctx)
	if err != nil {
		return "", fmt.Errorf("inspect plan workload liveness: %w", err)
	}
	if !alive {
		return "", fmt.Errorf("plan workload for scenario %03d is not running", code)
	}
	if fault, faultErr := backend.ResolveFault(ctx, code); faultErr == nil {
		return "", fmt.Errorf("plan fault %s is already active", fault.RunID)
	} else if !errors.Is(faultErr, errPlanFaultNotFound) {
		return "", fmt.Errorf("resolve existing plan fault: %w", faultErr)
	}

	runID = newID()
	if err := backend.StartFault(ctx, runID, code); err != nil {
		return "", fmt.Errorf("record plan fault: %w", err)
	}
	if applyErr := backend.ApplyFault(ctx, runID, code); applyErr != nil {
		return runID, recordPlanFaultFailure(
			ctx,
			backend,
			runID,
			code,
			"apply plan fault",
			applyErr,
		)
	}
	if verifyErr := backend.VerifyFault(ctx, code); verifyErr != nil {
		warning := PrecheckWarning{
			ScenarioCode: code,
			Scenario:     DefaultScenarioCatalog().MustCode(code).Name,
			Check:        "fault_effect",
			Object:       "changed_plan",
			Actual:       verifyErr.Error(),
			Expected:     "expected_fault_plan_shape",
			Impact:       "fault_is_retained_for_manual_recovery",
		}
		if len(reportWarning) != 0 && reportWarning[0] != nil {
			reportWarning[0](warning)
		}
	}
	if err := backend.MarkFaultActive(ctx, runID); err != nil {
		return runID, recordPlanFaultFailure(
			ctx,
			backend,
			runID,
			code,
			"mark plan fault active",
			err,
		)
	}
	return runID, nil
}

func recordPlanFaultFailure(
	ctx context.Context,
	backend planActionBackend,
	runID string,
	code ScenarioCode,
	operation string,
	faultErr error,
) error {
	finalizeCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		30*time.Second,
	)
	defer cancel()
	markErr := backend.MarkFaultFailed(
		finalizeCtx,
		runID,
		faultErr,
		false,
	)
	return errors.Join(
		fmt.Errorf(
			"%s: %w; recovery remains pending, inspect with gsbench run %03d recover",
			operation,
			faultErr,
			code,
		),
		markErr,
	)
}

type databasePlanActionBackend struct {
	db      *Database
	cfg     BenchConfig
	log     *RunLog
	control *planControlStore
}

type planMaintenanceActionDatabase struct {
	db *Database
}

func (d planMaintenanceActionDatabase) Exec(
	ctx context.Context,
	query string,
	args ...any,
) (sql.Result, error) {
	return d.db.execMaintenance(ctx, query, args...)
}

func (d planMaintenanceActionDatabase) Scan(
	ctx context.Context,
	query string,
	args []any,
	dest ...any,
) error {
	return d.db.Scan(ctx, query, args, dest...)
}

func (d planMaintenanceActionDatabase) ExecSession(
	ctx context.Context,
	statements ...string,
) error {
	return d.db.ExecMaintenanceSession(ctx, statements...)
}

func planMaintenanceContext(
	parent context.Context,
	cfg BenchConfig,
) (context.Context, context.CancelFunc) {
	timeout := cfg.Safety.RestoreTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return context.WithTimeout(parent, timeout)
}

func planHeartbeatError(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil {
		return nil
	}
	return err
}

func acquirePlanFinalizationLock(
	ctx context.Context,
	acquire func(context.Context) (func() error, error),
) (func() error, error) {
	if acquire == nil {
		return nil, fmt.Errorf("plan finalization lock acquire is unavailable")
	}
	const retryInterval = 50 * time.Millisecond
	for {
		release, err := acquire(ctx)
		if err == nil {
			if release == nil {
				return nil, fmt.Errorf("plan finalization lock release is unavailable")
			}
			return release, nil
		}
		if !errors.Is(err, errDatabaseRunLockBusy) {
			return nil, err
		}
		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

func newPlanSQLJournalWithValidation(
	db *Database,
	schema string,
	validationEnabled bool,
) *Journal {
	store := newSQLJournalStore(databaseJournalDB{db: db}, schema)
	return NewJournalWithValidation(
		store,
		dbActionExecutor{db: planMaintenanceActionDatabase{db: db}},
		validationEnabled,
		db.journalTargetProduct(),
	)
}

func newDatabasePlanActionBackend(
	db *Database,
	cfg BenchConfig,
	log *RunLog,
) (*databasePlanActionBackend, error) {
	if db == nil {
		return nil, fmt.Errorf("plan action database is required")
	}
	control, err := newPlanControlStore(
		databaseJournalDB{db: db},
		cfg.Data.Schema,
	)
	if err != nil {
		return nil, err
	}
	return &databasePlanActionBackend{
		db: db, cfg: cfg, log: log, control: control,
	}, nil
}

func (b *databasePlanActionBackend) Lock(
	ctx context.Context,
) (func() error, error) {
	return AcquireDatabaseRunLock(
		ctx,
		b.db,
		planDatabaseLockIdentity(b.cfg),
	)
}

func (b *databasePlanActionBackend) ResolveWorkload(
	ctx context.Context,
	code ScenarioCode,
) (planRunRecord, error) {
	return b.control.ResolveWorkload(ctx, code)
}

func (b *databasePlanActionBackend) WorkloadAlive(
	ctx context.Context,
) (bool, error) {
	return DatabaseRunLockHeld(
		ctx,
		b.db,
		planActivityLockIdentity(b.cfg),
	)
}

func (b *databasePlanActionBackend) ResolveFault(
	ctx context.Context,
	code ScenarioCode,
) (planRunRecord, error) {
	return b.control.ResolveFault(ctx, code)
}

func (b *databasePlanActionBackend) StartFault(
	ctx context.Context,
	runID string,
	code ScenarioCode,
) error {
	for candidate := ScenarioCode(601); candidate <= 606; candidate++ {
		fault, err := b.control.ResolveFault(ctx, candidate)
		if err == nil {
			return fmt.Errorf(
				"plan fault %s for scenario %03d is already active",
				fault.RunID,
				fault.Code,
			)
		}
		if !errors.Is(err, errPlanFaultNotFound) {
			return err
		}
	}
	return b.control.StartFault(ctx, runID, code)
}

func (b *databasePlanActionBackend) ApplyFault(
	ctx context.Context,
	runID string,
	code ScenarioCode,
) error {
	operationCtx, cancel := planMaintenanceContext(ctx, b.cfg)
	defer cancel()
	definition, err := planScenarioDefinitionForCode(b.cfg.Data.Schema, code)
	if err != nil {
		return err
	}
	mutations, err := PlanMutationSet(
		runID,
		b.cfg.Data.Schema,
		definition.Name,
	)
	if err != nil {
		return err
	}
	journal := newPlanSQLJournalWithValidation(
		b.db,
		b.cfg.Data.Schema,
		b.cfg.Run.ValidationEnabled,
	)
	for _, mutation := range mutations {
		if err := journal.Apply(operationCtx, mutation); err != nil {
			return err
		}
	}
	return nil
}

func (b *databasePlanActionBackend) VerifyFault(
	ctx context.Context,
	code ScenarioCode,
) error {
	if code != 602 {
		return nil
	}
	operationCtx, cancel := planMaintenanceContext(ctx, b.cfg)
	defer cancel()
	definition, err := planScenarioDefinitionForCode(
		b.cfg.Data.Schema,
		code,
	)
	if err != nil {
		return err
	}
	return verifyPlanFaultPlans(
		operationCtx,
		databasePlanBaselineExplainer{db: b.db},
		definition,
	)
}

func (b *databasePlanActionBackend) MarkFaultActive(
	ctx context.Context,
	runID string,
) error {
	return b.control.SetFaultPhase(
		ctx,
		runID,
		PhaseHold,
		"three_phase fault active",
	)
}

func (b *databasePlanActionBackend) MarkFaultFailed(
	ctx context.Context,
	runID string,
	faultErr error,
	restored bool,
) error {
	return b.control.MarkFaultFailed(ctx, runID, faultErr, restored)
}

func planScenarioDefinitionForCode(
	schema string,
	code ScenarioCode,
) (PlanScenarioDefinition, error) {
	definitions, err := PlanScenarioDefinitions(schema)
	if err != nil {
		return PlanScenarioDefinition{}, err
	}
	for _, definition := range definitions {
		if definition.Code == code {
			return definition, nil
		}
	}
	return PlanScenarioDefinition{}, fmt.Errorf(
		"plan scenario %03d is unavailable",
		code,
	)
}

func commandPlanRunAction(
	ctx context.Context,
	db *Database,
	cfg BenchConfig,
	environment Environment,
	caps Capabilities,
	log *RunLog,
	options CLIOptions,
	runID string,
) int {
	if len(cfg.Run.ScenarioCodes) != 1 ||
		!isPlanChangeCode(cfg.Run.ScenarioCodes[0]) {
		log.Error("plan action requires exactly one scenario from 601-606")
		return 1
	}
	code := cfg.Run.ScenarioCodes[0]
	definition, err := planScenarioDefinitionForCode(cfg.Data.Schema, code)
	if err != nil {
		log.Error("resolve plan scenario: %v", err)
		return 1
	}
	if err := validatePlanCapability(definition.Name, caps); err != nil {
		log.Warn("%s", (PrecheckWarning{
			ScenarioCode: definition.Code,
			Scenario:     definition.Name,
			Check:        "capability",
			Object:       "plan_scenario",
			Actual:       err.Error(),
			Expected:     "catalog_capability_available",
			Impact:       "plan_action_will_attempt_execution",
		}).LogLine())
	}
	if options.PlanAction == PlanRunRecover {
		return commandRecoveryPlan(ctx, db, cfg, log, "", &code)
	}
	backend, err := newDatabasePlanActionBackend(db, cfg, log)
	if err != nil {
		log.Error("initialize plan action: %v", err)
		return 1
	}
	switch options.PlanAction {
	case PlanRunInit:
		return runPlanInit(
			ctx,
			db,
			cfg,
			environment,
			caps,
			log,
			backend,
			definition,
			options.PlanWorkers,
			runID,
		)
	case PlanRunFault:
		faultRunID, err := executePlanFaultAction(
			ctx,
			code,
			backend,
			newRunID,
			func(warning PrecheckWarning) {
				log.Warn("%s", warning.LogLine())
			},
		)
		if err != nil {
			log.Error("plan fault scenario=%03d run_id=%s: %v", code, faultRunID, err)
			return 1
		}
		log.Info("plan fault SUCCESS scenario=%03d fault_run_id=%s", code, faultRunID)
		return 0
	default:
		log.Error("unknown plan action %q", options.PlanAction)
		return 1
	}
}

func runPlanInit(
	ctx context.Context,
	db *Database,
	cfg BenchConfig,
	environment Environment,
	caps Capabilities,
	log *RunLog,
	backend *databasePlanActionBackend,
	definition PlanScenarioDefinition,
	workers int,
	runID string,
) int {
	if workers <= 0 {
		log.Error("plan init workers must be positive")
		return 1
	}
	if cfg.Run.Duration <= 0 {
		log.Error("plan init duration must be positive")
		return 1
	}

	releaseControl, err := backend.Lock(ctx)
	if err != nil {
		log.Error("acquire plan control lock: %v", err)
		return 1
	}
	controlHeld := true
	releaseControlNow := func() error {
		if !controlHeld {
			return nil
		}
		controlHeld = false
		return releaseControl()
	}
	defer func() { _ = releaseControlNow() }()

	if active, activeErr := backend.control.ResolveAnyWorkload(ctx); activeErr == nil {
		alive, liveErr := backend.WorkloadAlive(ctx)
		if liveErr != nil {
			log.Error("inspect active plan workload: %v", liveErr)
			return 1
		}
		if alive {
			log.Warn(
				"plan workload %s scenario=%03d is already running",
				active.RunID,
				active.Code,
			)
		} else {
			log.Warn(
				"stale plan workload %s scenario=%03d action=report_only",
				active.RunID,
				active.Code,
			)
			if markErr := backend.control.MarkWorkloadsStale(ctx); markErr != nil {
				log.Warn("mark stale plan workloads report-only: %v", markErr)
			}
		}
	} else if !errors.Is(activeErr, errPlanWorkloadNotFound) {
		log.Error("resolve active plan workload: %v", activeErr)
		return 1
	}
	for candidate := ScenarioCode(601); candidate <= 606; candidate++ {
		if fault, faultErr := backend.ResolveFault(ctx, candidate); faultErr == nil {
			log.Warn(
				"plan fault %s scenario=%03d remains active; review recovery SQL: "+
					"gsbench run %03d recover",
				fault.RunID,
				fault.Code,
				fault.Code,
			)
		} else if !errors.Is(faultErr, errPlanFaultNotFound) {
			log.Warn("inspect active plan fault scenario=%03d: %v", candidate, faultErr)
		}
	}
	prepareCtx, cancelPrepare := planMaintenanceContext(ctx, cfg)
	prepareErr := preparePlanRunBaseline(
		prepareCtx,
		db,
		cfg,
		log,
		nil,
		VerifyPlanBaseline,
	)
	if definition.Code == 602 {
		if err := VerifyPlanBaselineScenarios(
			prepareCtx,
			db,
			cfg.Data.Schema,
			[]ScenarioCode{definition.Code},
		); err != nil {
			log.Warn("%s", (PrecheckWarning{
				ScenarioCode: definition.Code,
				Scenario:     definition.Name,
				Check:        "plan_baseline",
				Object:       "scenario_baseline",
				Actual:       err.Error(),
				Expected:     "expected_baseline_plan",
				Impact:       "plan_workload_will_still_start",
			}).LogLine())
		}
	}
	cancelPrepare()
	if prepareErr != nil {
		log.Error("prepare plan baseline: %v", prepareErr)
		return 1
	}
	releaseActivity, err := AcquireDatabaseRunLock(
		ctx,
		db,
		planActivityLockIdentity(cfg),
	)
	if err != nil {
		log.Error("acquire plan workload lock: %v", err)
		return 1
	}
	activityHeld := true
	releaseActivityNow := func() error {
		if !activityHeld {
			return nil
		}
		activityHeld = false
		return releaseActivity()
	}
	defer func() { _ = releaseActivityNow() }()
	if err := backend.control.StartWorkload(
		ctx,
		runID,
		definition.Code,
		workers,
	); err != nil {
		log.Error("record plan workload: %v", err)
		return 1
	}
	if err := releaseControlNow(); err != nil {
		log.Error("release plan control lock: %v", err)
		return 1
	}

	for index, sqlText := range definition.Candidates {
		plan, err := explainLiteral(ctx, db, sqlText)
		if err != nil {
			log.Warn("baseline plan candidate=%d unavailable: %v", index+1, err)
			continue
		}
		log.Info(
			"plan baseline scenario=%03d candidate=%d sql=%s plan=%s",
			definition.Code,
			index+1,
			sqlText,
			plan,
		)
	}
	runtime := &Runtime{
		Config: cfg, Database: db, Capabilities: caps,
		Environment: environment, Catalog: DefaultScenarioCatalog(),
		Log: log, RunID: runID,
	}
	trafficCtx, cancelTraffic := context.WithCancel(ctx)
	traffic, err := newPlanTraffic(
		trafficCtx,
		runtime,
		definition,
		workers,
	)
	if err != nil {
		cancelTraffic()
		log.Error("create plan traffic: %v", err)
		return 1
	}
	heartbeatErr := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	heartbeatStarted := false
	announceReady := func(readyCtx context.Context) error {
		if err := backend.control.MarkWorkloadRunning(readyCtx, runID); err != nil {
			return err
		}
		heartbeatStarted = true
		go func() {
			defer close(heartbeatDone)
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-trafficCtx.Done():
					return
				case <-ticker.C:
					if err := planHeartbeatError(
						trafficCtx,
						backend.control.HeartbeatWorkload(
							trafficCtx,
							runID,
						),
					); err != nil {
						select {
						case heartbeatErr <- err:
						default:
						}
						cancelTraffic()
						return
					}
				}
			}
		}()
		log.Info(
			"plan init RUNNING scenario=%03d workload_run_id=%s workers=%d "+
				"duration=%s recover_command=%q",
			definition.Code,
			runID,
			workers,
			cfg.Run.Duration,
			fmt.Sprintf("gsbench run %03d recover", definition.Code),
		)
		return nil
	}
	snapshot, trafficErr := traffic.RunWithReady(
		trafficCtx,
		cfg.Run.Duration,
		announceReady,
	)
	cancelTraffic()
	if heartbeatStarted {
		<-heartbeatDone
	}
	select {
	case err := <-heartbeatErr:
		trafficErr = errors.Join(trafficErr, fmt.Errorf("plan workload heartbeat: %w", err))
	default:
	}

	finishOutcome := OutcomeUnverified
	finishDetail := fmt.Sprintf(
		"duration complete workers=%d operations=%d errors=%d",
		workers,
		snapshot.Operations,
		snapshot.Errors,
	)
	if trafficErr != nil {
		finishOutcome = OutcomeFailed
		finishDetail = journalSafeErrorText(trafficErr.Error())
	}
	finishCtx, cancelFinish := context.WithTimeout(
		context.Background(),
		maximumRestoreFinalizationTimeout,
	)
	defer cancelFinish()
	finishRelease, lockErr := acquirePlanFinalizationLock(
		finishCtx,
		backend.Lock,
	)
	if lockErr == nil {
		lockErr = backend.control.FinishWorkload(
			finishCtx,
			runID,
			finishOutcome,
			finishDetail,
		)
		lockErr = errors.Join(lockErr, finishRelease())
	}
	if releaseErr := releaseActivityNow(); releaseErr != nil {
		lockErr = errors.Join(lockErr, releaseErr)
	}
	if lockErr != nil {
		log.Error("finalize plan workload: %v", lockErr)
		return 1
	}
	if trafficErr != nil {
		log.Error("plan init scenario=%03d: %v", definition.Code, trafficErr)
		return 1
	}
	log.Info(
		"plan init SUCCESS scenario=%03d workers=%d operations=%d duration=%s",
		definition.Code,
		workers,
		snapshot.Operations,
		cfg.Run.Duration,
	)
	return 0
}

func strictPlanInitVerificationRequired(
	validationEnabled bool,
	code ScenarioCode,
) bool {
	return !validationEnabled && code == 602
}
