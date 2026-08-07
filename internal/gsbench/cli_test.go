package gsbench

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReadOnlyCLICommandsDoNotCreateLogsOrDirectories(t *testing.T) {
	t.Setenv("GSBENCH_TEST_PASSWORD", "test-only")
	configPath := writeTestConfig(
		t,
		minimalConfig()+"\n[database]\nconnect_timeout = 1ms\n",
	)
	for _, test := range []struct {
		name    string
		command string
		dryRun  bool
	}{
		{name: "restore dry run", command: "restore", dryRun: true},
		{name: "cleanup dry run", command: "cleanup", dryRun: true},
		{name: "doctor", command: "doctor"},
		{name: "status", command: "status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			working := t.TempDir()
			t.Chdir(working)
			configDir := filepath.Dir(configPath)
			before, err := os.ReadDir(configDir)
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			_ = executeCommand(
				context.Background(),
				CLIOptions{
					Command:    test.command,
					ConfigPath: configPath,
					DryRun:     test.dryRun,
				},
				&stdout,
				&stderr,
			)
			entries, err := os.ReadDir(working)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				var names []string
				for _, entry := range entries {
					names = append(names, filepath.Clean(entry.Name()))
				}
				t.Fatalf(
					"read-only %s created filesystem entries %v",
					test.command,
					names,
				)
			}
			after, err := os.ReadDir(configDir)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf(
					"read-only %s changed config directory entries: before=%v after=%v",
					test.command,
					before,
					after,
				)
			}
		})
	}
}

func TestRestoreCLIExecutesLocalInverseWhenInitialDatabaseIsUnreachable(
	t *testing.T,
) {
	t.Setenv("GSBENCH_TEST_PASSWORD", "test-only")
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "recovery.json")
	configPath := filepath.Join(dir, "gsbench.cfg")
	config := fmt.Sprintf(`[database]
host = 127.0.0.1
port = 1
database = postgres
user = bench
password_env = GSBENCH_TEST_PASSWORD
connect_timeout = 1ms

[run]
scenarios = tp_cpu

[data]
schema = gsbench

[safety]
query_timeout = 5ms
restore_timeout = 30ms

[fault_provider]
type = local
ledger_path = %s
`, ledgerPath)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := NewFileRecoveryLedger(ledgerPath)
	action := restoreTestAction(
		"run-offline",
		7,
		ActionNetworkFirewall,
		"firewall-rule",
	)
	action.Node = "dn-1"
	action.State = MutationApplied
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	provider := &recordingRestoreProvider{}
	registry := NewFaultProviderRegistry()
	if err := registry.Register(
		"local",
		func(FaultProviderConfig) (FaultProvider, error) {
			return provider, nil
		},
	); err != nil {
		t.Fatal(err)
	}
	previousRegistry := defaultFaultProviderRegistry
	defaultFaultProviderRegistry = registry
	t.Cleanup(func() {
		defaultFaultProviderRegistry = previousRegistry
	})
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	code := executeCommand(
		context.Background(),
		CLIOptions{Command: "restore", ConfigPath: configPath},
		&stdout,
		&stderr,
	)
	if code != 1 {
		t.Fatalf(
			"exit code=%d want failure until DB/local converge; output=%s stderr=%s",
			code,
			stdout.String(),
			stderr.String(),
		)
	}
	if len(provider.restored) != 1 ||
		provider.restored[0].Target != action.Target {
		t.Fatalf("provider restores=%v want exactly once", provider.restored)
	}
	pending, err := ledger.Pending(context.Background(), action.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("local recovery did not converge: %+v", pending)
	}
}

func TestCLIVersionPrintsAuthor(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunCLI(context.Background(), []string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Author: WangYingJie <sqlrush@gmail.com>") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.HasPrefix(stdout.String(), "gsbench v1.1.7\n") {
		t.Fatalf("version=%q", stdout.String())
	}
}

