package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// LockRole deliberately carries only internal, fixed SQL. It is not a user
// input surface and makes holder/waiter identities explicit in evidence.
type LockRole struct {
	Tag           string
	SQL           []string
	Transactional bool
}

type LockDefinition struct {
	Code         ScenarioCode
	Name         string
	Object       string
	HolderSQL    []string
	WaiterSQL    []string
	HolderMode   string
	WaiterMode   string
	ExpectedKind string
	Deadlock     bool

	HolderTag           string
	WaiterTag           string
	HolderTransactional bool
	WaiterTransactional bool
	ChainRows           []int
	ChainTags           []string
	Waiters             []LockWaiterRole
	ExpectedEdges       []LockExpectedEdge
	RequestedSessions   int
	RequestedChainDepth int
	BranchLengths       []int
}

type LockEvidence struct {
	Node       string
	Object     string
	LockType   string
	HolderMode string
	WaiterMode string
	Granted    bool
	BlockerTag string
	WaiterTag  string
	WaitAge    time.Duration
}

type lockRollback interface{ Rollback() error }
type lockClose interface{ Close() error }

// LockEngine owns exactly the tagged sessions it opens. The stop path is kept
// intentionally small and ordered so a waiter never keeps a blocker alive.
type LockEngine struct {
	definition LockDefinition
	runtime    *Runtime

	holderConn *TaggedConn
	waiterConn *TaggedConn
	holderTx   lockRollback
	waiterTx   lockRollback
	chainTx    []lockRollback
	chainConn  []*TaggedConn

	cancelHolder func()
	cancelWaiter func()
	waiterWG     sync.WaitGroup
	holderWG     sync.WaitGroup
	mu           sync.Mutex
	waiterErr    error
	holderErr    error
	evidence     []LockEvidence
	observe      func(context.Context, *Runtime, LockDefinition) ([]LockEvidence, error)
}

func NewLockEngine(definition LockDefinition) *LockEngine {
	return &LockEngine{definition: definition}
}

func (e *LockEngine) Prepare(ctx context.Context, rt *Runtime) error {
	if rt == nil || rt.Database == nil {
		return fmt.Errorf("lock runtime database is unavailable")
	}
	e.runtime = rt
	if e.definition.HolderTag == "" {
		e.definition.HolderTag = "blocker"
	}
	if e.definition.WaiterTag == "" {
		e.definition.WaiterTag = "waiter"
	}
	holder, err := rt.Database.OpenTagged(ctx, rt.RunID, e.definition.Name, e.definition.HolderTag)
	if err != nil {
		return err
	}
	e.holderConn = holder
	if e.definition.HolderTransactional {
		tx, err := holder.Conn.BeginTx(ctx, nil)
		if err != nil {
			_ = holder.Close()
			return err
		}
		e.holderTx = tx
		if err := executeLockSQL(ctx, tx, e.definition.HolderSQL[:firstLockSQLCount(e.definition)]); err != nil {
			_ = tx.Rollback()
			_ = holder.Close()
			return err
		}
		return nil
	}
	if e.definition.Deadlock {
		return fmt.Errorf("deadlock definition %d requires a holder transaction", e.definition.Code)
	}
	holderCtx, cancel := context.WithCancel(ctx)
	e.cancelHolder = cancel
	e.holderWG.Add(1)
	go func() {
		defer e.holderWG.Done()
		e.setHolderError(executeLockSQL(holderCtx, holder.Conn, e.definition.HolderSQL))
	}()
	return nil
}

func firstLockSQLCount(definition LockDefinition) int {
	if definition.Deadlock && len(definition.HolderSQL) > 1 {
		return 1
	}
	return len(definition.HolderSQL)
}

