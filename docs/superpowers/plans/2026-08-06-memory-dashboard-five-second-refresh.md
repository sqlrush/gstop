# `m` Memory Dashboard Five-Second Refresh Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the `m` memory dashboard refresh all four panels every 5 seconds by default without changing the `h` health dashboard's CPU and minimum-interval protection.

**Architecture:** Keep `MemoryMonitor`'s existing elapsed-time-compensated loop and make every `Refresh` cycle collect all four panels. Change only the `m` fallback/default configuration to 5 seconds; retain the shared `Health` gate for `h`, whose collector continues to call it with the independent `health-dashboard` key.

**Tech Stack:** Go, standard `testing` package, existing `config`, `dbconn`, `health`, and `logging` packages.

## Global Constraints

- Only `m` stops using `main.dynamic_mem_interval` and `main.dynamic_mem_cpu_thresh`.
- `h` keeps its CPU threshold, minimum refresh interval, and single-flight protections.
- `main.mem_interval = 0` and `main.dynamic_mem_enable = false` keep disabling the `m` dashboard.
- Explicit positive `main.mem_interval` values continue to override the 5-second default.
- Do not add a new `h` refresh configuration option or change database queries/layouts.

---

### Task 1: Make every `m` cycle refresh all panels at a five-second fallback

**Files:**
- Create: `internal/monitor/memory_refresh_test.go`
- Modify: `internal/monitor/memory.go:12-24,175-195,244-253`
- Modify: `internal/app/app.go:125-135`

**Interfaces:**
- Consumes: `NewMemoryMonitor(deps Deps) *MemoryMonitor`, `(*MemoryMonitor).Refresh()`, `(*MemoryMonitor).memInterval() time.Duration`.
- Produces: `memDefaultInterval = 5`; `Refresh` always publishes panels 0–3 without calling `Health.ShouldRefreshMemory("memory")`.

- [ ] **Step 1: Write tests that describe the new default and CPU-independent refresh**

Create `internal/monitor/memory_refresh_test.go`:

```go
package monitor

import (
	"testing"
	"time"

	"gstop/internal/config"
	"gstop/internal/dbconn"
	"gstop/internal/health"
	"gstop/internal/logging"
)

func TestMemoryMonitorDefaultsToFiveSecondInterval(t *testing.T) {
	m := NewMemoryMonitor(Deps{Cfg: config.FromMap(map[string]any{})})
	if got := m.memInterval(); got != 5*time.Second {
		t.Fatalf("memInterval() = %s, want 5s", got)
	}
}

func TestMemoryMonitorRefreshIgnoresDynamicMemoryHealthGate(t *testing.T) {
	cfg := config.FromMap(map[string]any{"main": map[string]any{
		"dynamic_mem_cpu_thresh": int64(50),
		"dynamic_mem_interval":   int64(60),
	}})
	logger := logging.New("memory-refresh-test", "")
	db := dbconn.New(cfg, logger)
	db.Cancel()
	defer db.Close()

	memHealth := health.New(cfg)
	memHealth.UpdateCPUUsage(100)
	m := NewMemoryMonitor(Deps{Cfg: cfg, DB: db, Logger: logger, Health: memHealth})
	m.panels[2] = memPanel{title: "stale session panel"}
	m.panels[3] = memPanel{title: "stale thread panel"}

	m.Refresh()

	if got := m.panels[2].title; got != memPanel2Title {
		t.Fatalf("session panel title = %q, want %q", got, memPanel2Title)
	}
	if got := m.panels[3].title; got != memPanel3Title {
		t.Fatalf("thread panel title = %q, want %q", got, memPanel3Title)
	}
}
```

The cancelled database makes all four queries return immediately. The sentinel titles prove that the session/thread refresh functions ran even though the Health gate would reject a refresh at 100% CPU.

- [ ] **Step 2: Run the focused tests and verify the old implementation fails**

Run:

```bash
go test ./internal/monitor -run 'TestMemoryMonitor(DefaultsToFiveSecondInterval|RefreshIgnoresDynamicMemoryHealthGate)$' -count=1
```

Expected: FAIL because `memInterval()` returns `30s`, and the session panel retains `"stale session panel"` after the Health gate rejects the old conditional refresh.

- [ ] **Step 3: Implement the minimal `m` behavior change**

In `internal/monitor/memory.go`, change the fallback constant:

```go
// memDefaultInterval is the fallback refresh cadence when main.mem_interval is
// missing or non-positive (which the app never creates the monitor for).
memDefaultInterval = 5
```