func TestCLIRejectsUnknownCommandAfterBanner(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunCLI(context.Background(), []string{"explode"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code=%d", code)
	}
	if !strings.HasPrefix(stdout.String(), "gsbench ") {
		t.Fatalf("banner missing: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestCLIHelpDocumentsThreeDigitScenarios(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunCLI(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	for _, token := range []string{
		"GSBENCH_CONFIG", "-s", "-d", "gsbench scenarios",
		"101=tp_cpu", "601=planchange_stats_target", "621=hardparse_literal_flood",
	} {
		if !strings.Contains(stdout.String(), token) {
			t.Fatalf("help missing %q:\n%s", token, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "\n  1=tp_cpu\n") {
		t.Fatalf("help contains legacy scenario:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "lock_storm") {
		t.Fatalf("help contains legacy token %q:\n%s", "lock_storm", stdout.String())
	}
}

func TestCLIHelpDocumentsIndependentPlanScenariosAndRestore(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := RunCLI(context.Background(), []string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	text := stdout.String()
	for _, token := range []string{
		"gsbench restore", "601=planchange_stats_target", "602=planchange_stats_lookup",
		"603=planchange_stats_ndistinct", "604=planchange_stats_extended",
		"605=planchange_index_drop", "606=planchange_index_shape",
		"gsbench run 601 init --worker N --duration DURATION",
		"gsbench run 601 fault", "gsbench run 601 recover",
		"--worker N", "601-606 init", "--workers alias",
	} {
		if !strings.Contains(text, token) {
			t.Errorf("help missing %q", token)
		}
	}
}

func TestParseCLIArgsSupportsScenarioDurationAndDryRun(t *testing.T) {
	options, err := ParseCLIArgs([]string{"run", "-c", "custom.cfg", "--scenario", "101,501", "--duration", "30s", "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Command != "run" || options.ConfigPath != "custom.cfg" || options.Duration.String() != "30s" || !options.DryRun {
		t.Fatalf("options=%+v", options)
	}
	if !reflect.DeepEqual(
		options.ScenarioCodes,
		[]ScenarioCode{101, 501},
	) {
		t.Fatalf("codes=%v", options.ScenarioCodes)
	}
}

func TestParseCLIArgsSupportsPoolTargetPercent(t *testing.T) {
	for _, test := range []struct {
		args  []string
		codes []ScenarioCode
		want  int
	}{
		{[]string{"run", "401", "--percent", "90"}, []ScenarioCode{401}, 90},
		{[]string{"run", "402", "--percent=100"}, []ScenarioCode{402}, 100},
		{[]string{"run", "301,401,402", "--percent", "1"}, []ScenarioCode{301, 401, 402}, 1},
		{[]string{"run", "--percent", "90"}, nil, 90},
	} {
		options, err := ParseCLIArgs(test.args)
		if err != nil {
			t.Fatalf("ParseCLIArgs(%v): %v", test.args, err)
		}
		if options.PoolPercent != test.want ||
			len(options.ScenarioCodes) != len(test.codes) ||
			(len(test.codes) > 0 &&
				!reflect.DeepEqual(options.ScenarioCodes, test.codes)) {
			t.Fatalf("ParseCLIArgs(%v)=%+v", test.args, options)
		}
	}
}

func TestParseCLIArgsRejectsInvalidPoolTargetPercent(t *testing.T) {
	for _, args := range [][]string{
		{"run", "401", "--percent", "0"},
		{"run", "401", "--percent", "-1"},
		{"run", "401", "--percent", "1.5"},
		{"run", "301", "--percent", "90"},
		{"doctor", "--scenario", "401", "--percent", "90"},
	} {
		if _, err := ParseCLIArgs(args); err == nil {
			t.Fatalf("ParseCLIArgs(%v) accepted invalid pool target", args)
		}
	}
}

func TestParseCLIArgsAllowsPoolTargetAboveOneHundred(t *testing.T) {
	options, err := ParseCLIArgs([]string{"run", "401", "--percent", "125"})
	if err != nil {
		t.Fatalf("advisory pool target was rejected: %v", err)
	}
	if options.PoolPercent != 125 {
		t.Fatalf("pool percent=%d want=125", options.PoolPercent)
	}
}

func TestParseCLIArgsAllowsChainDepthAboveLegacyMaximum(t *testing.T) {
	options, err := ParseCLIArgs([]string{
		"run", "501", "--sessions", "8", "--chain-depth", "6",
	})
	if err != nil {
		t.Fatalf("advisory chain depth was rejected: %v", err)
	}
	if options.ChainDepth != 6 || options.Sessions != 8 {
		t.Fatalf("lock overrides=%+v", options)
	}
}

func TestCLIHelpDocumentsPoolTargetPercent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := RunCLI(
		context.Background(), []string{"help"}, &stdout, &stderr,
	); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	for _, token := range []string{
		"--percent N", "gsbench run 401 --percent",
		"gsbench run 402 --percent",
	} {
		if !strings.Contains(stdout.String(), token) {
			t.Errorf("help missing %q:\n%s", token, stdout.String())
		}
	}
}

func TestParseCLIArgsSupportsFixedWorkerOverrides(t *testing.T) {
	for _, test := range []struct {
		name      string
		args      []string
		workers   int
		tpWorkers int
		apWorkers int
	}{
		{
			name:    "101 TP workers",
			args:    []string{"run", "101", "--workers", "7", "--duration", "30s"},
			workers: 7,
		},
		{
			name:    "102 AP workers",
			args:    []string{"run", "--scenario", "102", "--workers=5"},
			workers: 5,
		},
		{
			name:    "101 and 102 shared workers",
			args:    []string{"run", "--scenario", "101,102", "--workers", "3"},
			workers: 3,
		},
		{
			name:      "103 independent lanes",
			args:      []string{"run", "103", "--tp-workers", "4", "--ap-workers", "2"},
			tpWorkers: 4,
			apWorkers: 2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			options, err := ParseCLIArgs(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if options.Workers != test.workers ||
				options.TPWorkers != test.tpWorkers ||
				options.APWorkers != test.apWorkers {
				t.Fatalf("worker options=%+v", options)
			}
		})
	}
}

func TestParseCLIArgsSupportsThreePhasePlanCommands(t *testing.T) {
	for _, test := range []struct {
		name     string
		args     []string
		code     ScenarioCode
		action   PlanRunAction
		workers  int
		duration time.Duration
	}{
		{
			name: "init singular worker",
			args: []string{
				"run", "601", "init", "--worker", "10", "--duration", "1m",
			},
			code: 601, action: PlanRunInit, workers: 10, duration: time.Minute,
		},
		{
			name: "init plural worker alias",
			args: []string{
				"run", "606", "init", "--workers", "3", "--duration", "30s",
			},
			code: 606, action: PlanRunInit, workers: 3, duration: 30 * time.Second,
		},
		{
			name: "fault",
			args: []string{"run", "603", "fault"},
			code: 603, action: PlanRunFault,
		},
		{
			name: "recover",
			args: []string{"run", "planchange_index_drop", "recover"},
			code: 605, action: PlanRunRecover,
		},
		{
			name: "legacy 602 name",
			args: []string{
				"run", "planchange_index_unusable", "fault",
			},
			code: 602, action: PlanRunFault,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			options, err := ParseCLIArgs(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(options.ScenarioCodes, []ScenarioCode{test.code}) ||
				options.PlanAction != test.action ||
				options.PlanWorkers != test.workers ||
				options.Duration != test.duration {
				t.Fatalf("options=%+v", options)
			}
		})
	}
}

func TestParseCLIArgsRejectsInvalidThreePhasePlanCommands(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "bare old command", args: []string{"run", "601"}, want: "requires an action"},
		{name: "unknown action", args: []string{"run", "601", "break"}, want: "unknown plan action"},
		{name: "non plan action", args: []string{"run", "101", "init", "--worker", "1", "--duration", "1m"}, want: "only valid for scenarios 601-606"},
		{name: "multiple scenarios", args: []string{"run", "601,602", "fault"}, want: "exactly one"},
		{name: "init missing worker", args: []string{"run", "601", "init", "--duration", "1m"}, want: "--worker"},
		{name: "init missing duration", args: []string{"run", "601", "init", "--worker", "1"}, want: "--duration"},
		{name: "fault worker", args: []string{"run", "601", "fault", "--worker", "1"}, want: "does not accept"},
		{name: "recover duration", args: []string{"run", "601", "recover", "--duration", "1m"}, want: "does not accept"},
		{name: "worker aliases together", args: []string{"run", "601", "init", "--worker", "1", "--workers", "1", "--duration", "1m"}, want: "cannot be combined"},
		{name: "plan run id", args: []string{"run", "601", "fault", "--run-id", "manual"}, want: "does not accept --run-id"},
		{name: "plan dry run", args: []string{"run", "605", "fault", "--dry-run"}, want: "does not support --dry-run"},
		{name: "singular worker non plan", args: []string{"run", "101", "--worker", "1"}, want: "only valid for scenarios 601-606"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseCLIArgs(test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want containing %q", err, test.want)
			}
		})
	}
}

func TestParseCLIArgsSupportsMemoryWorkloadOverrides(t *testing.T) {
	for _, test := range []struct {
		name      string
		args      []string
		workers   int
		workMemKB int64
	}{
		{
			name:      "201 positional scenario",
			args:      []string{"run", "201", "--workers", "7", "--work-mem", "256MB", "--duration", "30s"},
			workers:   7,
			workMemKB: 256 * 1024,
		},
		{
			name:      "202 named scenario",
			args:      []string{"run", "--scenario", "memory_workmem_hash", "--workers=5", "--work-mem=1GB"},
			workers:   5,
			workMemKB: 1024 * 1024,
		},
		{
			name:      "work mem without worker override",
			args:      []string{"run", "202", "--work-mem=128MB"},
			workMemKB: 128 * 1024,
		},
		{
			name:    "workers without work mem override",
			args:    []string{"run", "201", "--workers=2"},
			workers: 2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			options, err := ParseCLIArgs(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if options.Workers != test.workers ||
				options.WorkMemKB != test.workMemKB {
				t.Fatalf(
					"memory options workers=%d work_mem_kB=%d want workers=%d work_mem_kB=%d",
					options.Workers,
					options.WorkMemKB,
					test.workers,
					test.workMemKB,
				)
			}
		})
	}
}

func TestParseCLIArgsSupportsLockWorkloadOverrides(t *testing.T) {
	for _, test := range []struct {
		name       string
		args       []string
		sessions   int
		chainDepth int
	}{
		{
			name:       "501 sessions and depth",
			args:       []string{"run", "501", "--sessions=10", "--chain-depth=3"},
			sessions:   10,
			chainDepth: 3,
		},
		{
			name:     "502 sessions",
			args:     []string{"run", "502", "--sessions=8"},
			sessions: 8,
		},
		{
			name:       "shared lock family override",
			args:       []string{"run", "501,503", "--sessions=6", "--chain-depth=2"},
			sessions:   6,
			chainDepth: 2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			options, err := ParseCLIArgs(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if options.Sessions != test.sessions ||
				options.ChainDepth != test.chainDepth {
				t.Fatalf("options=%+v", options)
			}
		})
	}
}

func TestParseCLIArgsRejectsInvalidLockWorkloadOverrides(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "one session", args: []string{"run", "501", "--sessions=1"}},
		{name: "depth exceeds sessions", args: []string{"run", "501", "--sessions=2", "--chain-depth=2"}},
		{name: "zero depth", args: []string{"run", "501", "--chain-depth=0"}},
		{name: "depth on 502", args: []string{"run", "502", "--chain-depth=2"}},
		{name: "sessions on CPU scenario", args: []string{"run", "101", "--sessions=2"}},
		{name: "non-run command", args: []string{"doctor", "--sessions=2"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseCLIArgs(test.args); err == nil {
				t.Fatalf("ParseCLIArgs(%v) accepted invalid lock override", test.args)
			}
		})
	}
}

func TestParseCLIArgsRejectsInvalidMemoryWorkloadOverrides(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "below minimum", args: []string{"run", "201", "--work-mem=63kB"}},
		{name: "zero", args: []string{"run", "201", "--work-mem=0kB"}},
		{name: "negative", args: []string{"run", "201", "--work-mem=-1MB"}},
		{name: "fractional", args: []string{"run", "201", "--work-mem=1.5MB"}},
		{name: "missing unit", args: []string{"run", "201", "--work-mem=256"}},
		{name: "unknown unit", args: []string{"run", "201", "--work-mem=1TB"}},
		{name: "overflow", args: []string{"run", "201", "--work-mem=9223372036854775807GB"}},
		{name: "byte conversion overflow", args: []string{"run", "201", "--work-mem=9223372036854775807kB"}},
		{name: "unsafe suffix", args: []string{"run", "201", "--work-mem=64kB;RESET ALL"}},
		{name: "CPU scenario", args: []string{"run", "101", "--work-mem=256MB"}},
		{name: "other memory scenario", args: []string{"run", "203", "--work-mem=256MB"}},
		{name: "memory plus CPU", args: []string{"run", "201,101", "--work-mem=256MB"}},
		{name: "memory plus another scenario", args: []string{"run", "201,203", "--workers=2"}},
		{name: "two memory scenarios share unsafe override", args: []string{"run", "201,202", "--workers=2", "--work-mem=64MB"}},
		{name: "three memory inputs", args: []string{"run", "201,202,201", "--workers=2"}},
		{name: "lane workers", args: []string{"run", "201", "--tp-workers=2", "--ap-workers=1"}},
		{name: "non-run command", args: []string{"doctor", "--work-mem=256MB"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseCLIArgs(test.args); err == nil {
				t.Fatalf("ParseCLIArgs(%v) accepted invalid memory override", test.args)
			}
		})
	}
}

