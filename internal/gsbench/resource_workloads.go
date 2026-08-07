package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultPlanCacheObjectLimit = 64
	defaultSessionCursorLimit   = 256
	totalMemoryCompositeWorkMem = "SET work_mem='64MB'"
)

// ResourceWorkload is the allowlisted SQL shape for one lightweight resource
// scenario. Dynamic values are always parameters; the only interpolated name
// is a validated, quoted benchmark schema.
type ResourceWorkload struct {
	Code           ScenarioCode
	Setup          string
	Statement      string
	Cleanup        string
	RequiredStream string
}

func ResourceWorkloadFor(code ScenarioCode, schema string, _ Environment) (ResourceWorkload, error) {
	quoted, ok := quoteDatasetSchema(schema)
	if !ok {
		return ResourceWorkload{}, fmt.Errorf("unsafe schema %q", schema)
	}
	fact := quoted + `.fact_sales`
	accounts := quoted + `.accounts`
	workload := ResourceWorkload{Code: code}
	switch code {
	case 201:
		workload.Setup, workload.Cleanup = "SET work_mem='256MB'", "RESET work_mem"
		workload.Statement = "SELECT CAST(id AS text) AS id_text,CAST(customer_id AS text) AS customer_id_text,amount,payload FROM " + fact + " WHERE id BETWEEN $1 AND $2 ORDER BY payload,amount DESC,id"
	case 202:
		workload.Setup, workload.Cleanup = "SET work_mem='256MB'", "RESET work_mem"
		workload.Statement = "SELECT f1.product_id,sum(f1.amount),count(*) FROM " + fact + " f1 JOIN " + fact + " f2 ON f1.customer_id=f2.customer_id WHERE mod(f1.id,16)=0 GROUP BY f1.product_id ORDER BY sum(f1.amount) DESC"
	case 203, 301:
		workload.Statement = "SELECT sum(amount),avg(quantity),CAST(count(payload) AS text) AS payload_count_text FROM " + fact + " WHERE id BETWEEN $1 AND $2"
	case 204:
		workload.Cleanup = "DEALLOCATE ALL"
		workload.Statement = "SELECT sum(amount) FROM " + fact + " WHERE customer_id=$1 AND id >= $2"
	case 205:
		workload.Cleanup = "CLOSE ALL; ROLLBACK"
		workload.Statement = "SELECT id,payload FROM " + fact + " WHERE mod(id,97)=$1"
	case 207:
		workload.Setup, workload.Cleanup = "SET work_mem='256MB'", "DEALLOCATE ALL; CLOSE ALL; ROLLBACK; RESET work_mem"
		workload.Statement = "SELECT f.product_id,sum(f.amount),count(*) FROM " + fact + " f JOIN " + fact + " f2 ON f.customer_id=f2.customer_id WHERE mod(f.id,16)=0 GROUP BY f.product_id"
	case 208:
		workload.Cleanup = "DEALLOCATE ALL; CLOSE ALL; ROLLBACK"
		workload.Statement = "SELECT sum(amount) FROM " + fact + " WHERE customer_id=$1 AND id >= $2"
	case 302:
		workload.Statement = "SELECT balance,payload FROM " + accounts + " WHERE id=$1"
	case 303:
		workload.Statement = "UPDATE " + accounts + " SET balance=balance+0.01,updated_at=current_timestamp WHERE id=$1"
	case 304:
		workload.Setup, workload.Cleanup = "SET work_mem='64kB'", "RESET work_mem"
		workload.Statement = "SELECT customer_id,sum(amount) FROM " + fact + " GROUP BY customer_id ORDER BY sum(amount) DESC"
	case 321:
		workload.Statement = "SELECT CAST(id AS text) AS id_text,CAST(customer_id AS text) AS customer_id_text,CAST(product_id AS text) AS product_id_text,CAST(store_id AS text) AS store_id_text,amount,payload FROM " + fact + " WHERE id BETWEEN $1 AND $2"
	case 322:
		workload.Statement = "INSERT INTO " + quoted + ".network_ingress(run_id,dist_key,seq,payload) VALUES($1,$2,$3,$4)"
	case 331:
		workload.RequiredStream = "GATHER"
		workload.Statement = "SELECT store_id,sum(amount),count(*) FROM " + fact + " GROUP BY store_id"
	case 332:
		workload.RequiredStream = "REDISTRIBUTE"
		workload.Statement = "SELECT /*+ redistribute(f d) */ f.store_id,count(*),sum(f.amount) FROM " + fact + " f JOIN " + quoted + ".dist_join_data d ON f.customer_id=d.join_key GROUP BY f.store_id"
	case 333:
		workload.RequiredStream = "BROADCAST"
		workload.Statement = "SELECT /*+ broadcast(d) */ f.product_id,sum(f.amount) FROM " + fact + " f JOIN " + quoted + ".dist_small_hash d ON f.product_id=d.product_id GROUP BY f.product_id"
	case 403:
		workload.Statement = "SELECT 1"
	case 404:
		workload.Statement = "SELECT sum(f.amount) FROM " + fact + " f JOIN " + quoted + ".dim_product p ON p.id=f.product_id WHERE mod(f.id,101)=$1"
	default:
		return ResourceWorkload{}, fmt.Errorf("resource scenario %d is not implemented", code)
	}
	return workload, nil
}

