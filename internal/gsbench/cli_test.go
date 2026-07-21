package gsbench

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestCLIVersionPrintsAuthor(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunCLI(context.Background(), []string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Author: WangYingJie <sqlrush@gmail.com>") {
		t.Fatalf("stdout=%q", stdout.String())
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

func TestCLIHelpDocumentsShortcuts(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunCLI(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	for _, token := range []string{"GSBENCH_CONFIG", "-s", "-d", "1=tp_cpu"} {
		if !strings.Contains(stdout.String(), token) {
			t.Fatalf("help missing %q:\n%s", token, stdout.String())
		}
	}
}

func TestParseCLIArgsSupportsScenarioDurationAndDryRun(t *testing.T) {
	options, err := ParseCLIArgs([]string{"run", "-c", "custom.cfg", "--scenario", "tp_cpu,locks", "--duration", "30s", "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Command != "run" || options.ConfigPath != "custom.cfg" || options.Duration.String() != "30s" || !options.DryRun {
		t.Fatalf("options=%+v", options)
	}
	if len(options.Scenarios) != 2 || options.Scenarios[1] != "lock_storm" {
		t.Fatalf("scenarios=%v", options.Scenarios)
	}
}

func TestParseCLIArgsAcceptsPositionalRunScenarios(t *testing.T) {
	options, err := ParseCLIArgs([]string{"run", "tp,ap,locks"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tp_cpu", "ap_cpu", "lock_storm"}
	if len(options.Scenarios) != len(want) {
		t.Fatalf("scenarios=%v", options.Scenarios)
	}
	for i := range want {
		if options.Scenarios[i] != want[i] {
			t.Fatalf("scenarios=%v", options.Scenarios)
		}
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

func TestParseCLIArgsSupportsShortFlags(t *testing.T) {
	options, err := ParseCLIArgs([]string{"run", "-s", "1,ap,lock_storm", "-d", "45s"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tp_cpu", "ap_cpu", "lock_storm"}
	if len(options.Scenarios) != len(want) {
		t.Fatalf("scenarios=%v", options.Scenarios)
	}
	for i := range want {
		if options.Scenarios[i] != want[i] {
			t.Fatalf("scenarios=%v", options.Scenarios)
		}
	}
	if options.Duration != 45*time.Second {
		t.Fatalf("duration=%s", options.Duration)
	}
}

func TestParseCLIArgsMapsAllScenarioNumbers(t *testing.T) {
	options, err := ParseCLIArgs([]string{"run", "-s", "1,2,3,4,5,6,7,8,9"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"tp_cpu", "ap_cpu", "mixed_cpu", "connection_pool", "thread_pool",
		"dynamic_memory", "plan_regression", "lock_storm", "vacuum_pressure",
	}
	if len(options.Scenarios) != len(want) {
		t.Fatalf("scenarios=%v", options.Scenarios)
	}
	for i := range want {
		if options.Scenarios[i] != want[i] {
			t.Fatalf("scenarios=%v", options.Scenarios)
		}
	}
}

func TestParseCLIArgsAcceptsNumericPositionalRunScenarios(t *testing.T) {
	options, err := ParseCLIArgs([]string{"run", "1,2,8"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tp_cpu", "ap_cpu", "lock_storm"}
	for i := range want {
		if options.Scenarios[i] != want[i] {
			t.Fatalf("scenarios=%v", options.Scenarios)
		}
	}
}
