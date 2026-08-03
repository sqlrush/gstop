# gsbench Work-Memory EXPLAIN Compatibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make scenarios 201/202 calibrate reliably when GaussDB defaults `explain_perf_mode` to `pretty`, without weakening Sort/Hash memory and spill evidence.

**Architecture:** Read the original EXPLAIN mode once on the tagged calibration connection, force `normal` only inside each calibration transaction, and retain the existing strict normal-format parser. Wrap parser failures with detected-format and bounded escaped-plan diagnostics, then expose the original/effective modes in successful evidence.

**Tech Stack:** Go, `database/sql`, the existing controlled SQL test connector, openGauss/GaussDB session GUCs.

## Global Constraints

- Change only scenarios 201/202 and their documentation/tests.
- Never change user-, database-, or instance-level GUCs.
- `SET LOCAL explain_perf_mode=normal` applies only to calibration transactions, not pressure workers.
- Do not accept pretty `Peak Memory` as sufficient Hash spill evidence.
- Preserve existing worker, duration, Ctrl+C, calibration range, and recovery behavior.
- Diagnostics must escape control characters and truncate the plan to 512 Unicode code points.

---

### Task 1: Calibration-mode override and diagnostic parser context

**Files:**
- Modify: `internal/gsbench/scenario_workmem.go:20-40,275-355,427-447,514-579`
- Modify: `internal/gsbench/scenario_workmem_test.go:13-64,134-242`

**Interfaces:**
- Consumes: `workMemSessionSetup(kind workMemKind, targetKB int64)`, `parseWorkMemPlan(kind workMemKind, plan string)`, and the tagged `*sql.Conn`.
- Produces: `readWorkMemExplainPerfMode(context.Context, *sql.Conn) (string, error)`, `workMemCalibrationSessionSetup(workMemKind, int64) ([]string, error)`, and `parseWorkMemPlanWithContext(workMemKind, string, string) (workMemObservation, error)`.

- [ ] **Step 1: Extend the controlled connector and write failing mode/ordering tests**

Add `explainPerfMode string` to `workMemProbeTestConnector` and `workMemProbeTestConn`. Make `QueryContext` return that value for `SHOW explain_perf_mode` and the configured plan for EXPLAIN. Add this behavior test:

```go
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
```

Update `TestProbeWorkMemUsesOneTransactionForSettingsAndExplain` to pass original mode `pretty`, require `SET LOCAL explain_perf_mode=normal` between `query_dop` and EXPLAIN, require no SHOW inside the probe, and require the returned observation to contain original `pretty` and effective `normal` modes.

- [ ] **Step 2: Run the focused tests and confirm RED**

Run:

```bash
go test ./internal/gsbench -run 'Test(ReadWorkMemExplainPerfMode|ProbeWorkMemUsesOneTransactionForSettingsAndExplain)$' -count=1
```

Expected: build/test failure because the reader, calibration-only setup, observation fields, and new probe parameter do not exist.

- [ ] **Step 3: Implement one-time mode reading and calibration-only normal setup**

Add to `workMemObservation`:

```go
OriginalExplainPerfMode   string
EffectiveExplainPerfMode string
```

Add:

```go
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
```

Refactor setup through a private builder so worker setup stays unchanged while calibration inserts `SET LOCAL explain_perf_mode=normal` after `query_dop` and before Hash join GUCs. In `calibrateWorkMemDatabase`, read the original mode once after `OpenTagged`, then pass it into every probe. Extend `probeWorkMemOnConnection` with `originalExplainPerfMode string`, use calibration setup, and attach both modes to a successfully parsed observation.

- [ ] **Step 4: Run the focused tests and confirm GREEN**

Run the command from Step 2. Expected: PASS.

- [ ] **Step 5: Write failing pretty-output and truncation tests**

Add a pretty plan containing `id | operation | Peak Memory` and assert that `parseWorkMemPlanWithContext(workMemSort, plan, "pretty")` fails with all of:

```text
original_explain_perf_mode="pretty"
requested_explain_perf_mode="normal"
detected_output_mode="pretty"
requested Sort operator
```

Add a plan longer than 512 Unicode code points with a newline/tab and assert the diagnostic contains escaped `\\n`/`\\t`, contains the truncation marker, and excludes a unique suffix beyond the limit.

- [ ] **Step 6: Run the diagnostic tests and confirm RED**

Run:

```bash
go test ./internal/gsbench -run 'TestWorkMemPlan(ParseErrorIdentifiesPrettyOutput|DiagnosticIsEscapedAndTruncated)$' -count=1
```

Expected: build failure because the contextual parser and diagnostic helpers do not exist.

- [ ] **Step 7: Implement strict contextual diagnostics**

Add a 512-rune formatter using `strconv.QuoteToASCII`, a pretty detector requiring both `operation` and `peak memory`, and this wrapper:

```go
func parseWorkMemPlanWithContext(
	kind workMemKind,
	plan string,
	originalExplainPerfMode string,
) (workMemObservation, error) {
	observation, err := parseWorkMemPlan(kind, plan)
	if err != nil {
		return workMemObservation{}, fmt.Errorf(
			"parse work_mem plan: original_explain_perf_mode=%q requested_explain_perf_mode=%q detected_output_mode=%q plan=%s: %w",
			originalExplainPerfMode, "normal", detectWorkMemExplainOutputMode(plan),
			workMemPlanDiagnostic(plan), err,
		)
	}
	observation.OriginalExplainPerfMode = originalExplainPerfMode
	observation.EffectiveExplainPerfMode = "normal"
	return observation, nil
}
```

Route only the database probe through this wrapper; leave `parseWorkMemPlan` available for focused normal-format unit tests.

- [ ] **Step 8: Run all work-memory tests and commit**

Run:

```bash
gofmt -w internal/gsbench/scenario_workmem.go internal/gsbench/scenario_workmem_test.go
go test ./internal/gsbench -run 'Test.*WorkMem|TestParseWorkMem|TestProbeWorkMem' -count=1
```

Expected: PASS. Then commit:

```bash
git add internal/gsbench/scenario_workmem.go internal/gsbench/scenario_workmem_test.go
git commit -m "fix(gsbench): normalize work_mem EXPLAIN output"
```

### Task 2: Evidence, operator documentation, and full verification

**Files:**
- Modify: `internal/gsbench/scenario_workmem.go:234-257`
- Modify: `internal/gsbench/scenario_workmem_test.go:415-439`
- Modify: `docs/gsbench/README.md`

**Interfaces:**
- Consumes: `workMemObservation.OriginalExplainPerfMode` and `EffectiveExplainPerfMode` from Task 1.
- Produces: `work_mem_kb.details.original_explain_perf_mode` and `work_mem_kb.details.explain_perf_mode`.

- [ ] **Step 1: Write a failing evidence regression test**

Extend `TestWorkMemCalibrationEvidenceReportsTargetMiss` with an observation containing `pretty`/`normal`, then require:

```go
if details["original_explain_perf_mode"] != "pretty" ||
	details["explain_perf_mode"] != "normal" {
	t.Fatalf("details=%v want pretty-to-normal compatibility evidence", details)
}
```

- [ ] **Step 2: Run the evidence test and confirm RED**

Run:

```bash
go test ./internal/gsbench -run TestWorkMemCalibrationEvidenceReportsTargetMiss -count=1
```

Expected: FAIL because the evidence map lacks the two mode keys.

- [ ] **Step 3: Add evidence fields and documentation**

Add both keys to `calibrationEvidence()` without changing availability or target logic. In `docs/gsbench/README.md`, document that 201/202 force `normal` only in their calibration transaction because GaussDB defaults to pretty, restore automatically via ROLLBACK, and never treat pretty Peak Memory as complete spill evidence.

- [ ] **Step 4: Run focused and full verification**

Run:

```bash
gofmt -w internal/gsbench/scenario_workmem.go internal/gsbench/scenario_workmem_test.go
go test ./internal/gsbench -run 'Test.*WorkMem|TestParseWorkMem|TestProbeWorkMem' -count=1
go test ./internal/gsbench -count=1
go build ./cmd/gsbench
git diff --check HEAD~1 --
```

Expected: every command exits 0 with no test failures or formatting errors.

- [ ] **Step 5: Commit the evidence and documentation**

```bash
git add internal/gsbench/scenario_workmem.go internal/gsbench/scenario_workmem_test.go docs/gsbench/README.md
git commit -m "docs(gsbench): report work_mem EXPLAIN compatibility"
```

- [ ] **Step 6: Perform the platform-required minimal code review**

Review only the commits from Tasks 1-2 for correctness, transaction scoping, secret exposure, and test coverage. Resolve only Critical/Important findings, then rerun the full verification command from Step 4.
