# gsbench Work-Memory Calibration Fallback Design

## Problem

Scenarios 201 and 202 calibrate a data range before starting workers. The current
calibrator accepts only an in-memory Sort or Hash observation between 90% and
97% of the requested `work_mem`. If that band cannot be reached, `Prepare`
returns an error and no pressure workers start.

On openGauss, an operator can spill before exhausting its session-local
`work_mem` because the global logical-memory protector also participates in the
decision. In the reproduced 64 MB Sort case, the largest non-spilling plan used
47,095 kB (71.9%). Adding thirteen rows caused an early spill even though the
configured `work_mem` was active. Treating this database decision as a fatal
gsbench error rejects a usable workload.

## Required Behavior

- The preferred non-spilling calibration band is 70% through 97% of requested
  `work_mem`, inclusive.
- A non-spilling observation inside that band completes calibration normally.
- If the band is unreachable but at least one usable non-spilling observation
  exists, calibration returns the best such observation and pressure execution
  continues.
- The fallback emits one `WARN` line containing the scenario, requested kB,
  observed kB, observed percentage, calibrated range, attempts, and a clear
  statement that the preferred band was not reached.
- Evidence records whether the preferred target was met and the observed
  percentage, so a successful workload is not confused with a successful
  calibration target.
- SQL/connection errors, a missing expected plan operator, or the absence of
  every usable non-spilling observation remain fatal errors. These conditions
  mean gsbench cannot construct the requested pressure workload.

## Calibration Selection

The binary search keeps its existing attempt and range bounds. During probing it
tracks the best usable non-spilling observation. Preference is deterministic:

1. Return immediately for an observation in the 70%-97% band.
2. Otherwise prefer the largest non-spilling observation below 70%.
3. If no observation exists below the band, retain the smallest non-spilling
   observation above 97%.

A spill is never selected as the worker data range. Spill observations only
move the binary-search upper bound downward.

## Runtime Flow

`Prepare` stores the calibration result before creating worker sessions. For a
fallback result it logs the warning, then constructs exactly the same fixed
worker workload using the selected non-spilling range and the user-requested
`work_mem`. Worker count, duration, Ctrl+C handling, SQL shape, and cleanup are
unchanged.

The database may still spill under concurrent workers as global pressure rises.
That is valid database behavior: gsbench controls session count, duration, and
requested `work_mem`, while openGauss retains authority over physical memory and
spill decisions.

## Verification

Automated regression tests must prove:

- 70%-97% is accepted as the preferred band.
- The reproduced 64 MB cliff (47,095 kB non-spilling, next range spilling)
  meets the new 70% lower bound and returns normally rather than as an error.
- The same approximately 47 MB database cap against a 256 MB request preserves
  the best non-spilling observation, marks the preferred target as unmet, and
  continues through the fallback path.
- A calibration with no usable non-spilling observation still fails.
- Existing spill backoff and bounded-attempt behavior remain intact.

After tests pass, build and deploy the local binary, then run scenario 201
against og5 with five workers, 64 MB `work_mem`, and a short duration. Success
requires a warning or normal calibration result followed by actual worker
startup and duration completion, not a prepare-stage calibration failure.

## Out of Scope

- Disabling openGauss memory protection.
- Guaranteeing that physical memory use equals requested `work_mem`.
- Changing scenarios other than 201 and 202.
- Changing worker-count, duration, or signal-handling semantics.
