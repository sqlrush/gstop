# gsbench Single Ctrl+C Exit Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the first Ctrl+C terminate gsbench and its active traffic immediately, without entering any cleanup or automatic-restore wait.

**Architecture:** Remove gsbench's `signal.NotifyContext` interception and run the CLI with a non-cancelable background command context. SIGINT and SIGTERM then retain the operating system's default termination behavior, so one signal terminates the process and closes its database sockets; normal completion, `gsbench stop`, and explicit `gsbench restore` remain unchanged.

**Tech Stack:** Go standard library process signals and contexts, Go unit tests, real openGauss `og5` integration test.

## Global Constraints

- One Ctrl+C must immediately terminate the process; no second signal and no grace period are required.
- Do not run automatic restore after Ctrl+C.
- Preserve recovery journal state for the next startup or `gsbench restore --run-id RUN_ID`.
- Preserve normal-completion and explicit stop/restore behavior.
- Modify the current dirty working tree in place without reverting unrelated changes.

---

### Task 1: Specify the default-signal policy

**Files:**
- Create: `cmd/gsbench/main_test.go`
- Modify: `cmd/gsbench/main.go`

**Interfaces:**
- Consumes: `gsbench.RunCLI(context.Context, []string, io.Writer, io.Writer) int`
- Produces: `commandContext() context.Context`, intentionally having no cancellation channel so the process does not intercept SIGINT/SIGTERM, plus a subprocess regression test of the actual binary entrypoint.

- [x] **Step 1: Write the failing test**

Add `TestCommandContextLeavesInterruptsToOperatingSystem`, call `commandContext()`, and assert `ctx.Done() == nil`. The test initially fails to compile because `commandContext` does not exist. Add `TestGSBenchProcessExitsOnFirstInterrupt`, which builds the actual command, blocks it in a local TCP database handshake, sends one SIGINT, and asserts signal-driven exit within one second.

- [x] **Step 2: Run the test and verify RED**

Run:

```bash
go test ./cmd/gsbench -run 'TestCommandContextLeavesInterruptsToOperatingSystem|TestGSBenchProcessExitsOnFirstInterrupt' -count=1
```

Expected: FAIL because `commandContext` is undefined.

- [x] **Step 3: Implement the minimal signal change**

Remove `os/signal`, `syscall`, and `signal.NotifyContext` from `main.go`. Add:

```go
func commandContext() context.Context {
	return context.Background()
}
```

Pass `commandContext()` to `gsbench.RunCLI`. The operating system now terminates the process on the first Ctrl+C.

- [x] **Step 4: Run the focused test and verify GREEN**

Run:

```bash
go test ./cmd/gsbench -run 'TestCommandContextLeavesInterruptsToOperatingSystem|TestGSBenchProcessExitsOnFirstInterrupt' -count=1
```

Expected: PASS.

---

### Task 2: Build and reproduce against og5

**Files:**
- Replace deployment artifact: `/Users/sqlrush/gstop/gsbench-local/bin/gsbench`

**Interfaces:**
- Consumes: current `/Users/sqlrush/gstop/gsbench-local/gsbench.cfg`, openGauss container `og5`, initialized 100GB schema.
- Produces: a locally installed macOS ARM64 gsbench binary with measured one-Ctrl+C termination behavior.

- [x] **Step 1: Run minimum package verification**

Run:

```bash
go test ./internal/gsbench ./cmd/gsbench
go build ./cmd/gsbench
```

Expected: PASS.

- [x] **Step 2: Build and deploy the native binary**

Build the current host target to a temporary file, then replace `/Users/sqlrush/gstop/gsbench-local/bin/gsbench` only after the build succeeds.

- [x] **Step 3: Recover the prior interrupted run**

Run:

```bash
gsbench restore --run-id 20260802T093145-2poyq
```

Expected: recovery completes and no tagged sessions remain.

- [x] **Step 4: Reproduce the exact 101 command**

Start:

```bash
gsbench run 101 --workers 10 --duration 1m
```

After workers are active, send one Ctrl+C. Assert the process exits due to SIGINT without entering Runner restore, and the tagged session count falls to zero as the process sockets close.

- [x] **Step 5: Report the measured result**

Report the exit latency, worker/session state after exit, unit-test result, deployed binary path, and explicit recovery behavior for interrupted runs.