func TestParseCLIArgsRejectsInvalidFixedWorkerOverrides(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "zero workers", args: []string{"run", "101", "--workers=0"}},
		{name: "negative workers", args: []string{"run", "102", "--workers=-1"}},
		{name: "zero TP workers", args: []string{"run", "103", "--tp-workers=0", "--ap-workers=1"}},
		{name: "negative AP workers", args: []string{"run", "103", "--tp-workers=1", "--ap-workers=-1"}},
		{name: "missing AP pair", args: []string{"run", "103", "--tp-workers=1"}},
		{name: "missing TP pair", args: []string{"run", "103", "--ap-workers=1"}},
		{name: "generic workers for 103", args: []string{"run", "103", "--workers=2"}},
		{name: "lane workers for 101", args: []string{"run", "101", "--tp-workers=2", "--ap-workers=1"}},
		{name: "mixed override forms", args: []string{"run", "103", "--workers=2", "--tp-workers=2", "--ap-workers=1"}},
		{name: "fixed plus unrelated scenario", args: []string{"run", "101,201", "--workers=2"}},
		{name: "103 plus another scenario", args: []string{"run", "103,101", "--tp-workers=2", "--ap-workers=1"}},
		{name: "non-run command", args: []string{"doctor", "--workers=2"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseCLIArgs(test.args); err == nil {
				t.Fatalf("ParseCLIArgs(%v) accepted invalid worker override", test.args)
			}
		})
	}
}

