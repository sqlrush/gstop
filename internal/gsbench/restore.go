package gsbench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type RestoreRequest struct {
	RunID  string
	DryRun bool

	afterSuccess     func(context.Context, RestoreLock) error
	completedOutcome Outcome
}

type RestoreRun struct {
	RunID     string
	StartedAt time.Time
}

type RestoreDiscovery struct {
	Runs            []RestoreRun
	DatabaseActions []Action
	LocalActions    []Action
}

type RestoreLock interface {
	Release() error
}

type localRestoreLock struct {
	once       sync.Once
	process    *sync.Mutex
	parent     int
	descriptor int
	err        error
}

func acquireLocalRestoreLock(
	ctx context.Context,
	recoveryLedgerPath string,
) (RestoreLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	safePath, err := safeRecoveryLedgerPath(recoveryLedgerPath)
	if err != nil {
		return nil, fmt.Errorf("resolve restore lock identity: %w", err)
	}
	lockIdentity := strings.TrimSuffix(safePath, filepath.Ext(safePath)) +
		"_restore.json"
	process := recoveryLedgerMutex(lockIdentity)
	if !process.TryLock() {
		return nil, fmt.Errorf(
			"restore is busy for config identity %q",
			filepath.Base(safePath),
		)
	}
	releaseProcess := true
	defer func() {
		if releaseProcess {
			process.Unlock()
		}
	}()

	ledger := &fileRecoveryLedger{
		path:          lockIdentity,
		syncDirectory: syncRecoveryLedgerDirectory,
	}
	parent, err := ledger.openPinnedParent(lockIdentity, true)
	if err != nil {
		return nil, fmt.Errorf("open restore lock parent: %w", err)
	}
	closeParent := true
	defer func() {
		if closeParent {
			_ = unix.Close(parent.descriptor)
		}
	}()
	descriptor, err := ledger.openTrustedLock(parent)
	if err != nil {
		return nil, fmt.Errorf("open restore lock file: %w", err)
	}
	closeDescriptor := true
	defer func() {
		if closeDescriptor {
			_ = unix.Close(descriptor)
		}
	}()
	if err := unix.Flock(descriptor, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) ||
			errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf(
				"restore is busy for config identity %q",
				filepath.Base(safePath),
			)
		}
		return nil, fmt.Errorf("lock restore file: %w", err)
	}
	if err := verifyPinnedLockName(parent, descriptor); err != nil {
		_ = unix.Flock(descriptor, unix.LOCK_UN)
		return nil, fmt.Errorf("verify restore lock file: %w", err)
	}

	releaseProcess = false
	closeParent = false
	closeDescriptor = false
	return &localRestoreLock{
		process: process, parent: parent.descriptor, descriptor: descriptor,
	}, nil
}

func (l *localRestoreLock) Release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		var errs []error
		if l.descriptor >= 0 {
			if err := unix.Flock(l.descriptor, unix.LOCK_UN); err != nil {
				errs = append(errs, fmt.Errorf("unlock restore file: %w", err))
			}
			if err := unix.Close(l.descriptor); err != nil {
				errs = append(errs, fmt.Errorf("close restore lock file: %w", err))
			}
			l.descriptor = -1
		}
		if l.parent >= 0 {
			if err := unix.Close(l.parent); err != nil {
				errs = append(errs, fmt.Errorf(
					"close restore lock parent: %w", err,
				))
			}
			l.parent = -1
		}
		if l.process != nil {
			l.process.Unlock()
			l.process = nil
		}
		l.err = errors.Join(errs...)
	})
	return l.err
}

type RestoreCoordinatorBackend interface {
	AcquireRestoreLock(context.Context) (RestoreLock, error)
	DiscoverRestore(context.Context, string, bool) (RestoreDiscovery, error)
	MarkRestoreRequested(context.Context, string) error
	StopTaggedSessions(context.Context, string) error
	RestoreActionGroup(context.Context, []Action) error
	RepairBaseline(context.Context) error
	RedetectTopology(context.Context) error
	VerifyRestore(context.Context, []string, []Action) error
	MarkRestoreOutcome(context.Context, string, Outcome) error
}

