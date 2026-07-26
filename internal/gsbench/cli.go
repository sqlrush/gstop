package gsbench

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const Version = "v0.1.0-dev"

const ConfigEnv = "GSBENCH_CONFIG"

var lifecycleCommands = map[string]struct{}{
	"init": {}, "run": {}, "status": {}, "stop": {}, "cleanup": {}, "doctor": {},
}

type CLIOptions struct {
	Command    string
	ConfigPath string
	Scenarios  []string
	Duration   time.Duration
	Profile    string
	DryRun     bool
	WithData   bool
	RunID      string
}

func ParseCLIArgs(args []string) (CLIOptions, error) {
	if len(args) == 0 {
		return CLIOptions{Command: "help"}, nil
	}
	command := strings.ToLower(args[0])
	if command == "help" || command == "-h" || command == "--help" || command == "version" || command == "--version" || command == "-version" {
		return CLIOptions{Command: command, ConfigPath: defaultBenchConfigPath()}, nil
	}
	if _, ok := lifecycleCommands[command]; !ok {
		return CLIOptions{}, fmt.Errorf("unknown command %q", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var configPath, scenarios, durationText string
	options := CLIOptions{Command: command}
	flags.StringVar(&configPath, "c", "", "config path")
	flags.StringVar(&configPath, "config", "", "config path")
	flags.StringVar(&scenarios, "s", "", "comma-separated scenarios")
	flags.StringVar(&scenarios, "scenario", "", "comma-separated scenarios")
	flags.StringVar(&durationText, "d", "", "run duration")
	flags.StringVar(&durationText, "duration", "", "run duration")
	flags.StringVar(&options.Profile, "profile", "", "data profile")
	flags.BoolVar(&options.DryRun, "dry-run", false, "show actions without mutation")
	flags.BoolVar(&options.WithData, "data", false, "include benchmark data")
	flags.StringVar(&options.RunID, "run-id", "", "specific run id")
	if err := flags.Parse(args[1:]); err != nil {
		return CLIOptions{}, err
	}
	if flags.NArg() != 0 {
		if command != "run" || flags.NArg() != 1 || scenarios != "" {
			return CLIOptions{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
		}
		scenarios = flags.Arg(0)
	}
	if configPath == "" {
		configPath = defaultBenchConfigPath()
	}
	options.ConfigPath = configPath
	if durationText != "" {
		duration, err := time.ParseDuration(durationText)
		if err != nil {
			return CLIOptions{}, fmt.Errorf("duration: %w", err)
		}
		options.Duration = duration
	}
	options.Scenarios = normalizeScenarios(splitList(scenarios))
	return options, nil
}

func normalizeScenario(name string) string {
	aliases := map[string]string{
		"1": "tp_cpu", "2": "ap_cpu", "3": "mixed_cpu",
		"4": "connection_pool", "5": "thread_pool", "6": "dynamic_memory",
		"7": "plan_regression", "8": "lock_storm", "9": "vacuum_pressure",
		"tp": "tp_cpu", "ap": "ap_cpu", "mixed": "mixed_cpu",
		"connections": "connection_pool", "threads": "thread_pool", "memory": "dynamic_memory",
		"plan": "plan_regression", "locks": "lock_storm", "vacuum": "vacuum_pressure",
	}
	if expanded := aliases[name]; expanded != "" {
		return expanded
	}
	return name
}

func normalizeScenarios(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, normalizeScenario(name))
	}
	return out
}

func defaultBenchConfigPath() string {
	if path := strings.TrimSpace(os.Getenv(ConfigEnv)); path != "" {
		return path
	}
	if _, err := os.Stat("gsbench.cfg"); err == nil {
		return "gsbench.cfg"
	}
	return filepath.Join("configs", "gsbench.cfg")
}

func RunCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	options, err := ParseCLIArgs(args)
	if err != nil {
		_, _ = io.WriteString(stdout, Banner(Version))
		fmt.Fprintln(stderr, err)
		printUsage(stderr)
		return 2
	}
	if options.Command == "help" || options.Command == "-h" || options.Command == "--help" {
		_, _ = io.WriteString(stdout, Banner(Version))
		printUsage(stdout)
		return 0
	}
	switch options.Command {
	case "version", "--version", "-version":
		_, _ = io.WriteString(stdout, Banner(Version))
		return 0
	default:
		return executeCommand(ctx, options, stdout, stderr)
	}
}

func printUsage(w io.Writer) {
	_, _ = io.WriteString(w, `Usage:
  gsbench <init|run|status|stop|cleanup|doctor|version> [options]
  gsbench run [-s LIST] [-d DURATION]
  gsbench run SCENARIO[,SCENARIO...]

Options:
  -c, --config PATH       configuration file (legacy-compatible override)
  -s, --scenario LIST    comma-separated scenario numbers or names
  -d, --duration VALUE   run duration, for example 30s or 5m
      --profile VALUE    data profile: quick or stress
      --dry-run          validate and show actions without workload mutation
      --run-id ID        select one run for status, stop, or cleanup
      --data             also remove benchmark data during cleanup

Configuration:
  -c/--config > GSBENCH_CONFIG > ./gsbench.cfg > ./configs/gsbench.cfg

Scenarios:
  1=tp_cpu  2=ap_cpu  3=mixed_cpu  4=connection_pool  5=thread_pool
  6=dynamic_memory  7=plan_regression  8=lock_storm  9=vacuum_pressure
  Numbers, full names, and aliases may be mixed, for example: -s 1,ap,locks
`)
}
