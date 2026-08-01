package gsbench

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

type countingCPUSampler struct {
	calls atomic.Int64
}

func (s *countingCPUSampler) SampleCPU(context.Context) (float64, bool) {
	s.calls.Add(1)
	return 75, true
}

func TestCgroupCPUSamplerNormalizesUsageAgainstQuota(t *testing.T) {
	stat := "usage_usec 1000000\n"
	now := time.Unix(100, 0)
	readFile := func(path string) ([]byte, error) {
		switch path {
		case "cpu.max":
			return []byte("200000 100000\n"), nil
		case "cpu.stat":
			return []byte(stat), nil
		default:
			return nil, fmt.Errorf("unexpected path %q", path)
		}
	}
	sampler, err := newCgroupCPUSampler(
		"cpu.stat",
		"cpu.max",
		readFile,
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	stat = "usage_usec 2000000\n"
	now = now.Add(time.Second)
	value, available, err := sampler.SampleCPUResult(context.Background())
	if err != nil || !available {
		t.Fatalf("value=%v available=%v err=%v", value, available, err)
	}
	if value != 50 {
		t.Fatalf("quota-normalized CPU=%v, want 50", value)
	}
}

func TestCgroupV2PathRequiresMatchingDatabaseMembership(t *testing.T) {
	path, ok := sharedCgroupV2Path(
		"0::/docker/og5\n",
		"0::/docker/og5\n",
	)
	if !ok || path != "/docker/og5" {
		t.Fatalf("path=%q ok=%v", path, ok)
	}
	if _, ok := sharedCgroupV2Path(
		"0::/docker/gsbench\n",
		"0::/docker/database\n",
	); ok {
		t.Fatal("different database and gsbench cgroups were treated as shared")
	}
}

func TestParseCgroupCPUQuotaRejectsUnlimitedQuota(t *testing.T) {
	if _, err := parseCgroupCPUQuota([]byte("max 100000\n")); err == nil {
		t.Fatal("unlimited cgroup quota unexpectedly accepted")
	}
	quota, err := parseCgroupCPUQuota([]byte("400000 100000\n"))
	if err != nil || quota != 4 {
		t.Fatalf("quota=%v err=%v", quota, err)
	}
}

func TestSharedCPUSamplerReturnsOneWindowToConcurrentConsumers(t *testing.T) {
	underlying := &countingCPUSampler{}
	now := time.Unix(100, 0)
	sampler := newSharedCPUSampler(
		underlying,
		100*time.Millisecond,
		func() time.Time { return now },
	)
	first, firstAvailable := sampler.SampleCPU(context.Background())
	second, secondAvailable := sampler.SampleCPU(context.Background())
	if first != 75 || second != 75 || !firstAvailable || !secondAvailable {
		t.Fatalf("first=%v/%v second=%v/%v", first, firstAvailable, second, secondAvailable)
	}
	if calls := underlying.calls.Load(); calls != 1 {
		t.Fatalf("same sample window consumed %d delta samples", calls)
	}
}

func TestContainerInitMustBeDatabaseProcessForCgroupSampling(t *testing.T) {
	for _, accepted := range []string{"gaussdb\n", "postgres\n", "opengauss\n"} {
		if !databaseInitProcess([]byte(accepted)) {
			t.Fatalf("database init process %q rejected", accepted)
		}
	}
	for _, rejected := range []string{"bash\n", "tini\n", "systemd\n", ""} {
		if databaseInitProcess([]byte(rejected)) {
			t.Fatalf("non-database init process %q accepted", rejected)
		}
	}
}