type restoreOwnershipValidator interface {
	ValidateRestoreOwnership(context.Context) error
}

type restoreDeadlineBackend interface {
	RestoreTimeout() time.Duration
}

type restoreFinalizationDeadlineBackend interface {
	RestoreFinalizationTimeout() time.Duration
}

type restoreActionReconciliationBackend interface {
	ReconcileRestoredActions(context.Context, []Action) error
}

const maximumRestoreFinalizationTimeout = 5 * time.Second

type RestoreCoordinator struct {
	backend           RestoreCoordinatorBackend
	validationEnabled bool
}

type RestoreSummary struct {
	RunIDs         []string
	PlannedActions []Action
	Outcome        Outcome
	Failed         bool
	Err            error
}

func NewRestoreCoordinator(backend RestoreCoordinatorBackend) *RestoreCoordinator {
	return NewRestoreCoordinatorWithValidation(backend, true)
}

func NewRestoreCoordinatorWithValidation(
	backend RestoreCoordinatorBackend,
	validationEnabled bool,
) *RestoreCoordinator {
	return &RestoreCoordinator{
		backend:           backend,
		validationEnabled: validationEnabled,
	}
}

func (r *RestoreCoordinator) Restore(
	ctx context.Context,
	request RestoreRequest,
) RestoreSummary {
	if r == nil || r.backend == nil {
		err := fmt.Errorf("restore coordinator backend is required")
		return failedRestoreSummary(nil, nil, err)
	}
	request.RunID = strings.TrimSpace(request.RunID)
	if request.RunID != "" && !tagComponentRE.MatchString(request.RunID) {
		err := fmt.Errorf("unsafe restore run ID %q", request.RunID)
		return failedRestoreSummary(nil, nil, err)
	}

	if request.DryRun {
		discovery, err := r.backend.DiscoverRestore(
			ctx,
			request.RunID,
			true,
		)
		runs, actions, mergeErr := prepareRestorePlan(discovery, request.RunID)
		err = errors.Join(err, mergeErr)
		if err != nil {
			return failedRestoreSummary(runs, actions, err)
		}
		return RestoreSummary{
			RunIDs:         runs,
			PlannedActions: actions,
			Outcome:        OutcomeSuccess,
		}
	}

	cancel := func() {}
	if deadlineBackend, ok := r.backend.(restoreDeadlineBackend); ok {
		if maximum := deadlineBackend.RestoreTimeout(); maximum > 0 {
			ctx, cancel = context.WithTimeout(ctx, maximum)
		}
	}
	defer cancel()

	lock, err := r.backend.AcquireRestoreLock(ctx)
	if err != nil {
		return failedRestoreSummary(
			nil, nil, fmt.Errorf("acquire restore mutex: %w", err),
		)
	}
	if lock == nil {
		return failedRestoreSummary(
			nil, nil, fmt.Errorf("acquire restore mutex: no lock returned"),
		)
	}
	if validator, ok := r.backend.(restoreOwnershipValidator); ok {
		if err := validator.ValidateRestoreOwnership(ctx); err != nil {
			releaseErr := lock.Release()
			return failedRestoreSummary(
				nil,
				nil,
				errors.Join(
					fmt.Errorf("validate restore schema ownership: %w", err),
					wrapRestoreError("release restore mutex", releaseErr),
				),
			)
		}
	}

	discovery, discoverErr := r.backend.DiscoverRestore(
		ctx,
		request.RunID,
		false,
	)
	runs, actions, planErr := prepareRestorePlan(discovery, request.RunID)
	if err := errors.Join(discoverErr, planErr); err != nil {
		releaseErr := lock.Release()
		return failedRestoreSummary(
			runs,
			actions,
			errors.Join(err, wrapRestoreError("release restore mutex", releaseErr)),
		)
	}

	var errs []error
	var actionErrs []error
	for _, runID := range runs {
		if err := r.backend.MarkRestoreRequested(ctx, runID); err != nil {
			errs = append(errs, fmt.Errorf(
				"mark run %s restore requested: %w", runID, err,
			))
		}
	}
	for _, runID := range runs {
		if err := r.backend.StopTaggedSessions(ctx, runID); err != nil {
			errs = append(errs, fmt.Errorf(
				"stop tagged sessions for run %s: %w", runID, err,
			))
		}
	}

	for stage := restoreStageSessions; stage <= restoreStageDatabase; stage++ {
		for _, runID := range runs {
			group := restoreActionGroup(actions, runID, stage)
			if len(group) == 0 {
				continue
			}
			if err := r.backend.RestoreActionGroup(ctx, group); err != nil {
				actionErrs = append(actionErrs, fmt.Errorf(
					"restore run %s stage %s: %w",
					runID, stage, err,
				))
			}
		}
	}

	if restoreActionsContainPlanChange(actions) {
		if err := r.backend.RepairBaseline(ctx); err != nil {
			errs = append(errs, fmt.Errorf("repair benchmark baseline: %w", err))
		}
	}
	actionErrorsReconciled := false
	if len(actionErrs) != 0 {
		if reconciler, ok := r.backend.(restoreActionReconciliationBackend); ok {
			if err := reconciler.ReconcileRestoredActions(ctx, actions); err != nil {
				errs = append(errs, fmt.Errorf(
					"reconcile restored actions: %w",
					err,
				))
			} else {
				actionErrorsReconciled = true
			}
		}
	}
	if err := r.backend.RedetectTopology(ctx); err != nil {
		errs = append(errs, fmt.Errorf("re-detect topology: %w", err))
	}
	verifyErr := r.backend.VerifyRestore(ctx, runs, actions)
	if verifyErr != nil {
		errs = append(errs, actionErrs...)
		errs = append(errs, fmt.Errorf("verify restored state: %w", verifyErr))
	} else if !actionErrorsReconciled {
		errs = append(errs, actionErrs...)
	}

	outcome := OutcomeSuccess
	if len(errs) != 0 {
		outcome = OutcomeRestoreFailed
	}
	persistedOutcome := outcome
	if outcome == OutcomeSuccess {
		persistedOutcome = successfulRestoreRunOutcome(request)
	}
	finalizationTimeout := maximumRestoreFinalizationTimeout
	if deadlineBackend, ok := r.backend.(restoreFinalizationDeadlineBackend); ok {
		if configured := deadlineBackend.RestoreFinalizationTimeout(); configured > 0 && configured < finalizationTimeout {
			finalizationTimeout = configured
		}
	}
	finalizationCtx, cancelFinalization := context.WithTimeout(
		context.WithoutCancel(ctx),
		finalizationTimeout,
	)
	errs = append(
		errs,
		r.markTerminalRestoreOutcomes(
			finalizationCtx,
			runs,
			persistedOutcome,
		)...,
	)
	cancelFinalization()
	if len(errs) == 0 && request.afterSuccess != nil {
		if err := request.afterSuccess(ctx, lock); err != nil {
			errs = append(errs, fmt.Errorf(
				"complete restore-protected operation: %w",
				err,
			))
		}
	}
	if err := lock.Release(); err != nil {
		errs = append(errs, fmt.Errorf("release restore mutex: %w", err))
	}

	err = errors.Join(errs...)
	if err != nil {
		outcome = OutcomeRestoreFailed
	}
	return RestoreSummary{
		RunIDs:         runs,
		PlannedActions: actions,
		Outcome:        outcome,
		Failed:         err != nil,
		Err:            err,
	}
}