type resourceEvidence struct {
	plan        string
	node        string
	streamBytes float64
}

func verifyResourceWorkload(code ScenarioCode, snapshot WorkerSnapshot, evidence resourceEvidence) Result {
	result := Result{Scenario: DefaultScenarioCatalog().MustCode(code).Name, Outcome: OutcomeFailed}
	result.Evidence = []Evidence{{Metric: "operations", Actual: float64(snapshot.Operations), Available: true}}
	if required := resourceRequiredStream(code); required != "" {
		matched := strings.Contains(strings.ToUpper(evidence.plan), required)
		direct := matched && evidence.node != "" && evidence.streamBytes > 0
		result.Evidence = append(result.Evidence, Evidence{Metric: "required_" + strings.ToLower(required) + "_stream", Actual: evidence.streamBytes, Available: direct, Details: map[string]any{"node": evidence.node}})
		if direct && snapshot.Operations > 0 {
			result.Outcome = OutcomeSuccess
			result.Message = "required distributed stream observed on " + evidence.node
			return result
		}
		result.Outcome = OutcomeDegraded
		result.Message = "required distributed stream or direct node evidence is unavailable"
		return result
	}
	if snapshot.Operations > 0 {
		result.Outcome = OutcomeDegraded
		result.Message = "bounded workload completed; required resource metric is unavailable"
		return result
	}
	result.Message = "workload performed no operations"
	return result
}

func resourceRequiredStream(code ScenarioCode) string {
	switch code {
	case 331:
		return "GATHER"
	case 332:
		return "REDISTRIBUTE"
	case 333:
		return "BROADCAST"
	default:
		return ""
	}
}

type resourceScenario struct {
	code              ScenarioCode
	name              string
	workload          ResourceWorkload
	workers           *sqlWorkload
	evidence          resourceEvidence
	sequence          atomic.Int64
	memoryMu          sync.Mutex
	memoryObjects     map[int][]string
	memoryObjectCount int
	workerCapacity    int
}

func newResourceScenario(code ScenarioCode, name string) *resourceScenario {
	return &resourceScenario{code: code, name: name}
}

func (s *resourceScenario) Code() ScenarioCode { return s.code }
func (s *resourceScenario) Name() string       { return s.name }
func (s *resourceScenario) Strategy() string   { return "bounded_resource_sql" }

func (s *resourceScenario) Prepare(ctx context.Context, rt *Runtime) error {
	if rt == nil || rt.Database == nil {
		return sql.ErrConnDone
	}
	workload, err := ResourceWorkloadFor(s.code, rt.Config.Data.Schema, rt.Environment)
	if err != nil {
		return err
	}
	s.workload = workload
	if s.code == 322 {
		if rt.Journal == nil {
			return fmt.Errorf("mutation journal is unavailable")
		}
		runID, err := resourceRunID(rt.RunID)
		if err != nil {
			return err
		}
		if err := rt.Journal.Apply(ctx, Mutation{
			RunID: rt.RunID, ScenarioCode: s.code, Kind: "network_ingress_cleanup",
			Target:     quotedResourceTable(rt.Config.Data.Schema, "network_ingress"),
			ForwardSQL: "SELECT 1",
			InverseSQL: "DELETE FROM " + quotedResourceTable(rt.Config.Data.Schema, "network_ingress") + " WHERE run_id='" + runID + "'",
		}); err != nil {
			return err
		}
	}
	if workload.RequiredStream != "" {
		if err := s.observeRequiredStream(ctx, rt); err != nil {
			return err
		}
	}
	workerCapacity := max(1, s.workerCapacity)
	s.workers = newSQLWorkloadWithCleanup(ctx, rt, s.name, workerCapacity, s.operation(rt), func(ctx context.Context, conn *sql.Conn, _ int) error {
		var cleanupErrors []error
		for _, statement := range strings.Split(s.workload.Cleanup, ";") {
			if statement = strings.TrimSpace(statement); statement != "" {
				if _, err := conn.ExecContext(ctx, statement); err != nil && !errors.Is(err, sql.ErrTxDone) {
					cleanupErrors = append(cleanupErrors, err)
				}
			}
		}
		switch len(cleanupErrors) {
		case 0:
			return nil
		case 1:
			return cleanupErrors[0]
		default:
			return errors.Join(cleanupErrors...)
		}
	})
	return nil
}

