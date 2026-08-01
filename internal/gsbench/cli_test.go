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
	if !strings.HasPrefix(stdout.String(), "gsbench v1.1.1\n") {
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
		"gsbench restore", "601=planchange_stats_target", "602=planchange_index_unusable",
		"603=planchange_stats_ndistinct", "604=planchange_stats_extended",
		"605=planchange_index_drop", "606=planchange_index_shape",
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

func TestPreparePlanRunBaselineAlwaysRepairsAndOnlyStrictlyVerifies(t *testing.T) {
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
			wantVerifyCalls := 0
			if validationEnabled {
				wantVerifyCalls = 1
			}
			if repairCalls != 1 || verifyCalls != wantVerifyCalls {
				t.Fatalf("repair calls=%d verify calls=%d want=1/%d", repairCalls, verifyCalls, wantVerifyCalls)
			}
			if !strings.Contains(output.String(), "target=plan_index status=RESTORED") {
				t.Fatalf("baseline repair result was not logged: %q", output.String())
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
	options, err := ParseCLIArgs([]string{"run", "-s", "101,102,103,401,402,201,501,601,801"})
	if err != nil {
		t.Fatal(err)
	}
	want := []ScenarioCode{
		101, 102, 103, 401, 402, 201, 501, 601, 801,
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
	options, err := ParseCLIArgs([]string{"run", "-s", "601,602,603,604,605,606"})
	if err != nil {
		t.Fatal(err)
	}
	want := []ScenarioCode{
		601, 602, 603, 604, 605, 606,
	}
	if !reflect.DeepEqual(options.ScenarioCodes, want) {
		t.Fatalf(
			"scenario codes=%v want=%v",
			options.ScenarioCodes,
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
	for _, input := range []string{"", "0GB", "512MB", "2.01TB", "2049GB", "1.234TB"} {
		if _, err := ParseDatasetSize(input); err == nil {
			t.Fatalf("%s: expected error", input)
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