func restoreActionsContainPlanChange(actions []Action) bool {
	for _, action := range actions {
		if isPlanChangeCode(action.ScenarioCode) {
			return true
		}
	}
	return false
}

func (r *RestoreCoordinator) markTerminalRestoreOutcomes(
	ctx context.Context,
	runs []string,
	initialOutcome Outcome,
) []error {
	outcome := initialOutcome
	var errs []error
	var reconcile []string
	for _, runID := range runs {
		attemptedOutcome := outcome
		err := r.backend.MarkRestoreOutcome(ctx, runID, attemptedOutcome)
		if attemptedOutcome != OutcomeRestoreFailed {
			reconcile = append(reconcile, runID)
			if err != nil {
				outcome = OutcomeRestoreFailed
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"mark run %s outcome %s: %w",
				runID,
				attemptedOutcome,
				err,
			))
		}
	}
	if outcome != OutcomeRestoreFailed ||
		initialOutcome == OutcomeRestoreFailed {
		return errs
	}
	for _, runID := range reconcile {
		if err := r.backend.MarkRestoreOutcome(
			ctx,
			runID,
			OutcomeRestoreFailed,
		); err != nil {
			errs = append(errs, fmt.Errorf(
				"reconcile run %s outcome %s: %w",
				runID,
				OutcomeRestoreFailed,
				err,
			))
		}
	}
	return errs
}

