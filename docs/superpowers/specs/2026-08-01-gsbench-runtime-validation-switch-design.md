# gsbench Runtime Validation Switch Design

## Goal

Use one configuration parameter, `run.validation_enabled`, to control gsbench runtime validation. Its default is `false`, so validation failures do not block test initialization, workload execution, or restoration unless the user explicitly enables validation.

## Behavior

- `run.validation_enabled = false` (default): skip dataset capacity rejection and per-batch capacity checks, dataset object/layout and hard-target validation, plan preflight/baseline validation, scenario result verification, action preflight/restore verification, and final topology/restore-state verification.
- `run.validation_enabled = true`: retain the existing validation behavior.
- Keep configuration and identifier safety checks, risk authorization, actual DDL/DML/workload errors, restore action execution errors, locking, and journal state persistence. These are program correctness and security boundaries rather than optional result validation.
- Keep baseline repair and other restoration actions. Only their follow-up validation is controlled by the switch.

## Wiring

Parse the parameter into `RunConfig.ValidationEnabled` and pass that value to the dataset manager, journal, runner, and restore coordinator/backend. The shipped `configs/gsbench.cfg` documents the default as disabled. Runtime logs print the selected validation mode so test output is unambiguous.

## Verification

Add focused tests for the default and explicit config values, then tests showing disabled validation bypasses dataset, runner, journal, and restore validation while enabled mode preserves current behavior. Run the internal gsbench test package and cross-compile Linux ARM64.
