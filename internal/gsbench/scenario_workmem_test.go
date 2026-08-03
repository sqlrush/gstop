package gsbench

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

type workMemProbeTestConnector struct {
	statements      *[]string
	plan            string
	explainPerfMode string
}

func (c *workMemProbeTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &workMemProbeTestConn{
		statements: c.statements, plan: c.plan,
		explainPerfMode: c.explainPerfMode,
	}, nil
}

func (*workMemProbeTestConnector) Driver() driver.Driver { return workMemProbeTestDriver{} }

type workMemProbeTestDriver struct{}

func (workMemProbeTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use connector")
}

type workMemProbeTestConn struct {
	statements      *[]string
	plan            string
	explainPerfMode string
}

func (*workMemProbeTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (*workMemProbeTestConn) Close() error { return nil }

func (*workMemProbeTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *workMemProbeTestConn) ExecContext(
	_ context.Context,
	statement string,
	_ []driver.NamedValue,
) (driver.Result, error) {
	*c.statements = append(*c.statements, statement)
	return driver.RowsAffected(1), nil
}

func (c *workMemProbeTestConn) QueryContext(
	_ context.Context,
	statement string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	*c.statements = append(*c.statements, statement)
	if statement == "SHOW explain_perf_mode" {
		return &explainRowsTestRows{
			columns: []string{"explain_perf_mode"},
			values:  [][]driver.Value{{c.explainPerfMode}},
		}, nil
	}
	return &explainRowsTestRows{
		columns: []string{"QUERY PLAN"},
		values:  [][]driver.Value{{c.plan}},
	}, nil
}

func openWorkMemProbeTestConn(
	t *testing.T,
	statements *[]string,
	explainPerfMode string,
	plan string,
) *sql.Conn {
	t.Helper()
	pool := sql.OpenDB(&workMemProbeTestConnector{
		statements: statements, plan: plan, explainPerfMode: explainPerfMode,
	})
	t.Cleanup(func() { _ = pool.Close() })
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestWorkMemSQLUsesBoundedSortDataOperators(t *testing.T) {
	tests := []struct {
		name       string
		kind       workMemKind
		want       []string
		forbidden  []string
		wantCursor []string
	}{
		{
			name: "sort",
			kind: workMemSort,
			want: []string{
				`EXPLAIN (ANALYZE`, `"select".sort_data`,
				`dist_key BETWEEN 1 AND 18462`, `row_number() OVER`,
				`ORDER BY payload,sort_key DESC,id`, `max(rn)`,
			},
			forbidden:  []string{"fact_sales"},
			wantCursor: []string{`"select".sort_data`, `ORDER BY payload,sort_key DESC,id`},
		},
		{
			name: "hash",
			kind: workMemHash,
			want: []string{
				`EXPLAIN (ANALYZE`, `"select".sort_data`,
				`dist_key BETWEEN 1 AND 18462`, `LEFT JOIN`,
				`length(h.payload)`,
			},
			forbidden:  []string{"fact_sales", "GROUP BY", "ORDER BY"},
			wantCursor: []string{`"select".sort_data`, `LEFT JOIN`, `h.payload`},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calibration, err := workMemCalibrationSQL(test.kind, "select", 18462)
			if err != nil {
				t.Fatal(err)
			}
			cursor, err := workMemCursorSQL(test.kind, "select", 18462)
			if err != nil {
				t.Fatal(err)
			}
			for _, fragment := range test.want {
				if !strings.Contains(calibration, fragment) {
					t.Errorf("calibration SQL missing %q:\n%s", fragment, calibration)
				}
			}
			for _, fragment := range test.forbidden {
				if strings.Contains(calibration, fragment) {
					t.Errorf("calibration SQL contains forbidden %q:\n%s", fragment, calibration)
				}
			}
			for _, fragment := range test.wantCursor {
				if !strings.Contains(cursor, fragment) {
					t.Errorf("cursor SQL missing %q:\n%s", fragment, cursor)
				}
			}
		})
	}

	if _, err := workMemCalibrationSQL(workMemSort, "unsafe-name", 10); err == nil {
		t.Fatal("unsafe schema was accepted")
	}
	if _, err := workMemCursorSQL(workMemHash, "gsbench", 0); err == nil {
		t.Fatal("zero range was accepted")
	}
}

func TestParseWorkMemPlanMeasuresOnlyTheRequestedOperator(t *testing.T) {
	tests := []struct {
		name      string
		kind      workMemKind
		plan      string
		wantUsed  int64
		wantSum   int64
		wantCount int
		wantSpill bool
	}{
		{
			name: "two in-memory sorts",
			kind: workMemSort,
			plan: "Sort Method: quicksort  Memory: 221184kB\n" +
				"Sort Method: quicksort  Memory: 245760kB",
			wantUsed: 245760, wantSum: 466944, wantCount: 2,
		},
		{
			name:      "sort spill",
			kind:      workMemSort,
			plan:      "Sort Method: external merge  Disk: 131072kB",
			wantCount: 1,
			wantSpill: true,
		},
		{
			name:      "single-batch hash",
			kind:      workMemHash,
			plan:      "Hash  (actual time=1..2 rows=1 loops=1)\n  Buckets: 262144  Batches: 1  Memory Usage: 240000kB",
			wantUsed:  240000,
			wantSum:   240000,
			wantCount: 1,
		},
		{
			name:      "multi-batch hash spill",
			kind:      workMemHash,
			plan:      "Buckets: 65536 (originally 131072)  Batches: 4 (originally 1)  Memory Usage: 32768kB",
			wantUsed:  32768,
			wantSum:   32768,
			wantCount: 1,
			wantSpill: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseWorkMemPlan(test.kind, test.plan)
			if err != nil {
				t.Fatal(err)
			}
			if got.UsedKB != test.wantUsed || got.TotalUsedKB != test.wantSum ||
				got.OperatorCount != test.wantCount || got.Spilled != test.wantSpill {
				t.Fatalf("observation=%+v", got)
			}
		})
	}

	if _, err := parseWorkMemPlan(workMemSort, "Seq Scan on sort_data"); err == nil {
		t.Fatal("plan without a Sort operator was accepted")
	}
	if _, err := parseWorkMemPlan(
		workMemSort,
		"Sort Method: top-N heapsort  Memory: 240000kB",
	); err == nil {
		t.Fatal("non-quicksort memory operator was accepted")
	}
}

