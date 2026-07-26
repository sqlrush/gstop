package gsbench

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFoundationSmokeWithoutLiveDatabase(t *testing.T) {
	t.Run("shipped config loads safe foundation defaults", func(t *testing.T) {
		configPath := filepath.Join("..", "..", "configs", "gsbench.cfg")
		cfg, err := LoadConfig(configPath, Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Raw.GetInt("run.scenarios", 0); got != 101 {
			t.Fatalf("configured scenario=%d want three-digit default", got)
		}
		if !reflect.DeepEqual(
			cfg.Run.ScenarioCodes,
			[]ScenarioCode{101},
		) {
			t.Fatalf("scenario codes=%v", cfg.Run.ScenarioCodes)
		}
		if cfg.Safety.AllowAdminMutation ||
			cfg.Safety.AllowInfrastructureFault ||
			cfg.Safety.RestoreOriginalRole ||
			cfg.Safety.AllowInstanceParameterChange ||
			cfg.Safety.AllowDatabaseRestart {
			t.Fatalf("unsafe shipped safety defaults=%+v", cfg.Safety)
		}
		if cfg.FaultProvider.Type != "none" ||
			cfg.FaultProvider.LedgerPath == "" {
			t.Fatalf(
				"fault provider defaults=%+v",
				cfg.FaultProvider,
			)
		}
	})

	t.Run("help and scenarios need no config or database", func(t *testing.T) {
		t.Setenv(ConfigEnv, filepath.Join(t.TempDir(), "missing.cfg"))
		t.Chdir(t.TempDir())

		for _, test := range []struct {
			command string
			want    string
		}{
			{command: "help", want: "101=tp_cpu"},
			{command: "scenarios", want: "CODE  CATEGORY  NAME  RISK  APPLIES_TO"},
		} {
			var stdout, stderr bytes.Buffer
			if code := RunCLI(
				context.Background(),
				[]string{test.command},
				&stdout,
				&stderr,
			); code != 0 {
				t.Fatalf(
					"%s code=%d stderr=%q",
					test.command,
					code,
					stderr.String(),
				)
			}
			if !strings.Contains(stdout.String(), test.want) {
				t.Fatalf(
					"%s output missing %q:\n%s",
					test.command,
					test.want,
					stdout.String(),
				)
			}
		}
	})

	t.Run("doctor detects a centralized GaussDB fixture", func(t *testing.T) {
		env := DetectEnvironment(
			context.Background(),
			fakeCapabilityProber{values: gaussDBValues("centralized", "")},
		)
		if !env.Supported ||
			env.Product != ProductGaussDB ||
			env.Topology != TopologyCentralized {
			t.Fatalf("environment=%+v", env)
		}
		report := strings.Join(
			doctorEnvironmentReport(
				env,
				DefaultScenarioCatalog().Definitions(),
			),
			"\n",
		)
		for _, want := range []string{
			"product=GaussDB",
			"topology=centralized_gaussdb",
			"scenario=101 name=tp_cpu decision=SUPPORTED",
		} {
			if !strings.Contains(report, want) {
				t.Fatalf("doctor report missing %q:\n%s", want, report)
			}
		}
	})

	t.Run("init dry run selects the detected dataset dialect", func(t *testing.T) {
		cfg := BenchConfig{
			Run: RunConfig{Profile: "quick", DryRun: true},
			Data: DataConfig{
				Schema:             "gsbench",
				TargetBytes:        1 << 30,
				MinFreeDiskPercent: 20,
			},
		}
		capacity := Capacity{TotalBytes: 10 << 30, FreeBytes: 9 << 30}
		tests := []struct {
			name        string
			env         Environment
			distributed bool
		}{
			{
				name: "centralized",
				env: Environment{
					Product:  ProductGaussDB,
					Topology: TopologyCentralized,
				},
			},
			{
				name: "distributed",
				env: Environment{
					Product:  ProductGaussDB,
					Topology: TopologyDistributed,
				},
				distributed: true,
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				plan, err := PlanDataset(cfg, capacity, test.env)
				if err != nil {
					t.Fatal(err)
				}
				hasDistribution := strings.Contains(
					strings.Join(plan.DDL, "\n"),
					"DISTRIBUTE BY",
				)
				if hasDistribution != test.distributed {
					t.Fatalf(
						"topology=%s distributed DDL=%v",
						test.env.Topology,
						hasDistribution,
					)
				}
			})
		}
	})

	t.Run("restore dry run discovers without mutation or files", func(t *testing.T) {
		action := restoreTestAction(
			"run-smoke",
			7,
			ActionNetworkQDisc,
			"eth0",
		)
		backend := &fakeRestoreBackend{discovery: RestoreDiscovery{
			DatabaseActions: []Action{action},
		}}
		summary := NewRestoreCoordinator(backend).Restore(
			context.Background(),
			RestoreRequest{RunID: action.RunID, DryRun: true},
		)
		if summary.Failed ||
			len(summary.PlannedActions) != 1 ||
			summary.PlannedActions[0].Sequence != action.Sequence {
			t.Fatalf("summary=%+v", summary)
		}
		if want := []string{"discover:run-smoke"}; !reflect.DeepEqual(
			backend.events,
			want,
		) {
			t.Fatalf("events=%v want=%v", backend.events, want)
		}

		parent := t.TempDir()
		ledger := NewFileRecoveryLedger(
			filepath.Join(parent, "recovery.json"),
		)
		pending, err := ledger.Pending(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 0 {
			t.Fatalf("pending=%+v", pending)
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("dry-run discovery created files: %+v", entries)
		}
	})
}

func TestIntegrationDoctor(t *testing.T) {
	configPath := os.Getenv("GSBENCH_INTEGRATION_CONFIG")
	if configPath == "" {
		t.Skip("set GSBENCH_INTEGRATION_CONFIG to run live openGauss integration")
	}
	cfg, err := LoadConfig(configPath, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	db, err := OpenDatabase(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	capabilities := DetectCapabilities(context.Background(), db)
	if !capabilities.Supported {
		t.Fatalf("unsupported live target: %+v", capabilities)
	}
}
