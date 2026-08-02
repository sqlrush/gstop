package gsbench

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const Version = "v1.1.2"

const ConfigEnv = "GSBENCH_CONFIG"

const maxDatasetBytes int64 = 2 << 40

var datasetSizeRE = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]{1,2})?)(GB|TB)$`)

var workMemSizeRE = regexp.MustCompile(`(?i)^([1-9][0-9]*)(kB|MB|GB)$`)

const minWorkMemKB int64 = 64
const defaultWorkMemKB int64 = 256 * 1024

var lifecycleCommands = map[string]struct{}{
	"init": {}, "run": {}, "status": {}, "stop": {}, "restore": {}, "cleanup": {}, "doctor": {}, "scenarios": {},
}

type CLIOptions struct {
	Command       string
	ConfigPath    string
	AllowRisk     RiskLevel
	ScenarioCodes []ScenarioCode
	Duration      time.Duration
	Workers       int
	TPWorkers     int
	APWorkers     int
	WorkMemKB     int64
	Sessions      int
	ChainDepth    int
	Profile       string
	DryRun        bool
	WithData      bool
	RunID         string
	DatasetBytes  int64
	DatasetSize   string
}

func ParseDatasetSize(value string) (int64, error) {
	match := datasetSizeRE.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return 0, fmt.Errorf("size must use GB or TB with at most two decimals")
	}
	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("size must be positive")
	}
	unit := float64(int64(1 << 30))
	if strings.EqualFold(match[2], "TB") {
		unit = float64(int64(1 << 40))
	}
	bytes := int64(math.Round(number * unit))
	if bytes < 1<<30 || bytes > maxDatasetBytes {
		return 0, fmt.Errorf("size must be between 1GB and 2TB")
	}
	return bytes, nil
}

func ParseWorkMemKB(value string) (int64, error) {
	match := workMemSizeRE.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return 0, fmt.Errorf("work_mem must be a positive integer using kB, MB, or GB")
	}
	quantity, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("work_mem value is out of range")
	}
	multiplier := uint64(1)
	switch strings.ToUpper(match[2]) {
	case "MB":
		multiplier = 1024
	case "GB":
		multiplier = 1024 * 1024
	}
	if quantity > uint64(math.MaxInt64)/multiplier {
		return 0, fmt.Errorf("work_mem value is out of range")
	}
	workMemKB := int64(quantity * multiplier)
	if workMemKB < minWorkMemKB {
		return 0, fmt.Errorf("work_mem must be at least %dkB", minWorkMemKB)
	}
	if workMemKB > math.MaxInt64/1024 {
		return 0, fmt.Errorf("work_mem value is out of range")
	}
	return workMemKB, nil
}

func ParseCLIArgs(args []string) (CLIOptions, error) {
	if len(args) == 0 {
		return CLIOptions{Command: "help"}, nil
	}
	command := strings.ToLower(args[0])
	if command == "help" || command == "-h" || command == "--help" || command == "version" || command == "--version" || command == "-version" {
		return CLIOptions{Command: command}, nil
	}
	if _, ok := lifecycleCommands[command]; !ok {
		return CLIOptions{}, fmt.Errorf("unknown command %q", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var configPath, scenarios, durationText, sizeText, workMemText, allowRiskText string
	options := CLIOptions{Command: command}
	flags.StringVar(&configPath, "c", "", "config path")
	flags.StringVar(&configPath, "config", "", "config path")
	flags.StringVar(&scenarios, "s", "", "comma-separated scenarios")
	flags.StringVar(&scenarios, "scenario", "", "comma-separated scenarios")
	flags.StringVar(&durationText, "d", "", "pressure duration")
	flags.StringVar(&durationText, "duration", "", "pressure duration")
	flags.IntVar(&options.Workers, "workers", 0, "fixed workers for scenarios 101/102 or 201/202")
	flags.IntVar(&options.TPWorkers, "tp-workers", 0, "fixed TP workers for scenario 103")
	flags.IntVar(&options.APWorkers, "ap-workers", 0, "fixed AP workers for scenario 103")
	flags.StringVar(&workMemText, "work-mem", "", "work_mem for scenarios 201/202 (kB, MB, or GB)")
	flags.IntVar(&options.Sessions, "sessions", 0, "total holder plus waiter sessions for scenarios 501-503")
	flags.IntVar(&options.ChainDepth, "chain-depth", 0, "row wait chain depth for scenario 501 (1-5)")
	flags.StringVar(&options.Profile, "profile", "", "data profile")
	flags.BoolVar(&options.DryRun, "dry-run", false, "show actions without mutation")
	flags.BoolVar(&options.WithData, "data", false, "include benchmark data")
	flags.StringVar(&options.RunID, "run-id", "", "specific run id")
	flags.StringVar(&sizeText, "size", "", "target init data size (1GB-2TB)")
	flags.StringVar(&allowRiskText, "allow-risk", "", "explicit maximum risk authorization (A, B, or C)")
	parseArgs := append([]string(nil), args[1:]...)
	if command == "run" && len(parseArgs) > 1 &&
		!strings.HasPrefix(parseArgs[0], "-") {
		positionalScenario := parseArgs[0]
		parseArgs = append(parseArgs[1:], positionalScenario)
	}
	if err := flags.Parse(parseArgs); err != nil {
		return CLIOptions{}, err
	}
	sizeSet := false
	allowRiskSet := false
	workersSet := false
	tpWorkersSet := false
	apWorkersSet := false
	workMemSet := false
	sessionsSet := false
	chainDepthSet := false
	flags.Visit(func(value *flag.Flag) {
		switch value.Name {
		case "size":
			sizeSet = true
		case "allow-risk":
			allowRiskSet = true
		case "workers":
			workersSet = true
		case "tp-workers":
			tpWorkersSet = true
		case "ap-workers":
			apWorkersSet = true
		case "work-mem":
			workMemSet = true
		case "sessions":
			sessionsSet = true
		case "chain-depth":
			chainDepthSet = true
		}
	})
	workerOverrideSet := workersSet || tpWorkersSet || apWorkersSet
	lockOverrideSet := sessionsSet || chainDepthSet
	if (workerOverrideSet || workMemSet || lockOverrideSet) && command != "run" {
		return CLIOptions{}, fmt.Errorf("workload overrides are only valid with run")
	}
	if (workersSet && options.Workers <= 0) ||
		(tpWorkersSet && options.TPWorkers <= 0) ||
		(apWorkersSet && options.APWorkers <= 0) {
		return CLIOptions{}, fmt.Errorf("worker counts must be positive")
	}
	if tpWorkersSet != apWorkersSet {
		return CLIOptions{}, fmt.Errorf("--tp-workers and --ap-workers must be provided together")
	}
	if workersSet && (tpWorkersSet || apWorkersSet) {
		return CLIOptions{}, fmt.Errorf("--workers cannot be combined with --tp-workers or --ap-workers")
	}
	if sessionsSet && options.Sessions < 2 {
		return CLIOptions{}, fmt.Errorf("--sessions must be at least 2")
	}
	if chainDepthSet && (options.ChainDepth < 1 || options.ChainDepth > 5) {
		return CLIOptions{}, fmt.Errorf("--chain-depth must be between 1 and 5")
	}
	if workMemSet {
		workMemKB, err := ParseWorkMemKB(workMemText)
		if err != nil {
			return CLIOptions{}, err
		}
		options.WorkMemKB = workMemKB
	}
	if sizeSet {
		if command != "init" {
			return CLIOptions{}, fmt.Errorf("--size is only valid with init")
		}
		size, err := ParseDatasetSize(sizeText)
		if err != nil {
			return CLIOptions{}, fmt.Errorf("size: %w", err)
		}
		options.DatasetBytes = size
		options.DatasetSize = strings.TrimSpace(sizeText)
	}
	if flags.NArg() != 0 {
		if command == "run" && flags.NArg() == 3 && scenarios == "" && !allowRiskSet &&
			flags.Arg(1) == "--allow-risk" {
			scenarios = flags.Arg(0)
			allowRiskText = flags.Arg(2)
			allowRiskSet = true
		} else {
			if command != "run" || flags.NArg() != 1 || scenarios != "" {
				return CLIOptions{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
			}
			scenarios = flags.Arg(0)
		}
	}
	if command == "cleanup" && options.WithData &&
		strings.TrimSpace(options.RunID) != "" {
		return CLIOptions{}, fmt.Errorf(
			"cleanup --data cannot be combined with --run-id",
		)
	}
	if allowRiskSet {
		options.AllowRisk = RiskLevel(allowRiskText)
		if riskLevelRank(options.AllowRisk) == 0 {
			return CLIOptions{}, fmt.Errorf("allow-risk must be A, B, or C")
		}
	}
	if configPath == "" {
		configPath = strings.TrimSpace(os.Getenv(ConfigEnv))
	}
	options.ConfigPath = configPath
	if durationText != "" {
		duration, err := time.ParseDuration(durationText)
		if err != nil {
			return CLIOptions{}, fmt.Errorf("duration: %w", err)
		}
		if duration <= 0 {
			return CLIOptions{}, fmt.Errorf("duration must be positive")
		}
		options.Duration = duration
	}
	definitions, err := resolveScenarioInputs(splitList(scenarios))
	if err != nil {
		return CLIOptions{}, err
	}
	options.ScenarioCodes = make([]ScenarioCode, len(definitions))
	for i, definition := range definitions {
		options.ScenarioCodes[i] = definition.Code
	}
	if (workerOverrideSet || workMemSet) && len(options.ScenarioCodes) > 0 {
		if err := validateFixedWorkerOverrideCompatibility(
			options.ScenarioCodes,
			options.Workers,
			options.TPWorkers,
			options.APWorkers,
			options.WorkMemKB,
		); err != nil {
			return CLIOptions{}, err
		}
	}
	if lockOverrideSet && len(options.ScenarioCodes) > 0 {
		if err := validateLockOverrideCompatibility(
			options.ScenarioCodes,
			options.Sessions,
			options.ChainDepth,
		); err != nil {
			return CLIOptions{}, err
		}
	}
	return options, nil
}

func isPlanChangeCode(code ScenarioCode) bool {
	definition, err := DefaultScenarioCatalog().LookupCode(code)
	return err == nil && definition.Category == CategoryPlan && code >= 601 && code <= 606
}

func resolveScenarioInputs(inputs []string) ([]ScenarioDefinition, error) {
	catalog := DefaultScenarioCatalog()
	definitions := make([]ScenarioDefinition, 0, len(inputs))
	for _, input := range inputs {
		definition, err := catalog.Resolve(input)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

func resolveConfigPath(explicit string, executable string, cwd string) (string, error) {
	cwd, err := filepath.Abs(filepath.Clean(cwd))
	if err != nil {
		return "", fmt.Errorf("resolve current directory for config discovery: %w", err)
	}
	resolve := func(path string) (string, error) {
		path = strings.TrimSpace(path)
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		absolute, err := filepath.Abs(filepath.Clean(path))
		if err != nil {
			return "", err
		}
		return filepath.Clean(absolute), nil
	}
	regularConfig := func(path string) (bool, error) {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return info.Mode().IsRegular(), nil
	}

	if strings.TrimSpace(explicit) != "" {
		path, err := resolve(explicit)
		if err != nil {
			return "", fmt.Errorf("resolve configured config path: %w", err)
		}
		regular, err := regularConfig(path)
		if err != nil {
			return "", fmt.Errorf("inspect configured config path %q: %w", path, err)
		}
		if !regular {
			return "", fmt.Errorf("configured config path %q does not exist or is not a regular file", path)
		}
		return path, nil
	}

	candidates := []string{
		filepath.Join(cwd, "gsbench.cfg"),
		filepath.Join(cwd, "configs", "gsbench.cfg"),
	}
	if strings.TrimSpace(executable) != "" {
		executablePath, err := resolve(executable)
		if err != nil {
			return "", fmt.Errorf("resolve executable for config discovery: %w", err)
		}
		executableDir := filepath.Dir(executablePath)
		candidates = append(
			candidates,
			filepath.Join(executableDir, "gsbench.cfg"),
			filepath.Join(filepath.Dir(executableDir), "configs", "gsbench.cfg"),
		)
	}
	seen := make(map[string]struct{}, len(candidates))
	searched := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if _, duplicate := seen[candidate]; duplicate {
			continue
		}
		seen[candidate] = struct{}{}
		searched = append(searched, candidate)
		regular, err := regularConfig(candidate)
		if err != nil {
			return "", fmt.Errorf("inspect config candidate %q: %w", candidate, err)
		}
		if regular {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("gsbench config not found; searched %s", strings.Join(searched, ", "))
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
	case "scenarios":
		printScenarios(stdout)
		return 0
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "resolve config: current directory:", err)
		return 1
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "resolve config: executable:", err)
		return 1
	}
	options.ConfigPath, err = resolveConfigPath(
		options.ConfigPath,
		executable,
		cwd,
	)
	if err != nil {
		fmt.Fprintln(stderr, "resolve config:", err)
		return 1
	}
	return executeCommand(ctx, options, stdout, stderr)
}

func printUsage(w io.Writer) {
	_, _ = io.WriteString(w, `Usage:
	gsbench <init|run|status|stop|restore|cleanup|doctor|scenarios|version> [options]
  gsbench run [-s LIST] [-d DURATION]
  gsbench run SCENARIO[,SCENARIO...]
  gsbench run 101 --workers N --duration DURATION
  gsbench run 102 --workers N --duration DURATION
  gsbench run 103 --tp-workers N --ap-workers N --duration DURATION
  gsbench run 201 --workers N --work-mem VALUE --duration DURATION
  gsbench run 202 --workers N --work-mem VALUE --duration DURATION
  gsbench run 501 --sessions N --chain-depth N --duration DURATION
  gsbench run 502 --sessions N --duration DURATION
  gsbench run 503 --sessions N --duration DURATION
  gsbench restore [--run-id RUN_ID]
	gsbench cleanup [--data]
  gsbench init --size 100GB

