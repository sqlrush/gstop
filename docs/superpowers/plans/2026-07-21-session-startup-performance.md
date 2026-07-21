# gstop Session Startup Performance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate the 7-second gstop first-frame delay caused by multiplied thread-pool session rows and quadratic false-blocker analysis.

**Architecture:** Keep the existing synchronous first snapshot and concurrent monitor refresh architecture. Fix cardinality at the session SQL boundary, normalize blocker values while building rows, and classify holders/waiters through an integer set so work remains linear in the number of real sessions.

**Tech Stack:** Go 1.26.5, `database/sql`, openGauss connector-go-pq, existing monitor/model/logging packages, openGauss Lite 5.0.3 live verification.

## Global Constraints

- Do not change `collect_timeout`, refresh intervals, TUI layout, or first-frame synchronization.
- Preserve correct `H`, `W`, and `H&W` classifications and deadlock-edge construction.
- Treat SQL NULL, empty string, whitespace, and nonnumeric blocker values as no blocker.
- Use `(a.pid=t.tid, a.sessionid=t.sessionid)` as the wait-row identity.
- Do not log or alarm for unblocked sessions.

---

### Task 1: Correct Session Query Cardinality

**Files:**
- Create: `internal/monitor/session_performance_test.go`
- Modify: `internal/monitor/session_queries.go:92-94`

**Interfaces:**
- Consumes: `sessionQueryFor(kind dbcompat.Kind) string`.
- Produces: a query containing `LEFT JOIN pg_thread_wait_status t ON a.pid = t.tid AND a.sessionid = t.sessionid`.

- [ ] **Step 1: Write the failing query-identity test**

```go
func TestSessionQueryUsesUniqueThreadIdentity(t *testing.T) {
	query := sessionQueryFor(dbcompat.KindOpenGauss)
	want := "ON a.pid = t.tid AND a.sessionid = t.sessionid"
	if !strings.Contains(query, want) {
		t.Fatalf("session query must join on unique thread identity %q", want)
	}
}
```

- [ ] **Step 2: Run the test and verify RED**

Run: `go test ./internal/monitor -run TestSessionQueryUsesUniqueThreadIdentity -count=1`

Expected: FAIL because the current query contains only `a.sessionid = t.sessionid`.

- [ ] **Step 3: Apply the minimal SQL fix**

```sql
LEFT JOIN pg_thread_wait_status t
  ON a.pid = t.tid AND a.sessionid = t.sessionid
```

- [ ] **Step 4: Run the test and verify GREEN**

Run: `go test ./internal/monitor -run TestSessionQueryUsesUniqueThreadIdentity -count=1`

Expected: PASS.

### Task 2: Normalize Blockers and Make Classification Linear

**Files:**
- Modify: `internal/monitor/session_performance_test.go`
- Modify: `internal/monitor/session.go:145-171`
- Modify: `internal/monitor/session_block.go:14-54`
- Modify: `internal/monitor/session_ops.go:49-62`

**Interfaces:**
- Produces: `type blockerPIDSet map[int64]struct{}`.
- Produces: `newBlockerPIDSet([]any) blockerPIDSet` and `(blockerPIDSet).contains(any) bool`.
- Produces: `blockRole(pid, blocker any, blockers blockerPIDSet) string`.
- `buildSessionRow` returns a nil blocker signal and a blank display cell for invalid/empty blockers.

- [ ] **Step 1: Write failing normalization and role tests**

