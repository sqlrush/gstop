package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type workMemKind uint8

const (
	workMemSort workMemKind = iota + 1
	workMemHash

	workMemCalibrationMaxAttempts  = 16
	workMemCalibrationMaxRange     = int64(1_048_576)
	workMemCalibrationLowerPercent = int64(70)
	workMemCalibrationUpperPercent = int64(97)
	workMemPlanDiagnosticMaxRunes  = 512
	workMemExplainNormal           = "normal"
	workMemHashPlanHint            = "/*+ leading((p h)) hashjoin(p h) set(enable_index_nestloop off) */ "
)

type workMemObservation struct {
	UsedKB                   int64
	TotalUsedKB              int64
	OperatorCount            int
	Spilled                  bool
	Batches                  int
	Plan                     string
	OriginalExplainPerfMode  string
	EffectiveExplainPerfMode string
}

type workMemCalibration struct {
	RangeEnd    int64
	Attempts    int
	Observation workMemObservation
	TargetMet   bool
}

type workMemProbe func(context.Context, int64) (workMemObservation, error)

type workMemScenario struct {
	code       ScenarioCode
	name       string
	kind       workMemKind
	workers    int
	targetKB   int64
	ready      chan int
	workload   *sqlWorkload
	run        *fixedWorkerRun
	calibrated workMemCalibration
}

var (
	workMemAmountRE = regexp.MustCompile(`(?i)\b(Memory|Disk):\s*([0-9]+)\s*kB\b`)
	hashMemoryRE    = regexp.MustCompile(`(?i)\bBuckets:.*?\bBatches:\s*([0-9]+)(?:\s*\([^)]*\))?.*?\bMemory Usage:\s*([0-9]+)\s*kB\b`)
)

func newWorkMemScenario(code ScenarioCode, name string) (*workMemScenario, error) {
	kind := workMemKind(0)
	switch code {
	case 201:
		kind = workMemSort
	case 202:
		kind = workMemHash
	default:
		return nil, fmt.Errorf("scenario %d has no calibrated work_mem workload", code)
	}
	return &workMemScenario{code: code, name: name, kind: kind}, nil
}

func (s *workMemScenario) Code() ScenarioCode { return s.code }
func (s *workMemScenario) Name() string       { return s.name }
func (s *workMemScenario) Strategy() string {
	if s.kind == workMemHash {
		return "work_mem_hash_fixed_workers"
	}
	return "work_mem_sort_fixed_workers"
}
func (*workMemScenario) OwnsWorkloadDuration() bool { return true }

