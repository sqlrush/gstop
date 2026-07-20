// Package oscmd runs shell commands for the OS-level monitors and emergency
// remediation, reproducing util.execute_os_command and util.run_command_background.
//
// Faithful to the original: a command whose exit status is non-zero (when
// checking is enabled) or that writes anything to stderr is treated as a
// failure and yields no output, so a partially-working pipeline never feeds
// garbage into the display.
package oscmd

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"

	"gstop/internal/logging"
	"gstop/internal/timing"
)

// Runner executes shell commands, logging slow ones past the configured threshold.
type Runner struct {
	logger    *logging.Logger
	slowAfter time.Duration
}

// New builds a Runner. slowAfter is the threshold past which a command is logged
// as slow (config main.sql_command_time_thresh).
func New(logger *logging.Logger, slowAfter time.Duration) *Runner {
	return &Runner{logger: logger, slowAfter: slowAfter}
}

// Run executes command through /bin/sh and returns its trimmed stdout. It
// returns ok=false when the command errors, when check is set and the exit
// status is non-zero, or when anything is written to stderr.
func (r *Runner) Run(command string, check bool) (out string, ok bool) {
	timing.LogSlow(r.logger, "command", command, r.slowAfter, func() {
		out, ok = r.run(command, check)
	})
	return out, ok
}

func (r *Runner) run(command string, check bool) (string, bool) {
	cmd := exec.Command("/bin/sh", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil && check {
		r.logger.Error("Exec command '%s' failed: %v", command, err)
		return "", false
	}
	if stderr.Len() > 0 {
		r.logger.Error("Exec command '%s' meets error: %s", command, stderr.String())
		return "", false
	}
	return strings.TrimSpace(stdout.String()), true
}

// RunBackground starts command detached and lets it run up to timeout, after
// which it is terminated (SIGTERM, then SIGKILL after a grace period). Mirrors
// util.run_command_background; used for perf data collection on jitter alerts.
func (r *Runner) RunBackground(command string, timeout time.Duration) {
	go func() {
		r.logger.Warning("Exec background command: %s", command)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		// WaitDelay bounds the grace period between SIGTERM (on ctx timeout) and
		// the forced SIGKILL, mirroring the Python terminate-then-kill sequence.
		cmd.WaitDelay = 5 * time.Second

		if err := cmd.Start(); err != nil {
			r.logger.Error("Start background command failed: %v", err)
			return
		}
		if err := cmd.Wait(); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				r.logger.Warning("Exec background command timeout")
				return
			}
			r.logger.Warning("Exec background command finished, err: %v", err)
			return
		}
		r.logger.Warning("Exec background command finished, ret: 0")
	}()
}
