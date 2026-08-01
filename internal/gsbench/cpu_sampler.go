package gsbench

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type cpuFileReader func(string) ([]byte, error)

type sharedCPUSampler struct {
	underlying CPUSampler
	minimumAge time.Duration
	now        func() time.Time

	mu        sync.Mutex
	have      bool
	sampledAt time.Time
	value     float64
	available bool
	err       error
}

func newSharedCPUSampler(
	underlying CPUSampler,
	minimumAge time.Duration,
	now func() time.Time,
) *sharedCPUSampler {
	if minimumAge <= 0 {
		minimumAge = 100 * time.Millisecond
	}
	if now == nil {
		now = time.Now
	}
	return &sharedCPUSampler{
		underlying: underlying,
		minimumAge: minimumAge,
		now:        now,
	}
}

func (s *sharedCPUSampler) SampleCPU(ctx context.Context) (float64, bool) {
	value, available, _ := s.SampleCPUResult(ctx)
	return value, available
}

func (s *sharedCPUSampler) SampleCPUResult(
	ctx context.Context,
) (float64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.have && now.Sub(s.sampledAt) >= 0 &&
		now.Sub(s.sampledAt) < s.minimumAge {
		return s.value, s.available, s.err
	}
	var value float64
	var available bool
	var err error
	if detailed, ok := s.underlying.(cpuResultSampler); ok {
		value, available, err = detailed.SampleCPUResult(ctx)
	} else if s.underlying != nil {
		value, available = s.underlying.SampleCPU(ctx)
	}
	s.have = true
	s.sampledAt = s.now()
	s.value = value
	s.available = available
	s.err = err
	return value, available, err
}

type cgroupCPUSampler struct {
	statPath  string
	readFile  cpuFileReader
	now       func() time.Time
	quotaCPUs float64

	mu          sync.Mutex
	usageMicros int64
	sampledAt   time.Time
}

func newCgroupCPUSampler(
	statPath string,
	maxPath string,
	readFile cpuFileReader,
	now func() time.Time,
) (*cgroupCPUSampler, error) {
	if readFile == nil {
		readFile = os.ReadFile
	}
	if now == nil {
		now = time.Now
	}
	maximum, err := readFile(maxPath)
	if err != nil {
		return nil, fmt.Errorf("read cgroup cpu quota: %w", err)
	}
	quotaCPUs, err := parseCgroupCPUQuota(maximum)
	if err != nil {
		return nil, err
	}
	stat, err := readFile(statPath)
	if err != nil {
		return nil, fmt.Errorf("read cgroup cpu usage: %w", err)
	}
	usageMicros, err := parseCgroupCPUUsage(stat)
	if err != nil {
		return nil, err
	}
	return &cgroupCPUSampler{
		statPath: statPath, readFile: readFile, now: now,
		quotaCPUs: quotaCPUs, usageMicros: usageMicros, sampledAt: now(),
	}, nil
}

func (s *cgroupCPUSampler) SampleCPU(ctx context.Context) (float64, bool) {
	value, available, _ := s.SampleCPUResult(ctx)
	return value, available
}

func (s *cgroupCPUSampler) SampleCPUResult(
	ctx context.Context,
) (float64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stat, err := s.readFile(s.statPath)
	if err != nil {
		return 0, false, fmt.Errorf("read cgroup cpu usage: %w", err)
	}
	usageMicros, err := parseCgroupCPUUsage(stat)
	if err != nil {
		return 0, false, err
	}
	sampledAt := s.now()

	deltaUsage := usageMicros - s.usageMicros
	deltaTime := sampledAt.Sub(s.sampledAt)
	s.usageMicros = usageMicros
	s.sampledAt = sampledAt
	if deltaUsage < 0 || deltaTime <= 0 || s.quotaCPUs <= 0 {
		return 0, false, nil
	}
	capacityMicros := float64(deltaTime.Microseconds()) * s.quotaCPUs
	if capacityMicros <= 0 {
		return 0, false, nil
	}
	value := float64(deltaUsage) / capacityMicros * 100
	if value < 0 {
		value = 0
	}
	if value > 100 {
		value = 100
	}
	return value, true, nil
}

