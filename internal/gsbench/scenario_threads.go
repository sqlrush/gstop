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

var threadWorkerRE = regexp.MustCompile(`actual:\s*(\d+)\s+idle:\s*(\d+)(?:\s+pending:\s*(\d+))?`)

type ThreadPoolStatus struct {
	Actual  int
	Idle    int
	Pending int
}

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
	if capabilities.Admin && cfg.Safety.AllowInstanceParameterChange && cfg.Safety.AllowDatabaseRestart && cfg.Safety.RestartCommand != "" && len(cfg.Run.ScenarioCodes) == 1 && cfg.Run.ScenarioCodes[0] == 402 {
		return "enable_with_restart"
	}
	return "active_backend_fallback"
}

func ParseThreadPoolWorkers(lines []string) (actual, idle int, ok bool) {
	status, ok := ParseThreadPoolStatus(lines)
	return status.Actual, status.Idle, ok
}

func ParseThreadPoolStatus(lines []string) (ThreadPoolStatus, bool) {
	var status ThreadPoolStatus
	var ok bool
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
		status.Actual += a
		status.Idle += i
		if len(match) > 3 && match[3] != "" {
			pending, err := strconv.Atoi(match[3])
			if err != nil {
				continue
			}
			status.Pending += pending
		}
		ok = true
	}
	return status, ok
}

func threadSessionCapacity(instanceMax, reserved, existing, maxWorkers, maxConnections int) int {
	return max(0, min(instanceMax-reserved-existing, maxWorkers, maxConnections))
}

func threadUtilizationCeiling(actualWorkers, sessionCapacity int) float64 {
	if actualWorkers <= 0 || sessionCapacity <= 0 {
		return 0
	}
	return min(100, float64(sessionCapacity)/float64(actualWorkers)*100)
}

func sampleThreadPoolStatus(ctx context.Context, rt *Runtime) (ThreadPoolStatus, error) {
	rows, err := rt.Database.Query(ctx, "SELECT worker_info FROM dbe_perf.global_threadpool_status")
	if err != nil {
		return ThreadPoolStatus{}, err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return ThreadPoolStatus{}, err
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return ThreadPoolStatus{}, err
	}
	status, ok := ParseThreadPoolStatus(lines)
	if !ok || status.Actual <= 0 || status.Idle < 0 || status.Idle > status.Actual {
		return ThreadPoolStatus{}, fmt.Errorf("thread pool worker status is unavailable")
	}
	return status, nil
}

type ThreadScenario struct {
	workload        *sqlWorkload
	control         ControlResult
	loop            continuousControl
	target          float64
	real            bool
	strategy        string
	changed         bool
	maxWorkers      int
	capacityCeiling float64
	lastStatus      ThreadPoolStatus
}

func NewThreadScenario() *ThreadScenario { return &ThreadScenario{} }
func (s *ThreadScenario) Code() ScenarioCode {
	return 402
}
func (s *ThreadScenario) Name() string { return "thread_pool" }
func (s *ThreadScenario) Strategy() string {
	if s.strategy == "" {
		return "thread_pool_capability_select"
	}
	return s.strategy
}
func (s *ThreadScenario) Prepare(ctx context.Context, rt *Runtime) error {
	s.strategy = selectThreadStrategyForRun(rt.Capabilities, rt.Config)
	s.real = s.strategy == "real"
	if s.strategy == "enable_with_restart" {
		if rt.Journal == nil {
			return fmt.Errorf("mutation journal is unavailable")
		}
		mutation := Mutation{RunID: rt.RunID, ScenarioCode: s.Code(), Kind: "instance_parameter", Target: "enable_thread_pool", Original: "off", ForwardSQL: "ALTER SYSTEM SET enable_thread_pool TO on", InverseSQL: "ALTER SYSTEM SET enable_thread_pool TO off"}
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
	facts, err := probeConnectionCapacity(ctx, rt)
	if err != nil {
		return err
	}
	s.maxWorkers = threadSessionCapacity(
		facts.InstanceMax, facts.Reserved, facts.Existing,
		rt.Config.Safety.MaxWorkers, rt.Config.Safety.MaxConnections,
	)
	if s.maxWorkers < 1 {
		return fmt.Errorf("thread pool target is unreachable: no safe workload session capacity")
	}
	s.workload = newSQLWorkload(ctx, rt, s.Name(), s.maxWorkers, func(ctx context.Context, conn *sql.Conn, _ int) error {
		_, err := conn.ExecContext(ctx, "SELECT pg_sleep(1)")
		return err
	})
	return nil
}
func (s *ThreadScenario) sample(ctx context.Context, rt *Runtime) Sample {
	snapshot := s.workload.Snapshot()
	if err := workerSnapshotError(snapshot); err != nil {
		return Sample{Errors: snapshot.Errors, Err: err}
	}
	if !s.real {
		return Sample{Available: false}
	}
	status, err := sampleThreadPoolStatus(ctx, rt)
	if err != nil {
		return Sample{Err: err}
	}
	s.lastStatus = status
	s.capacityCeiling = threadUtilizationCeiling(status.Actual, s.maxWorkers)
	return Sample{Available: true, Value: float64(status.Actual-status.Idle) / float64(status.Actual) * 100}
}
func (s *ThreadScenario) Ramp(ctx context.Context, rt *Runtime) error {
	c := Controller{Config: ControllerConfig{Target: s.target, Tolerance: 3, MinWorkers: 1, MaxWorkers: s.maxWorkers, RequiredSamples: 3, Interval: rt.Config.Run.RampInterval}, Actuator: s.workload, Sample: func(ctx context.Context) Sample { return s.sample(ctx, rt) }}
	s.loop.Start(ctx, c)
	return nil
}
func (s *ThreadScenario) Hold(ctx context.Context, rt *Runtime) error {
	var err error
	s.control, err = s.loop.Wait(ctx, rt.Config.Run.Duration)
	return err
}
func (s *ThreadScenario) Verify(context.Context, *Runtime) (Result, error) {
	if s.capacityCeiling < s.target && !s.control.Reached {
		s.control.Ceiling = true
	}
	result := verifyControlledCapacityResult(s.Name(), s.target, s.real, s.control, s.workload.Snapshot().Operations)
	result.Evidence = append(result.Evidence,
		Evidence{Metric: "thread_pool_actual_workers", Actual: float64(s.lastStatus.Actual), Available: s.real},
		Evidence{Metric: "thread_pool_idle_workers", Actual: float64(s.lastStatus.Idle), Available: s.real},
		Evidence{Metric: "thread_pool_pending_sessions", Actual: float64(s.lastStatus.Pending), Available: s.real},
		Evidence{Metric: "topology_session_ceiling_percent", Target: s.target, Actual: s.capacityCeiling, Available: s.real},
	)
	if s.control.Ceiling && !s.control.Reached {
		result.Message = fmt.Sprintf("thread_pool target %.1f%% is unreachable; topology/session ceiling %.1f%%, measured peak %.1f%%", s.target, s.capacityCeiling, s.control.ReachableMax)
	}
	return result, nil
}
func (s *ThreadScenario) ExecutionSnapshot() WorkerSnapshot {
	if s.workload == nil {
		return WorkerSnapshot{}
	}
	return s.workload.Snapshot()
}
func (s *ThreadScenario) Stop(ctx context.Context, _ *Runtime) error {
	s.control = s.loop.Stop()
	if s.workload == nil {
		return nil
	}
	return s.workload.Stop(ctx)
}
func (s *ThreadScenario) Restore(context.Context, *Runtime) error { return nil }

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