func (s *resourceScenario) operation(rt *Runtime) SQLWorkerOp {
	return func(ctx context.Context, conn *sql.Conn, workerID int) error {
		if s.workload.Setup != "" {
			if _, err := conn.ExecContext(ctx, s.workload.Setup); err != nil {
				return err
			}
		}
		n := s.sequence.Add(1)
		switch s.code {
		case 204:
			candidate := resourcePrepareName(rt.RunID, n)
			name, create, _ := s.boundedMemoryObject(
				workerID, candidate, 0, defaultPlanCacheObjectLimit,
			)
			if create {
				_, err := conn.ExecContext(ctx, resourcePrepareStatement(rt.RunID, n, s.workload.Statement))
				return err
			}
			if name == "" {
				_, err := conn.ExecContext(ctx, "SELECT 1")
				return err
			}
			_, err := conn.ExecContext(ctx, resourceExecutePrepared(name))
			return err
		case 205:
			candidate := "gsbench_cur_" + strconv.FormatInt(n, 10)
			name, create, first := s.boundedMemoryObject(
				workerID, candidate, 0, defaultSessionCursorLimit,
			)
			if !create {
				_, err := conn.ExecContext(ctx, "SELECT 1")
				return err
			}
			if first {
				if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
					return err
				}
			}
			if _, err := conn.ExecContext(ctx, "DECLARE "+name+" NO SCROLL CURSOR FOR "+s.workload.Statement, int(n%97)); err != nil {
				return err
			}
			_, err := conn.ExecContext(ctx, "FETCH 1 FROM "+name)
			return err
		case 322:
			_, err := conn.ExecContext(ctx, s.workload.Statement, rt.RunID, n%1024, n, strings.Repeat("x", 256))
			return err
		case 303:
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, s.workload.Statement, n%100000+1); err != nil {
				return err
			}
			return tx.Commit()
		case 201, 203, 301, 321:
			rows, err := conn.QueryContext(ctx, s.workload.Statement, n%100000+1, n%100000+10000)
			if err != nil {
				return err
			}
			defer rows.Close()
			return consumeRows(rows)
		case 302, 404:
			rows, err := conn.QueryContext(ctx, s.workload.Statement, n%100000+1)
			if err != nil {
				return err
			}
			defer rows.Close()
			return consumeRows(rows)
		default:
			rows, err := conn.QueryContext(ctx, s.workload.Statement)
			if err != nil {
				return err
			}
			defer rows.Close()
			return consumeRows(rows)
		}
	}
}

func (s *resourceScenario) boundedMemoryObject(
	workerID int,
	candidate string,
	reuseIndex int64,
	limit int,
) (name string, create, first bool) {
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	if s.memoryObjects == nil {
		s.memoryObjects = make(map[int][]string)
	}
	owned := s.memoryObjects[workerID]
	if s.memoryObjectCount < limit {
		s.memoryObjects[workerID] = append(owned, candidate)
		s.memoryObjectCount++
		return candidate, true, len(owned) == 0
	}
	if len(owned) == 0 {
		return "", false, false
	}
	if reuseIndex < 0 {
		reuseIndex = 0
	}
	return owned[reuseIndex%int64(len(owned))], false, false
}

func (s *resourceScenario) observeRequiredStream(ctx context.Context, rt *Runtime) error {
	rows, err := rt.Database.Query(ctx, "EXPLAIN "+s.workload.Statement)
	if err != nil {
		return err
	}
	defer rows.Close()
	plan, err := scanExplainRows(rows.Rows)
	if err != nil {
		return err
	}
	s.evidence.plan = plan
	if !strings.Contains(strings.ToUpper(s.evidence.plan), s.workload.RequiredStream) {
		return fmt.Errorf("required %s stream was not present in EXPLAIN", s.workload.RequiredStream)
	}
	for _, node := range rt.Environment.Nodes {
		if node.Role == NodeRoleDNPrimary || node.Role == NodeRoleCN {
			s.evidence.node = node.Name
			break
		}
	}
	return nil
}

