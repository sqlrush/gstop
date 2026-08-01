package gsbench

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
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
		workload.Statement = "SELECT id,customer_id,amount,payload FROM " + fact + " WHERE id BETWEEN $1 AND $2 ORDER BY payload,amount DESC,id"
	case 202:
		workload.Setup, workload.Cleanup = "SET work_mem='256MB'", "RESET work_mem"
		workload.Statement = "SELECT f1.product_id,sum(f1.amount),count(*) FROM " + fact + " f1 JOIN " + fact + " f2 ON f1.customer_id=f2.customer_id WHERE mod(f1.id,16)=0 GROUP BY f1.product_id ORDER BY sum(f1.amount) DESC"
	case 203, 301:
		workload.Statement = "SELECT sum(amount),avg(quantity),count(payload) FROM " + fact + " WHERE id BETWEEN $1 AND $2"
	case 204:
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
		workload.Statement = "SELECT id,customer_id,product_id,store_id,amount,payload FROM " + fact + " WHERE id BETWEEN $1 AND $2"
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
	code     ScenarioCode
	name     string
	workload ResourceWorkload
	workers  *sqlWorkload
	evidence resourceEvidence
	sequence atomic.Int64
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
	s.workers = newSQLWorkloadWithCleanup(ctx, rt, s.name, rt.Config.Safety.MaxWorkers, s.operation(rt), func(ctx context.Context, conn *sql.Conn, _ int) error {
		for _, statement := range strings.Split(s.workload.Cleanup, ";") {
			if statement = strings.TrimSpace(statement); statement != "" {
				if _, err := conn.ExecContext(ctx, statement); err != nil && err != sql.ErrTxDone {
					return err
				}
			}
		}
		return nil
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
			_, err := conn.ExecContext(ctx, resourcePrepareStatement(rt.RunID, n, s.workload.Statement))
			return err
		case 205:
			if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil && !strings.Contains(strings.ToLower(err.Error()), "transaction") {
				return err
			}
			name := "gsbench_cur_" + strconv.FormatInt(n, 10)
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

func (s *resourceScenario) observeRequiredStream(ctx context.Context, rt *Runtime) error {
	rows, err := rt.Database.Query(ctx, "EXPLAIN "+s.workload.Statement)
	if err != nil {
		return err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.evidence.plan = strings.Join(lines, "\n")
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
	return "PREPARE gsbench_pc_" + resourceIdentifier(runID) + "_" + strconv.FormatInt(sequence, 10) + "(bigint,bigint) AS " + statement
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
	if s.target < 1 || s.target > rt.Config.Safety.MaxWorkers {
		return fmt.Errorf("connection churn workers %d exceed safety maximum %d", s.target, rt.Config.Safety.MaxWorkers)
	}
	s.op = newConnectionChurnOperation(workload.Statement, func(opCtx context.Context, sequence int64) (churnConnection, error) {
		conn, err := rt.Database.OpenTagged(opCtx, rt.RunID, s.name, "churn-"+strconv.FormatInt(sequence, 10))
		if err != nil {
			return nil, err
		}
		return taggedChurnConnection{tagged: conn}, nil
	})
	s.group = NewWorkerGroup(ctx, rt.Config.Safety.MaxWorkers, s.op.Run)
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
	if capacity < 1 || safetyMaximum < 1 {
		return 0, fmt.Errorf("resource pressure capacity and safety maximum must be positive")
	}
	target := capacity + 1
	if target > safetyMaximum {
		return 0, fmt.Errorf("safety maximum %d cannot exceed resource capacity %d", safetyMaximum, capacity)
	}
	return target, nil
}

type resourcePressureScenario struct {
	*resourceScenario
	target   int
	capacity int
}

func newResourcePressureScenario(code ScenarioCode, name string) *resourcePressureScenario {
	return &resourcePressureScenario{resourceScenario: newResourceScenario(code, name)}
}

func (s *resourcePressureScenario) Strategy() string {
	return "thread_pool_queue_multi_session_pressure"
}

func (s *resourcePressureScenario) Prepare(ctx context.Context, rt *Runtime) error {
	if err := s.resourceScenario.Prepare(ctx, rt); err != nil {
		return err
	}
	capacity := runtimeInt(rt, "scenario.threadpool_queue.worker_capacity", max(1, rt.Config.Safety.MaxWorkers/2))
	target, err := resourcePressureTarget(s.code, capacity, rt.Config.Safety.MaxWorkers)
	if err != nil {
		return err
	}
	s.capacity, s.target = capacity, target
	return nil
}

func (s *resourcePressureScenario) Ramp(context.Context, *Runtime) error {
	return s.workers.SetTarget(s.target)
}

func (s *resourcePressureScenario) Verify(ctx context.Context, rt *Runtime) (Result, error) {
	result, err := s.resourceScenario.Verify(ctx, rt)
	if err != nil {
		return result, err
	}
	result.Evidence = append(result.Evidence,
		Evidence{Metric: "configured_resource_capacity", Actual: float64(s.capacity), Available: true},
		Evidence{Metric: "concurrent_pressure_sessions", Target: float64(s.target), Actual: float64(s.workers.Snapshot().Target), Available: true},
	)
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
	control   ControlResult
}

func newTotalMemoryScenario(name string) *totalMemoryScenario {
	return &totalMemoryScenario{name: name}
}
func (s *totalMemoryScenario) Code() ScenarioCode { return 207 }
func (s *totalMemoryScenario) Name() string       { return s.name }
func (s *totalMemoryScenario) Strategy() string   { return "composite_total_memory_controller" }

func (s *totalMemoryScenario) Prepare(ctx context.Context, rt *Runtime) error {
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
		s.composite.scenarios = append(s.composite.scenarios, child)
	}
	return nil
}

func (s *totalMemoryScenario) Ramp(ctx context.Context, rt *Runtime) error {
	minimum := len(s.composite.scenarios)
	if rt.Config.Safety.MaxWorkers < minimum {
		return fmt.Errorf("total memory requires at least %d safe workers", minimum)
	}
	s.control = (Controller{Config: ControllerConfig{Target: float64(runtimeInt(rt, "scenario.memory_total_pressure.target_percent", 90)), MinWorkers: minimum, MaxWorkers: minimum, Step: 1, RequiredSamples: 1, Interval: rt.Config.Run.RampInterval}, Actuator: &s.composite, Sample: func(context.Context) Sample {
		return Sample{Available: false, Errors: s.composite.Snapshot().Errors}
	}}).Run(ctx)
	return s.control.Err
}
func (s *totalMemoryScenario) Hold(ctx context.Context, rt *Runtime) error {
	return waitContext(ctx, rt.Config.Run.Duration)
}
func (s *totalMemoryScenario) Verify(context.Context, *Runtime) (Result, error) {
	result := verifyResourceWorkload(s.Code(), s.composite.Snapshot(), resourceEvidence{})
	result.Evidence = append(result.Evidence, Evidence{Metric: "composed_memory_mechanisms", Target: 4, Actual: float64(len(s.composite.scenarios)), Available: len(s.composite.scenarios) == 4})
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