Replace `Refresh` with:

```go
// Refresh rebuilds all four memory panels every cycle. The memory dashboard's
// explicit cadence is its only refresh throttle; the health dashboard keeps its
// independent CPU and minimum-interval gate.
func (m *MemoryMonitor) Refresh() {
	p0 := m.refreshSummaryInfo()
	p1 := m.refreshDynamicInfo()
	p2 := m.refreshSessionInfo()
	p3 := m.refreshThreadInfo()
	m.mu.Lock()
	m.panels[0] = p0
	m.panels[1] = p1
	m.panels[2] = p2
	m.panels[3] = p3
	m.mu.Unlock()
}
```

In `internal/app/app.go`, keep the existing disable check but align its fallback with the new default:

```go
if deps.Cfg.GetInt("main.mem_interval", 5) == 0 || !deps.Cfg.GetBool("main.dynamic_mem_enable", false) {
	return
}
```

- [ ] **Step 4: Run focused and affected-package tests**

Run:

```bash
gofmt -w internal/monitor/memory_refresh_test.go internal/monitor/memory.go internal/app/app.go
go test ./internal/monitor -run 'TestMemoryMonitor(DefaultsToFiveSecondInterval|RefreshIgnoresDynamicMemoryHealthGate)$' -count=1
go test ./internal/monitor ./internal/app -count=1
```

Expected: all commands PASS.

- [ ] **Step 5: Commit the tested behavior**

```bash
git add internal/monitor/memory_refresh_test.go internal/monitor/memory.go internal/app/app.go
git commit -m "feat(gstop): refresh memory dashboard every five seconds"
```

---

### Task 2: Publish the five-second default and document the `m`/`h` boundary

**Files:**
- Modify: `configs/gstop.cfg:6-8`
- Modify: `README.md:169-175`

**Interfaces:**
- Consumes: `main.mem_interval`, `main.dynamic_mem_interval`, and `main.dynamic_mem_cpu_thresh` configuration keys.
- Produces: shipping configuration with `mem_interval = 5` and user-facing documentation that applies the CPU/minimum-interval gate only to `h` dynamic memory collection.

- [ ] **Step 1: Update the shipping configuration**

In `configs/gstop.cfg`, replace the memory interval comment/value with:

```ini
# 内存监控大盘刷新间隔，为0则表示不开启内存监控大盘功能（单位：秒，默认值：5）
mem_interval = 5
```

Leave these `h` protection defaults unchanged:

```ini
dynamic_mem_interval = 60
dynamic_mem_cpu_thresh = 50
```

- [ ] **Step 2: Clarify the README behavior**

Replace the health-dashboard refresh paragraph in `README.md` with:

```markdown
立即取消明细查询和采集并退出进程。SQL/等待/计划跳变跟随 `main.interval`；`m` 内存大盘
跟随 `main.mem_interval`（默认 5 秒），每轮刷新全部内存面板；健康大盘的动态内存候选采集
也跟随 `main.mem_interval`，但仍受 CPU 阈值和 `main.dynamic_mem_interval` 最短间隔保护；
跨库慢项跟随 `main.health_slow_interval`（默认 300 秒）。
```

- [ ] **Step 3: Verify configuration, documentation, and unchanged `h` protection**

Run:

```bash
rg -n 'mem_interval = 5|dynamic_mem_interval = 60|dynamic_mem_cpu_thresh = 50' configs/gstop.cfg
go test ./internal/health ./internal/healthdash -count=1
git diff --check
```

Expected: `rg` shows all three defaults; both Go packages PASS; `git diff --check` is silent.

- [ ] **Step 4: Commit configuration and documentation**

```bash
git add configs/gstop.cfg README.md
git commit -m "docs(gstop): publish five-second memory refresh default"
```

---

### Task 3: Final minimum verification

**Files:**
- Verify only; no planned file changes.

**Interfaces:**
- Consumes: the completed `m` behavior, shipping configuration, and unchanged `h` tests.
- Produces: evidence that the affected packages pass and the worktree contains only the intended commits.

- [ ] **Step 1: Run the affected package suite once from a clean test cache**

```bash
go test ./internal/monitor ./internal/app ./internal/health ./internal/healthdash -count=1
```

Expected: PASS for all four packages.

- [ ] **Step 2: Inspect final repository state**

```bash
git status --short --branch
git log -4 --oneline
```

Expected: clean `main`; the latest commits are the implementation, docs/config, and this plan/design documentation.