func (s *resourceScenario) Ramp(_ context.Context, _ *Runtime) error { return s.workers.SetTarget(1) }
func (s *resourceScenario) Hold(ctx context.Context, rt *Runtime) error {
	return waitContext(ctx, rt.Config.Run.Duration)
}
func (s *resourceScenario) Verify(context.Context, *Runtime) (Result, error) {
	if s.workers == nil {
		return Result{}, fmt.Errorf("resource workload is unavailable")
	}
	return verifyResourceWorkload(s.code, s.workers.Snapshot(), s.evidence), nil
}
func (s *resourceScenario) ExecutionSnapshot() WorkerSnapshot {
	if s.workers == nil {
		return WorkerSnapshot{}
	}
	return s.workers.Snapshot()
}
func (s *resourceScenario) Stop(ctx context.Context, _ *Runtime) error {
	if s.workers == nil {
		return nil
	}
	return s.workers.Stop(ctx)
}
func (s *resourceScenario) Restore(context.Context, *Runtime) error { return nil }

func ResourceScenarioFactories() map[ScenarioCode]ScenarioFactory {
	factories := make(map[ScenarioCode]ScenarioFactory)
	for _, code := range []ScenarioCode{201, 202, 203, 204, 205, 207, 208, 301, 302, 303, 304, 321, 322, 331, 332, 333, 403, 404} {
		code := code
		factories[code] = func(definition ScenarioDefinition, _ Environment) (Scenario, error) {
			if definition.Code != code {
				return nil, fmt.Errorf("resource factory code mismatch: %d", definition.Code)
			}
			switch code {
			case 201, 202:
				return newWorkMemScenario(code, definition.Name)
			case 207:
				return newTotalMemoryScenario(definition.Name), nil
			case 208:
				return newMemoryRetentionScenario(definition.Name), nil
			case 403:
				return newConnectionChurnScenario(definition.Name), nil
			case 404:
				return newResourcePressureScenario(code, definition.Name), nil
			}
			return newResourceScenario(code, definition.Name), nil
		}
	}
	return factories
}

func resourceIdentifier(runID string) string {
	var b strings.Builder
	for _, r := range runID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "run"
	}
	return b.String()
}

func resourcePrepareStatement(runID string, sequence int64, statement string) string {
	return "PREPARE " + resourcePrepareName(runID, sequence) + "(bigint,bigint) AS " + statement
}

func resourcePrepareName(runID string, sequence int64) string {
	return "gsbench_pc_" + resourceIdentifier(runID) + "_" + strconv.FormatInt(sequence, 10)
}

func resourceExecutePrepared(name string) string {
	return "EXECUTE " + name + "(0,0)"
}

func resourceRunID(runID string) (string, error) {
	if _, _, err := TaggedSessionPredicate(runID); err != nil {
		return "", err
	}
	return runID, nil
}

func quotedResourceTable(schema, table string) string {
	quoted, _ := quoteDatasetSchema(schema)
	return quoted + "." + table
}

// churnConnection is deliberately small so a connection churn operation can
// be tested without a live database while production still uses TaggedConn.
type churnConnection interface {
	ExecContext(context.Context, string, ...any) error
	Close() error
}

type taggedChurnConnection struct{ tagged *TaggedConn }

func (c taggedChurnConnection) ExecContext(ctx context.Context, statement string, args ...any) error {
	_, err := c.tagged.Conn.ExecContext(ctx, statement, args...)
	return err
}

func (c taggedChurnConnection) Close() error { return c.tagged.Close() }

type ChurnMetrics struct {
	Created       int64
	Closed        int64
	Operations    int64
	Failures      int64
	CreateLatency time.Duration
}

type connectionChurnOperation struct {
	statement  string
	dial       func(context.Context, int64) (churnConnection, error)
	sequence   atomic.Int64
	created    atomic.Int64
	closed     atomic.Int64
	operations atomic.Int64
	failures   atomic.Int64
	latencyNS  atomic.Int64
}

func newConnectionChurnOperation(statement string, dial func(context.Context, int64) (churnConnection, error)) *connectionChurnOperation {
	return &connectionChurnOperation{statement: statement, dial: dial}
}

func (o *connectionChurnOperation) Run(ctx context.Context, _ int) (result error) {
	started := time.Now()
	connection, err := o.dial(ctx, o.sequence.Add(1))
	o.latencyNS.Add(time.Since(started).Nanoseconds())
	if err != nil {
		o.failures.Add(1)
		return err
	}
	o.created.Add(1)
	defer func() {
		if err := connection.Close(); err != nil {
			o.failures.Add(1)
			if result == nil {
				result = err
			}
			return
		}
		o.closed.Add(1)
	}()
	if err := connection.ExecContext(ctx, o.statement); err != nil {
		o.failures.Add(1)
		return err
	}
	o.operations.Add(1)
	return nil
}

func (o *connectionChurnOperation) Metrics() ChurnMetrics {
	return ChurnMetrics{
		Created: o.created.Load(), Closed: o.closed.Load(), Operations: o.operations.Load(),
		Failures: o.failures.Load(), CreateLatency: time.Duration(o.latencyNS.Load()),
	}
}