func (s *workMemScenario) Prepare(ctx context.Context, rt *Runtime) error {
	if rt == nil || rt.Database == nil {
		return sql.ErrConnDone
	}
	s.workers, s.targetKB = s.configuredPressure(rt.Config)
	if s.workers <= 0 {
		return fmt.Errorf("%s workers must be positive", s.Name())
	}
	if s.targetKB <= 0 {
		return fmt.Errorf("%s work_mem must be positive", s.Name())
	}
	calibrated, err := calibrateWorkMemDatabase(
		ctx, rt, s.Name(), s.kind, s.targetKB,
	)
	s.calibrated = calibrated
	if err != nil {
		return err
	}
	if rt.Log != nil && !calibrated.TargetMet {
		rt.Log.Warn(
			"scenario=%s work_mem calibration target=70%%..97%% not reached requested=%dkB observed=%dkB observed_percent=%.2f calibrated_range=1..%d attempts=%d continuing_with_best_non_spilling_range=true",
			s.Name(), s.targetKB, calibrated.Observation.UsedKB,
			workMemObservedPercent(calibrated.Observation.UsedKB, s.targetKB),
			calibrated.RangeEnd, calibrated.Attempts,
		)
	}
	s.ready = make(chan int, s.workers)
	operation, cleanup, err := workMemWorkerOperations(
		s.kind,
		rt.Config.Data.Schema,
		calibrated.RangeEnd,
		s.targetKB,
		s.ready,
	)
	if err != nil {
		return err
	}
	start := make(chan struct{})
	s.workload = newSQLWorkloadWithoutOperationTimeoutWithStartGate(
		ctx, rt, s.Name(), s.workers, operation, start,
	)
	s.workload.cleanup = cleanup
	lane := strings.ToLower(workMemKindName(s.kind))
	s.run = newFixedWorkerRun(
		rt.Config.Run.Duration,
		start,
		fixedWorkerLane{Name: lane, Workers: s.workers, Workload: s.workload},
	)
	if err := s.workload.PrepareSessions(ctx, s.workers); err != nil {
		return err
	}
	if rt.Log != nil {
		rt.Log.Info(
			"scenario=%s workers=%d duration=%s work_mem=%dkB calibrated_range=1..%d operator_memory=%dkB observed_percent=%.2f target_met=%t operator_count=%d attempts=%d spill=false query_timeout=disabled",
			s.Name(), s.workers, rt.Config.Run.Duration, s.targetKB,
			s.calibrated.RangeEnd, s.calibrated.Observation.UsedKB,
			workMemObservedPercent(s.calibrated.Observation.UsedKB, s.targetKB),
			s.calibrated.TargetMet,
			s.calibrated.Observation.OperatorCount, s.calibrated.Attempts,
		)
	}
	return nil
}

func (s *workMemScenario) configuredPressure(cfg BenchConfig) (int, int64) {
	if s.kind == workMemHash {
		return cfg.MemoryWorkloads.HashWorkers, cfg.MemoryWorkloads.HashWorkMemKB
	}
	return cfg.MemoryWorkloads.SortWorkers, cfg.MemoryWorkloads.SortWorkMemKB
}

func (s *workMemScenario) Ramp(ctx context.Context, _ *Runtime) error {
	if s.run == nil || s.ready == nil || s.workers <= 0 {
		return fmt.Errorf("%s pressure workload is unavailable", s.Name())
	}
	if err := s.run.Ramp(ctx); err != nil {
		return err
	}
	s.run.ReleaseWorkers()
	return s.waitPressureReady(ctx)
}

