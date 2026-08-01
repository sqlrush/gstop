# gsbench Runtime Validation Switch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add one default-off runtime validation switch and produce a Linux ARM64 gsbench release.

**Architecture:** `run.validation_enabled` is parsed once into `RunConfig` and propagated through existing orchestration objects. Each validation boundary branches on the same value, while mutations, restoration, security checks, and real execution errors remain unchanged.

**Tech Stack:** Go, existing gsbench config loader and tests, Linux ARM64 cross-compilation.

## Global Constraints

- The single parameter is `run.validation_enabled`.
- Its default is `false`.
- Do not disable configuration/identifier security, risk authorization, actual SQL errors, restore actions, or journal persistence.
- Perform only focused package verification and one target build.

---

### Task 1: Configuration contract

**Files:**
- Modify: `internal/gsbench/config.go`
- Modify: `internal/gsbench/config_test.go`
- Modify: `configs/gsbench.cfg`

**Interfaces:**
- Produces: `RunConfig.ValidationEnabled bool`

- [ ] Write config tests proving omitted means false and explicit true means true.
- [ ] Run the tests and confirm explicit true fails before implementation.
- [ ] Parse `run.validation_enabled` and document it in the shipped config.
- [ ] Run the config tests and confirm both behaviors pass.

### Task 2: Runtime validation boundaries

**Files:**
- Modify: `internal/gsbench/dataset.go`
- Modify: `internal/gsbench/journal.go`
- Modify: `internal/gsbench/restore.go`
- Modify: `internal/gsbench/runner.go`
- Modify: `internal/gsbench/app.go`
- Modify: focused tests in `internal/gsbench/*_test.go`

**Interfaces:**
- Consumes: `RunConfig.ValidationEnabled bool`
- Produces: validation-aware constructors/options for dataset, journal, and restore orchestration.

- [ ] Write focused failing tests that disabled mode ignores validation-only failures and enabled mode retains them.
- [ ] Run those tests and confirm they fail for the missing switch wiring.
- [ ] Gate dataset, plan/scenario, journal, and restore validations on the shared value.
- [ ] Wire the config value from all commands and log the selected mode.
- [ ] Run the focused gsbench tests and fix only regressions caused by this change.

### Task 3: Release artifact

**Files:**
- Create: `release/gsbench-v1.1.0-linux-arm64-validation-switch-default-off-20260801/`
- Create: `release/gsbench-v1.1.0-linux-arm64-validation-switch-default-off-20260801.tar.gz`

**Interfaces:**
- Consumes: completed gsbench source and shipped configuration.
- Produces: Linux ARM64 binary, config, installation guide, and checksum.

- [ ] Cross-compile `cmd/gsbench` for `linux/arm64` with the repository overlay required by this checkout.
- [ ] Package the binary, config, and concise install/configuration manual.
- [ ] Run the binary metadata check and compute SHA-256.
- [ ] Commit and push the implementation to GitHub `main`.

## Self-Review

- Spec coverage: configuration default, explicit enable, all named runtime validation boundaries, preserved execution/security behavior, and ARM64 packaging are represented.
- Placeholder scan: no deferred implementation placeholders are present.
- Type consistency: all tasks consume the same `RunConfig.ValidationEnabled bool` value.