type connectionChurnScenario struct {
	name    string
	group   *WorkerGroup
	op      *connectionChurnOperation
	target  int
	workSQL ResourceWorkload
}

func newConnectionChurnScenario(name string) *connectionChurnScenario {
	return &connectionChurnScenario{name: name}
}

func (s *connectionChurnScenario) Code() ScenarioCode { return 403 }
func (s *connectionChurnScenario) Name() string       { return s.name }
func (s *connectionChurnScenario) Strategy() string   { return "fresh_tagged_connection_churn" }

func (s *connectionChurnScenario) Prepare(ctx context.Context, rt *Runtime) error {
	if rt == nil || rt.Database == nil {
		return sql.ErrConnDone
	}
	workload, err := ResourceWorkloadFor(s.Code(), rt.Config.Data.Schema, rt.Environment)
	if err != nil {
		return err
	}
	s.workSQL = workload
	s.target = runtimeInt(rt, "scenario.connection_churn.workers", 1)
	if s.target < 1 {
		return fmt.Errorf("connection churn workers must be positive")
	}
	s.op = newConnectionChurnOperation(workload.Statement, func(opCtx context.Context, sequence int64) (churnConnection, error) {
		conn, err := rt.Database.OpenTagged(opCtx, rt.RunID, s.name, "churn-"+strconv.FormatInt(sequence, 10))
		if err != nil {
			return nil, err
		}
		return taggedChurnConnection{tagged: conn}, nil
	})
	s.group = NewWorkerGroup(ctx, s.target, s.op.Run)
	return nil
}

func (s *connectionChurnScenario) Ramp(context.Context, *Runtime) error {
	return s.group.SetTarget(s.target)
}
func (s *connectionChurnScenario) Hold(ctx context.Context, rt *Runtime) error {
	return waitContext(ctx, rt.Config.Run.Duration)
}
func (s *connectionChurnScenario) Verify(context.Context, *Runtime) (Result, error) {
	if s.group == nil || s.op == nil {
		return Result{}, fmt.Errorf("connection churn workload is unavailable")
	}
	result := verifyResourceWorkload(s.Code(), s.group.Snapshot(), resourceEvidence{})
	metrics := s.op.Metrics()
	result.Evidence = append(result.Evidence,
		Evidence{Metric: "connections_created", Actual: float64(metrics.Created), Available: true},
		Evidence{Metric: "connections_closed", Actual: float64(metrics.Closed), Available: true},
		Evidence{Metric: "connection_create_latency_ms", Actual: float64(metrics.CreateLatency.Milliseconds()), Available: metrics.Created > 0},
		Evidence{Metric: "connection_failures", Actual: float64(metrics.Failures), Available: true},
	)
	return result, nil
}
func (s *connectionChurnScenario) ExecutionSnapshot() WorkerSnapshot {
	if s.group == nil {
		return WorkerSnapshot{}
	}
	return s.group.Snapshot()
}
func (s *connectionChurnScenario) Stop(ctx context.Context, _ *Runtime) error {
	if s.group == nil {
		return nil
	}
	return s.group.Stop(ctx)
}
func (s *connectionChurnScenario) Restore(context.Context, *Runtime) error { return nil }

func resourcePressureTarget(code ScenarioCode, capacity, safetyMaximum int) (int, error) {
	if code != 404 {
		return 0, fmt.Errorf("scenario %d has no multi-session pressure target", code)
	}
	if capacity < 1 {
		return 0, fmt.Errorf("resource pressure capacity must be positive")
	}
	return capacity + 1, nil
}

type resourcePressureScenario struct {
	*resourceScenario
	target         int
	status         ThreadPoolStatus
	sessionCeiling int
	established    int
	peakPending    int
	metricWarned   bool
}

func newResourcePressureScenario(code ScenarioCode, name string) *resourcePressureScenario {
	return &resourcePressureScenario{resourceScenario: newResourceScenario(code, name)}
}

func (s *resourcePressureScenario) Strategy() string {
	return "thread_pool_queue_observed_pending"
}