func TestWorkMemPlanParseErrorIdentifiesPrettyOutput(t *testing.T) {
	plan := "id | operation | A-rows | Peak Memory\n" +
		"1 | -> Sort | 1000 | 240000 KB"
	_, err := parseWorkMemPlanWithContext(workMemSort, plan, "pretty")
	if err == nil {
		t.Fatal("pretty plan was accepted as normal work_mem evidence")
	}
	message := err.Error()
	for _, fragment := range []string{
		`original_explain_perf_mode="pretty"`,
		`requested_explain_perf_mode="normal"`,
		`detected_output_mode="pretty"`,
		"requested Sort operator",
	} {
		if !strings.Contains(message, fragment) {
			t.Errorf("error=%q missing %q", message, fragment)
		}
	}
}

func TestWorkMemPlanRejectsPrettyHashWithNormalStyleMemoryDetail(t *testing.T) {
	plan := "id | operation | A-rows | Peak Memory\n" +
		"1 | -> Hash Join (2,3) | 1000 | 430 KB\n" +
		"3 | -> Hash | 1000 | 430 KB\n" +
		"Buckets: 32768  Batches: 1  Memory Usage: 430kB"
	_, err := parseWorkMemPlanWithContext(workMemHash, plan, "pretty")
	if err == nil {
		t.Fatal("pretty Hash plan was accepted through its normal-style memory detail")
	}
	message := err.Error()
	if !strings.Contains(message, `detected_output_mode="pretty"`) ||
		!strings.Contains(message, "requested Hash operator") {
		t.Fatalf("error=%q want explicit pretty Hash rejection", message)
	}
}