func parseCgroupCPUQuota(data []byte) (float64, error) {
	fields := strings.Fields(string(data))
	if len(fields) < 2 || fields[0] == "max" {
		return 0, fmt.Errorf("cgroup CPU quota is unavailable")
	}
	quota, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || quota <= 0 {
		return 0, fmt.Errorf("invalid cgroup CPU quota %q", fields[0])
	}
	period, err := strconv.ParseFloat(fields[1], 64)
	if err != nil || period <= 0 {
		return 0, fmt.Errorf("invalid cgroup CPU period %q", fields[1])
	}
	return quota / period, nil
}

func parseCgroupCPUUsage(data []byte) (int64, error) {
	fields := strings.Fields(string(data))
	for index := 0; index+1 < len(fields); index += 2 {
		if fields[index] != "usage_usec" {
			continue
		}
		value, err := strconv.ParseInt(fields[index+1], 10, 64)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("invalid cgroup CPU usage %q", fields[index+1])
		}
		return value, nil
	}
	return 0, fmt.Errorf("cgroup CPU usage_usec is unavailable")
}

func sharedCgroupV2Path(self, database string) (string, bool) {
	selfPath, selfOK := cgroupV2Path(self)
	databasePath, databaseOK := cgroupV2Path(database)
	return selfPath, selfOK && databaseOK && selfPath == databasePath
}

func cgroupV2Path(membership string) (string, bool) {
	for _, line := range strings.Split(strings.TrimSpace(membership), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" && parts[1] == "" {
			path := strings.TrimSpace(parts[2])
			if path == "" {
				path = "/"
			}
			return path, true
		}
	}
	return "", false
}

func databaseHostIsLocal(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || host == "localhost" || strings.HasPrefix(host, "/") {
		return true
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func databaseInitProcess(comm []byte) bool {
	switch strings.ToLower(strings.TrimSpace(string(comm))) {
	case "gaussdb", "postgres", "opengauss":
		return true
	default:
		return false
	}
}

func newRuntimeCPUSampler(
	ctx context.Context,
	db *Database,
	cfg BenchConfig,
	log *RunLog,
) CPUSampler {
	fallback := newSharedCPUSampler(
		NewDatabaseCPUSampler(db),
		100*time.Millisecond,
		time.Now,
	)
	if !databaseHostIsLocal(cfg.Database.Host) {
		if log != nil {
			log.Info("cpu_sampler=database_os_runtime reason=remote_database")
		}
		return fallback
	}
	initProcess, initErr := os.ReadFile("/proc/1/comm")
	if initErr != nil || !databaseInitProcess(initProcess) {
		if log != nil {
			log.Info("cpu_sampler=database_os_runtime reason=database_not_container_init")
		}
		return fallback
	}
	selfMembership, selfErr := os.ReadFile("/proc/self/cgroup")
	containerMembership, containerErr := os.ReadFile("/proc/1/cgroup")
	path, shared := sharedCgroupV2Path(
		string(selfMembership),
		string(containerMembership),
	)
	if selfErr != nil || containerErr != nil || !shared {
		if log != nil {
			log.Info("cpu_sampler=database_os_runtime reason=cgroup_not_shared")
		}
		return fallback
	}
	root := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(path, "/"))
	sampler, err := newCgroupCPUSampler(
		filepath.Join(root, "cpu.stat"),
		filepath.Join(root, "cpu.max"),
		os.ReadFile,
		time.Now,
	)
	if err != nil {
		if log != nil {
			log.Info("cpu_sampler=database_os_runtime reason=cgroup_quota_unavailable")
		}
		return fallback
	}
	if log != nil {
		log.Info("cpu_sampler=cgroup_v2 quota_cores=%.2f", sampler.quotaCPUs)
	}
	return newSharedCPUSampler(
		sampler,
		100*time.Millisecond,
		time.Now,
	)
}
