# gstop Current Active SQL Elapsed TOP5 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a selectable health-dashboard TOP5 that immediately ranks currently active SQL by the longest running session for each SQL ID.

**Architecture:** Reuse the existing `activeSQLQuery` result from each fast refresh and aggregate it independently of statement-history metrics. Publish the result as an immutable `Snapshot` field, render it before the cumulative-average list, and reuse the existing SQL-detail selection path.

**Tech Stack:** Go, tcell TUI, openGauss/GaussDB catalog queries, Go `testing` package.

## Global Constraints

- Do not add a database query or change `activeSQLQuery` filtering.
- Group by `SQL_ID`; rank by the longest currently active session, descending, with `SQL_ID` ascending as the deterministic tie-break.
- Display at most five rows with `MAX_ELAPSED`, `SESSIONS`, and SQL text.
- Publish active results even if the statement-statistics query fails; clear active results if the active-session query fails.
- Preserve existing cumulative-average, execution-count, memory, plan-change, wait, lock, replication, and cluster calculations.
- Keep existing `s`/arrow/`p` detail navigation and use the longest session as the detail target.
- Set the product version to `gstop v1.6.3`.
- `/Users/sqlrush/gstop` is not a Git worktree, so commit steps are replaced by explicit file/test checkpoints.

## File Map

- `internal/healthdash/aggregate.go`: pure active-SQL aggregation and ranking.
- `internal/healthdash/model.go`: snapshot field and immutable clone support.
- `internal/healthdash/collector.go`: publish/clear the new snapshot field per fast refresh.
- `internal/healthdash/view.go`: render the new first section and make rows selectable.
- `internal/healthdash/{aggregate,collector,view}_test.go`: unit and integration-style collector/view regression tests.
- `cmd/gstop/{main.go,main_test.go,install_script_test.go}`, `scripts/{build.sh,install.sh}`, `README.md`: v1.6.3 and user documentation.

---

### Task 1: Active SQL aggregation and immutable model

**Files:**
- Modify: `internal/healthdash/aggregate.go`
- Modify: `internal/healthdash/model.go`
- Test: `internal/healthdash/aggregate_test.go`
- Test: `internal/healthdash/collector_test.go`

**Interfaces:**
- Consumes: `[]ActiveSQL`, `time.Time`, existing `activeIndex`, `applyRepresentative`, and `limitSQL`.
- Produces: `BuildActiveElapsedMetrics(active []ActiveSQL, capturedAt time.Time) []SQLMetric` and `Snapshot.ActiveElapsedSQL []SQLMetric`.

- [ ] **Step 1: Write failing aggregation tests**

Add tests equivalent to:

```go
func TestBuildActiveElapsedMetricsGroupsBySQLIDAndUsesLongestSession(t *testing.T) {
    capturedAt := time.Unix(100, 0)
    got := BuildActiveElapsedMetrics([]ActiveSQL{
        {SQLID: 42, PID: 1, SessionID: "s1", Query: "select 42", Database: "db1", User: "u1", ElapsedUS: 2_000_000},
        {SQLID: 42, PID: 2, SessionID: "s2", Query: "select 42", Database: "db2", User: "u2", ElapsedUS: 9_000_000},
        {SQLID: 7, PID: 3, SessionID: "s3", Query: "select 7", ElapsedUS: 5_000_000},
        {SQLID: 0, PID: 4, SessionID: "ignored", ElapsedUS: 99_000_000},
    }, capturedAt)
    if len(got) != 2 || got[0].SQLID != 42 || got[0].ActiveSessions != 2 ||
        got[0].RepresentativePID != 2 || got[0].RepresentativeSessionID != "s2" ||
        got[0].RepresentativeElapsedUS != 9_000_000 || !got[0].CapturedAt.Equal(capturedAt) {
        t.Fatalf("active elapsed metrics=%+v", got)
    }
}

func TestBuildActiveElapsedMetricsLimitsFiveAndBreaksTiesBySQLID(t *testing.T) {
    active := make([]ActiveSQL, 0, 6)
    for id := int64(6); id >= 1; id-- {
        active = append(active, ActiveSQL{SQLID: id, PID: id, ElapsedUS: 1_000_000})
    }
    got := BuildActiveElapsedMetrics(active, time.Unix(1, 0))
    if len(got) != 5 || got[0].SQLID != 1 || got[4].SQLID != 5 {
        t.Fatalf("stable top5=%+v", got)
    }
}
```

