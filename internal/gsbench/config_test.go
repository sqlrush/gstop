package gsbench

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
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

func TestLoadConfigStoresAbsoluteCleanIdentityAndDirectory(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "release", "configs")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "gsbench.cfg")
	if err := os.WriteFile(configPath, []byte(minimalConfig()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	cfg, err := LoadConfig(
		filepath.Join("release", "configs", "..", "configs", "gsbench.cfg"),
		Overrides{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path != configPath || cfg.ConfigDir != configDir {
		t.Fatalf("config identity path=%q dir=%q want path=%q dir=%q", cfg.Path, cfg.ConfigDir, configPath, configDir)
	}
}

func TestConfigStatePathsAndRelativeSecretsAreIndependentOfCWD(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "release", "configs")
	secretDir := filepath.Join(configDir, "secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "gsbench.cfg")
	body := strings.Replace(
		minimalConfig(),
		"password_env = GSBENCH_TEST_PASSWORD",
		"password_env = GSBENCH_TEST_PASSWORD\npassword_config = secrets/gstop.cfg",
		1,
	)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(secretDir, "gstop.cfg"),
		[]byte("[main]\ndb_password = config-relative-secret\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	loadFrom := func(cwd string) BenchConfig {
		t.Helper()
		if err := os.MkdirAll(cwd, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Chdir(cwd)
		cfg, err := LoadConfig(configPath, Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	first := loadFrom(filepath.Join(root, "cwd-a"))
	second := loadFrom(filepath.Join(root, "cwd-b"))
	if first.Path != second.Path || first.ConfigDir != second.ConfigDir {
		t.Fatalf("config identity changed with cwd: first=%q/%q second=%q/%q", first.Path, first.ConfigDir, second.Path, second.ConfigDir)
	}
	if first.Database.Password != "config-relative-secret" ||
		second.Database.Password != "config-relative-secret" {
		t.Fatalf("relative password config was not anchored to config directory")
	}
	if first.FaultProvider.LedgerPath != second.FaultProvider.LedgerPath {
		t.Fatalf("ledger changed with cwd: %q != %q", first.FaultProvider.LedgerPath, second.FaultProvider.LedgerPath)
	}
	wantLedgerDir := filepath.Join(configDir, "logs")
	if filepath.Dir(first.FaultProvider.LedgerPath) != wantLedgerDir {
		t.Fatalf("ledger dir=%q want=%q", filepath.Dir(first.FaultProvider.LedgerPath), wantLedgerDir)
	}
	firstLog, err := runLogPath(first.ConfigDir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	secondLog, err := runLogPath(second.ConfigDir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if firstLog != secondLog || filepath.Dir(firstLog) != wantLedgerDir {
		t.Fatalf("run log changed with cwd: first=%q second=%q", firstLog, secondLog)
	}
	if first.FaultProvider.LedgerPath+".lock" != second.FaultProvider.LedgerPath+".lock" {
		t.Fatalf("ledger lock identity changed with cwd")
	}
}

func TestConfigRejectsConcurrentPlanChangeScenarios(t *testing.T) {
	body := strings.Replace(
		minimalConfig(),
		"scenarios = tp_cpu",
		"scenarios = 601,605",
		1,
	)
	_, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err == nil || !strings.Contains(err.Error(), "601-606") ||
		!strings.Contains(err.Error(), "serial") {
		t.Fatalf("multiple plan-change scenarios error=%v", err)
	}
}

func TestConfigLoadsDefaultsAndDurations(t *testing.T) {
	cfg, err := LoadConfig(writeTestConfig(t, minimalConfig()), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Run.Duration != 10*time.Minute {
		t.Fatalf("duration = %v", cfg.Run.Duration)
	}
	if cfg.Safety.MaxWorkers != 256 {
		t.Fatalf("safety defaults = %+v", cfg.Safety)
	}
	if cfg.FixedWorkers != (FixedWorkerConfig{
		TPWorkers:      1,
		APWorkers:      1,
		MixedTPWorkers: 1,
		MixedAPWorkers: 1,
	}) {
		t.Fatalf("fixed worker defaults = %+v", cfg.FixedWorkers)
	}
	if cfg.MemoryWorkloads != (MemoryWorkloadConfig{
		SortWorkers:   1,
		SortWorkMemKB: 256 * 1024,
		HashWorkers:   1,
		HashWorkMemKB: 256 * 1024,
	}) {
		t.Fatalf("memory workload defaults = %+v", cfg.MemoryWorkloads)
	}
	if cfg.LockWorkloads != (LockWorkloadConfig{
		RowChainSessions:       2,
		RowChainDepth:          1,
		TableExclusiveSessions: 2,
		DDLWaitSessions:        2,
	}) {
		t.Fatalf("lock workload defaults = %+v", cfg.LockWorkloads)
	}
	if got := cfg.Run.ScenarioCodes; !reflect.DeepEqual(got, []ScenarioCode{101}) {
		t.Fatalf("scenario codes = %v", got)
	}
	if cfg.Data.CapacityProvider != "auto" ||
		cfg.Data.PhysicalSizeProvider != "auto" {
		t.Fatalf("dataset providers=%+v", cfg.Data)
	}
	if cfg.Run.ValidationEnabled {
		t.Fatal("runtime validation must default to disabled")
	}
}

func TestConfigLoadsFixedWorkerScenarioSettings(t *testing.T) {
	body := minimalConfig() + `
[scenario.tp_cpu]
workers = 7

[scenario.ap_cpu]
workers = 5

[scenario.mixed_cpu]
tp_workers = 4
ap_workers = 2
`
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	want := FixedWorkerConfig{
		TPWorkers:      7,
		APWorkers:      5,
		MixedTPWorkers: 4,
		MixedAPWorkers: 2,
	}
	if cfg.FixedWorkers != want {
		t.Fatalf("fixed workers=%+v want=%+v", cfg.FixedWorkers, want)
	}
}

func TestConfigLoadsMemoryWorkloadScenarioSettings(t *testing.T) {
	body := minimalConfig() + `
[scenario.memory_workmem_sort]
workers = 7
work_mem = 128MB

[scenario.memory_workmem_hash]
workers = 5
work_mem = 1GB
`
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	want := MemoryWorkloadConfig{
		SortWorkers:   7,
		SortWorkMemKB: 128 * 1024,
		HashWorkers:   5,
		HashWorkMemKB: 1024 * 1024,
	}
	if cfg.MemoryWorkloads != want {
		t.Fatalf("memory workloads=%+v want=%+v", cfg.MemoryWorkloads, want)
	}
}

func TestConfigLoadsLockWorkloadScenarioSettings(t *testing.T) {
	body := minimalConfig() + `
[scenario.lock_row_chain]
sessions = 10
chain_depth = 3

[scenario.lock_table_exclusive]
sessions = 8

[scenario.lock_ddl_wait]
sessions = 6
`
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	want := LockWorkloadConfig{
		RowChainSessions:       10,
		RowChainDepth:          3,
		TableExclusiveSessions: 8,
		DDLWaitSessions:        6,
	}
	if cfg.LockWorkloads != want {
		t.Fatalf("lock workloads=%+v want=%+v", cfg.LockWorkloads, want)
	}
}

func TestConfigFixedWorkerOverridesFollowFinalScenarios(t *testing.T) {
	path := writeTestConfig(t, minimalConfig()+`
[scenario.tp_cpu]
workers = 2

[scenario.ap_cpu]
workers = 3

[scenario.mixed_cpu]
tp_workers = 4
ap_workers = 5
`)

	tpAP, err := LoadConfig(path, Overrides{
		ScenarioCodes: []ScenarioCode{101, 102},
		Workers:       9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tpAP.FixedWorkers.TPWorkers != 9 ||
		tpAP.FixedWorkers.APWorkers != 9 {
		t.Fatalf("shared override not applied: %+v", tpAP.FixedWorkers)
	}

	mixed, err := LoadConfig(path, Overrides{
		ScenarioCodes: []ScenarioCode{103},
		TPWorkers:     8,
		APWorkers:     6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mixed.FixedWorkers.MixedTPWorkers != 8 ||
		mixed.FixedWorkers.MixedAPWorkers != 6 {
		t.Fatalf("mixed overrides not applied: %+v", mixed.FixedWorkers)
	}
}

func TestConfigMemoryOverridesFollowFinalScenarios(t *testing.T) {
	body := strings.Replace(
		minimalConfig(),
		"scenarios = tp_cpu",
		"scenarios = 201,202",
		1,
	) + `
[scenario.memory_workmem_sort]
workers = 2
work_mem = 128MB

[scenario.memory_workmem_hash]
workers = 3
work_mem = 512MB
`
	path := writeTestConfig(t, body)

	hashOnly, err := LoadConfig(path, Overrides{
		ScenarioCodes: []ScenarioCode{202},
		Workers:       9,
		WorkMemKB:     64 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantHashOnly := MemoryWorkloadConfig{
		SortWorkers:   2,
		SortWorkMemKB: 128 * 1024,
		HashWorkers:   9,
		HashWorkMemKB: 64 * 1024,
	}
	if hashOnly.MemoryWorkloads != wantHashOnly {
		t.Fatalf("hash memory overrides=%+v want=%+v", hashOnly.MemoryWorkloads, wantHashOnly)
	}

	sortOnly, err := LoadConfig(path, Overrides{
		ScenarioCodes: []ScenarioCode{201},
		WorkMemKB:     96 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sortOnly.MemoryWorkloads.SortWorkers != 2 ||
		sortOnly.MemoryWorkloads.SortWorkMemKB != 96*1024 ||
		sortOnly.MemoryWorkloads.HashWorkers != 3 ||
		sortOnly.MemoryWorkloads.HashWorkMemKB != 512*1024 {
		t.Fatalf("sort-only memory override=%+v", sortOnly.MemoryWorkloads)
	}
}

func TestConfigLockOverridesFollowFinalScenarios(t *testing.T) {
	body := strings.Replace(
		minimalConfig(),
		"scenarios = tp_cpu",
		"scenarios = 501,503",
		1,
	) + `
[scenario.lock_row_chain]
sessions = 4
chain_depth = 2

[scenario.lock_table_exclusive]
sessions = 3

[scenario.lock_ddl_wait]
sessions = 5
`
	configured, err := LoadConfig(writeTestConfig(t, body), Overrides{
		Sessions:   10,
		ChainDepth: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := LockWorkloadConfig{
		RowChainSessions:       10,
		RowChainDepth:          3,
		TableExclusiveSessions: 3,
		DDLWaitSessions:        10,
	}
	if configured.LockWorkloads != want {
		t.Fatalf("lock overrides=%+v want=%+v", configured.LockWorkloads, want)
	}
}

func TestCLIConfigOverridesPropagateMemoryTuning(t *testing.T) {
	options := CLIOptions{
		ScenarioCodes: []ScenarioCode{201, 202},
		Duration:      45 * time.Second,
		Workers:       8,
		WorkMemKB:     192 * 1024,
		Profile:       "stress",
		DryRun:        true,
		DatasetBytes:  20 << 30,
		DatasetSize:   "20GB",
	}
	overrides := configOverridesFromCLI(options)
	if !reflect.DeepEqual(overrides.ScenarioCodes, options.ScenarioCodes) ||
		overrides.Duration != options.Duration ||
		overrides.Workers != options.Workers ||
		overrides.WorkMemKB != options.WorkMemKB ||
		overrides.Profile != options.Profile ||
		overrides.DryRun == nil || !*overrides.DryRun ||
		overrides.DatasetBytes != options.DatasetBytes ||
		overrides.DatasetSize != options.DatasetSize {
		t.Fatalf("CLI overrides were not propagated: options=%+v overrides=%+v", options, overrides)
	}
}

func TestCLIConfigOverridesPropagateLockTuning(t *testing.T) {
	options := CLIOptions{
		ScenarioCodes: []ScenarioCode{501},
		Sessions:      10,
		ChainDepth:    3,
	}
	overrides := configOverridesFromCLI(options)
	if !reflect.DeepEqual(overrides.ScenarioCodes, options.ScenarioCodes) ||
		overrides.Sessions != options.Sessions ||
		overrides.ChainDepth != options.ChainDepth {
		t.Fatalf("CLI lock overrides were not propagated: options=%+v overrides=%+v", options, overrides)
	}
}

func TestConfigRejectsLockOverridesIncompatibleWithFinalScenarios(t *testing.T) {
	path := writeTestConfig(t, minimalConfig())
	for _, override := range []Overrides{
		{ScenarioCodes: []ScenarioCode{101}, Sessions: 2},
		{ScenarioCodes: []ScenarioCode{502}, ChainDepth: 2},
		{ScenarioCodes: []ScenarioCode{501, 504}, Sessions: 3},
		{ScenarioCodes: []ScenarioCode{501}, Sessions: 2, ChainDepth: 2},
	} {
		if _, err := LoadConfig(path, override); err == nil {
			t.Fatalf("accepted incompatible lock override %+v", override)
		}
	}
}

func TestConfigRejectsFixedWorkerOverridesIncompatibleWithFinalScenarios(t *testing.T) {
	path := writeTestConfig(t, minimalConfig())
	for _, override := range []Overrides{
		{ScenarioCodes: []ScenarioCode{103}, Workers: 2},
		{ScenarioCodes: []ScenarioCode{101}, TPWorkers: 2, APWorkers: 1},
		{ScenarioCodes: []ScenarioCode{101, 201}, Workers: 2},
		{ScenarioCodes: []ScenarioCode{103, 102}, TPWorkers: 2, APWorkers: 1},
		{ScenarioCodes: []ScenarioCode{103}, TPWorkers: 2},
		{ScenarioCodes: []ScenarioCode{103}, APWorkers: 1},
		{ScenarioCodes: []ScenarioCode{101}, WorkMemKB: 256 * 1024},
		{ScenarioCodes: []ScenarioCode{203}, WorkMemKB: 256 * 1024},
		{ScenarioCodes: []ScenarioCode{201, 203}, Workers: 2},
		{ScenarioCodes: []ScenarioCode{201, 202}, Workers: 2, WorkMemKB: 256 * 1024},
		{ScenarioCodes: []ScenarioCode{201, 202, 201}, WorkMemKB: 256 * 1024},
		{ScenarioCodes: []ScenarioCode{201}, WorkMemKB: 63},
	} {
		if _, err := LoadConfig(path, override); err == nil {
			t.Fatalf("accepted incompatible override %+v", override)
		}
	}
}

func TestConfigValidatesFixedWorkerTotalsAgainstBothHardCaps(t *testing.T) {
	for _, test := range []struct {
		name      string
		scenarios string
		settings  string
		workers   int
		conns     int
	}{
		{
			name:      "selected 101 and 102 sum exceeds max workers",
			scenarios: "101,102",
			settings:  "[scenario.tp_cpu]\nworkers = 3\n[scenario.ap_cpu]\nworkers = 4\n",
			workers:   6,
			conns:     20,
		},
		{
			name:      "103 sum exceeds max connections",
			scenarios: "103",
			settings:  "[scenario.mixed_cpu]\ntp_workers = 4\nap_workers = 3\n",
			workers:   20,
			conns:     6,
		},
		{
			name:      "201 and 202 sum exceeds max workers",
			scenarios: "201,202",
			settings: "[scenario.memory_workmem_sort]\nworkers = 3\n" +
				"[scenario.memory_workmem_hash]\nworkers = 4\n",
			workers: 6,
			conns:   20,
		},
		{
			name:      "202 exceeds max connections",
			scenarios: "202",
			settings:  "[scenario.memory_workmem_hash]\nworkers = 7\n",
			workers:   20,
			conns:     6,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := strings.Replace(
				minimalConfig(),
				"scenarios = tp_cpu",
				"scenarios = "+test.scenarios,
				1,
			) + "\n[safety]\nmax_workers = " + strconv.Itoa(test.workers) +
				"\nmax_connections = " + strconv.Itoa(test.conns) + "\n" + test.settings
			if _, err := LoadConfig(writeTestConfig(t, body), Overrides{}); err == nil ||
				!strings.Contains(err.Error(), "fixed workers") {
				t.Fatalf("hard-cap error=%v", err)
			}
		})
	}
}

func TestConfigValidatesLockSessionTotalsAgainstConnectionCap(t *testing.T) {
	for _, test := range []struct {
		name      string
		scenarios string
		settings  string
		maxConns  int
	}{
		{
			name:      "two lock scenarios exceed cap",
			scenarios: "501,502",
			settings: "[scenario.lock_row_chain]\nsessions = 4\nchain_depth = 2\n" +
				"[scenario.lock_table_exclusive]\nsessions = 4\n",
			maxConns: 7,
		},
		{
			name:      "fixed worker and lock sessions exceed cap",
			scenarios: "101,501",
			settings: "[scenario.tp_cpu]\nworkers = 2\n" +
				"[scenario.lock_row_chain]\nsessions = 4\nchain_depth = 2\n",
			maxConns: 5,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := strings.Replace(
				minimalConfig(),
				"scenarios = tp_cpu",
				"scenarios = "+test.scenarios,
				1,
			) + "\n[safety]\nmax_workers = 20\nmax_connections = " +
				strconv.Itoa(test.maxConns) + "\n" + test.settings
			_, err := LoadConfig(writeTestConfig(t, body), Overrides{})
			if err == nil || !strings.Contains(err.Error(), "max_connections") {
				t.Fatalf("connection-cap error=%v", err)
			}
		})
	}
}

func TestConfigDoesNotCountLockSessionsAsWorkers(t *testing.T) {
	body := strings.Replace(
		minimalConfig(),
		"scenarios = tp_cpu",
		"scenarios = 501",
		1,
	) + `
[safety]
max_workers = 1
max_connections = 4

[scenario.lock_row_chain]
sessions = 4
chain_depth = 2
`
	if _, err := LoadConfig(writeTestConfig(t, body), Overrides{}); err != nil {
		t.Fatalf("lock sessions incorrectly consumed worker budget: %v", err)
	}
}

func TestConfigRejectsFixedWorkersCombinedWithUnbudgetedScenarios(t *testing.T) {
	for _, scenarios := range []string{"101,203", "201,203", "202,301"} {
		body := strings.Replace(
			minimalConfig(),
			"scenarios = tp_cpu",
			"scenarios = "+scenarios,
			1,
		)
		if _, err := LoadConfig(writeTestConfig(t, body), Overrides{}); err == nil ||
			!strings.Contains(err.Error(), "fixed-worker scenarios") {
			t.Fatalf("scenarios=%s error=%v", scenarios, err)
		}
	}
}

func TestConfigRejectsNonPositiveFixedWorkers(t *testing.T) {
	body := minimalConfig() + "\n[scenario.tp_cpu]\nworkers = 0\n"
	if _, err := LoadConfig(writeTestConfig(t, body), Overrides{}); err == nil ||
		!strings.Contains(err.Error(), "workers") {
		t.Fatalf("non-positive fixed workers error=%v", err)
	}
}

func TestConfigRejectsInvalidMemoryWorkloadSettings(t *testing.T) {
	for _, test := range []struct {
		name     string
		settings string
	}{
		{name: "zero sort workers", settings: "[scenario.memory_workmem_sort]\nworkers = 0\n"},
		{name: "zero hash workers", settings: "[scenario.memory_workmem_hash]\nworkers = 0\n"},
		{name: "sort work mem below minimum", settings: "[scenario.memory_workmem_sort]\nwork_mem = 63kB\n"},
		{name: "hash work mem missing unit", settings: "[scenario.memory_workmem_hash]\nwork_mem = 256\n"},
		{name: "hash work mem unsafe", settings: "[scenario.memory_workmem_hash]\nwork_mem = 64kB;RESET ALL\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadConfig(
				writeTestConfig(t, minimalConfig()+"\n"+test.settings),
				Overrides{},
			)
			if err == nil ||
				(!strings.Contains(err.Error(), "workers") &&
					!strings.Contains(err.Error(), "work_mem")) {
				t.Fatalf("invalid memory setting error=%v", err)
			}
		})
	}
}

func TestConfigRejectsInvalidLockWorkloadSettings(t *testing.T) {
	for _, test := range []struct {
		name     string
		settings string
	}{
		{name: "row chain sessions below two", settings: "[scenario.lock_row_chain]\nsessions = 1\n"},
		{name: "row chain depth below one", settings: "[scenario.lock_row_chain]\nchain_depth = 0\n"},
		{name: "row chain depth above five", settings: "[scenario.lock_row_chain]\nsessions = 7\nchain_depth = 6\n"},
		{name: "row chain depth exceeds waiter count", settings: "[scenario.lock_row_chain]\nsessions = 2\nchain_depth = 2\n"},
		{name: "table sessions below two", settings: "[scenario.lock_table_exclusive]\nsessions = 1\n"},
		{name: "DDL sessions below two", settings: "[scenario.lock_ddl_wait]\nsessions = 1\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadConfig(
				writeTestConfig(t, minimalConfig()+"\n"+test.settings),
				Overrides{},
			)
			if err == nil {
				t.Fatalf("accepted invalid lock settings %q", test.settings)
			}
		})
	}
}

func TestConfigIgnoresLegacyCPUTargetSetting(t *testing.T) {
	body := minimalConfig() + "\n[safety]\ncpu_target_percent = 0\n"
	if _, err := LoadConfig(writeTestConfig(t, body), Overrides{}); err != nil {
		t.Fatalf("obsolete CPU target still controls fixed workers: %v", err)
	}
}

func TestConfigEnablesRuntimeValidationExplicitly(t *testing.T) {
	body := strings.Replace(
		minimalConfig(),
		"duration = 10m",
		"duration = 10m\nvalidation_enabled = true",
		1,
	)
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Run.ValidationEnabled {
		t.Fatal("runtime validation must honor explicit true")
	}
}

func TestConfigParsesExplicitDatasetProviders(t *testing.T) {
	body := strings.Replace(
		minimalConfig(),
		"schema = gsbench",
		"schema = gsbench\ncapacity_provider = local_data_directory\ndata_directory_host_root = /var/chroot\nphysical_size_provider = catalog",
		1,
	)
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Data.CapacityProvider != "local_data_directory" ||
		cfg.Data.DataDirectoryHostRoot != "/var/chroot" ||
		cfg.Data.PhysicalSizeProvider != "catalog" {
		t.Fatalf("dataset providers=%+v", cfg.Data)
	}
}

func TestConfigRejectsRelativeDataDirectoryHostRoot(t *testing.T) {
	body := strings.Replace(
		minimalConfig(),
		"schema = gsbench",
		"schema = gsbench\ndata_directory_host_root = var/chroot",
		1,
	)
	if _, err := LoadConfig(
		writeTestConfig(t, body), Overrides{},
	); err == nil || !strings.Contains(err.Error(), "data_directory_host_root") {
		t.Fatalf("err=%v", err)
	}
}

func TestConfigRejectsUnavailableDatasetProviders(t *testing.T) {
	for _, tc := range []struct {
		field string
		value string
	}{
		{field: "capacity_provider", value: "imaginary"},
		{field: "capacity_provider", value: "external"},
		{field: "capacity_provider", value: "gaussdb_api"},
		{field: "physical_size_provider", value: "external"},
		{field: "physical_size_provider", value: "gaussdb_api"},
	} {
		t.Run(tc.field+"_"+tc.value, func(t *testing.T) {
			body := strings.Replace(
				minimalConfig(),
				"schema = gsbench",
				"schema = gsbench\n"+tc.field+" = "+tc.value,
				1,
			)
			if _, err := LoadConfig(
				writeTestConfig(t, body), Overrides{},
			); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestShippedConfigDoesNotAdvertiseUnavailableDatasetProviders(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "configs", "gsbench.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, unavailable := range []string{
		"capacity_provider = external",
		"physical_size_provider = external",
		"gaussdb_api",
		"must set external",
		"must use external",
	} {
		if strings.Contains(text, unavailable) {
			t.Fatalf("shipped config advertises unavailable provider %q", unavailable)
		}
	}
}

func TestSafetyConfigParsesSupportedFaultAuthorizationSettings(t *testing.T) {
	body := minimalConfig() + `
[safety]
restore_timeout = 7m
profile_cap_gb = 128
allow_admin_mutation = true
allow_infrastructure_fault = true
`
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Safety.RestoreTimeout != 7*time.Minute || cfg.Safety.ProfileCapGB != 128 ||
		!cfg.Safety.AllowAdminMutation || !cfg.Safety.AllowInfrastructureFault || cfg.Safety.RestoreOriginalRole {
		t.Fatalf("safety=%+v", cfg.Safety)
	}
}

func TestConfigRejectsUnsupportedRestoreSafetySettings(t *testing.T) {
	for _, setting := range []string{
		"restore_on_exit = false",
		"restore_original_role = true",
	} {
		body := minimalConfig() + "\n[safety]\n" + setting + "\n"
		if _, err := LoadConfig(writeTestConfig(t, body), Overrides{}); err == nil ||
			!strings.Contains(err.Error(), strings.Split(setting, " =")[0]) {
			t.Fatalf("setting=%q error=%v", setting, err)
		}
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

func TestConfigPasswordConfigOverridesStaleEnvironment(t *testing.T) {
	t.Setenv("GSBENCH_TEST_PASSWORD", "stale-password")
	path := writeTestConfig(t, strings.Replace(
		minimalConfig(),
		"password_env = GSBENCH_TEST_PASSWORD",
		"password_env = GSBENCH_TEST_PASSWORD\npassword_config = gstop.cfg",
		1,
	))
	secretPath := filepath.Join(filepath.Dir(path), "gstop.cfg")
	if err := os.WriteFile(secretPath, []byte("[main]\ndb_password = correct-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Password != "correct-password" {
		t.Fatalf("password config did not override stale environment")
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
	if cfg.Data.TargetBytes != 20<<30 {
		t.Fatalf("stress target bytes=%d", cfg.Data.TargetBytes)
	}
}

func TestConfigProfileOverrideSelectsStressDefaultBeforeTargetCalculation(t *testing.T) {
	cfg, err := LoadConfig(
		writeTestConfig(t, minimalConfig()),
		Overrides{Profile: "stress"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Run.Profile != "stress" || cfg.Data.MaxSizeGB != 20 ||
		cfg.Data.TargetBytes != 20<<30 {
		t.Fatalf("stress override config=%+v", cfg)
	}
}

func TestConfigDatasetBytesOverrideProfileAndConfig(t *testing.T) {
	body := strings.Replace(
		minimalConfig(),
		"schema = gsbench",
		"schema = gsbench\nmax_size_gb = 5",
		1,
	)
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{
		DatasetBytes: 100 << 30,
		DatasetSize:  "100GB",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Data.TargetBytes != 100<<30 {
		t.Fatalf("target bytes=%d", cfg.Data.TargetBytes)
	}
	if cfg.Data.RequestedSize != "100GB" {
		t.Fatalf("requested size=%q", cfg.Data.RequestedSize)
	}
}

func TestConfigRejectsMoreThanTwoTiB(t *testing.T) {
	body := strings.Replace(
		minimalConfig(),
		"schema = gsbench",
		"schema = gsbench\nmax_size_gb = 2049",
		1,
	)
	if _, err := LoadConfig(writeTestConfig(t, body), Overrides{}); err == nil {
		t.Fatal("accepted data.max_size_gb > 2048")
	}
}

func TestConfigResolvesThreeDigitScenarioCodesAndCanonicalNames(t *testing.T) {
	body := strings.Replace(minimalConfig(), "scenarios = tp_cpu", "scenarios = 203,io_sequential_read,501", 1)
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Run.ScenarioCodes, []ScenarioCode{203, 301, 501}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scenario codes=%v want=%v", got, want)
	}
}

func TestConfigResolvesSingleThreeDigitScenario(t *testing.T) {
	body := strings.Replace(minimalConfig(), "scenarios = tp_cpu", "scenarios = 101", 1)
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Run.ScenarioCodes; !reflect.DeepEqual(
		got,
		[]ScenarioCode{101},
	) {
		t.Fatalf("scenario codes=%v", got)
	}
}

func TestConfigRejectsInvalidScenarioNumbers(t *testing.T) {
	for _, identifier := range []string{"0", "16", "-1", "1", "tp"} {
		t.Run(identifier, func(t *testing.T) {
			body := strings.Replace(minimalConfig(), "scenarios = tp_cpu", "scenarios = "+identifier, 1)
			if _, err := LoadConfig(writeTestConfig(t, body), Overrides{}); err == nil {
				t.Fatalf("expected unknown scenario %q error", identifier)
			}
		})
	}
}

func TestConfigRejectsLegacyPlanAlias(t *testing.T) {
	body := strings.Replace(minimalConfig(), "scenarios = tp_cpu", "scenarios = plan_regression", 1)
	if _, err := LoadConfig(writeTestConfig(t, body), Overrides{}); err == nil {
		t.Fatal("legacy plan alias accepted")
	}
}

func TestConfigValidationTreatsCodesAsAuthoritative(t *testing.T) {
	cfg, err := LoadConfig(writeTestConfig(t, minimalConfig()), Overrides{})
	if err != nil {
		t.Fatal(err)
	}

	cfg.Run.ScenarioCodes = []ScenarioCode{101, 101}
	if err := cfg.Validate(); err == nil {
		t.Fatal("duplicate scenario code accepted")
	}

	cfg.Run.ScenarioCodes = []ScenarioCode{999}
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown scenario code accepted")
	}
}

func TestConfigValidationUsesScenarioCodesWithoutNameBridge(t *testing.T) {
	cfg, err := LoadConfig(
		writeTestConfig(t, minimalConfig()),
		Overrides{},
	)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Run = RunConfig{
		ScenarioCodes: []ScenarioCode{101},
		Duration:      10 * time.Minute,
		RampInterval:  2 * time.Second,
		Profile:       "quick",
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("code-only run config rejected: %v", err)
	}
}

func TestConfigParsesFaultProviderAndResolvesRelativeLedgerPath(t *testing.T) {
	path := writeTestConfig(t, minimalConfig()+`
[fault_provider]
type = ssh
ledger_path = recovery/custom.json
`)
	cfg, err := LoadConfig(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(filepath.Dir(path), "recovery", "custom.json")
	if cfg.FaultProvider.Type != "ssh" ||
		cfg.FaultProvider.LedgerPath != wantPath {
		t.Fatalf("fault provider = %+v, want path %q", cfg.FaultProvider, wantPath)
	}
}

func TestConfigAcceptsOnlyFaultProviderProtocolNames(t *testing.T) {
	for _, providerType := range []string{
		"none",
		"local",
		"ssh",
		"gaussdb_api",
	} {
		t.Run(providerType, func(t *testing.T) {
			cfg, err := LoadConfig(writeTestConfig(
				t,
				minimalConfig()+"\n[fault_provider]\ntype = "+providerType+"\n",
			), Overrides{})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.FaultProvider.Type != providerType {
				t.Fatalf("provider type = %q", cfg.FaultProvider.Type)
			}
		})
	}

	for _, providerType := range []string{"auto", "external", "imaginary"} {
		t.Run("reject_"+providerType, func(t *testing.T) {
			_, err := LoadConfig(writeTestConfig(
				t,
				minimalConfig()+"\n[fault_provider]\ntype = "+providerType+"\n",
			), Overrides{})
			if err == nil || !strings.Contains(err.Error(), "fault_provider.type") {
				t.Fatalf("LoadConfig() error = %v", err)
			}
		})
	}
}

func TestConfigDefaultLedgerPathIsStableAndConfigSpecificUnderLogs(t *testing.T) {
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.cfg")
	secondPath := filepath.Join(dir, "second.cfg")
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte(minimalConfig()), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	first, err := LoadConfig(firstPath, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	firstAgain, err := LoadConfig(firstPath, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadConfig(secondPath, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if first.FaultProvider.Type != "none" {
		t.Fatalf("default provider type = %q", first.FaultProvider.Type)
	}
	if first.FaultProvider.LedgerPath != firstAgain.FaultProvider.LedgerPath {
		t.Fatalf(
			"default ledger path changed: %q != %q",
			first.FaultProvider.LedgerPath,
			firstAgain.FaultProvider.LedgerPath,
		)
	}
	if first.FaultProvider.LedgerPath == second.FaultProvider.LedgerPath {
		t.Fatalf("different configs share ledger %q", first.FaultProvider.LedgerPath)
	}
	if !filepath.IsAbs(first.FaultProvider.LedgerPath) ||
		filepath.Dir(first.FaultProvider.LedgerPath) != filepath.Join(dir, "logs") {
		t.Fatalf("default ledger is not resolved under logs: %q", first.FaultProvider.LedgerPath)
	}
	if matched := regexp.MustCompile(
		`^gsbench_[0-9a-f]{16}_recovery\.json$`,
	).MatchString(filepath.Base(first.FaultProvider.LedgerPath)); !matched {
		t.Fatalf("default ledger name is not identity-specific: %q", first.FaultProvider.LedgerPath)
	}
}

func TestConfigDefaultLedgerIdentityCanonicalizesConfigSymlink(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.cfg")
	if err := os.WriteFile(realPath, []byte(minimalConfig()), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "alias.cfg")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}

	realConfig, err := LoadConfig(realPath, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	linkConfig, err := LoadConfig(linkPath, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if realConfig.FaultProvider.LedgerPath !=
		linkConfig.FaultProvider.LedgerPath {
		t.Fatalf(
			"config aliases use different ledgers: %q != %q",
			realConfig.FaultProvider.LedgerPath,
			linkConfig.FaultProvider.LedgerPath,
		)
	}
}

func TestConfigRejectsUnsafeLedgerPaths(t *testing.T) {
	dir := t.TempDir()
	directoryPath := filepath.Join(dir, "directory.json")
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	realPath := filepath.Join(dir, "real.json")
	if err := os.WriteFile(
		realPath,
		[]byte(`{"version":1,"actions":[]}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(dir, "symlink.json")
	if err := os.Symlink(realPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		string(filepath.Separator),
		directoryPath,
		symlinkPath,
		filepath.Join(dir, "ledger.txt"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, home)
	}
	for index, ledgerPath := range paths {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			_, err := LoadConfig(writeTestConfig(
				t,
				minimalConfig()+"\n[fault_provider]\nledger_path = "+ledgerPath+"\n",
			), Overrides{})
			if err == nil || !strings.Contains(err.Error(), "fault_provider.ledger_path") {
				t.Fatalf("LoadConfig(%q) error = %v", ledgerPath, err)
			}
		})
	}
}

func TestShippedConfigUsesFailClosedFaultProvider(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "gsbench.cfg")
	cfg, err := LoadConfig(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FaultProvider.Type != "none" {
		t.Fatalf("shipped fault provider type = %q", cfg.FaultProvider.Type)
	}
	provider, err := NewFaultProvider(
		NewFaultProviderRegistry(),
		cfg.FaultProvider,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Apply(
		context.Background(),
		validLedgerAction("run-1", "target-1"),
	); err == nil {
		t.Fatal("shipped no-op provider reported success")
	}
}