func (s *workMemScenario) waitPressureReady(ctx context.Context) error {
	seen := make(map[int]struct{}, s.workers)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for len(seen) < s.workers {
		select {
		case workerID := <-s.ready:
			if workerID < 0 || workerID >= s.workers {
				return fmt.Errorf("%s reported invalid ready worker %d", s.Name(), workerID)
			}
			seen[workerID] = struct{}{}
		case <-ticker.C:
			snapshot := s.run.Snapshot()
			if err := workerSnapshotError(snapshot); err != nil {
				return fmt.Errorf("initialize %s pressure operators: %w", s.Name(), err)
			}
			if snapshot.Started >= s.workers && snapshot.Active < s.workers {
				return fmt.Errorf(
					"initialize %s pressure operators: active workers=%d require=%d",
					s.Name(), snapshot.Active, s.workers,
				)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *workMemScenario) Hold(ctx context.Context, rt *Runtime) error {
	if s.run == nil {
		return fmt.Errorf("%s pressure workload is unavailable", s.Name())
	}
	return s.run.Hold(ctx, fixedWorkerStopTimeout(rt))
}

func (s *workMemScenario) Verify(context.Context, *Runtime) (Result, error) {
	result := verifyFixedWorkerResult(
		s.Name(), s.run,
		map[string]string{strings.ToLower(workMemKindName(s.kind)): "workers"},
	)
	result.Evidence = append(result.Evidence, s.calibrationEvidence()...)
	return result, nil
}

func (s *workMemScenario) ExecutionSnapshot() WorkerSnapshot {
	if s.run == nil {
		return WorkerSnapshot{}
	}
	return s.run.Snapshot()
}

func (s *workMemScenario) RuntimeEvidence() []Evidence {
	if s.run == nil {
		return s.calibrationEvidence()
	}
	return append(
		fixedWorkerEvidence(
			s.run,
			map[string]string{strings.ToLower(workMemKindName(s.kind)): "workers"},
		),
		s.calibrationEvidence()...,
	)
}

func (s *workMemScenario) calibrationEvidence() []Evidence {
	observation := s.calibrated.Observation
	return []Evidence{
		{
			Metric: "work_mem_kb", Target: float64(s.targetKB),
			Actual: float64(observation.UsedKB), Available: observation.OperatorCount > 0,
			Details: map[string]any{
				"operator":                   workMemKindName(s.kind),
				"range_end":                  s.calibrated.RangeEnd,
				"attempts":                   s.calibrated.Attempts,
				"operator_count":             observation.OperatorCount,
				"total_used_kb":              observation.TotalUsedKB,
				"spilled":                    observation.Spilled,
				"batches":                    observation.Batches,
				"target_met":                 s.calibrated.TargetMet,
				"original_explain_perf_mode": observation.OriginalExplainPerfMode,
				"explain_perf_mode":          observation.EffectiveExplainPerfMode,
				"observed_percent": workMemObservedPercent(
					observation.UsedKB, s.targetKB,
				),
				"target_lower_percent": workMemCalibrationLowerPercent,
				"target_upper_percent": workMemCalibrationUpperPercent,
			},
		},
	}
}

func workMemObservedPercent(usedKB, targetKB int64) float64 {
	if targetKB <= 0 {
		return 0
	}
	return float64(usedKB) * 100 / float64(targetKB)
}

func (s *workMemScenario) Stop(ctx context.Context, _ *Runtime) error {
	if s.run == nil {
		return nil
	}
	return s.run.Stop(ctx)
}

func (*workMemScenario) Restore(context.Context, *Runtime) error { return nil }

func calibrateWorkMemDatabase(
	ctx context.Context,
	rt *Runtime,
	scenario string,
	kind workMemKind,
	targetKB int64,
) (workMemCalibration, error) {
	tagged, err := rt.Database.OpenTagged(
		ctx, rt.RunID, scenario, "calibration",
	)
	if err != nil {
		return workMemCalibration{}, fmt.Errorf("open work_mem calibration session: %w", err)
	}
	defer tagged.Close()
	originalExplainPerfMode, err := readWorkMemExplainPerfMode(ctx, tagged.Conn)
	if err != nil {
		return workMemCalibration{}, err
	}
	probe := func(probeCtx context.Context, rangeEnd int64) (workMemObservation, error) {
		queryTimeout := rt.Config.Safety.QueryTimeout
		if queryTimeout <= 0 {
			queryTimeout = 30 * time.Second
		}
		attemptCtx, cancelAttempt := context.WithTimeout(probeCtx, queryTimeout)
		defer cancelAttempt()
		return probeWorkMemOnConnection(
			attemptCtx, tagged.Conn, kind, rt.Config.Data.Schema,
			rangeEnd, targetKB, originalExplainPerfMode, queryTimeout,
		)
	}
	return calibrateWorkMemRange(ctx, targetKB, kind, probe)
}

func probeWorkMemOnConnection(
	ctx context.Context,
	conn *sql.Conn,
	kind workMemKind,
	schema string,
	rangeEnd int64,
	targetKB int64,
	originalExplainPerfMode string,
	cleanupTimeout time.Duration,
) (observation workMemObservation, resultErr error) {
	if conn == nil {
		return workMemObservation{}, sql.ErrConnDone
	}
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return workMemObservation{}, err
	}
	defer func() {
		if cleanupTimeout <= 0 {
			cleanupTimeout = 30 * time.Second
		}
		cleanupCtx, cancelCleanup := context.WithTimeout(
			context.Background(), cleanupTimeout,
		)
		defer cancelCleanup()
		if _, err := conn.ExecContext(cleanupCtx, "ROLLBACK"); err != nil &&
			!errors.Is(err, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, fmt.Errorf("rollback calibration: %w", err))
		}
	}()
	setup, err := workMemCalibrationSessionSetup(kind, targetKB)
	if err != nil {
		return workMemObservation{}, err
	}
	for _, statement := range setup {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return workMemObservation{}, err
		}
	}
	statement, err := workMemCalibrationSQL(kind, schema, rangeEnd)
	if err != nil {
		return workMemObservation{}, err
	}
	rows, err := conn.QueryContext(ctx, statement)
	if err != nil {
		return workMemObservation{}, err
	}
	plan, scanErr := scanExplainRows(rows)
	closeErr := rows.Close()
	if scanErr != nil || closeErr != nil {
		return workMemObservation{}, errors.Join(scanErr, closeErr)
	}
	return parseWorkMemPlanWithContext(kind, plan, originalExplainPerfMode)
}