Extend the existing snapshot clone test with `ActiveElapsedSQL` containing database/user slices, mutate the clone, and assert the original slices are unchanged.

- [ ] **Step 2: Run tests and verify RED**

Run:

```bash
go test ./internal/healthdash -run 'TestBuildActiveElapsedMetrics|TestSnapshotClone' -count=1
```

Expected: compilation failure because `BuildActiveElapsedMetrics` and `Snapshot.ActiveElapsedSQL` do not exist.

- [ ] **Step 3: Implement the minimal aggregator and model field**

Add the field to `Snapshot`, clone its slice, and pass it through `cloneSQLMetricIdentity`. Implement:

```go
func BuildActiveElapsedMetrics(active []ActiveSQL, capturedAt time.Time) []SQLMetric {
    grouped := activeIndex(active)
    rows := make([]SQLMetric, 0, len(grouped))
    for sqlID, running := range grouped {
        metric := SQLMetric{
            SQLID: sqlID, Query: running.query,
            Databases: running.databases, Users: running.users,
            ActiveSessions: running.count,
        }
        applyRepresentative(&metric, running, capturedAt)
        rows = append(rows, metric)
    }
    sort.Slice(rows, func(i, j int) bool {
        if rows[i].RepresentativeElapsedUS != rows[j].RepresentativeElapsedUS {
            return rows[i].RepresentativeElapsedUS > rows[j].RepresentativeElapsedUS
        }
        return rows[i].SQLID < rows[j].SQLID
    })
    return limitSQL(rows, topSQLCount)
}
```

- [ ] **Step 4: Format and verify GREEN**

```bash
gofmt -w internal/healthdash/aggregate.go internal/healthdash/model.go internal/healthdash/aggregate_test.go internal/healthdash/collector_test.go
go test ./internal/healthdash -run 'TestBuildActiveElapsedMetrics|TestSnapshotClone' -count=1
```

Expected: PASS.

- [ ] **Step 5: Record checkpoint**

Record the four changed files and the passing command in the execution log; no Git commit is possible in this workspace.

---

### Task 2: Collector publication and failure semantics

**Files:**
- Modify: `internal/healthdash/collector.go`
- Test: `internal/healthdash/collector_test.go`

**Interfaces:**
- Consumes: `BuildActiveElapsedMetrics(active []ActiveSQL, capturedAt time.Time)` from Task 1.
- Produces: a fresh `Snapshot.ActiveElapsedSQL` on every successful active sample, independent of statement sampling.

- [ ] **Step 1: Write failing collector tests**

Add one test where `statementQuery` returns `nil` while `activeSQLQuery` returns SQL ID 42 and assert:

```go
snapshot := collector.Snapshot()
if len(snapshot.ActiveElapsedSQL) != 1 || snapshot.ActiveElapsedSQL[0].SQLID != 42 {
    t.Fatalf("active SQL was hidden by statement failure: %+v", snapshot)
}
if len(snapshot.AverageSQL) != 0 {
    t.Fatalf("failed statement sample retained average rows: %+v", snapshot.AverageSQL)
}
```

Add a two-refresh test: the first active query returns SQL ID 42, the second returns `nil`; assert the second snapshot has zero `ActiveElapsedSQL` rows and contains the existing fast-error message for active SQL collection.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./internal/healthdash -run 'TestCollector.*ActiveElapsed' -count=1
```

Expected: FAIL because the collector does not publish or clear the new field.

- [ ] **Step 3: Publish active metrics independently**

Inside the existing `c.mu.Lock()` block in `RefreshFast`, use this structure:

```go
if activeOK {
    c.active = append([]ActiveSQL(nil), active...)
    active = append([]ActiveSQL(nil), c.active...)
    c.snapshot.ActiveElapsedSQL = BuildActiveElapsedMetrics(active, refreshedAt)
} else {
    active = nil
    c.snapshot.ActiveElapsedSQL = nil
}
```

Leave statement aggregation and all other refresh paths unchanged.

- [ ] **Step 4: Format and verify GREEN**

```bash
gofmt -w internal/healthdash/collector.go internal/healthdash/collector_test.go
go test ./internal/healthdash -run 'TestCollector.*ActiveElapsed|TestCollectorEstablishesStartupBaselinesThenPublishesDeltas' -count=1
```

Expected: PASS.

- [ ] **Step 5: Record checkpoint**

Record the collector files and passing command; do not create a commit outside a Git worktree.

---

### Task 3: Dashboard section, selection, and documentation

**Files:**
- Modify: `internal/healthdash/view.go`
- Modify: `internal/healthdash/view_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `Snapshot.ActiveElapsedSQL []SQLMetric`.
- Produces: section `1. CURRENT ACTIVE SQL ELAPSED TOP5` and selectable rows with section key `active-elapsed`.