func (s *resourcePressureScenario) Prepare(ctx context.Context, rt *Runtime) error {
	if rt == nil || rt.Database == nil {
		return sql.ErrConnDone
	}
	status, err := sampleThreadPoolStatus(ctx, rt)
	if err != nil {
		runtimeWarn(rt, PrecheckWarning{
			ScenarioCode: s.Code(), Scenario: s.Name(),
			Check: "thread_pool_metric", Object: "global_threadpool_status",
			Actual: err.Error(), Expected: "readable_worker_status",
			Impact: "one_worker_plus_one_session_will_be_attempted",
		})
		s.metricWarned = true
		status = ThreadPoolStatus{Actual: 1}
	}
	facts, err := probeConnectionCapacity(ctx, rt)
	if err != nil {
		runtimeWarn(rt, PrecheckWarning{
			ScenarioCode: s.Code(), Scenario: s.Name(),
			Check: "capacity", Object: "connection_headroom",
			Actual: err.Error(), Expected: "readable_capacity",
			Impact: "requested_sessions_will_still_be_attempted",
		})
	}
	sessionCeiling := physicalSessionHeadroom(
		facts.InstanceMax,
		facts.Reserved,
		facts.Existing,
	)
	target, err := resourcePressureTarget(s.code, status.Actual, sessionCeiling)
	if err != nil {
		return err
	}
	if sessionCeiling < target {
		runtimeWarn(rt, PrecheckWarning{
			ScenarioCode: s.Code(), Scenario: s.Name(),
			Check: "capacity", Object: "thread_queue_sessions",
			Actual:   fmt.Sprintf("ceiling=%d target=%d", sessionCeiling, target),
			Expected: fmt.Sprintf(">=%d", target),
			Impact:   "target_may_not_be_reached",
		})
	}
	s.resourceScenario.workerCapacity = target
	if err := s.resourceScenario.Prepare(ctx, rt); err != nil {
		return err
	}
	s.status, s.sessionCeiling, s.target = status, sessionCeiling, target
	return nil
}

func (s *resourcePressureScenario) Ramp(ctx context.Context, rt *Runtime) error {
	if s.workers == nil {
		return fmt.Errorf("thread queue workload is unavailable")
	}
	if err := s.workers.SetTarget(s.target); err != nil {
		return err
	}
	return s.waitForSessions(ctx, rt)
}

func (s *resourcePressureScenario) waitForSessions(ctx context.Context, rt *Runtime) error {
	timeout := 5 * time.Second
	if rt != nil && rt.Config.Safety.QueryTimeout > 0 {
		timeout = rt.Config.Safety.QueryTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot := s.workers.Snapshot()
		if err := workerSnapshotError(snapshot); err != nil {
			return fmt.Errorf("thread queue target is unreachable while establishing %d sessions: %w", s.target, err)
		}
		s.workers.mu.Lock()
		established := len(s.workers.sessions)
		s.workers.mu.Unlock()
		if established >= s.target {
			s.established = established
			return nil
		}
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.established = established
			runtimeWarn(rt, PrecheckWarning{
				ScenarioCode: s.Code(), Scenario: s.Name(),
				Check: "runtime_target", Object: "thread_queue_sessions",
				Actual:   fmt.Sprintf("established=%d target=%d", established, s.target),
				Expected: fmt.Sprintf("%d", s.target),
				Impact:   "established_sessions_will_be_held",
			})
			return nil
		case <-ticker.C:
		}
	}
}