func readWorkMemExplainPerfMode(ctx context.Context, conn *sql.Conn) (string, error) {
	if conn == nil {
		return "", sql.ErrConnDone
	}
	var mode string
	if err := conn.QueryRowContext(ctx, "SHOW explain_perf_mode").Scan(&mode); err != nil {
		return "", fmt.Errorf("read explain_perf_mode: %w", err)
	}
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return "", fmt.Errorf("read explain_perf_mode: database returned an empty value")
	}
	return mode, nil
}

func workMemCalibrationSQL(
	kind workMemKind,
	schema string,
	rangeEnd int64,
) (string, error) {
	table, err := workMemSortDataTable(schema, rangeEnd)
	if err != nil {
		return "", err
	}
	bound := strconv.FormatInt(rangeEnd, 10)
	switch kind {
	case workMemSort:
		return "EXPLAIN (ANALYZE, BUFFERS) " +
			"SELECT max(rn),sum(payload_len) FROM (" +
			"SELECT row_number() OVER (ORDER BY payload,sort_key DESC,id) AS rn," +
			"CAST(length(payload) AS bigint) AS payload_len FROM " + table +
			" WHERE dist_key BETWEEN 1 AND " + bound + ") AS gsbench_sorted", nil
	case workMemHash:
		return "EXPLAIN (ANALYZE, BUFFERS) " +
			"SELECT " + workMemHashPlanHint +
			"count(*),sum(length(h.payload)) FROM (" +
			"SELECT dist_key,id FROM " + table +
			" WHERE dist_key BETWEEN 1 AND " + bound + ") AS p LEFT JOIN (" +
			"SELECT dist_key,id,payload FROM " + table +
			" WHERE dist_key BETWEEN 1 AND " + bound + ") AS h " +
			"ON h.dist_key=p.dist_key AND h.id=p.id", nil
	default:
		return "", fmt.Errorf("unsupported work_mem operator kind %d", kind)
	}
}

func workMemCursorSQL(
	kind workMemKind,
	schema string,
	rangeEnd int64,
) (string, error) {
	table, err := workMemSortDataTable(schema, rangeEnd)
	if err != nil {
		return "", err
	}
	bound := strconv.FormatInt(rangeEnd, 10)
	switch kind {
	case workMemSort:
		return "SELECT id,sort_key,payload FROM " + table +
			" WHERE dist_key BETWEEN 1 AND " + bound +
			" ORDER BY payload,sort_key DESC,id", nil
	case workMemHash:
		return "SELECT " + workMemHashPlanHint +
			"p.id,h.payload FROM (SELECT dist_key,id FROM " + table +
			" WHERE dist_key BETWEEN 1 AND " + bound + ") AS p LEFT JOIN (" +
			"SELECT dist_key,id,payload FROM " + table +
			" WHERE dist_key BETWEEN 1 AND " + bound + ") AS h " +
			"ON h.dist_key=p.dist_key AND h.id=p.id", nil
	default:
		return "", fmt.Errorf("unsupported work_mem operator kind %d", kind)
	}
}