- [ ] **Step 1: Write failing view tests**

Update the main render test to supply:

```go
ActiveElapsedSQL: []SQLMetric{{
    SQLID: 99, Query: "select current", ActiveSessions: 6,
    RepresentativeElapsedUS: 12_345_000,
    RepresentativePID: 990, RepresentativeSessionID: "s99",
    CapturedAt: now,
}},
```

Assert all 11 section headers occur in order, beginning with `1. CURRENT ACTIVE SQL ELAPSED TOP5` and ending with `11. DISTRIBUTED CLUSTER HEALTH`. Assert the rendered row contains `12.35s`, `6`, and `select current`; assert the first selection has section `active-elapsed`, SQL ID 99, PID 990, and session `s99`. Add an empty-state assertion for `当前无活跃 SQL`.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./internal/healthdash -run 'TestView.*Active|TestViewRenders' -count=1
```

Expected: FAIL because the new section and selection do not exist.

- [ ] **Step 3: Implement the new section and renumber existing sections**

Call `v.renderActiveElapsed(snapshot.ActiveElapsedSQL)` before `renderAverage`. Implement:

```go
func (v *View) renderActiveElapsed(rows []SQLMetric) {
    v.section("1. CURRENT ACTIVE SQL ELAPSED TOP5")
    v.add("#  SQL_ID              MAX_ELAPSED       SESSIONS  SQL", model.Normal)
    if len(rows) == 0 {
        v.add("当前无活跃 SQL", model.Normal)
    }
    for i, row := range rows {
        text := fmt.Sprintf("%-2d %-19d %-17s %-9d %s", i+1, row.SQLID,
            formatMicroseconds(row.RepresentativeElapsedUS), row.ActiveSessions, oneLine(row.Query))
        v.sqlLine("active-elapsed", row, text)
    }
    v.blank()
}
```

Renumber current sections 1–10 to 2–11 without changing their data or rendering. Add a README section immediately before the cumulative-average documentation explaining that `MAX_ELAPSED` is the longest currently active session for the SQL ID and `SESSIONS` is its current concurrency.

- [ ] **Step 4: Format and verify GREEN**

```bash
gofmt -w internal/healthdash/view.go internal/healthdash/view_test.go
go test ./internal/healthdash -run 'TestView' -count=1
go test ./internal/app -run 'TestHealth' -count=1
```

Expected: PASS; existing `s`/arrow/`p` behavior remains green without production changes in `internal/app`.

- [ ] **Step 5: Record checkpoint**

Record the view/documentation files and passing commands.

---

### Task 4: Version, full verification, og5 acceptance, and local deployment

**Files:**
- Modify: `cmd/gstop/main.go`
- Modify: `cmd/gstop/main_test.go`
- Modify: `cmd/gstop/install_script_test.go`
- Modify: `scripts/build.sh`
- Modify: `scripts/install.sh`
- Modify: `README.md`
- Test: `internal/healthdash/live_smoke_test.go`
- Build: `.deploy-stage/gstop-v1.6.3-local-20260803`
- Deploy: `gsbench-local/bin/gstop.real`

**Interfaces:**
- Produces: locally callable `gstop v1.6.3` and a timestamped backup of deployed v1.6.2.

- [ ] **Step 1: Make version expectations fail first**

Change the version tests to require `v1.6.3` and installer text `gstop v1.6.3`, then run:

```bash
go test ./cmd/gstop -run 'TestVersionIsV163|TestInstallScriptValidatesV163' -count=1
```

Expected: FAIL while production/version scripts still say v1.6.2.

- [ ] **Step 2: Update production version references**

Set `cmd/gstop/main.go` to:

```go
const version = "v1.6.3"
```

Update `scripts/build.sh`, `scripts/install.sh`, README version text/examples, and rename version-specific tests from `V162` to `V163`.

- [ ] **Step 3: Run fresh automated verification**

```bash
rg --files -g '*.go' | xargs gofmt -l
go test ./... -count=1
go test -race ./internal/healthdash ./internal/app -count=1
go vet ./...
bash -n scripts/build.sh scripts/install.sh
```

Expected: no gofmt output and all commands exit 0.

- [ ] **Step 4: Build and inspect the local arm64 candidate**

```bash
go build -trimpath -ldflags='-s -w' \
  -o /Users/sqlrush/gstop/.deploy-stage/gstop-v1.6.3-local-20260803 ./cmd/gstop