func TestParseCLIArgsRejectsNonPositivePressureDuration(t *testing.T) {
	for _, value := range []string{"0s", "-1s"} {
		if _, err := ParseCLIArgs([]string{
			"run", "101", "--workers=1", "--duration=" + value,
		}); err == nil {
			t.Fatalf("accepted non-positive pressure duration %q", value)
		}
	}
}

func TestCLIHelpDocumentsFixedWorkerParameters(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := RunCLI(context.Background(), []string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	for _, token := range []string{
		"--workers N", "--work-mem VALUE", "--tp-workers N", "--ap-workers N",
		"gsbench run 101 --workers", "gsbench run 103 --tp-workers",
		"gsbench run 201 --workers", "gsbench run 202 --workers",
	} {
		if !strings.Contains(stdout.String(), token) {
			t.Errorf("help missing %q:\n%s", token, stdout.String())
		}
	}
}

func TestCLIHelpDocumentsLockWorkloadParameters(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := RunCLI(context.Background(), []string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	for _, token := range []string{
		"--sessions N", "--chain-depth N",
		"gsbench run 501 --sessions", "gsbench run 502 --sessions",
		"gsbench run 503 --sessions",
	} {
		if !strings.Contains(stdout.String(), token) {
			t.Errorf("help missing %q:\n%s", token, stdout.String())
		}
	}
}

func TestParseCLIArgsRejectsCleanupDataWithRunID(t *testing.T) {
	_, err := ParseCLIArgs([]string{
		"cleanup", "--data", "--run-id", "run-1",
	})
	if err == nil || !strings.Contains(err.Error(), "--data") ||
		!strings.Contains(err.Error(), "--run-id") {
		t.Fatalf("error=%v", err)
	}
}

func TestParseCLIArgsRequiresExplicitValidRiskAuthorization(t *testing.T) {
	options, err := ParseCLIArgs([]string{"run", "--allow-risk", "C", "343"})
	if err != nil {
		t.Fatal(err)
	}
	if options.AllowRisk != RiskC {
		t.Fatalf("allow risk=%q", options.AllowRisk)
	}
	for _, value := range []string{"", "D", "b", "risk-c"} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseCLIArgs([]string{"run", "--allow-risk", value, "343"}); err == nil {
				t.Fatalf("accepted invalid risk authorization %q", value)
			}
		})
	}
}