func TestWorkMemPlanDiagnosticIsEscapedAndTruncated(t *testing.T) {
	plan := "id | operation | Peak Memory\n\t" +
		strings.Repeat("界", 600) + "UNIQUE_SUFFIX_AFTER_LIMIT"
	_, err := parseWorkMemPlanWithContext(workMemHash, plan, "pretty")
	if err == nil {
		t.Fatal("unparseable plan was accepted")
	}
	message := err.Error()
	for _, fragment := range []string{`\n`, `\t`, "truncated"} {
		if !strings.Contains(message, fragment) {
			t.Errorf("error=%q missing escaped diagnostic %q", message, fragment)
		}
	}
	if strings.Contains(message, "UNIQUE_SUFFIX_AFTER_LIMIT") {
		t.Fatalf("error exposed plan content beyond the diagnostic limit: %q", message)
	}
}

func TestWorkMemPlanParseErrorDoesNotBypassDiagnosticLimit(t *testing.T) {
	plan := "Sort Method: " + strings.Repeat("x", 600) +
		"UNIQUE_SUFFIX_FROM_RAW_PARSER_ERROR"
	_, err := parseWorkMemPlanWithContext(workMemSort, plan, "normal")
	if err == nil {
		t.Fatal("malformed Sort plan was accepted")
	}
	message := err.Error()
	if !strings.Contains(message, "truncated") {
		t.Fatalf("error=%q want a truncated plan diagnostic", message)
	}
	if strings.Contains(message, "UNIQUE_SUFFIX_FROM_RAW_PARSER_ERROR") {
		t.Fatalf("error bypassed the plan diagnostic limit: %q", message)
	}
}

func TestReadWorkMemExplainPerfMode(t *testing.T) {
	statements := []string{}
	conn := openWorkMemProbeTestConn(t, &statements, "pretty", "")
	mode, err := readWorkMemExplainPerfMode(context.Background(), conn)
	if err != nil || mode != "pretty" {
		t.Fatalf("mode=%q err=%v", mode, err)
	}
	if len(statements) != 1 || statements[0] != "SHOW explain_perf_mode" {
		t.Fatalf("statements=%v", statements)
	}
}