Options:
  -c, --config PATH       configuration file (legacy-compatible override)
  -s, --scenario LIST    comma-separated three-digit scenario codes or canonical names
  -d, --duration VALUE   pressure duration, for example 30s or 5m
      --workers N        fixed workers for scenarios 101/102 or 201/202
      --tp-workers N     fixed TP workers for scenario 103 (use with --ap-workers)
      --ap-workers N     fixed AP workers for scenario 103 (use with --tp-workers)
      --work-mem VALUE   work_mem for scenarios 201/202: integer kB, MB, or GB (minimum 64kB)
      --sessions N       total holder plus waiter sessions for scenarios 501-503
      --chain-depth N    row wait chain depth for scenario 501 (1-5, default 1)
      --profile VALUE    data profile: quick or stress
      --dry-run          validate and show actions without workload mutation
      --run-id ID        select one run for status, stop, or restore; cannot combine with cleanup --data
      --data             also remove benchmark data during cleanup
      --size VALUE       init target data size, for example 100GB or 1.5TB (maximum 2TB)
      --allow-risk A|B|C explicitly authorize the maximum permitted scenario risk

Configuration:
  -c/--config > GSBENCH_CONFIG > ./gsbench.cfg > ./configs/gsbench.cfg
  > executable-dir/gsbench.cfg > executable-parent/configs/gsbench.cfg

Scenarios:
`)
	for _, definition := range implementedScenarioDefinitions() {
		_, _ = fmt.Fprintf(w, "  %03d=%s\n", definition.Code, definition.Name)
	}
	_, _ = io.WriteString(w, "  Run gsbench scenarios for implemented scenarios with risk and applicability.\n")
}

func printScenarios(w io.Writer) {
	_, _ = io.WriteString(w, "CODE  CATEGORY  NAME  RISK  APPLIES_TO\n")
	for _, definition := range implementedScenarioDefinitions() {
		appliesTo := make([]string, len(definition.AppliesTo))
		for i, class := range definition.AppliesTo {
			appliesTo[i] = string(class)
		}
		_, _ = fmt.Fprintf(w, "%03d  %d  %s  %s  %s\n", definition.Code, definition.Category, definition.Name, definition.Risk, strings.Join(appliesTo, ","))
	}
}