/Users/sqlrush/gstop/.deploy-stage/gstop-v1.6.3-local-20260803 --version
file /Users/sqlrush/gstop/.deploy-stage/gstop-v1.6.3-local-20260803
shasum -a 256 /Users/sqlrush/gstop/.deploy-stage/gstop-v1.6.3-local-20260803
```

Expected: `gstop v1.6.3`, Mach-O arm64, and a recorded SHA-256.

- [ ] **Step 5: Run real og5 scenario 601 acceptance**

First add this opt-in live assertion to `internal/healthdash/live_smoke_test.go` (with `strconv` imported):

```go
func TestIntegrationActiveElapsedSQL(t *testing.T) {
    path := os.Getenv("GSTOP_INTEGRATION_CONFIG")
    expectedText := os.Getenv("GSTOP_INTEGRATION_ACTIVE_SQL_ID")
    if path == "" || expectedText == "" {
        t.Skip("set GSTOP_INTEGRATION_CONFIG and GSTOP_INTEGRATION_ACTIVE_SQL_ID")
    }
    expectedID, err := strconv.ParseInt(expectedText, 10, 64)
    if err != nil || expectedID <= 0 {
        t.Fatalf("invalid expected SQL ID %q: %v", expectedText, err)
    }
    cfg, err := config.Load(path, config.Args{})
    if err != nil {
        t.Fatalf("load integration config: %v", err)
    }
    logger := logging.New("health-active-live-test", "")
    db := dbconn.New(cfg, logger)
    defer db.Close()
    collector := NewCollector(cfg.With("main.dynamic_mem_enable", false), db, logger, nil, nil)
    collector.lastSlowRefresh = collector.now()
    collector.RefreshFast()
    for _, metric := range collector.Snapshot().ActiveElapsedSQL {
        if metric.SQLID == expectedID && metric.ActiveSessions > 0 &&
            metric.RepresentativeElapsedUS > 0 {
            t.Logf("active elapsed metric: %+v", metric)
            return
        }
    }
    t.Fatalf("SQL_ID %d absent from current active TOP5: %+v",
        expectedID, collector.Snapshot().ActiveElapsedSQL)
}
```

Use the existing local environment:

```bash
source /Users/sqlrush/gstop/gsbench-local/env.sh
gsbench restore --dry-run
gsbench run 601 init --worker 6 --duration 10m
```

While it runs, launch the candidate with the wrapper-equivalent database environment, then execute in another terminal:

```bash
gsbench run 601 fault
GSTOP_INTEGRATION_CONFIG=/Users/sqlrush/gstop/gsbench-local/gstop.cfg \
GSTOP_INTEGRATION_ACTIVE_SQL_ID=3877360001 \
go test ./internal/healthdash -run '^TestIntegrationActiveElapsedSQL$' -count=1 -v
```

Verify the new first section contains SQL ID `3877360001`, shows active sessions and a large current `MAX_ELAPSED`, and selecting it opens details that show the exact `gsbench_e2e_20260801_100g.plan_data` current `Seq Scan` plus the `lookup_key` index suggestion. Always finish with:

```bash
gsbench run 601 recover
gsbench restore
gsbench restore --dry-run
```

Expected: recover succeeds; final dry run reports `runs=0 actions=0`; the lookup index exists and EXPLAIN returns `Index Scan`.

- [ ] **Step 6: Atomically deploy with rollback backup**

After confirming no gstop process is running, copy the deployed binary to an explicit timestamped `gstop.real.pre-v1.6.3-*` backup, copy the candidate to a temporary file in the same directory, verify its version/SHA, and `mv` the temporary file over `gstop.real`. Do not modify the wrapper `gsbench-local/bin/gstop` or configuration files.

- [ ] **Step 7: Verify the deployed entry point and database health**

```bash
source /Users/sqlrush/gstop/gsbench-local/env.sh
gstop --version
cmp /Users/sqlrush/gstop/.deploy-stage/gstop-v1.6.3-local-20260803 \
    /Users/sqlrush/gstop/gsbench-local/bin/gstop.real
docker inspect -f 'oom_killed={{.State.OOMKilled}} restart_count={{.RestartCount}} status={{.State.Status}}' og5
docker stats --no-stream og5
```

Expected: wrapper reports v1.6.3, binaries match, og5 is running with no OOM/restart, and no acceptance run remains active.