func TestProbeWorkMemUsesOneTransactionForSettingsAndExplain(t *testing.T) {
	statements := []string{}
	conn := openWorkMemProbeTestConn(
		t, &statements, "pretty",
		"Sort Method: quicksort  Memory: 240000kB",
	)
	observation, err := probeWorkMemOnConnection(
		context.Background(), conn, workMemSort, "gsbench",
		18462, 256*1024, "pretty", time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if observation.UsedKB != 240000 || observation.Spilled ||
		observation.OriginalExplainPerfMode != "pretty" ||
		observation.EffectiveExplainPerfMode != "normal" {
		t.Fatalf("observation=%+v", observation)
	}
	want := []string{
		"BEGIN",
		"SET LOCAL work_mem='262144kB'",
		"SET LOCAL query_dop=1",
		"SET LOCAL explain_perf_mode=normal",
	}
	if len(statements) != 6 {
		t.Fatalf("statements=%v", statements)
	}
	for index, statement := range want {
		if statements[index] != statement {
			t.Errorf("statement[%d]=%q want=%q", index, statements[index], statement)
		}
	}
	if !strings.HasPrefix(statements[4], "EXPLAIN (ANALYZE, BUFFERS)") {
		t.Errorf("probe query=%q", statements[4])
	}
	if statements[5] != "ROLLBACK" {
		t.Errorf("last statement=%q want ROLLBACK", statements[5])
	}
}

func TestCalibrateWorkMemRangeFindsSeventyToNinetySevenPercent(t *testing.T) {
	const targetKB int64 = 256 * 1024
	calls := 0
	calibration, err := calibrateWorkMemRange(
		context.Background(), targetKB, workMemSort,
		func(_ context.Context, rangeEnd int64) (workMemObservation, error) {
			calls++
			return workMemObservation{
				UsedKB:        rangeEnd * 13,
				TotalUsedKB:   rangeEnd * 13,
				OperatorCount: 1,
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	lower := targetKB * 70 / 100
	upper := targetKB * 97 / 100
	if calibration.Observation.UsedKB < lower || calibration.Observation.UsedKB > upper {
		t.Fatalf("used=%dkB want %d..%dkB", calibration.Observation.UsedKB, lower, upper)
	}
	if !calibration.TargetMet {
		t.Fatal("preferred calibration target was not marked met")
	}
	if calibration.RangeEnd <= 0 || calls != calibration.Attempts || calls > workMemCalibrationMaxAttempts {
		t.Fatalf("calibration=%+v calls=%d", calibration, calls)
	}
}

func TestCalibrateWorkMemRangeBracketsSmallTargetBeforeBinarySearch(t *testing.T) {
	calibration, err := calibrateWorkMemRange(
		context.Background(), 64, workMemSort,
		func(_ context.Context, rangeEnd int64) (workMemObservation, error) {
			return workMemObservation{
				UsedKB: rangeEnd, TotalUsedKB: rangeEnd, OperatorCount: 1,
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calibration.Observation.UsedKB < 45 || calibration.Observation.UsedKB > 62 {
		t.Fatalf("calibration=%+v want 45..62kB", calibration)
	}
	if calibration.Attempts > workMemCalibrationMaxAttempts {
		t.Fatalf("attempts=%d max=%d", calibration.Attempts, workMemCalibrationMaxAttempts)
	}
}

func TestCalibrateWorkMemRangeBacksOffFromSpill(t *testing.T) {
	const targetKB int64 = 256 * 1024
	sawSpill := false
	calibration, err := calibrateWorkMemRange(
		context.Background(), targetKB, workMemSort,
		func(_ context.Context, rangeEnd int64) (workMemObservation, error) {
			used := rangeEnd * 14
			if used > targetKB*97/100 {
				sawSpill = true
				return workMemObservation{OperatorCount: 1, Spilled: true}, nil
			}
			return workMemObservation{UsedKB: used, TotalUsedKB: used, OperatorCount: 1}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !sawSpill {
		t.Fatal("calibration never exercised spill backoff")
	}
	if calibration.Observation.Spilled {
		t.Fatalf("spill was accepted as calibrated: %+v", calibration)
	}
}

func TestCalibrateWorkMemRangeContinuesWhenTargetCannotBeReached(t *testing.T) {
	calibration, err := calibrateWorkMemRange(
		context.Background(), 256*1024, workMemHash,
		func(context.Context, int64) (workMemObservation, error) {
			return workMemObservation{UsedKB: 1024, TotalUsedKB: 1024, OperatorCount: 1}, nil
		},
	)
	if err != nil {
		t.Fatalf("fallback calibration returned error: %v", err)
	}
	if calibration.Attempts == 0 || calibration.RangeEnd == 0 ||
		calibration.Observation.OperatorCount != 1 {
		t.Fatalf("fallback calibration lost its evidence: %+v", calibration)
	}
	if calibration.TargetMet {
		t.Fatalf("unreachable calibration target was marked met: %+v", calibration)
	}
}

func TestCalibrationFallbackKeepsBestNonSpillingEvidence(t *testing.T) {
	calibration, err := calibrateWorkMemRange(
		context.Background(), 256*1024, workMemHash,
		func(_ context.Context, rangeEnd int64) (workMemObservation, error) {
			if rangeEnd > 5000 {
				return workMemObservation{OperatorCount: 1, Spilled: true}, nil
			}
			used := rangeEnd * 10
			return workMemObservation{
				UsedKB: used, TotalUsedKB: used, OperatorCount: 1,
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("fallback calibration returned error: %v", err)
	}
	if calibration.Observation.Spilled || calibration.Observation.UsedKB == 0 {
		t.Fatalf("fallback=%+v want best in-memory observation", calibration)
	}
	if calibration.TargetMet {
		t.Fatalf("fallback unexpectedly marked target met: %+v", calibration)
	}
}

func TestCalibrateWorkMemRangeAcceptsOpenGaussEarlySpillCliff(t *testing.T) {
	calibration, err := calibrateWorkMemRange(
		context.Background(), 64*1024, workMemSort,
		func(_ context.Context, rangeEnd int64) (workMemObservation, error) {
			if rangeEnd > 3455 {
				return workMemObservation{OperatorCount: 1, Spilled: true}, nil
			}
			return workMemObservation{
				UsedKB: 47095, TotalUsedKB: 47095, OperatorCount: 1,
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !calibration.TargetMet || calibration.Observation.Spilled {
		t.Fatalf("calibration=%+v want accepted non-spilling 70%% result", calibration)
	}
}

func TestCalibrateWorkMemRangeContinuesWithBestNonSpillingFallback(t *testing.T) {
	calibration, err := calibrateWorkMemRange(
		context.Background(), 256*1024, workMemSort,
		func(_ context.Context, rangeEnd int64) (workMemObservation, error) {
			if rangeEnd > 3455 {
				return workMemObservation{OperatorCount: 1, Spilled: true}, nil
			}
			return workMemObservation{
				UsedKB: 47095, TotalUsedKB: 47095, OperatorCount: 1,
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("fallback calibration returned error: %v", err)
	}
	if calibration.TargetMet || calibration.Observation.Spilled ||
		calibration.Observation.UsedKB != 47095 {
		t.Fatalf("calibration=%+v want unmet non-spilling fallback", calibration)
	}
}

func TestCalibrateWorkMemRangeFailsWithoutNonSpillingObservation(t *testing.T) {
	_, err := calibrateWorkMemRange(
		context.Background(), 64*1024, workMemSort,
		func(context.Context, int64) (workMemObservation, error) {
			return workMemObservation{OperatorCount: 1, Spilled: true}, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "no usable non-spilling") {
		t.Fatalf("error=%v want no usable non-spilling error", err)
	}
}

func TestWorkMemCalibrationEvidenceReportsTargetMiss(t *testing.T) {
	scenario := &workMemScenario{
		kind:     workMemSort,
		targetKB: 256 * 1024,
		calibrated: workMemCalibration{
			RangeEnd: 3455,
			Attempts: 13,
			Observation: workMemObservation{
				UsedKB: 47095, TotalUsedKB: 47095, OperatorCount: 1,
				OriginalExplainPerfMode:  "pretty",
				EffectiveExplainPerfMode: "normal",
			},
		},
	}
	details := scenario.calibrationEvidence()[0].Details
	if details["target_met"] != false {
		t.Fatalf("details=%v want target_met=false", details)
	}
	observedPercent, ok := details["observed_percent"].(float64)
	if !ok || observedPercent < 17.9 || observedPercent > 18.0 {
		t.Fatalf("observed_percent=%v want about 17.97", details["observed_percent"])
	}
	if details["target_lower_percent"] != int64(70) ||
		details["target_upper_percent"] != int64(97) {
		t.Fatalf("details=%v want target band 70..97", details)
	}
	if details["original_explain_perf_mode"] != "pretty" ||
		details["explain_perf_mode"] != "normal" {
		t.Fatalf("details=%v want pretty-to-normal compatibility evidence", details)
	}
}

func TestWorkMemWorkerHoldsOneCalibratedCursorUntilCancellation(t *testing.T) {
	state := &resourceExecTestState{}
	conn := openResourceExecTestConn(t, state)
	ready := make(chan int, 1)
	op, cleanup, err := workMemWorkerOperations(
		workMemSort, "gsbench", 18462, 256*1024, ready,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- op(ctx, conn, 7) }()
	select {
	case workerID := <-ready:
		if workerID != 7 {
			t.Fatalf("ready worker=%d want=7", workerID)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not finish DECLARE/FETCH")
	}
	select {
	case err := <-done:
		t.Fatalf("worker returned before cancellation: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("canceled worker error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not return immediately after cancellation")
	}
	if err := cleanup(context.Background(), conn, 7); err != nil {
		t.Fatal(err)
	}

	wantPrefixes := []string{
		"BEGIN",
		"SET LOCAL work_mem='262144kB'",
		"SET LOCAL query_dop=1",
		"DECLARE gsbench_workmem_7 NO SCROLL CURSOR FOR ",
		"FETCH 1 FROM gsbench_workmem_7",
		"CLOSE ALL",
		"ROLLBACK",
	}
	if len(state.statements) != len(wantPrefixes) {
		t.Fatalf("statements=%v", state.statements)
	}
	for index, prefix := range wantPrefixes {
		if !strings.HasPrefix(state.statements[index], prefix) {
			t.Errorf("statement[%d]=%q want prefix %q", index, state.statements[index], prefix)
		}
	}
}

func TestWorkMemHashWorkerForcesOneHashStrategy(t *testing.T) {
	state := &resourceExecTestState{}
	conn := openResourceExecTestConn(t, state)
	ready := make(chan int, 1)
	op, cleanup, err := workMemWorkerOperations(
		workMemHash, "gsbench", 12000, 128*1024, ready,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- op(ctx, conn, 0) }()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("hash worker did not become ready")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := cleanup(context.Background(), conn, 0); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"SET LOCAL enable_nestloop=off",
		"SET LOCAL enable_mergejoin=off",
		"SET LOCAL enable_hashjoin=on",
	} {
		if statementPrefixCount(state.statements, statement) != 1 {
			t.Errorf("missing exactly one %q in %v", statement, state.statements)
		}
	}
}

func TestFixedWorkerRunCanReleaseInitializationBeforeDurationStarts(t *testing.T) {
	start := make(chan struct{})
	ready := make(chan struct{}, 1)
	group := NewWorkerGroupWithStartGate(
		context.Background(), 1,
		func(ctx context.Context, _ int) error {
			ready <- struct{}{}
			<-ctx.Done()
			return nil
		},
		start,
	)
	run := newFixedWorkerRun(
		25*time.Millisecond,
		start,
		fixedWorkerLane{
			Name: "sort", Workers: 1,
			Workload: fixedWorkerGroupAdapter{group: group},
		},
	)
	if err := run.Ramp(context.Background()); err != nil {
		t.Fatal(err)
	}
	run.ReleaseWorkers()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("released worker did not initialize its pressure operator")
	}
	time.Sleep(30 * time.Millisecond)
	started := time.Now()
	if err := run.Hold(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > 300*time.Millisecond {
		t.Fatalf("duration after readiness=%s, want configured duration", elapsed)
	}
}

func TestResourceFactoriesBuildDedicatedWorkMemScenarios(t *testing.T) {
	tests := []struct {
		code     ScenarioCode
		kind     workMemKind
		strategy string
	}{
		{code: 201, kind: workMemSort, strategy: "work_mem_sort_fixed_workers"},
		{code: 202, kind: workMemHash, strategy: "work_mem_hash_fixed_workers"},
	}
	for _, test := range tests {
		definition := DefaultScenarioCatalog().MustCode(test.code)
		scenario, err := ResourceScenarioFactories()[test.code](
			definition, centralizedFixture(),
		)
		if err != nil {
			t.Fatal(err)
		}
		workMem, ok := scenario.(*workMemScenario)
		if !ok {
			t.Fatalf("scenario %d type=%T want *workMemScenario", test.code, scenario)
		}
		if workMem.kind != test.kind || workMem.Strategy() != test.strategy {
			t.Fatalf("scenario %d kind=%d strategy=%q", test.code, workMem.kind, workMem.Strategy())
		}
		if !workMem.OwnsWorkloadDuration() {
			t.Fatalf("scenario %d does not own its post-readiness duration", test.code)
		}
	}
}

func TestScenarioWorkloadStatementsMatchesDedicatedWorkMemSQL(t *testing.T) {
	runtime := &Runtime{Config: BenchConfig{
		Data: DataConfig{Schema: "gsbench"},
		MemoryWorkloads: MemoryWorkloadConfig{
			SortWorkMemKB: 256 * 1024,
			HashWorkMemKB: 128 * 1024,
		},
	}}
	for _, scenario := range []string{"memory_workmem_sort", "memory_workmem_hash"} {
		statements, err := ScenarioWorkloadStatements(runtime, scenario)
		if err != nil {
			t.Fatal(err)
		}
		if len(statements) != 1 || !strings.Contains(statements[0], `"gsbench".sort_data`) {
			t.Fatalf("scenario=%s statements=%v", scenario, statements)
		}
		if strings.Contains(statements[0], "fact_sales") {
			t.Fatalf("scenario=%s retained old fact_sales workload: %s", scenario, statements[0])
		}
	}
}

func TestWorkMemScenarioRampWaitsForEveryPressureOperator(t *testing.T) {
	const workers = 2
	start := make(chan struct{})
	ready := make(chan int, workers)
	group := NewWorkerGroupWithStartGate(
		context.Background(), workers,
		func(ctx context.Context, workerID int) error {
			ready <- workerID
			<-ctx.Done()
			return nil
		},
		start,
	)
	run := newFixedWorkerRun(
		20*time.Millisecond,
		start,
		fixedWorkerLane{
			Name: "sort", Workers: workers,
			Workload: fixedWorkerGroupAdapter{group: group},
		},
	)
	scenario := &workMemScenario{
		code: 201, name: "memory_workmem_sort", kind: workMemSort,
		workers: workers, ready: ready, run: run,
	}
	rampCtx, cancelRamp := context.WithTimeout(context.Background(), time.Second)
	defer cancelRamp()
	if err := scenario.Ramp(rampCtx, nil); err != nil {
		t.Fatal(err)
	}
	if snapshot := run.Snapshot(); snapshot.PeakActive != workers {
		t.Fatalf("snapshot=%+v want %d active pressure workers", snapshot, workers)
	}
	if err := scenario.Hold(context.Background(), &Runtime{
		Config: BenchConfig{Safety: SafetyConfig{QueryTimeout: time.Second}},
	}); err != nil {
		t.Fatal(err)
	}
	if snapshot := run.Snapshot(); snapshot.Active != 0 || snapshot.Operations != workers {
		t.Fatalf("completed snapshot=%+v want exactly one held operator per worker", snapshot)
	}
}

func TestWorkMemScenarioRampReportsWorkerFailureInsteadOfHanging(t *testing.T) {
	start := make(chan struct{})
	ready := make(chan int, 1)
	group := NewWorkerGroupWithStartGate(
		context.Background(), 1,
		func(context.Context, int) error { return errors.New("declare cursor failed") },
		start,
	)
	run := newFixedWorkerRun(
		time.Minute,
		start,
		fixedWorkerLane{
			Name: "hash", Workers: 1,
			Workload: fixedWorkerGroupAdapter{group: group},
		},
	)
	scenario := &workMemScenario{
		code: 202, name: "memory_workmem_hash", kind: workMemHash,
		workers: 1, ready: ready, run: run,
	}
	rampCtx, cancelRamp := context.WithTimeout(context.Background(), time.Second)
	defer cancelRamp()
	started := time.Now()
	err := scenario.Ramp(rampCtx, nil)
	if err == nil || !strings.Contains(err.Error(), "declare cursor failed") {
		t.Fatalf("Ramp error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("worker failure took %s to surface", elapsed)
	}
	if stopErr := scenario.Stop(context.Background(), nil); stopErr != nil {
		t.Fatal(stopErr)
	}
}