```go
func TestBuildSessionRowIgnoresEmptyBlockers(t *testing.T) {
	for _, blockerValue := range []any{nil, "", "  ", []byte(" ")} {
		raw := make(dbconn.Row, 12)
		raw[7] = blockerValue
		row, blocker := buildSessionRow(raw, nil, nil)
		if blocker != nil || row.Display(model.SIdxBlocker) != "" {
			t.Fatalf("blocker %q became signal=%v display=%q", blockerValue, blocker, row.Display(model.SIdxBlocker))
		}
	}
}

func TestBlockRoleUsesRealBlockerSet(t *testing.T) {
	blockers := newBlockerPIDSet([]any{int64(10), int64(20)})
	tests := []struct{ pid, blocker any; want string }{
		{30, "", ""},
		{30, 10, lockWaiter},
		{10, "", lockHolder},
		{20, 10, lockHolderWaiter},
	}
	for _, tc := range tests {
		if got := blockRole(tc.pid, tc.blocker, blockers); got != tc.want {
			t.Fatalf("blockRole(%v,%v)=%q, want %q", tc.pid, tc.blocker, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests and verify RED**

Run: `go test ./internal/monitor -run 'TestBuildSessionRowIgnoresEmptyBlockers|TestBlockRoleUsesRealBlockerSet' -count=1`

Expected: FAIL because empty blockers are returned as non-nil and set/role helpers do not exist.

- [ ] **Step 3: Implement blocker normalization**

In `buildSessionRow`, accept a blocker only when `sInt64(cv)` succeeds; store the numeric ID in both the display row and return signal. Otherwise store `""` in the display row and return nil.

- [ ] **Step 4: Implement set-based classification**

```go
type blockerPIDSet map[int64]struct{}

func newBlockerPIDSet(values []any) blockerPIDSet {
	set := make(blockerPIDSet, len(values))
	for _, value := range values {
		if id, ok := sInt64(value); ok {
			set[id] = struct{}{}
		}
	}
	return set
}

func (s blockerPIDSet) contains(value any) bool {
	id, ok := sInt64(value)
	if !ok {
		return false
	}
	_, ok = s[id]
	return ok
}
```

Use one set per refresh in `analyzeBlockStatus`. Implement `blockRole` with the four outcomes and make `classifyRow` log/report only when the role is non-empty. Preserve numeric waiter-to-holder edges in `blockingMap`.

- [ ] **Step 5: Run focused monitor tests and verify GREEN**

Run: `go test ./internal/monitor -run 'TestSessionQuery|TestBuildSessionRow|TestBlockRole' -count=1`

Expected: PASS with no warning flood.

### Task 3: Regression, Live Verification, Deployment, and GitHub Update

**Files:**
- Modify: `gstop-og/gstop` (rebuilt macOS ARM64 binary)
- Keep: `gstop-og/configs/monitor/instance.cfg` (previous fixed composite join)
- Update: open draft PR branch `agent/gstop-v1.5.0`

**Interfaces:**
- Consumes: existing `./cmd/gstop` build target and `gstop-og/configs/gstop.cfg`.
- Produces: a deployed binary whose session refresh returns one row per `pg_stat_activity` row.

- [ ] **Step 1: Run repository verification**

Run:

```bash
go test ./...
go test -race ./internal/...
go vet ./...
```

Expected: every command exits 0.

- [ ] **Step 2: Verify database cardinality**

Run both live counts on og5. Expected: composite session JOIN count equals `SELECT count(*) FROM pg_stat_activity`; real waiters remain independently counted through non-null `block_sessionid`.

- [ ] **Step 3: Build and deploy macOS ARM64**

Build `./cmd/gstop` to a fresh temporary file, back up the current `gstop-og/gstop`, and atomically replace it. Do not modify database credentials or user-specific gstop configuration.

- [ ] **Step 4: Measure startup behavior**

Start the deployed gstop once, then inspect `gstop_app.log`. Expected: no `Module: "session" executed finished` warning above the 3-second threshold and no bulk `BLK: W` lines when `real_waiters=0`.

- [ ] **Step 5: Publish the follow-up commit**

Sync only the changed source, tests, and design/plan documents into the existing `agent/gstop-v1.5.0` Git clone; commit as `Optimize session startup refresh`; push to the existing draft PR. Do not add runtime logs, `gstop-og`, backups, credentials, or generated release bundles to the source commit.