func successfulRestoreRunOutcome(request RestoreRequest) Outcome {
	switch request.completedOutcome {
	case OutcomeSuccess,
		OutcomeNotApplicable,
		OutcomeDegraded,
		OutcomeNotImplemented,
		OutcomeFailed:
		return request.completedOutcome
	default:
		return OutcomeSuccess
	}
}

func failedRestoreSummary(
	runs []string,
	actions []Action,
	err error,
) RestoreSummary {
	return RestoreSummary{
		RunIDs:         runs,
		PlannedActions: actions,
		Outcome:        OutcomeRestoreFailed,
		Failed:         true,
		Err:            err,
	}
}

func wrapRestoreError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

type restoreStage int

const (
	restoreStageSessions restoreStage = iota
	restoreStageExternalControl
	restoreStageConfiguration
	restoreStageDatabase
)

func (stage restoreStage) String() string {
	switch stage {
	case restoreStageSessions:
		return "sessions"
	case restoreStageExternalControl:
		return "external"
	case restoreStageConfiguration:
		return "configuration"
	case restoreStageDatabase:
		return "database"
	default:
		return "unknown"
	}
}

func prepareRestorePlan(
	discovery RestoreDiscovery,
	requestedRunID string,
) ([]string, []Action, error) {
	runByID := make(map[string]RestoreRun)
	for _, run := range discovery.Runs {
		run.RunID = strings.TrimSpace(run.RunID)
		if run.RunID == "" {
			continue
		}
		if requestedRunID != "" && run.RunID != requestedRunID {
			continue
		}
		existing, ok := runByID[run.RunID]
		if !ok || run.StartedAt.After(existing.StartedAt) {
			runByID[run.RunID] = run
		}
	}

	actions, err := mergeRestoreActions(
		discovery.DatabaseActions,
		discovery.LocalActions,
		requestedRunID,
	)
	for _, action := range actions {
		if _, ok := runByID[action.RunID]; !ok {
			runByID[action.RunID] = RestoreRun{RunID: action.RunID}
		}
	}
	if requestedRunID != "" {
		if _, ok := runByID[requestedRunID]; !ok {
			runByID[requestedRunID] = RestoreRun{RunID: requestedRunID}
		}
	}

	runs := make([]RestoreRun, 0, len(runByID))
	for _, run := range runByID {
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool {
		if !runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].StartedAt.After(runs[j].StartedAt)
		}
		return runs[i].RunID > runs[j].RunID
	})
	runIDs := make([]string, len(runs))
	runOrder := make(map[string]int, len(runs))
	for index, run := range runs {
		runIDs[index] = run.RunID
		runOrder[run.RunID] = index
	}
	sort.SliceStable(actions, func(i, j int) bool {
		leftStage, leftPriority := restoreActionOrder(actions[i])
		rightStage, rightPriority := restoreActionOrder(actions[j])
		if leftStage != rightStage {
			return leftStage < rightStage
		}
		if runOrder[actions[i].RunID] != runOrder[actions[j].RunID] {
			return runOrder[actions[i].RunID] < runOrder[actions[j].RunID]
		}
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		if actions[i].Sequence != actions[j].Sequence {
			return actions[i].Sequence > actions[j].Sequence
		}
		if actions[i].Target != actions[j].Target {
			return actions[i].Target < actions[j].Target
		}
		return actions[i].Kind < actions[j].Kind
	})
	return runIDs, actions, err
}