func workMemSortDataTable(schema string, rangeEnd int64) (string, error) {
	if rangeEnd <= 0 || rangeEnd > workMemCalibrationMaxRange {
		return "", fmt.Errorf(
			"work_mem calibration range must be within 1..%d",
			workMemCalibrationMaxRange,
		)
	}
	quoted, ok := quoteDatasetSchema(schema)
	if !ok {
		return "", fmt.Errorf("unsafe schema %q", schema)
	}
	return quoted + ".sort_data", nil
}

func workMemSessionSetup(kind workMemKind, targetKB int64) ([]string, error) {
	return buildWorkMemSessionSetup(kind, targetKB, false)
}

func workMemCalibrationSessionSetup(kind workMemKind, targetKB int64) ([]string, error) {
	return buildWorkMemSessionSetup(kind, targetKB, true)
}

func buildWorkMemSessionSetup(
	kind workMemKind,
	targetKB int64,
	forceNormalExplain bool,
) ([]string, error) {
	if targetKB <= 0 {
		return nil, fmt.Errorf("work_mem must be positive")
	}
	statements := []string{
		"SET LOCAL work_mem='" + strconv.FormatInt(targetKB, 10) + "kB'",
		"SET LOCAL query_dop=1",
	}
	if forceNormalExplain {
		statements = append(statements, "SET LOCAL explain_perf_mode=normal")
	}
	switch kind {
	case workMemSort:
		return statements, nil
	case workMemHash:
		return append(statements,
			"SET LOCAL enable_nestloop=off",
			"SET LOCAL enable_mergejoin=off",
			"SET LOCAL enable_hashjoin=on",
		), nil
	default:
		return nil, fmt.Errorf("unsupported work_mem operator kind %d", kind)
	}
}