func executeLockSQL(ctx context.Context, executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, statements []string) error {
	for _, statement := range statements {
		if _, err := executor.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (e *LockEngine) Ramp(ctx context.Context, rt *Runtime) error {
	if e.definition.Deadlock {
		return e.rampDeadlock(ctx, rt)
	}
	if e.definition.ExpectedKind == "row_chain" {
		return e.rampRowChain(ctx, rt)
	}
	return e.rampWaiter(ctx, rt)
}

func (e *LockEngine) rampRowChain(ctx context.Context, rt *Runtime) error {
	if len(e.definition.ChainRows) != 3 || len(e.definition.ChainTags) != 3 {
		return fmt.Errorf("row chain definition requires root and two waiter roles")
	}
	chainCtx, cancel := context.WithCancel(ctx)
	e.cancelWaiter = cancel
	open := func(tag string) (*TaggedConn, *sql.Tx, error) {
		conn, err := rt.Database.OpenTagged(chainCtx, rt.RunID, e.definition.Name, tag)
		if err != nil {
			return nil, nil, err
		}
		tx, err := conn.Conn.BeginTx(chainCtx, nil)
		if err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
		return conn, tx, nil
	}
	secondConn, secondTx, err := open(e.definition.ChainTags[1])
	if err != nil {
		return err
	}
	e.waiterConn, e.waiterTx = secondConn, secondTx
	if err := executeLockSQL(chainCtx, secondTx, []string{rowUpdate(rt.Config.Data.Schema, "lock_targets", e.definition.ChainRows[1])}); err != nil {
		return err
	}
	e.waiterWG.Add(1)
	go func() {
		defer e.waiterWG.Done()
		e.setWaiterError(executeLockSQL(chainCtx, secondTx, []string{rowUpdate(rt.Config.Data.Schema, "lock_targets", e.definition.ChainRows[0])}))
	}()
	thirdConn, thirdTx, err := open(e.definition.ChainTags[2])
	if err != nil {
		return err
	}
	e.chainConn = append(e.chainConn, thirdConn)
	e.chainTx = append(e.chainTx, thirdTx)
	if err := executeLockSQL(chainCtx, thirdTx, []string{rowUpdate(rt.Config.Data.Schema, "lock_targets", e.definition.ChainRows[2])}); err != nil {
		return err
	}
	e.waiterWG.Add(1)
	go func() {
		defer e.waiterWG.Done()
		e.setWaiterError(executeLockSQL(chainCtx, thirdTx, []string{rowUpdate(rt.Config.Data.Schema, "lock_targets", e.definition.ChainRows[1])}))
	}()
	return e.captureExpectedEvidence(chainCtx, rt)
}

func (e *LockEngine) rampWaiter(ctx context.Context, rt *Runtime) error {
	waiter, err := rt.Database.OpenTagged(ctx, rt.RunID, e.definition.Name, e.definition.WaiterTag)
	if err != nil {
		return err
	}
	e.waiterConn = waiter
	waitCtx, cancel := context.WithCancel(ctx)
	e.cancelWaiter = cancel
	if e.definition.WaiterTransactional {
		tx, err := waiter.Conn.BeginTx(waitCtx, nil)
		if err != nil {
			cancel()
			_ = waiter.Close()
			return err
		}
		e.waiterTx = tx
		statements := e.definition.WaiterSQL
		if e.definition.Deadlock && len(statements) > 1 {
			statements = statements[:1]
		}
		e.waiterWG.Add(1)
		go func() {
			defer e.waiterWG.Done()
			e.setWaiterError(executeLockSQL(waitCtx, tx, statements))
		}()
		return nil
	}
	if e.definition.Deadlock {
		return fmt.Errorf("deadlock definition %d requires a waiter transaction", e.definition.Code)
	}
	e.waiterWG.Add(1)
	go func() {
		defer e.waiterWG.Done()
		e.setWaiterError(executeLockSQL(waitCtx, waiter.Conn, e.definition.WaiterSQL))
	}()
	return nil
}

func (e *LockEngine) rampDeadlock(ctx context.Context, rt *Runtime) error {
	waiter, err := rt.Database.OpenTagged(ctx, rt.RunID, e.definition.Name, e.definition.WaiterTag)
	if err != nil {
		return err
	}
	e.waiterConn = waiter
	waitCtx, cancelWaiter := context.WithCancel(ctx)
	e.cancelWaiter = cancelWaiter
	waiterTx, err := waiter.Conn.BeginTx(waitCtx, nil)
	if err != nil {
		cancelWaiter()
		_ = waiter.Close()
		return err
	}
	e.waiterTx = waiterTx
	// The waiter must own row 101 before the holder starts waiting on it.
	if err := executeLockSQL(waitCtx, waiterTx, e.definition.WaiterSQL[:1]); err != nil {
		_ = waiterTx.Rollback()
		_ = waiter.Close()
		return err
	}
	holderCtx, cancel := context.WithCancel(ctx)
	e.cancelHolder = cancel
	holder, ok := e.holderTx.(*sql.Tx)
	if !ok {
		return fmt.Errorf("deadlock holder transaction is unavailable")
	}
	e.holderWG.Add(1)
	go func() {
		defer e.holderWG.Done()
		e.setHolderError(executeLockSQL(holderCtx, holder, e.definition.HolderSQL[1:]))
	}()
	if err := e.waitForDeadlockEdge(ctx, rt); err != nil {
		return err
	}
	// The final statement creates the second edge while Verify keeps polling
	// direct lock evidence until the database returns a deadlock SQLSTATE.
	e.waiterWG.Add(1)
	go func() {
		defer e.waiterWG.Done()
		e.setWaiterError(executeLockSQL(waitCtx, waiterTx, e.definition.WaiterSQL[1:]))
	}()
	return nil
}

func (e *LockEngine) waitForDeadlockEdge(ctx context.Context, rt *Runtime) error {
	deadline := time.NewTimer(500 * time.Millisecond)
	defer deadline.Stop()
	for {
		if err := e.captureEvidence(ctx, rt); err != nil {
			return err
		}
		e.mu.Lock()
		observed := append([]LockEvidence(nil), e.evidence...)
		e.mu.Unlock()
		for _, item := range observed {
			if !item.Granted && strings.Contains(item.BlockerTag, e.definition.WaiterTag) && strings.Contains(item.WaiterTag, e.definition.HolderTag) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("deadlock first wait edge was not observed")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (e *LockEngine) Hold(ctx context.Context, rt *Runtime) error {
	if e.definition.Deadlock {
		return nil
	}
	if err := e.captureExpectedEvidence(ctx, rt); err != nil {
		return err
	}
	return waitContext(ctx, rt.Config.Run.Duration)
}

func (e *LockEngine) Verify(ctx context.Context, rt *Runtime) (Result, error) {
	if e.definition.Deadlock {
		return e.verifyDeadlock(ctx, rt), nil
	}
	e.mu.Lock()
	evidence := append([]LockEvidence(nil), e.evidence...)
	e.mu.Unlock()
	return verifyLock(e.definition, evidence), nil
}

func (e *LockEngine) verifyDeadlock(ctx context.Context, rt *Runtime) Result {
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		_ = e.captureEvidence(ctx, rt)
		e.mu.Lock()
		err := errors.Join(e.holderErr, e.waiterErr)
		e.mu.Unlock()
		result := verifyDeadlock(e.definition, e.evidence, err)
		if result.Outcome == OutcomeSuccess || err != nil {
			return result
		}
		select {
		case <-ctx.Done():
			return verifyDeadlock(e.definition, e.evidence, ctx.Err())
		case <-deadline.C:
			return result
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (e *LockEngine) Stop(_ context.Context, _ *Runtime) error {
	if e.cancelWaiter != nil {
		e.cancelWaiter()
	}
	e.waiterWG.Wait()
	var errs []error
	for index := len(e.chainTx) - 1; index >= 0; index-- {
		errs = append(errs, rollbackLock(e.chainTx[index]))
	}
	if e.waiterTx != nil {
		errs = append(errs, rollbackLock(e.waiterTx))
	}
	if e.cancelHolder != nil {
		e.cancelHolder()
	}
	e.holderWG.Wait()
	if e.holderTx != nil {
		errs = append(errs, rollbackLock(e.holderTx))
	}
	if e.waiterConn != nil {
		errs = append(errs, e.waiterConn.Close())
	}
	for index := len(e.chainConn) - 1; index >= 0; index-- {
		errs = append(errs, e.chainConn[index].Close())
	}
	if e.holderConn != nil {
		errs = append(errs, e.holderConn.Close())
	}
	return errors.Join(errs...)
}

func rollbackLock(tx lockRollback) error {
	err := tx.Rollback()
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return err
}

func (e *LockEngine) Restore(ctx context.Context, rt *Runtime) error {
	if rt == nil || rt.Database == nil {
		return fmt.Errorf("lock runtime database is unavailable")
	}
	predicate, arguments, err := TaggedSessionPredicate(rt.RunID)
	if err != nil {
		return err
	}
	var sessions, locks int
	if err := rt.Database.Scan(ctx, "SELECT count(*) FROM pg_stat_activity WHERE "+predicate, arguments, &sessions); err != nil {
		return err
	}
	if err := rt.Database.Scan(ctx, "SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE "+predicate, arguments, &locks); err != nil {
		return err
	}
	if sessions != 0 || locks != 0 {
		return fmt.Errorf("owned lock sessions remain: sessions=%d locks=%d", sessions, locks)
	}
	return nil
}

func (e *LockEngine) setWaiterError(err error) {
	if err == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.waiterErr = err
}

func (e *LockEngine) setHolderError(err error) {
	if err == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.holderErr = err
}

func (e *LockEngine) captureEvidence(ctx context.Context, rt *Runtime) error {
	observer := e.observe
	if observer == nil {
		observer = observeLockEvidence
	}
	evidence, err := observer(ctx, rt, e.definition)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.evidence = appendLockEvidence(e.evidence, evidence)
	e.mu.Unlock()
	return nil
}

// captureExpectedEvidence runs while the workload context and waiter are live.
// Verify intentionally consumes this retained direct evidence later because
// the runner has already cancelled workloadCtx then.
func (e *LockEngine) captureExpectedEvidence(ctx context.Context, rt *Runtime) error {
	deadline := time.NewTimer(500 * time.Millisecond)
	defer deadline.Stop()
	for {
		e.mu.Lock()
		evidence := append([]LockEvidence(nil), e.evidence...)
		e.mu.Unlock()
		if verifyLock(e.definition, evidence).Outcome == OutcomeSuccess {
			return nil
		}
		observer := e.observe
		if observer == nil {
			observer = observeLockEvidence
		}
		observed, err := observer(ctx, rt, e.definition)
		if err != nil {
			return fmt.Errorf("capture direct lock evidence: %w", err)
		}
		e.mu.Lock()
		e.evidence = appendExpectedLockEvidence(e.definition, e.evidence, observed)
		evidence = append([]LockEvidence(nil), e.evidence...)
		e.mu.Unlock()
		if verifyLock(e.definition, evidence).Outcome == OutcomeSuccess {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("required direct lock waiter evidence was not observed while waiter was live")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func appendLockEvidence(existing, observed []LockEvidence) []LockEvidence {
	return append(existing, observed...)
}

func appendExpectedLockEvidence(
	definition LockDefinition,
	existing []LockEvidence,
	observed []LockEvidence,
) []LockEvidence {
	for _, candidate := range observed {
		if !isExpectedLockEvidence(definition, candidate) {
			continue
		}
		duplicate := false
		for _, retained := range existing {
			if sameLockEvidenceEdge(retained, candidate) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			existing = append(existing, candidate)
		}
	}
	return existing
}

func isExpectedLockEvidence(definition LockDefinition, item LockEvidence) bool {
	if item.Granted || item.Object != definition.Object ||
		!lockModeMatches(item.HolderMode, definition.HolderMode) ||
		!lockModeMatches(item.WaiterMode, definition.WaiterMode) {
		return false
	}
	if definition.ExpectedKind != "row_chain" {
		return strings.Contains(item.BlockerTag, definition.HolderTag) &&
			strings.Contains(item.WaiterTag, definition.WaiterTag)
	}
	if item.LockType != "transactionid" {
		return false
	}
	for index := 0; index+1 < len(definition.ChainTags); index++ {
		if strings.Contains(item.BlockerTag, definition.ChainTags[index]) &&
			strings.Contains(item.WaiterTag, definition.ChainTags[index+1]) {
			return true
		}
	}
	return false
}

func sameLockEvidenceEdge(first, second LockEvidence) bool {
	return first.Node == second.Node &&
		first.Object == second.Object &&
		first.LockType == second.LockType &&
		lockModeMatches(first.HolderMode, second.HolderMode) &&
		lockModeMatches(first.WaiterMode, second.WaiterMode) &&
		first.Granted == second.Granted &&
		first.BlockerTag == second.BlockerTag &&
		first.WaiterTag == second.WaiterTag
}

func observeLockEvidence(ctx context.Context, rt *Runtime, definition LockDefinition) ([]LockEvidence, error) {
	if rt == nil || rt.Database == nil {
		return nil, fmt.Errorf("lock runtime database is unavailable")
	}
	applicationPrefix, err := taggedScenarioPattern(rt.RunID, definition.Name)
	if err != nil {
		return nil, fmt.Errorf("lock observer application identity: %w", err)
	}
	distributed := rt.Environment.Topology == TopologyDistributed
	if distributed && !rt.Environment.Capabilities[CapabilityGlobalLockViews] {
		return nil, fmt.Errorf("distributed lock evidence requires global lock views")
	}
	rows, err := rt.Database.pool.QueryContext(ctx, lockEvidenceQuery(distributed), applicationPrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var evidence []LockEvidence
	for rows.Next() {
		var waitAgeSeconds float64
		var item LockEvidence
		if err := rows.Scan(&item.Node, &item.Object, &item.LockType, &item.HolderMode, &item.WaiterMode, &item.Granted, &item.BlockerTag, &item.WaiterTag, &waitAgeSeconds); err != nil {
			return nil, err
		}
		if definition.ExpectedKind == "row_chain" && item.LockType == "transactionid" {
			item.Object = definition.Object
		}
		item.WaitAge = time.Duration(waitAgeSeconds * float64(time.Second))
		evidence = append(evidence, item)
	}
	return evidence, rows.Err()
}

func lockEvidenceQuery(distributed bool) string {
	if distributed {
		return "SELECT COALESCE(w.node_name,''), COALESCE(c.relname,''), w.locktype, COALESCE(h.mode,''), COALESCE(w.mode,''), w.granted, COALESCE(ah.application_name,''), COALESCE(aw.application_name,''), COALESCE(EXTRACT(EPOCH FROM clock_timestamp()-aw.query_start),0) " +
			"FROM dbe_perf.global_locks w JOIN pg_stat_activity aw ON aw.pid=w.pid " +
			"JOIN dbe_perf.global_locks h ON h.locktype=w.locktype AND h.database IS NOT DISTINCT FROM w.database AND h.relation IS NOT DISTINCT FROM w.relation AND h.page IS NOT DISTINCT FROM w.page AND h.tuple IS NOT DISTINCT FROM w.tuple AND h.virtualxid IS NOT DISTINCT FROM w.virtualxid AND h.transactionid IS NOT DISTINCT FROM w.transactionid AND h.classid IS NOT DISTINCT FROM w.classid AND h.objid IS NOT DISTINCT FROM w.objid AND h.objsubid IS NOT DISTINCT FROM w.objsubid AND h.granted " +
			"JOIN pg_stat_activity ah ON ah.pid=h.pid LEFT JOIN pg_class c ON c.oid=w.relation " +
			"WHERE NOT w.granted AND aw.application_name LIKE $1 ESCAPE E'\\\\' AND ah.application_name LIKE $1 ESCAPE E'\\\\'"
	}
	node := "COALESCE(inet_server_addr()::text,'local')"
	return "SELECT " + node + ", COALESCE(c.relname,''), w.locktype, COALESCE(h.mode,''), COALESCE(w.mode,''), w.granted, COALESCE(ah.application_name,''), COALESCE(aw.application_name,''), COALESCE(EXTRACT(EPOCH FROM clock_timestamp()-aw.query_start),0) " +
		"FROM pg_locks w JOIN pg_stat_activity aw ON aw.pid=w.pid " +
		"JOIN pg_locks h ON h.locktype=w.locktype AND h.database IS NOT DISTINCT FROM w.database AND h.relation IS NOT DISTINCT FROM w.relation AND h.page IS NOT DISTINCT FROM w.page AND h.tuple IS NOT DISTINCT FROM w.tuple AND h.virtualxid IS NOT DISTINCT FROM w.virtualxid AND h.transactionid IS NOT DISTINCT FROM w.transactionid AND h.classid IS NOT DISTINCT FROM w.classid AND h.objid IS NOT DISTINCT FROM w.objid AND h.objsubid IS NOT DISTINCT FROM w.objsubid AND h.granted " +
		"JOIN pg_stat_activity ah ON ah.pid=h.pid LEFT JOIN pg_class c ON c.oid=w.relation " +
		"WHERE NOT w.granted AND aw.application_name LIKE $1 ESCAPE E'\\\\' AND ah.application_name LIKE $1 ESCAPE E'\\\\'"
}

func verifyLock(definition LockDefinition, evidence []LockEvidence) Result {
	if definition.ExpectedKind == "row_chain" {
		return verifyRowChain(definition, evidence)
	}
	result := Result{ScenarioCode: definition.Code, Scenario: definition.Name, Outcome: OutcomeFailed}
	for _, item := range evidence {
		if !item.Granted && item.Object == definition.Object &&
			lockModeMatches(item.HolderMode, definition.HolderMode) &&
			lockModeMatches(item.WaiterMode, definition.WaiterMode) &&
			strings.Contains(item.BlockerTag, definition.HolderTag) &&
			strings.Contains(item.WaiterTag, definition.WaiterTag) {
			result.Outcome = OutcomeSuccess
			result.Message = "direct lock waiter evidence observed"
			result.Evidence = []Evidence{{Metric: "lock_waiter", Target: 1, Actual: 1, Available: true, Details: map[string]any{"node": item.Node, "object": item.Object, "holder_mode": item.HolderMode, "waiter_mode": item.WaiterMode, "wait_age": item.WaitAge.String()}}}
			return result
		}
	}
	result.Message = "required direct lock waiter evidence was not observed"
	result.Evidence = []Evidence{{Metric: "lock_waiter", Target: 1, Actual: 0, Available: false}}
	return result
}

func verifyRowChain(definition LockDefinition, evidence []LockEvidence) Result {
	result := Result{ScenarioCode: definition.Code, Scenario: definition.Name, Outcome: OutcomeFailed, Message: "required multi-edge row wait evidence was not observed"}
	if len(definition.ChainTags) < 3 {
		return result
	}
	edges := make(map[string]bool)
	for _, item := range evidence {
		if item.Granted || item.LockType != "transactionid" || item.Object != definition.Object || !lockModeMatches(item.HolderMode, definition.HolderMode) || !lockModeMatches(item.WaiterMode, definition.WaiterMode) {
			continue
		}
		for index := 0; index+1 < len(definition.ChainTags); index++ {
			if strings.Contains(item.BlockerTag, definition.ChainTags[index]) && strings.Contains(item.WaiterTag, definition.ChainTags[index+1]) {
				edges[definition.ChainTags[index]+"->"+definition.ChainTags[index+1]] = true
			}
		}
	}
	if len(edges) == len(definition.ChainTags)-1 {
		result.Outcome = OutcomeSuccess
		result.Message = "direct multi-edge transaction-id row wait evidence observed"
		result.Evidence = []Evidence{{Metric: "row_lock_chain_edges", Target: float64(len(definition.ChainTags) - 1), Actual: float64(len(edges)), Available: true}}
		return result
	}
	result.Evidence = []Evidence{{Metric: "row_lock_chain_edges", Target: float64(len(definition.ChainTags) - 1), Actual: float64(len(edges)), Available: false}}
	return result
}

func lockModeMatches(observed, expected string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(strings.ReplaceAll(value, " ", ""))
		return strings.TrimSuffix(value, "lock")
	}
	return normalize(observed) == normalize(expected)
}

func verifyDeadlock(definition LockDefinition, evidence []LockEvidence, err error) Result {
	result := Result{ScenarioCode: definition.Code, Scenario: definition.Name, Outcome: OutcomeFailed, Message: "deadlock error and two-edge cycle were not both observed"}
	if hasDeadlockError(err) && hasTwoEdgeCycle(evidence, definition.HolderTag, definition.WaiterTag) {
		result.Outcome = OutcomeSuccess
		result.Message = "database deadlock error with two-edge wait cycle observed"
		result.Evidence = []Evidence{{Metric: "deadlock_cycle", Target: 2, Actual: 2, Available: true}}
		return result
	}
	result.Evidence = []Evidence{{Metric: "deadlock_cycle", Target: 2, Actual: 0, Available: false}}
	return result
}

func hasTwoEdgeCycle(evidence []LockEvidence, first, second string) bool {
	forward, reverse := false, false
	for _, item := range evidence {
		if item.Granted {
			continue
		}
		if strings.Contains(item.BlockerTag, first) && strings.Contains(item.WaiterTag, second) {
			forward = true
		}
		if strings.Contains(item.BlockerTag, second) && strings.Contains(item.WaiterTag, first) {
			reverse = true
		}
	}
	return forward && reverse
}

func hasDeadlockError(err error) bool {
	if err == nil {
		return false
	}
	type sqlStater interface{ SQLState() string }
	var state sqlStater
	if errors.As(err, &state) && state.SQLState() == "40P01" {
		return true
	}
	return false
}

func errLockDefinitionUnavailable(code ScenarioCode) error {
	return fmt.Errorf("lock definition %d is not implemented", code)
}