type restoreActionIdentity struct {
	runID    string
	sequence int64
	kind     ActionKind
	target   string
}

type restoreActionStateIdentity struct {
	runID    string
	sequence int64
}

type coordinatorActionSource struct {
	action Action
	db     bool
	local  bool
}

type coordinatorActionStore struct {
	database ActionStore
	ledger   RecoveryLedger
	sources  map[restoreActionIdentity]*coordinatorActionSource
	mu       sync.Mutex
	deferred []error
}

func newCoordinatorActionStore(
	database ActionStore,
	ledger RecoveryLedger,
	databaseActions []Action,
	localActions []Action,
) *coordinatorActionStore {
	store := &coordinatorActionStore{
		database: database,
		ledger:   ledger,
		sources:  make(map[restoreActionIdentity]*coordinatorActionSource),
	}
	register := func(action Action, database, local bool) {
		identity := restoreIdentity(action)
		source := store.sources[identity]
		if source == nil {
			source = &coordinatorActionSource{action: action}
			store.sources[identity] = source
		}
		source.db = source.db || database
		source.local = source.local || local
		if local {
			source.action = action
		}
	}
	for _, action := range databaseActions {
		register(action, true, false)
	}
	for _, action := range localActions {
		register(action, false, true)
	}
	return store
}

func (s *coordinatorActionStore) InsertPlanned(
	context.Context,
	Action,
) (Action, error) {
	return Action{}, fmt.Errorf(
		"restore coordinator store cannot insert forward actions",
	)
}

func (s *coordinatorActionStore) SetState(
	ctx context.Context,
	runID string,
	sequence int64,
	state MutationState,
	detail string,
) error {
	var matchedIdentity restoreActionIdentity
	var source *coordinatorActionSource
	for identity, candidate := range s.sources {
		if identity.runID != runID || identity.sequence != sequence {
			continue
		}
		if source != nil {
			return fmt.Errorf(
				"restore action source %s/%d is ambiguous",
				runID,
				sequence,
			)
		}
		matchedIdentity = identity
		source = candidate
	}
	if source == nil {
		return fmt.Errorf(
			"restore action source %s/%d is not registered",
			runID,
			sequence,
		)
	}
	return s.setSourceState(ctx, matchedIdentity, source, state, detail)
}

func (s *coordinatorActionStore) SetActionState(
	ctx context.Context,
	action Action,
	state MutationState,
	detail string,
) error {
	identity := restoreIdentity(action)
	source := s.sources[identity]
	if source == nil {
		return fmt.Errorf(
			"restore action source %s/%d/%s/%s is not registered",
			action.RunID,
			action.Sequence,
			action.Kind,
			action.Target,
		)
	}
	return s.setSourceState(ctx, identity, source, state, detail)
}

func (s *coordinatorActionStore) ClaimAction(
	ctx context.Context,
	action Action,
) (bool, error) {
	identity := restoreIdentity(action)
	source := s.sources[identity]
	if source == nil {
		return false, fmt.Errorf(
			"restore action source %s/%d/%s/%s is not registered",
			action.RunID,
			action.Sequence,
			action.Kind,
			action.Target,
		)
	}
	var databaseErr error
	databaseClaimed := !source.db
	if source.db {
		if s.database == nil {
			databaseErr = fmt.Errorf(
				"database journal store is unavailable for %s/%d",
				identity.runID,
				identity.sequence,
			)
		} else if claimer, ok := s.database.(actionClaimStore); ok {
			databaseClaimed, databaseErr = claimer.ClaimAction(ctx, action)
		} else {
			databaseErr = s.database.SetState(
				ctx,
				identity.runID,
				identity.sequence,
				MutationRestoring,
				"",
			)
			databaseClaimed = databaseErr == nil
		}
		if databaseErr != nil {
			databaseErr = fmt.Errorf(
				"claim database action mirror: %w",
				databaseErr,
			)
		}
		if databaseErr == nil && !databaseClaimed {
			return false, nil
		}
	}

	var localErr error
	if source.local {
		if s.ledger == nil {
			localErr = fmt.Errorf(
				"local recovery ledger is unavailable for %s/%d",
				identity.runID,
				identity.sequence,
			)
		} else {
			next := source.action
			next.State = MutationRestoring
			next.LastError = ""
			localErr = s.ledger.Put(ctx, next)
			if localErr == nil {
				source.action = next
			} else {
				localErr = fmt.Errorf(
					"claim local action mirror: %w",
					localErr,
				)
			}
		}
	}
	if databaseErr != nil && source.local && localErr == nil {
		s.mu.Lock()
		s.deferred = append(s.deferred, databaseErr)
		s.mu.Unlock()
		databaseErr = nil
		databaseClaimed = true
	}
	if err := errors.Join(databaseErr, localErr); err != nil {
		return false, err
	}
	return databaseClaimed, nil
}