func (s *resourcePressureScenario) Hold(ctx context.Context, rt *Runtime) error {
	if s.workers == nil {
		return fmt.Errorf("thread queue workload is unavailable")
	}
	if err := workerSnapshotError(s.workers.Snapshot()); err != nil {
		return err
	}
	if rt == nil || rt.Database == nil {
		return sql.ErrConnDone
	}
	interval := rt.Config.Run.RampInterval
	if interval <= 0 {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timer := time.NewTimer(rt.Config.Run.Duration)
	defer timer.Stop()
	for {
		if err := workerSnapshotError(s.workers.Snapshot()); err != nil {
			return err
		}
		status, err := sampleThreadPoolStatus(ctx, rt)
		if err != nil {
			if !s.metricWarned {
				runtimeWarn(rt, PrecheckWarning{
					ScenarioCode: s.Code(), Scenario: s.Name(),
					Check: "thread_pool_metric", Object: "global_threadpool_status",
					Actual: err.Error(), Expected: "readable_worker_status",
					Impact: "session_workload_continues_without_pending_metric",
				})
				s.metricWarned = true
			}
		} else {
			s.status = status
			if status.Pending > s.peakPending {
				s.peakPending = status.Pending
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-ticker.C:
		}
	}
}

func (s *resourcePressureScenario) Verify(context.Context, *Runtime) (Result, error) {
	if s.workers == nil {
		return Result{}, fmt.Errorf("thread queue workload is unavailable")
	}
	snapshot := s.workers.Snapshot()
	result := Result{Scenario: s.name, Outcome: OutcomeFailed, Evidence: []Evidence{
		{Metric: "operations", Actual: float64(snapshot.Operations), Available: true},
		{Metric: "thread_pool_actual_workers", Actual: float64(s.status.Actual), Available: s.status.Actual > 0},
		{Metric: "thread_pool_idle_workers", Actual: float64(s.status.Idle), Available: s.status.Actual > 0},
		{Metric: "thread_pool_pending_sessions", Actual: float64(s.peakPending), Available: s.status.Actual > 0},
		{Metric: "thread_queue_session_ceiling", Target: float64(s.target), Actual: float64(s.sessionCeiling), Available: s.sessionCeiling > 0},
	}}
	switch {
	case snapshot.Errors > 0:
		result.Message = workerSnapshotError(snapshot).Error()
	case s.established < s.target:
		result.Message = fmt.Sprintf("thread queue established %d sessions; require %d", s.established, s.target)
	case s.peakPending <= 0:
		result.Message = "thread queue produced no observed pending work"
	default:
		result.Outcome = OutcomeSuccess
		result.Message = "thread queue pending work observed with established workload sessions"
	}
	return result, nil
}

type memoryLifecycle struct {
	AllocationCodes []ScenarioCode
	Composite       bool
	RetainSessions  bool
}

func memoryLifecycleFor(code ScenarioCode) (memoryLifecycle, error) {
	switch code {
	case 207:
		return memoryLifecycle{AllocationCodes: []ScenarioCode{201, 202, 204, 205}, Composite: true}, nil
	case 208:
		return memoryLifecycle{AllocationCodes: []ScenarioCode{204, 205}, RetainSessions: true}, nil
	default:
		return memoryLifecycle{}, fmt.Errorf("scenario %d has no memory lifecycle", code)
	}
}

type memoryComposite struct{ scenarios []*resourceScenario }

func (c *memoryComposite) Target() int {
	total := 0
	for _, scenario := range c.scenarios {
		total += scenario.workers.Target()
	}
	return total
}

func (c *memoryComposite) SetTarget(target int) error {
	if target < 0 {
		return fmt.Errorf("memory target must not be negative")
	}
	for index, scenario := range c.scenarios {
		desired := target / len(c.scenarios)
		if index < target%len(c.scenarios) {
			desired++
		}
		if err := scenario.workers.SetTarget(desired); err != nil {
			return err
		}
	}
	return nil
}

func (c *memoryComposite) Snapshot() WorkerSnapshot {
	var snapshot WorkerSnapshot
	for _, scenario := range c.scenarios {
		part := scenario.workers.Snapshot()
		snapshot.Target += part.Target
		snapshot.Active += part.Active
		snapshot.Operations += part.Operations
		snapshot.Errors += part.Errors
		snapshot.TotalLatency += part.TotalLatency
		if snapshot.FirstError == "" {
			snapshot.FirstError = part.FirstError
		}
	}
	return snapshot
}

func (c *memoryComposite) Stop(ctx context.Context) error {
	var result error
	for _, scenario := range c.scenarios {
		if err := scenario.Stop(ctx, nil); err != nil && result == nil {
			result = err
		}
	}
	return result
}

type totalMemoryScenario struct {
	name      string
	composite memoryComposite
	target    int
}

func newTotalMemoryScenario(name string) *totalMemoryScenario {
	return &totalMemoryScenario{name: name}
}
func (s *totalMemoryScenario) Code() ScenarioCode { return 207 }
func (s *totalMemoryScenario) Name() string       { return s.name }
func (s *totalMemoryScenario) Strategy() string   { return "memory_pressure_workers" }

func (s *totalMemoryScenario) Prepare(ctx context.Context, rt *Runtime) error {
	if rt == nil {
		return sql.ErrConnDone
	}
	target := runtimeInt(rt, "scenario.memory_total_pressure.workers", 4)
	if target < 1 {
		return fmt.Errorf("memory pressure workers must be positive")
	}
	plan, err := memoryLifecycleFor(s.Code())
	if err != nil {
		return err
	}
	for _, code := range plan.AllocationCodes {
		definition := DefaultScenarioCatalog().MustCode(code)
		child := newResourceScenario(code, s.name+"_"+definition.Name)
		child.workerCapacity = target
		if err := child.Prepare(ctx, rt); err != nil {
			return err
		}
		if code == 201 || code == 202 {
			child.workload.Setup = totalMemoryCompositeWorkMem
		}
		s.composite.scenarios = append(s.composite.scenarios, child)
	}
	s.target = target
	return nil
}

func (s *totalMemoryScenario) Ramp(context.Context, *Runtime) error {
	if len(s.composite.scenarios) == 0 {
		return fmt.Errorf("memory pressure workload is unavailable")
	}
	return s.composite.SetTarget(s.target)
}
func (s *totalMemoryScenario) Hold(ctx context.Context, rt *Runtime) error {
	if err := workerSnapshotError(s.composite.Snapshot()); err != nil {
		return err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(rt.Config.Run.Duration)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-ticker.C:
			if err := workerSnapshotError(s.composite.Snapshot()); err != nil {
				return err
			}
		}
	}
}
func (s *totalMemoryScenario) Verify(context.Context, *Runtime) (Result, error) {
	result := verifyResourceWorkload(s.Code(), s.composite.Snapshot(), resourceEvidence{})
	result.Evidence = append(result.Evidence, Evidence{Metric: "memory_pressure_workers", Target: float64(s.target), Actual: float64(s.composite.Target()), Available: true})
	return result, nil
}
func (s *totalMemoryScenario) ExecutionSnapshot() WorkerSnapshot {
	return s.composite.Snapshot()
}
func (s *totalMemoryScenario) Stop(ctx context.Context, _ *Runtime) error {
	return s.composite.Stop(ctx)
}
func (s *totalMemoryScenario) Restore(context.Context, *Runtime) error { return nil }

type memoryRetentionScenario struct {
	name                 string
	scenarios            []*resourceScenario
	allocationStopped    bool
	operationsBeforeHold int64
	operationsAfterHold  int64
	released             bool
}

func newMemoryRetentionScenario(name string) *memoryRetentionScenario {
	return &memoryRetentionScenario{name: name}
}
func (s *memoryRetentionScenario) Code() ScenarioCode { return 208 }
func (s *memoryRetentionScenario) Name() string       { return s.name }
func (s *memoryRetentionScenario) Strategy() string   { return "allocate_retain_release_memory_sessions" }

func (s *memoryRetentionScenario) Prepare(ctx context.Context, rt *Runtime) error {
	plan, err := memoryLifecycleFor(s.Code())
	if err != nil {
		return err
	}
	for _, code := range plan.AllocationCodes {
		definition := DefaultScenarioCatalog().MustCode(code)
		child := newResourceScenario(code, s.name+"_"+definition.Name)
		if err := child.Prepare(ctx, rt); err != nil {
			return err
		}
		s.scenarios = append(s.scenarios, child)
	}
	return nil
}

func (s *memoryRetentionScenario) Ramp(ctx context.Context, rt *Runtime) error {
	for _, scenario := range s.scenarios {
		if err := scenario.workers.SetTarget(1); err != nil {
			return err
		}
	}
	window := rt.Config.Run.RampInterval
	if window <= 0 {
		window = 10 * time.Millisecond
	}
	if err := waitContext(ctx, window); err != nil {
		return err
	}
	for _, scenario := range s.scenarios {
		if err := scenario.workers.SetTarget(0); err != nil {
			return err
		}
	}
	s.allocationStopped = true
	return nil
}

func (s *memoryRetentionScenario) Hold(ctx context.Context, rt *Runtime) error {
	s.operationsBeforeHold = s.operations()
	if !s.allocationStopped {
		return fmt.Errorf("memory retention allocation is still active")
	}
	if err := waitContext(ctx, rt.Config.Run.Duration); err != nil {
		return err
	}
	s.operationsAfterHold = s.operations()
	return nil
}

func (s *memoryRetentionScenario) Verify(context.Context, *Runtime) (Result, error) {
	snapshot := WorkerSnapshot{Operations: s.operationsAfterHold}
	result := verifyResourceWorkload(s.Code(), snapshot, resourceEvidence{})
	stable := s.allocationStopped && s.operationsAfterHold == s.operationsBeforeHold
	result.Evidence = append(result.Evidence, Evidence{Metric: "retained_session_observation", Actual: float64(s.operationsAfterHold - s.operationsBeforeHold), Available: stable})
	if !stable {
		result.Outcome = OutcomeFailed
		result.Message = "allocation continued during the retention observation window"
	}
	return result, nil
}
func (s *memoryRetentionScenario) ExecutionSnapshot() WorkerSnapshot {
	var combined WorkerSnapshot
	for _, scenario := range s.scenarios {
		part := scenario.ExecutionSnapshot()
		combined.Target += part.Target
		combined.Active += part.Active
		combined.Operations += part.Operations
		combined.Errors += part.Errors
		combined.TotalLatency += part.TotalLatency
		if combined.FirstError == "" {
			combined.FirstError = part.FirstError
		}
	}
	return combined
}

func (s *memoryRetentionScenario) Stop(ctx context.Context, _ *Runtime) error {
	var result error
	for _, scenario := range s.scenarios {
		if err := scenario.Stop(ctx, nil); err != nil && result == nil {
			result = err
		}
	}
	s.released = result == nil
	return result
}
func (s *memoryRetentionScenario) Restore(context.Context, *Runtime) error { return nil }

func (s *memoryRetentionScenario) operations() int64 {
	var total int64
	for _, scenario := range s.scenarios {
		total += scenario.workers.Snapshot().Operations
	}
	return total
}
