// Package timing centralises the elapsed-time instrumentation the original tool
// implemented as decorators and context managers in common/util.py: a slow-call
// logger for SQL and shell commands, and a wrapper that times a monitor refresh
// while turning panics into logged tracebacks instead of crashing the loop.
//
// Thresholds are passed in as durations so this package stays free of any
// configuration dependency; callers resolve the config value once.
package timing

import (
	"runtime/debug"
	"time"

	"gstop/internal/logging"
)

// Clock returns the current time; overridable in tests.
var Clock = time.Now

// LogSlow runs fn and logs a warning when it takes longer than threshold.
// label identifies the call (e.g. the SQL text or command) for the log line.
func LogSlow(logger *logging.Logger, kind, label string, threshold time.Duration, fn func()) {
	start := Clock()
	fn()
	if elapsed := Clock().Sub(start); elapsed > threshold {
		logger.Warning("Slow %s  Time used: %.3fs  detail: %s", kind, elapsed.Seconds(), label)
	}
}

// RefreshAnalyze runs fn under an elapsed-time guard and a panic recovery,
// matching refresh_analyze_wrapper + time_statistics_context. A panic in one
// module is logged with its stack and swallowed so sibling modules keep running.
func RefreshAnalyze(logger *logging.Logger, name string, threshold time.Duration, fn func()) {
	start := Clock()
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Exception in %q: %v\n%s", name, r, debug.Stack())
		}
		if elapsed := Clock().Sub(start); elapsed > threshold {
			logger.Warning("Module: %q executed finished in %.4f seconds", name, elapsed.Seconds())
		}
	}()
	fn()
}