func (s *coordinatorActionStore) setSourceState(
	ctx context.Context,
	identity restoreActionIdentity,
	source *coordinatorActionSource,
	state MutationState,
	detail string,
) error {
	var databaseErr error
	if source.db {
		if s.database == nil {
			databaseErr = fmt.Errorf(
				"database journal store is unavailable for %s/%d",
				identity.runID,
				identity.sequence,
			)
		} else if err := s.database.SetState(
			ctx, identity.runID, identity.sequence, state, detail,
		); err != nil {
			databaseErr = fmt.Errorf("update database action mirror: %w", err)
		}
	}
	var localErr error
	if source.local {
		if s.ledger == nil {
			localErr = fmt.Errorf(
				"local recovery ledger is unavailable for %s/%d",
				identity.runID,
				identity.sequence,
			)
		} else if state == MutationRestored {
			if err := s.ledger.MarkRestored(
				ctx, source.action.RunID, source.action.Target,
			); err != nil {
				localErr = fmt.Errorf("update local action mirror: %w", err)
			}
		} else {
			next := source.action
			next.State = state
			next.LastError = journalSafeErrorText(detail)
			if err := s.ledger.Put(ctx, next); err != nil {
				localErr = fmt.Errorf("update local action mirror: %w", err)
			} else {
				source.action = next
			}
		}
	}
	// The local ledger exists specifically so an external inverse can run
	// while its database mirror is unreachable. Preserve the database error
	// for the coordinator outcome, but do not block the provider once the
	// local state transition is durable.
	if databaseErr != nil && source.local && localErr == nil {
		s.mu.Lock()
		s.deferred = append(s.deferred, databaseErr)
		s.mu.Unlock()
		databaseErr = nil
	}
	return errors.Join(databaseErr, localErr)
}

func (s *coordinatorActionStore) DrainErrors() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := errors.Join(s.deferred...)
	s.deferred = nil
	return err
}

func restoreIdentity(action Action) restoreActionIdentity {
	return restoreActionIdentity{
		runID: action.RunID, sequence: action.Sequence,
		kind: action.Kind, target: action.Target,
	}
}

func (s *coordinatorActionStore) Pending(
	_ context.Context,
	runID string,
) ([]Action, error) {
	var actions []Action
	for _, source := range s.sources {
		if source.action.RunID == runID &&
			source.action.State != MutationRestored {
			actions = append(actions, source.action)
		}
	}
	sortActionsReverse(actions)
	return actions, nil
}

func (s *coordinatorActionStore) StaleRuns(
	context.Context,
) ([]string, error) {
	seen := make(map[string]bool)
	for _, source := range s.sources {
		if source.action.State != MutationRestored {
			seen[source.action.RunID] = true
		}
	}
	runs := make([]string, 0, len(seen))
	for runID := range seen {
		runs = append(runs, runID)
	}
	sort.Strings(runs)
	return runs, nil
}

type restoreDispatchExecutor struct {
	database    ActionExecutor
	provider    FaultProvider
	providerErr error
	environment Environment
}