func TestParseCLIArgsAcceptsRiskAuthorizationAfterRunScenario(t *testing.T) {
	options, err := ParseCLIArgs([]string{"run", "343", "--allow-risk", "C"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(
		options.ScenarioCodes,
		[]ScenarioCode{343},
	) || options.AllowRisk != RiskC {
		t.Fatalf("options=%+v", options)
	}
}

func TestParseCLIArgsAcceptsPositionalRunScenarios(t *testing.T) {
	options, err := ParseCLIArgs([]string{"run", "101,ap_cpu,lock_row_chain"})
	if err != nil {
		t.Fatal(err)
	}
	want := []ScenarioCode{101, 102, 501}
	if !reflect.DeepEqual(options.ScenarioCodes, want) {
		t.Fatalf(
			"scenario codes=%v want=%v",
			options.ScenarioCodes,
			want,
		)
	}
}

func TestParseCLIArgsUsesConfigEnvironment(t *testing.T) {
	t.Setenv("GSBENCH_CONFIG", "/tmp/from-environment.cfg")

	options, err := ParseCLIArgs([]string{"doctor"})
	if err != nil {
		t.Fatal(err)
	}
	if options.ConfigPath != "/tmp/from-environment.cfg" {
		t.Fatalf("config path=%q", options.ConfigPath)
	}
}

func TestParseCLIArgsExplicitConfigOverridesEnvironment(t *testing.T) {
	t.Setenv("GSBENCH_CONFIG", "/tmp/from-environment.cfg")

	options, err := ParseCLIArgs([]string{"doctor", "-c", "explicit.cfg"})
	if err != nil {
		t.Fatal(err)
	}
	if options.ConfigPath != "explicit.cfg" {
		t.Fatalf("config path=%q", options.ConfigPath)
	}
}

func TestResolveConfigPathUsesDeterministicPrecedence(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "work")
	executable := filepath.Join(root, "release", "bin", "gsbench")
	candidates := []string{
		filepath.Join(cwd, "gsbench.cfg"),
		filepath.Join(cwd, "configs", "gsbench.cfg"),
		filepath.Join(filepath.Dir(executable), "gsbench.cfg"),
		filepath.Join(filepath.Dir(filepath.Dir(executable)), "configs", "gsbench.cfg"),
	}
	for _, path := range candidates {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("candidate"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	explicit := filepath.Join(cwd, "explicit.cfg")
	if err := os.WriteFile(explicit, []byte("explicit"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := resolveConfigPath("./explicit.cfg", executable, cwd)
	if err != nil || got != explicit {
		t.Fatalf("explicit path=%q err=%v want=%q", got, err, explicit)
	}
	for index, want := range candidates {
		got, err = resolveConfigPath("", executable, cwd)
		if err != nil || got != want {
			t.Fatalf("candidate %d path=%q err=%v want=%q", index, got, err, want)
		}
		if err := os.Remove(want); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := resolveConfigPath("", executable, cwd); err == nil {
		t.Fatal("missing discovered config did not fail")
	}
}

func TestResolveConfigPathRejectsMissingExplicitWithoutFallback(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "work")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	fallback := filepath.Join(cwd, "gsbench.cfg")
	if err := os.WriteFile(fallback, []byte("fallback"), 0o600); err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(cwd, "missing.cfg")
	if _, err := resolveConfigPath(missing, filepath.Join(root, "bin", "gsbench"), cwd); err == nil ||
		!strings.Contains(err.Error(), "missing.cfg") {
		t.Fatalf("missing explicit path error=%v", err)
	}
}

func TestExecuteCommandRejectsUnsafeRunIDBeforeCreatingLog(t *testing.T) {
	workingDirectory := t.TempDir()
	t.Chdir(workingDirectory)
	configPath := writeTestConfig(t, minimalConfig())
	var stdout, stderr bytes.Buffer

	code := executeCommand(
		context.Background(),
		CLIOptions{
			Command:    "run",
			ConfigPath: configPath,
			RunID:      " ../../escape ",
		},
		&stdout,
		&stderr,
	)
	if code != 1 || !strings.Contains(stderr.String(), "unsafe run ID") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	for _, directory := range []string{
		filepath.Join(workingDirectory, "logs"),
		filepath.Join(filepath.Dir(configPath), "logs"),
	} {
		if _, err := os.Lstat(directory); !os.IsNotExist(err) {
			t.Fatalf("unsafe run ID created log directory %q: %v", directory, err)
		}
	}
}

func TestExecuteCommandCreatesDefaultLogUnderConfigDirectory(t *testing.T) {
	workingDirectory := t.TempDir()
	t.Chdir(workingDirectory)
	configPath := writeTestConfig(
		t,
		strings.Replace(
			minimalConfig(),
			"password_env = GSBENCH_TEST_PASSWORD",
			"password_env = GSBENCH_TEST_PASSWORD\nconnect_timeout = 1ms",
			1,
		),
	)
	var stdout, stderr bytes.Buffer

	code := executeCommand(
		context.Background(),
		CLIOptions{
			Command:    "run",
			ConfigPath: configPath,
			RunID:      "run-log-anchor",
		},
		&stdout,
		&stderr,
	)
	if code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	wantPath := filepath.Join(
		filepath.Dir(configPath),
		"logs",
		"gsbench_run-log-anchor.log",
	)
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("config-anchored run log %q: %v", wantPath, err)
	}
	if _, err := os.Lstat(filepath.Join(workingDirectory, "logs")); !os.IsNotExist(err) {
		t.Fatalf("run log used process cwd: %v", err)
	}
}

func TestPreparePlanRunBaselineInspectsWithoutRepairing(t *testing.T) {
	for _, validationEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("validation_%v", validationEnabled), func(t *testing.T) {
			var output bytes.Buffer
			log, err := NewRunLog(&output, "", Version)
			if err != nil {
				t.Fatal(err)
			}
			repairCalls := 0
			verifyCalls := 0
			err = preparePlanRunBaseline(
				context.Background(),
				nil,
				BenchConfig{
					Run:  RunConfig{ValidationEnabled: validationEnabled},
					Data: DataConfig{Schema: "bench"},
				},
				log,
				func(context.Context, *Database, string) ([]BaselineRepairResult, error) {
					repairCalls++
					return []BaselineRepairResult{{
						Target: "plan_index",
						Status: "RESTORED",
					}}, nil
				},
				func(context.Context, *Database, string) error {
					verifyCalls++
					return nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if repairCalls != 0 || verifyCalls != 1 {
				t.Fatalf("repair calls=%d verify calls=%d want=0/1", repairCalls, verifyCalls)
			}
			if strings.Contains(output.String(), "status=RESTORED") {
				t.Fatalf("baseline repair result was logged: %q", output.String())
			}
		})
	}
}

func TestVerifyRestorePlanBaselineSkipsNonPlanActions(t *testing.T) {
	backend := &databaseRestoreBackend{}
	action := Action{ScenarioCode: 101}
	if err := backend.verifyPlanBaselineForActions(
		context.Background(),
		[]Action{action},
	); err != nil {
		t.Fatalf("non-plan restore checked plan baseline: %v", err)
	}
}

func TestVerifyRestorePlanBaselineHonorsDisabledValidation(t *testing.T) {
	backend := &databaseRestoreBackend{
		cfg: BenchConfig{Run: RunConfig{ValidationEnabled: false}},
	}
	action := Action{ScenarioCode: 601}
	if err := backend.verifyPlanBaselineForActions(
		context.Background(),
		[]Action{action},
	); err != nil {
		t.Fatalf("disabled validation checked plan baseline: %v", err)
	}
}

func TestRestorePlanVerificationIsMandatoryFor602(t *testing.T) {
	actions := []Action{{
		ScenarioCode: 602,
		Kind:         ActionSQLMutation,
	}}
	if !restorePlanVerificationRequired(false, actions) {
		t.Fatal("602 recover skipped strict plan verification")
	}
	if restorePlanVerificationRequired(false, []Action{{
		ScenarioCode: 601,
		Kind:         ActionSQLMutation,
	}}) {
		t.Fatal("601 disabled-validation behavior changed")
	}
}

func TestParseCLIArgsSupportsShortFlags(t *testing.T) {
	options, err := ParseCLIArgs([]string{"run", "-s", "101,ap_cpu,lock_row_chain", "-d", "45s"})
	if err != nil {
		t.Fatal(err)
	}
	want := []ScenarioCode{101, 102, 501}
	if !reflect.DeepEqual(options.ScenarioCodes, want) {
		t.Fatalf(
			"scenario codes=%v want=%v",
			options.ScenarioCodes,
			want,
		)
	}
	if options.Duration != 45*time.Second {
		t.Fatalf("duration=%s", options.Duration)
	}
}

func TestParseCLIArgsResolvesThreeDigitScenarioCodes(t *testing.T) {
	options, err := ParseCLIArgs([]string{"run", "-s", "101,102,103,401,402,201,501,801"})
	if err != nil {
		t.Fatal(err)
	}
	want := []ScenarioCode{
		101, 102, 103, 401, 402, 201, 501, 801,
	}
	if !reflect.DeepEqual(options.ScenarioCodes, want) {
		t.Fatalf(
			"scenario codes=%v want=%v",
			options.ScenarioCodes,
			want,
		)
	}
}

func TestParseCLIArgsResolvesIndependentPlanScenarioCodes(t *testing.T) {
	definitions, err := resolveScenarioInputs([]string{
		"601", "602", "603", "604", "605", "606",
	})
	if err != nil {
		t.Fatal(err)
	}
	options := make([]ScenarioCode, len(definitions))
	for index, definition := range definitions {
		options[index] = definition.Code
	}
	want := []ScenarioCode{
		601, 602, 603, 604, 605, 606,
	}
	if !reflect.DeepEqual(options, want) {
		t.Fatalf(
			"scenario codes=%v want=%v",
			options,
			want,
		)
	}
}

func TestParseCLIArgsRejectsLegacyNumbersAndAliases(t *testing.T) {
	for _, input := range []string{"1", "tp", "locks", "plan_regression"} {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseCLIArgs([]string{"run", input}); err == nil {
				t.Fatalf("legacy scenario %q accepted", input)
			}
		})
	}
}

func TestCLIScenariosListsStableCatalogWithoutDatabaseConnection(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := RunCLI(context.Background(), []string{"scenarios"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	text := stdout.String()
	for _, token := range []string{"CODE  CATEGORY  NAME  RISK  APPLIES_TO", "101", "tp_cpu", "801", "vacuum_pressure"} {
		if !strings.Contains(text, token) {
			t.Fatalf("catalog output missing %q:\n%s", token, text)
		}
	}
	factories := DefaultScenarioFactories()
	seen := make(map[ScenarioCode]bool, len(factories))
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if got := len(lines) - 1; got != len(factories) {
		t.Fatalf("scenario output rows=%d want implemented=%d", got, len(factories))
	}
	for _, line := range lines[1:] {
		var code ScenarioCode
		if _, err := fmt.Sscanf(line, "%d", &code); err != nil {
			t.Fatalf("parse scenario line %q: %v", line, err)
		}
		if factories[code] == nil {
			t.Fatalf("scenarios listed unimplemented code %03d", code)
		}
		if seen[code] {
			t.Fatalf("scenarios listed duplicate code %03d", code)
		}
		seen[code] = true
	}
	if len(seen) != len(factories) {
		t.Fatalf("listed scenario count=%d want implemented=%d", len(seen), len(factories))
	}
	for code := range factories {
		if !seen[code] {
			t.Errorf("implemented scenario %03d is missing", code)
		}
	}

	var usage bytes.Buffer
	printUsage(&usage)
	for _, definition := range DefaultScenarioCatalog().Definitions() {
		entry := fmt.Sprintf("\n  %03d=%s\n", definition.Code, definition.Name)
		listed := strings.Contains(usage.String(), entry)
		if (factories[definition.Code] != nil) != listed {
			t.Errorf(
				"usage listing for %03d implemented=%v listed=%v",
				definition.Code,
				factories[definition.Code] != nil,
				listed,
			)
		}
	}
}

func TestParseCLIArgsAcceptsRestoreWithOptionalRunID(t *testing.T) {
	all, err := ParseCLIArgs([]string{"restore"})
	if err != nil {
		t.Fatal(err)
	}
	if all.Command != "restore" || all.RunID != "" {
		t.Fatalf("all=%+v", all)
	}
	one, err := ParseCLIArgs([]string{"restore", "--run-id", "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if one.Command != "restore" || one.RunID != "run-1" {
		t.Fatalf("one=%+v", one)
	}
}

func TestParseDatasetSize(t *testing.T) {
	for input, want := range map[string]int64{
		"1GB":   1 << 30,
		"100gb": 100 << 30,
		"1.5TB": 1536 << 30,
		"2TB":   2 << 40,
	} {
		got, err := ParseDatasetSize(input)
		if err != nil || got != want {
			t.Fatalf("%s: got=%d err=%v want=%d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "0GB", "512MB", "1.234TB"} {
		if _, err := ParseDatasetSize(input); err == nil {
			t.Fatalf("%s: expected error", input)
		}
	}
}

func TestParseDatasetSizeAllowsValuesOutsideLegacyPolicyRange(t *testing.T) {
	for input, want := range map[string]int64{
		"0.5GB": 1 << 29,
		"4TB":   4 << 40,
	} {
		got, err := ParseDatasetSize(input)
		if err != nil || got != want {
			t.Fatalf("%s: got=%d err=%v want=%d", input, got, err, want)
		}
	}
}

func TestParseCLIArgsAcceptsInitSizeOnly(t *testing.T) {
	got, err := ParseCLIArgs([]string{"init", "--size", "100GB"})
	if err != nil || got.DatasetBytes != 100<<30 || got.DatasetSize != "100GB" {
		t.Fatalf("options=%+v err=%v", got, err)
	}
	if _, err := ParseCLIArgs([]string{"run", "--size", "1GB"}); err == nil {
		t.Fatal("run accepted --size")
	}
	if _, err := ParseCLIArgs([]string{"init", "--size="}); err == nil {
		t.Fatal("init accepted an explicitly empty --size")
	}
}

func TestCLIHelpDocumentsInitSize(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := RunCLI(context.Background(), []string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	for _, token := range []string{"gsbench init --size 100GB", "--size VALUE", "maximum 2TB"} {
		if !strings.Contains(stdout.String(), token) {
			t.Fatalf("help missing %q:\n%s", token, stdout.String())
		}
	}
}

func TestCLIHelpDocumentsRiskAuthorization(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := RunCLI(context.Background(), []string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "--allow-risk A|B|C") {
		t.Fatalf("help does not document risk authorization:\n%s", stdout.String())
	}
}
