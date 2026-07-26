package gsbench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gsbench.cfg")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func minimalConfig() string {
	return `[database]
host = 127.0.0.1
port = 5433
database = postgres
user = bench
password_env = GSBENCH_TEST_PASSWORD

[run]
scenarios = tp_cpu
duration = 10m

[data]
schema = gsbench
`
}

func TestConfigLoadsDefaultsAndDurations(t *testing.T) {
	cfg, err := LoadConfig(writeTestConfig(t, minimalConfig()), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Run.Duration != 10*time.Minute {
		t.Fatalf("duration = %v", cfg.Run.Duration)
	}
	if cfg.Safety.CPUTargetPercent != 95 || cfg.Safety.MaxWorkers != 256 {
		t.Fatalf("safety defaults = %+v", cfg.Safety)
	}
	if got := cfg.Run.Scenarios; len(got) != 1 || got[0] != "tp_cpu" {
		t.Fatalf("scenarios = %v", got)
	}
}

func TestConfigRejectsUnknownScenario(t *testing.T) {
	body := strings.Replace(minimalConfig(), "tp_cpu", "not_real", 1)
	if _, err := LoadConfig(writeTestConfig(t, body), Overrides{}); err == nil {
		t.Fatal("expected unknown scenario error")
	}
}

func TestConfigRejectsUnsafeSchema(t *testing.T) {
	body := strings.Replace(minimalConfig(), "schema = gsbench", "schema = x;drop schema public", 1)
	if _, err := LoadConfig(writeTestConfig(t, body), Overrides{}); err == nil {
		t.Fatal("expected unsafe schema error")
	}
}

func TestConfigReadsPasswordFromEnvironmentAndRedactsDSN(t *testing.T) {
	t.Setenv("GSBENCH_TEST_PASSWORD", "s'ecret value")
	cfg, err := LoadConfig(writeTestConfig(t, minimalConfig()), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	dsn := cfg.DSN("postgres", "gsbench/run/tp_cpu/1")
	if !strings.Contains(dsn, `password='s\'ecret value'`) {
		t.Fatalf("dsn did not quote password: %q", dsn)
	}
	if strings.Contains(cfg.Redacted(), "s'ecret value") || strings.Contains(RedactDSN(dsn), "s'ecret value") {
		t.Fatal("password leaked through redaction")
	}
}

func TestConfigStressProfileDefaultsToTwentyGB(t *testing.T) {
	body := strings.Replace(minimalConfig(), "duration = 10m", "duration = 10m\nprofile = stress", 1)
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Data.MaxSizeGB != 20 {
		t.Fatalf("stress default max_size_gb=%d", cfg.Data.MaxSizeGB)
	}
}

func TestConfigNormalizesScenarioNumbersNamesAndAliases(t *testing.T) {
	body := strings.Replace(minimalConfig(), "scenarios = tp_cpu", "scenarios = 1,ap,8", 1)
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tp_cpu", "ap_cpu", "lock_storm"}
	for i := range want {
		if cfg.Run.Scenarios[i] != want[i] {
			t.Fatalf("scenarios=%v", cfg.Run.Scenarios)
		}
	}
}

func TestConfigNormalizesSingleNumericScenario(t *testing.T) {
	body := strings.Replace(minimalConfig(), "scenarios = tp_cpu", "scenarios = 1", 1)
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Run.Scenarios) != 1 || cfg.Run.Scenarios[0] != "tp_cpu" {
		t.Fatalf("scenarios=%v", cfg.Run.Scenarios)
	}
}

func TestConfigRejectsInvalidScenarioNumbers(t *testing.T) {
	for _, identifier := range []string{"0", "10", "-1"} {
		t.Run(identifier, func(t *testing.T) {
			body := strings.Replace(minimalConfig(), "scenarios = tp_cpu", "scenarios = "+identifier, 1)
			if _, err := LoadConfig(writeTestConfig(t, body), Overrides{}); err == nil {
				t.Fatalf("expected unknown scenario %q error", identifier)
			}
		})
	}
}