func newRestoreDispatchExecutor(
	database ActionExecutor,
	provider FaultProvider,
	environment Environment,
) *restoreDispatchExecutor {
	return &restoreDispatchExecutor{
		database: database, provider: provider, environment: environment,
	}
}

func (e *restoreDispatchExecutor) Preflight(
	ctx context.Context,
	action Action,
) error {
	switch {
	case externalPersistentActionKind(action.Kind):
		if e.providerErr != nil {
			return fmt.Errorf(
				"fault provider is unavailable for pending action kind %q: %w",
				action.Kind,
				e.providerErr,
			)
		}
		if e.provider == nil {
			return fmt.Errorf(
				"fault provider is unavailable for pending action kind %q",
				action.Kind,
			)
		}
		return e.provider.Preflight(ctx, e.environment, action)
	case action.Kind == ActionSessionSet ||
		action.Kind == ActionSessionTransaction:
		return nil
	case action.Kind == ActionSQLMutation ||
		action.Kind == ActionDataBaseline:
		if e.database == nil {
			return fmt.Errorf(
				"database action executor is unavailable for kind %q",
				action.Kind,
			)
		}
		return e.database.Preflight(ctx, action)
	default:
		return fmt.Errorf("restore action kind %q is unsupported", action.Kind)
	}
}

func (e *restoreDispatchExecutor) Apply(
	ctx context.Context,
	action Action,
) error {
	switch {
	case externalPersistentActionKind(action.Kind):
		if e.providerErr != nil {
			return fmt.Errorf(
				"fault provider is unavailable for action kind %q: %w",
				action.Kind,
				e.providerErr,
			)
		}
		if e.provider == nil {
			return fmt.Errorf(
				"fault provider is unavailable for action kind %q",
				action.Kind,
			)
		}
		return e.provider.Apply(ctx, action)
	case action.Kind == ActionSQLMutation ||
		action.Kind == ActionDataBaseline:
		if e.database == nil {
			return fmt.Errorf(
				"database action executor is unavailable for kind %q",
				action.Kind,
			)
		}
		return e.database.Apply(ctx, action)
	default:
		return fmt.Errorf("apply action kind %q is unsupported", action.Kind)
	}
}

func (e *restoreDispatchExecutor) Restore(
	ctx context.Context,
	action Action,
) error {
	switch {
	case externalPersistentActionKind(action.Kind):
		if e.providerErr != nil {
			return fmt.Errorf(
				"fault provider is unavailable for pending action kind %q: %w",
				action.Kind,
				e.providerErr,
			)
		}
		if e.provider == nil {
			return fmt.Errorf(
				"fault provider is unavailable for pending action kind %q",
				action.Kind,
			)
		}
		return e.provider.Restore(ctx, action)
	case action.Kind == ActionSessionSet ||
		action.Kind == ActionSessionTransaction:
		return nil
	case action.Kind == ActionSQLMutation ||
		action.Kind == ActionDataBaseline:
		if e.database == nil {
			return fmt.Errorf(
				"database action executor is unavailable for kind %q",
				action.Kind,
			)
		}
		return e.database.Restore(ctx, action)
	default:
		return fmt.Errorf("restore action kind %q is unsupported", action.Kind)
	}
}

func (e *restoreDispatchExecutor) VerifyRestored(
	ctx context.Context,
	action Action,
) error {
	switch {
	case externalPersistentActionKind(action.Kind):
		if e.providerErr != nil {
			return fmt.Errorf(
				"fault provider is unavailable for pending action kind %q: %w",
				action.Kind,
				e.providerErr,
			)
		}
		if e.provider == nil {
			return fmt.Errorf(
				"fault provider is unavailable for pending action kind %q",
				action.Kind,
			)
		}
		return e.provider.VerifyRestored(ctx, action)
	case action.Kind == ActionSessionSet ||
		action.Kind == ActionSessionTransaction:
		return nil
	case action.Kind == ActionSQLMutation ||
		action.Kind == ActionDataBaseline:
		if e.database == nil {
			return fmt.Errorf(
				"database action executor is unavailable for kind %q",
				action.Kind,
			)
		}
		return e.database.VerifyRestored(ctx, action)
	default:
		return fmt.Errorf("verify restore action kind %q is unsupported", action.Kind)
	}
}

