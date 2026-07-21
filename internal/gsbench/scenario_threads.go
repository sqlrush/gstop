package gsbench

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var threadWorkerRE = regexp.MustCompile(`actual:\s*(\d+)\s+idle:\s*(\d+)`)

func selectThreadStrategy(capabilities Capabilities) string {
	if capabilities.ThreadPoolEnabled && capabilities.ThreadPoolView {
		return "real"
	}
	return "active_backend_fallback"
}

func selectThreadStrategyForRun(capabilities Capabilities, cfg BenchConfig) string {
	if selectThreadStrategy(capabilities) == "real" {
		return "real"
	}
	if capabilities.Admin && cfg.Safety.AllowInstanceParameterChange && cfg.Safety.AllowDatabaseRestart && cfg.Safety.RestartCommand != "" && len(cfg.Run.Scenarios) == 1 && cfg.Run.Scenarios[0] == "thread_pool" {
		return "enable_with_restart"
	}
	return "active_backend_fallback"
}

func ParseThreadPoolWorkers(lines []string) (actual, idle int, ok bool) {
	for _, line := range lines {
		match := threadWorkerRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		a, errA := strconv.Atoi(match[1])
		i, errI := strconv.Atoi(match[2])
		if errA != nil || errI != nil {
			continue
		}
		actual += a
		idle += i
		ok = true
	}
	return
}

type ThreadScenario struct {
	workload *sqlWorkload
	control  ControlResult
	target   float64
	real     bool
	strategy string
	changed  bool
}

func NewThreadScenario() *ThreadScenario { return &ThreadScenario{} }
func (s *ThreadScenario) Name() string   { return "thread_pool" }
func (s *ThreadScenario) Prepare(ctx context.Context, rt *Runtime) error {
	s.strategy = selectThreadStrategyForRun(rt.Capabilities, rt.Config)
	s.real = s.strategy == "real"
	if s.strategy == "enable_with_restart" {
		if rt.Journal == nil {
			return fmt.Errorf("mutation journal is unavailable")
		}
		mutation := Mutation{RunID: rt.RunID, Scenario: s.Name(), Kind: "instance_parameter", Target: "enable_thread_pool", Original: "off", ForwardSQL: "ALTER SYSTEM SET enable_thread_pool TO on", InverseSQL: "ALTER SYSTEM SET enable_thread_pool TO off"}
		if err := rt.Journal.Apply(ctx, mutation); err != nil {
			return err
		}
		s.changed = true
		if err := restartAndWait(ctx, rt); err != nil {
			return err
		}
		value, err := rt.Database.Probe(ctx, "enable_thread_pool", "SHOW enable_thread_pool")
		if err != nil || !truthy(value) {
			return fmt.Errorf("thread pool did not become active after restart: value=%q err=%v", value, err)
		}
		s.real = true
	}
	s.target = float64(runtimeInt(rt, "scenario.thread_pool.target_percent", 95))
	s.workload = newSQLWorkload(ctx, rt, s.Name(), rt.Config.Safety.MaxWorkers, func(ctx context.Context, conn *sql.Conn, _ int) error {
		_, err := conn.ExecContext(ctx, "SELECT pg_sleep(1)")
		return err
	})
	return nil
}
func (s *ThreadScenario) sample(ctx context.Context, rt *Runtime) Sample {
	if !s.real {
		return Sample{Available: false, Errors: s.workload.Snapshot().Errors}
	}
	rows, err := rt.Database.Query(ctx, "SELECT worker_info FROM dbe_perf.global_threadpool_status")
	if err != nil {
		return Sample{Errors: s.workload.Snapshot().Errors}
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if rows.Scan(&line) == nil {
			lines = append(lines, line)
		}
	}
	actual, idle, ok := ParseThreadPoolWorkers(lines)
	if !ok || actual == 0 {
		return Sample{Errors: s.workload.Snapshot().Errors}
	}
	return Sample{Available: true, Value: float64(actual-idle) / float64(actual) * 100, Errors: s.workload.Snapshot().Errors}
}
func (s *ThreadScenario) Ramp(ctx context.Context, rt *Runtime) error {
	c := Controller{Config: ControllerConfig{Target: s.target, Tolerance: 3, MinWorkers: 1, MaxWorkers: rt.Config.Safety.MaxWorkers, Step: 1, RequiredSamples: 3, Interval: rt.Config.Run.RampInterval}, Actuator: s.workload, Sample: func(ctx context.Context) Sample { return s.sample(ctx, rt) }}
	s.control = c.Run(ctx)
	return s.control.Err
}
func (s *ThreadScenario) Hold(ctx context.Context, rt *Runtime) error {
	return waitContext(ctx, rt.Config.Run.Duration)
}
func (s *ThreadScenario) Verify(context.Context, *Runtime) (Result, error) {
	return verifyCapacityResult(s.Name(), s.target, s.control.Actual, s.real, s.workload.Snapshot().Operations), nil
}
func (s *ThreadScenario) Stop(ctx context.Context, _ *Runtime) error {
	if s.workload == nil {
		return nil
	}
	return s.workload.Stop(ctx)
}
func (s *ThreadScenario) Restore(ctx context.Context, rt *Runtime) error {
	if !s.changed {
		return nil
	}
	if err := rt.Journal.RestoreScenario(ctx, rt.RunID, s.Name()); err != nil {
		return err
	}
	if err := restartAndWait(ctx, rt); err != nil {
		return err
	}
	value, err := rt.Database.Probe(ctx, "enable_thread_pool", "SHOW enable_thread_pool")
	if err != nil {
		return err
	}
	if truthy(value) {
		return fmt.Errorf("enable_thread_pool remained on after restore")
	}
	return nil
}

func restartAndWait(ctx context.Context, rt *Runtime) error {
	restartCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(restartCtx, "sh", "-c", rt.Config.Safety.RestartCommand)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("restart command failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := rt.Database.Ping(restartCtx); err == nil {
			return nil
		}
		select {
		case <-restartCtx.Done():
			return fmt.Errorf("database did not return after restart: %w", restartCtx.Err())
		case <-ticker.C:
		}
	}
}