func workMemWorkerOperations(
	kind workMemKind,
	schema string,
	rangeEnd int64,
	targetKB int64,
	ready chan<- int,
) (SQLWorkerOp, SQLWorkerOp, error) {
	cursorSQL, err := workMemCursorSQL(kind, schema, rangeEnd)
	if err != nil {
		return nil, nil, err
	}
	setup, err := workMemSessionSetup(kind, targetKB)
	if err != nil {
		return nil, nil, err
	}
	operation := func(ctx context.Context, conn *sql.Conn, workerID int) error {
		if conn == nil {
			return sql.ErrConnDone
		}
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			return err
		}
		for _, statement := range setup {
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		cursorName := workMemCursorName(workerID)
		if _, err := conn.ExecContext(
			ctx,
			"DECLARE "+cursorName+" NO SCROLL CURSOR FOR "+cursorSQL,
		); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "FETCH 1 FROM "+cursorName); err != nil {
			return err
		}
		select {
		case ready <- workerID:
		case <-ctx.Done():
			return nil
		}
		<-ctx.Done()
		return nil
	}
	cleanup := func(ctx context.Context, conn *sql.Conn, _ int) error {
		if conn == nil {
			return sql.ErrConnDone
		}
		var cleanupErr error
		for _, statement := range []string{"CLOSE ALL", "ROLLBACK"} {
			if _, err := conn.ExecContext(ctx, statement); err != nil &&
				!errors.Is(err, sql.ErrTxDone) {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
		return cleanupErr
	}
	return operation, cleanup, nil
}

func workMemCursorName(workerID int) string {
	return "gsbench_workmem_" + strconv.Itoa(workerID)
}

func parseWorkMemPlan(kind workMemKind, plan string) (workMemObservation, error) {
	observation := workMemObservation{Plan: plan}
	switch kind {
	case workMemSort:
		for _, line := range strings.Split(plan, "\n") {
			if !strings.Contains(strings.ToLower(line), "sort method:") {
				continue
			}
			observation.OperatorCount++
			amount := workMemAmountRE.FindStringSubmatch(line)
			if len(amount) != 3 {
				return workMemObservation{}, fmt.Errorf(
					"Sort operator did not report Memory or Disk usage",
				)
			}
			usedKB, err := strconv.ParseInt(amount[2], 10, 64)
			if err != nil {
				return workMemObservation{}, fmt.Errorf("parse Sort usage: invalid integer")
			}
			if strings.EqualFold(amount[1], "Disk") {
				observation.Spilled = true
				continue
			}
			if !strings.Contains(strings.ToLower(line), "sort method: quicksort") {
				return workMemObservation{}, fmt.Errorf(
					"Sort operator did not use in-memory quicksort",
				)
			}
			observation.TotalUsedKB += usedKB
			if usedKB > observation.UsedKB {
				observation.UsedKB = usedKB
			}
		}
	case workMemHash:
		for _, match := range hashMemoryRE.FindAllStringSubmatch(plan, -1) {
			batches, err := strconv.Atoi(match[1])
			if err != nil {
				return workMemObservation{}, fmt.Errorf("parse Hash batches: invalid integer")
			}
			usedKB, err := strconv.ParseInt(match[2], 10, 64)
			if err != nil {
				return workMemObservation{}, fmt.Errorf("parse Hash usage: invalid integer")
			}
			observation.OperatorCount++
			observation.TotalUsedKB += usedKB
			if usedKB > observation.UsedKB {
				observation.UsedKB = usedKB
			}
			if batches > observation.Batches {
				observation.Batches = batches
			}
			if batches > 1 {
				observation.Spilled = true
			}
		}
	default:
		return workMemObservation{}, fmt.Errorf("unsupported work_mem operator kind %d", kind)
	}
	if observation.OperatorCount == 0 {
		return workMemObservation{}, fmt.Errorf(
			"EXPLAIN ANALYZE did not report the requested %s operator",
			workMemKindName(kind),
		)
	}
	return observation, nil
}

func parseWorkMemPlanWithContext(
	kind workMemKind,
	plan string,
	originalExplainPerfMode string,
) (workMemObservation, error) {
	detectedOutputMode := detectWorkMemExplainOutputMode(plan)
	if detectedOutputMode != workMemExplainNormal {
		return workMemObservation{}, fmt.Errorf(
			"parse work_mem plan: original_explain_perf_mode=%q requested_explain_perf_mode=%q detected_output_mode=%q plan=%s: EXPLAIN ANALYZE %s output cannot validate requested %s operator",
			originalExplainPerfMode,
			workMemExplainNormal,
			detectedOutputMode,
			workMemPlanDiagnostic(plan),
			detectedOutputMode,
			workMemKindName(kind),
		)
	}
	observation, err := parseWorkMemPlan(kind, plan)
	if err != nil {
		return workMemObservation{}, fmt.Errorf(
			"parse work_mem plan: original_explain_perf_mode=%q requested_explain_perf_mode=%q detected_output_mode=%q plan=%s: %w",
			originalExplainPerfMode,
			workMemExplainNormal,
			detectedOutputMode,
			workMemPlanDiagnostic(plan),
			err,
		)
	}
	observation.OriginalExplainPerfMode = originalExplainPerfMode
	observation.EffectiveExplainPerfMode = workMemExplainNormal
	return observation, nil
}

func detectWorkMemExplainOutputMode(plan string) string {
	normalized := strings.ToLower(plan)
	if strings.Contains(normalized, "operation") &&
		strings.Contains(normalized, "peak memory") {
		return "pretty"
	}
	return workMemExplainNormal
}

func workMemPlanDiagnostic(plan string) string {
	runes := []rune(plan)
	if len(runes) > workMemPlanDiagnosticMaxRunes {
		plan = string(runes[:workMemPlanDiagnosticMaxRunes]) + "...<truncated>"
	}
	return strconv.QuoteToASCII(plan)
}

func calibrateWorkMemRange(
	ctx context.Context,
	targetKB int64,
	kind workMemKind,
	probe workMemProbe,
) (workMemCalibration, error) {
	if targetKB <= 0 {
		return workMemCalibration{}, fmt.Errorf("work_mem must be positive")
	}
	if probe == nil {
		return workMemCalibration{}, fmt.Errorf("work_mem calibration probe is unavailable")
	}
	lowerKB := (targetKB*workMemCalibrationLowerPercent + 99) / 100
	upperKB := targetKB * workMemCalibrationUpperPercent / 100
	if upperKB < lowerKB {
		upperKB = lowerKB
	}
	low, high := int64(1), workMemCalibrationMaxRange
	bracketed := false
	candidate := initialWorkMemRange(targetKB, kind)
	var last, bestBelow, bestAbove workMemCalibration
	for attempt := 1; attempt <= workMemCalibrationMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return workMemCalibration{}, err
		}
		observation, err := probe(ctx, candidate)
		if err != nil {
			return workMemCalibration{}, fmt.Errorf(
				"work_mem calibration attempt %d range 1..%d: %w",
				attempt, candidate, err,
			)
		}
		if observation.OperatorCount <= 0 {
			return workMemCalibration{}, fmt.Errorf(
				"work_mem calibration attempt %d observed no %s operator",
				attempt, workMemKindName(kind),
			)
		}
		last = workMemCalibration{
			RangeEnd: candidate, Attempts: attempt, Observation: observation,
		}
		if !observation.Spilled && observation.UsedKB > 0 {
			switch {
			case observation.UsedKB < lowerKB &&
				observation.UsedKB > bestBelow.Observation.UsedKB:
				bestBelow = last
			case observation.UsedKB > upperKB &&
				(bestAbove.Attempts == 0 ||
					observation.UsedKB < bestAbove.Observation.UsedKB):
				bestAbove = last
			}
		}
		if !observation.Spilled &&
			observation.UsedKB >= lowerKB && observation.UsedKB <= upperKB {
			last.TargetMet = true
			return last, nil
		}
		if observation.Spilled || observation.UsedKB > upperKB {
			high = candidate - 1
			bracketed = true
		} else {
			low = candidate + 1
		}
		if bracketed {
			if low > high {
				break
			}
			candidate = low + (high-low)/2
			continue
		}
		if candidate >= workMemCalibrationMaxRange {
			break
		}
		candidate *= 2
		if candidate < low {
			candidate = low
		}
		if candidate > workMemCalibrationMaxRange {
			candidate = workMemCalibrationMaxRange
		}
	}
	fallback := bestBelow
	if fallback.Attempts == 0 {
		fallback = bestAbove
	}
	if fallback.Attempts > 0 {
		fallback.Attempts = last.Attempts
		return fallback, nil
	}
	return last, fmt.Errorf(
		"could not calibrate %s memory: no usable non-spilling observation after %d attempts (target=70%%..97%% of %dkB; last range 1..%d used=%dkB spilled=%t)",
		workMemKindName(kind), last.Attempts, targetKB,
		last.RangeEnd, last.Observation.UsedKB, last.Observation.Spilled,
	)
}

func initialWorkMemRange(targetKB int64, kind workMemKind) int64 {
	divisor := int64(14)
	if kind == workMemHash {
		divisor = 8
	}
	candidate := targetKB / divisor
	if candidate < 1 {
		return 1
	}
	if candidate > workMemCalibrationMaxRange {
		return workMemCalibrationMaxRange
	}
	return candidate
}

func workMemKindName(kind workMemKind) string {
	switch kind {
	case workMemSort:
		return "Sort"
	case workMemHash:
		return "Hash"
	default:
		return "unknown"
	}
}