func mergeRestoreActions(
	databaseActions []Action,
	localActions []Action,
	requestedRunID string,
) ([]Action, error) {
	actions := make([]Action, 0, len(databaseActions)+len(localActions))
	positions := make(map[restoreActionIdentity]int)
	stateIdentities := make(map[restoreActionStateIdentity]restoreActionIdentity)
	var errs []error
	add := func(action Action, source string) {
		if requestedRunID != "" && action.RunID != requestedRunID {
			return
		}
		identity := restoreIdentity(action)
		stateIdentity := restoreActionStateIdentity{
			runID: action.RunID, sequence: action.Sequence,
		}
		if action.Sequence > 0 {
			if existing, ok := stateIdentities[stateIdentity]; ok &&
				existing != identity {
				errs = append(errs, fmt.Errorf(
					"conflicting %s action identity for run %s journal ID %d",
					source,
					action.RunID,
					action.Sequence,
				))
				return
			}
			stateIdentities[stateIdentity] = identity
		}
		if index, ok := positions[identity]; ok {
			if !sameRestoreAction(actions[index], action) {
				errs = append(errs, fmt.Errorf(
					"conflicting %s action mirror for run %s kind %s target %s",
					source, action.RunID, action.Kind, action.Target,
				))
			}
			return
		}
		positions[identity] = len(actions)
		actions = append(actions, action)
	}
	for _, action := range databaseActions {
		add(action, "database")
	}
	for _, action := range localActions {
		add(action, "local")
	}
	return actions, errors.Join(errs...)
}

func sameRestoreAction(left, right Action) bool {
	return left.Sequence == right.Sequence &&
		left.RunID == right.RunID &&
		left.ScenarioCode == right.ScenarioCode &&
		left.Kind == right.Kind &&
		left.TargetProduct == right.TargetProduct &&
		left.Target == right.Target &&
		left.Node == right.Node &&
		equalRestoreJSON(left.Original, right.Original) &&
		equalRestoreJSON(left.Forward, right.Forward) &&
		equalRestoreJSON(left.Inverse, right.Inverse) &&
		equalRestoreJSON(left.Verify, right.Verify)
}

func equalRestoreJSON(left, right []byte) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == len(right)
	}
	leftCanonical, leftErr := canonicalRecoveryJSON(left)
	rightCanonical, rightErr := canonicalRecoveryJSON(right)
	if leftErr != nil || rightErr != nil {
		return bytes.Equal(left, right)
	}
	return bytes.Equal(leftCanonical, rightCanonical)
}

func restoreActionOrder(action Action) (restoreStage, int) {
	switch action.Kind {
	case ActionSessionSet, ActionSessionTransaction:
		return restoreStageSessions, 0
	case ActionNetworkFirewall:
		return restoreStageExternalControl, 0
	case ActionNetworkQDisc:
		return restoreStageExternalControl, 1
	case ActionProcessState:
		return restoreStageExternalControl, 2
	case ActionCloudFaultJob:
		return restoreStageExternalControl, 3
	case ActionGUCFileChange:
		return restoreStageConfiguration, 0
	case ActionNodeRole:
		return restoreStageConfiguration, 1
	case ActionSQLMutation, ActionDataBaseline:
		if statement, ok := sqlStatementFromActionPayload(action.Inverse); ok {
			fields := strings.Fields(statement)
			if len(fields) > 0 && strings.EqualFold(fields[0], "ANALYZE") {
				return restoreStageDatabase, 1
			}
		}
		return restoreStageDatabase, 0
	default:
		return restoreStageDatabase, 1
	}
}

func restoreActionGroup(
	actions []Action,
	runID string,
	stage restoreStage,
) []Action {
	var group []Action
	for _, action := range actions {
		actionStage, _ := restoreActionOrder(action)
		if action.RunID == runID && actionStage == stage {
			group = append(group, action)
		}
	}
	return group
}
