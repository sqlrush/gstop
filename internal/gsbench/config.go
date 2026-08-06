package gsbench

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	baseconfig "gstop/internal/config"
)

var (
	identifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	passwordRE   = regexp.MustCompile(`(?i)password='(?:\\.|[^'])*'`)
)

type Overrides struct {
	ScenarioCodes []ScenarioCode
	Duration      time.Duration
	Workers       int
	TPWorkers     int
	APWorkers     int
	WorkMemKB     int64
	PoolPercent   int
	Sessions      int
	ChainDepth    int
	Profile       string
	DryRun        *bool
	DatasetBytes  int64
	DatasetSize   string
}

type DatabaseConfig struct {
	Host            string
	Port            int
	Database        string
	User            string
	PasswordEnv     string
	Password        string
	SSLMode         string
	ApplicationName string
	ConnectTimeout  time.Duration
}

type RunConfig struct {
	ScenarioCodes     []ScenarioCode
	Duration          time.Duration
	RampInterval      time.Duration
	Profile           string
	DryRun            bool
	ValidationEnabled bool
}

type FixedWorkerConfig struct {
	TPWorkers      int
	APWorkers      int
	MixedTPWorkers int
	MixedAPWorkers int
}

type MemoryWorkloadConfig struct {
	SortWorkers   int
	SortWorkMemKB int64
	HashWorkers   int
	HashWorkMemKB int64
}

type PoolTargetConfig struct {
	ConnectionPercent int
	ThreadPercent     int
}

type LockWorkloadConfig struct {
	RowChainSessions       int
	RowChainDepth          int
	TableExclusiveSessions int
	DDLWaitSessions        int
}

func (c LockWorkloadConfig) For(
	code ScenarioCode,
) (sessions, depth int, ok bool) {
	switch code {
	case 501:
		return c.RowChainSessions, c.RowChainDepth, true
	case 502:
		return c.TableExclusiveSessions, 1, true
	case 503:
		return c.DDLWaitSessions, 1, true
	default:
		return 0, 0, false
	}
}

type DataConfig struct {
	Schema                string
	MaxSizeGB             int
	TargetBytes           int64
	MinFreeDiskPercent    int
	ReuseExisting         bool
	RequestedSize         string
	CapacityProvider      string
	DataDirectoryHostRoot string
	PhysicalSizeProvider  string
}

type SafetyConfig struct {
	MaxConnections               int
	MaxWorkers                   int
	QueryTimeout                 time.Duration
	RestoreTimeout               time.Duration
	ProfileCapGB                 int
	RestoreOnExit                bool
	AllowAdminMutation           bool
	AllowInfrastructureFault     bool
	RestoreOriginalRole          bool
	AllowInstanceParameterChange bool
	AllowDatabaseRestart         bool
	RestartCommand               string
}

type FaultProviderConfig struct {
	Type       string
	LedgerPath string
}

type BenchConfig struct {
	Path            string
	ConfigDir       string
	Database        DatabaseConfig
	Run             RunConfig
	FixedWorkers    FixedWorkerConfig
	MemoryWorkloads MemoryWorkloadConfig
	LockWorkloads   LockWorkloadConfig
	PoolTargets     PoolTargetConfig
	Data            DataConfig
	Safety          SafetyConfig
	FaultProvider   FaultProviderConfig
	Raw             *baseconfig.Config
}

func LoadConfig(path string, overrides Overrides) (BenchConfig, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return BenchConfig{}, fmt.Errorf("config path is required")
	}
	absolutePath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return BenchConfig{}, fmt.Errorf("resolve config path %q: %w", path, err)
	}
	path = filepath.Clean(absolutePath)
	if _, err := os.Stat(path); err != nil {
		return BenchConfig{}, fmt.Errorf("open config %q: %w", path, err)
	}
	raw, err := baseconfig.Load(path, baseconfig.Args{})
	if err != nil {
		return BenchConfig{}, err
	}
	parseDuration := func(key, def string) (time.Duration, error) {
		value := raw.GetString(key, def)
		d, err := time.ParseDuration(value)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", key, err)
		}
		return d, nil
	}
	connectTimeout, err := parseDuration("database.connect_timeout", "5s")
	if err != nil {
		return BenchConfig{}, err
	}
	duration, err := parseDuration("run.duration", "10m")
	if err != nil {
		return BenchConfig{}, err
	}
	ramp, err := parseDuration("run.ramp_interval", "2s")
	if err != nil {
		return BenchConfig{}, err
	}
	queryTimeout, err := parseDuration("safety.query_timeout", "30s")
	if err != nil {
		return BenchConfig{}, err
	}
	restoreTimeout, err := parseDuration("safety.restore_timeout", "10m")
	if err != nil {
		return BenchConfig{}, err
	}
	passwordEnv := raw.GetString("database.password_env", "GSBENCH_PASSWORD")
	password, err := loadDatabasePassword(raw, path, passwordEnv)
	if err != nil {
		return BenchConfig{}, err
	}
	configuredDefinitions, err := resolveScenarioInputs(configuredScenarios(raw))
	if err != nil {
		return BenchConfig{}, err
	}
	faultProvider, err := resolveFaultProviderConfig(raw, path)
	if err != nil {
		return BenchConfig{}, err
	}
	parseWorkMem := func(key string) (int64, error) {
		value := "256MB"
		if configured := raw.Get(key); configured != nil {
			value = fmt.Sprint(configured)
		}
		workMemKB, err := ParseWorkMemKB(value)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", key, err)
		}
		return workMemKB, nil
	}
	sortWorkMemKB, err := parseWorkMem("scenario.memory_workmem_sort.work_mem")
	if err != nil {
		return BenchConfig{}, err
	}
	hashWorkMemKB, err := parseWorkMem("scenario.memory_workmem_hash.work_mem")
	if err != nil {
		return BenchConfig{}, err
	}
	cfg := BenchConfig{
		Path:      path,
		ConfigDir: filepath.Dir(path),
		Database: DatabaseConfig{
			Host:            raw.GetString("database.host", "127.0.0.1"),
			Port:            raw.GetInt("database.port", 5432),
			Database:        raw.GetString("database.database", "postgres"),
			User:            raw.GetString("database.user", "bench"),
			PasswordEnv:     passwordEnv,
			Password:        password,
			SSLMode:         raw.GetString("database.sslmode", "disable"),
			ApplicationName: raw.GetString("database.application_name", "gsbench"),
			ConnectTimeout:  connectTimeout,
		},
		Run: RunConfig{
			ScenarioCodes:     scenarioCodes(configuredDefinitions),
			Duration:          duration,
			RampInterval:      ramp,
			Profile:           strings.ToLower(raw.GetString("run.profile", "quick")),
			DryRun:            raw.GetBool("run.dry_run", false),
			ValidationEnabled: raw.GetBool("run.validation_enabled", false),
		},
		FixedWorkers: FixedWorkerConfig{
			TPWorkers:      raw.GetInt("scenario.tp_cpu.workers", 1),
			APWorkers:      raw.GetInt("scenario.ap_cpu.workers", 1),
			MixedTPWorkers: raw.GetInt("scenario.mixed_cpu.tp_workers", 1),
			MixedAPWorkers: raw.GetInt("scenario.mixed_cpu.ap_workers", 1),
		},
		MemoryWorkloads: MemoryWorkloadConfig{
			SortWorkers: raw.GetInt(
				"scenario.memory_workmem_sort.workers", 1,
			),
			SortWorkMemKB: sortWorkMemKB,
			HashWorkers: raw.GetInt(
				"scenario.memory_workmem_hash.workers", 1,
			),
			HashWorkMemKB: hashWorkMemKB,
		},
		LockWorkloads: LockWorkloadConfig{
			RowChainSessions: raw.GetInt(
				"scenario.lock_row_chain.sessions", 2,
			),
			RowChainDepth: raw.GetInt(
				"scenario.lock_row_chain.chain_depth", 1,
			),
			TableExclusiveSessions: raw.GetInt(
				"scenario.lock_table_exclusive.sessions", 2,
			),
			DDLWaitSessions: raw.GetInt(
				"scenario.lock_ddl_wait.sessions", 2,
			),
		},
		PoolTargets: PoolTargetConfig{
			ConnectionPercent: raw.GetInt(
				"scenario.connection_pool.target_percent", 95,
			),
			ThreadPercent: raw.GetInt(
				"scenario.thread_pool.target_percent", 95,
			),
		},
		Data: DataConfig{
			Schema:             raw.GetString("data.schema", "gsbench"),
			MaxSizeGB:          raw.GetInt("data.max_size_gb", 5),
			MinFreeDiskPercent: raw.GetInt("data.min_free_disk_percent", 20),
			ReuseExisting:      raw.GetBool("data.reuse_existing", true),
			CapacityProvider: strings.ToLower(
				raw.GetString("data.capacity_provider", "auto"),
			),
			DataDirectoryHostRoot: strings.TrimSpace(
				raw.GetString("data.data_directory_host_root", ""),
			),
			PhysicalSizeProvider: strings.ToLower(
				raw.GetString("data.physical_size_provider", "auto"),
			),
		},
		Safety: SafetyConfig{
			MaxConnections:               raw.GetInt("safety.max_connections", 500),
			MaxWorkers:                   raw.GetInt("safety.max_workers", 256),
			QueryTimeout:                 queryTimeout,
			RestoreTimeout:               restoreTimeout,
			ProfileCapGB:                 raw.GetInt("safety.profile_cap_gb", 256),
			RestoreOnExit:                raw.GetBool("safety.restore_on_exit", true),
			AllowAdminMutation:           raw.GetBool("safety.allow_admin_mutation", false),
			AllowInfrastructureFault:     raw.GetBool("safety.allow_infrastructure_fault", false),
			RestoreOriginalRole:          raw.GetBool("safety.restore_original_role", false),
			AllowInstanceParameterChange: raw.GetBool("safety.allow_instance_parameter_change", true),
			AllowDatabaseRestart:         raw.GetBool("safety.allow_database_restart", false),
			RestartCommand:               raw.GetString("safety.restart_command", ""),
		},
		FaultProvider: faultProvider,
		Raw:           raw,
	}
	if len(overrides.ScenarioCodes) > 0 {
		cfg.Run.ScenarioCodes = append(
			[]ScenarioCode(nil),
			overrides.ScenarioCodes...,
		)
	}
	if err := applyPoolTargetOverride(&cfg, overrides); err != nil {
		return BenchConfig{}, err
	}
	if err := applyFixedWorkerOverrides(&cfg, overrides); err != nil {
		return BenchConfig{}, err
	}
	if err := applyLockOverrides(&cfg, overrides); err != nil {
		return BenchConfig{}, err
	}
	if overrides.Duration > 0 {
		cfg.Run.Duration = overrides.Duration
	}
	if overrides.Profile != "" {
		cfg.Run.Profile = strings.ToLower(overrides.Profile)
	}
	if !raw.Has("data.max_size_gb") {
		cfg.Data.MaxSizeGB = 5
		if cfg.Run.Profile == "stress" {
			cfg.Data.MaxSizeGB = 20
		}
	}
	if overrides.DryRun != nil {
		cfg.Run.DryRun = *overrides.DryRun
	}
	cfg.Data.TargetBytes = int64(cfg.Data.MaxSizeGB) << 30
	if overrides.DatasetBytes > 0 {
		cfg.Data.TargetBytes = overrides.DatasetBytes
		cfg.Data.RequestedSize = strings.TrimSpace(overrides.DatasetSize)
	}
	if err := cfg.Validate(); err != nil {
		return BenchConfig{}, err
	}
	return cfg, nil
}

func applyPoolTargetOverride(
	cfg *BenchConfig,
	overrides Overrides,
) error {
	if overrides.PoolPercent == 0 {
		return nil
	}
	if cfg == nil {
		return fmt.Errorf("benchmark configuration is required")
	}
	if err := validatePoolPercentOverride(
		cfg.Run.ScenarioCodes,
		overrides.PoolPercent,
	); err != nil {
		return err
	}
	for _, code := range cfg.Run.ScenarioCodes {
		switch code {
		case 401:
			cfg.PoolTargets.ConnectionPercent = overrides.PoolPercent
		case 402:
			cfg.PoolTargets.ThreadPercent = overrides.PoolPercent
		}
	}
	return nil
}

func loadDatabasePassword(raw *baseconfig.Config, configPath, passwordEnv string) (string, error) {
	passwordConfig := strings.TrimSpace(raw.GetString("database.password_config", ""))
	if passwordConfig == "" {
		return os.Getenv(passwordEnv), nil
	}
	if !filepath.IsAbs(passwordConfig) {
		passwordConfig = filepath.Join(filepath.Dir(configPath), passwordConfig)
	}
	secretConfig, err := baseconfig.Load(filepath.Clean(passwordConfig), baseconfig.Args{})
	if err != nil {
		return "", fmt.Errorf("load database.password_config %q: %w", passwordConfig, err)
	}
	password := secretConfig.GetString("main.db_password", "")
	if password == "" {
		return "", fmt.Errorf("main.db_password is empty in database.password_config %q", passwordConfig)
	}
	return password, nil
}

func configuredScenarios(raw *baseconfig.Config) []string {
	value := raw.Get("run.scenarios")
	if value == nil {
		value = "tp_cpu"
	}
	names := splitList(fmt.Sprint(value))
	return names
}

func validateFixedWorkerOverrideCompatibility(
	codes []ScenarioCode,
	workers, tpWorkers, apWorkers int,
	workMemKB int64,
) error {
	if workers == 0 && tpWorkers == 0 && apWorkers == 0 && workMemKB == 0 {
		return nil
	}
	if workers < 0 || tpWorkers < 0 || apWorkers < 0 {
		return fmt.Errorf("worker counts must be positive")
	}
	if workMemKB < 0 || (workMemKB > 0 && workMemKB < minWorkMemKB) {
		return fmt.Errorf("work_mem must be at least %dkB", minWorkMemKB)
	}
	if (tpWorkers == 0) != (apWorkers == 0) {
		return fmt.Errorf("--tp-workers and --ap-workers must be provided together")
	}
	if workers > 0 && (tpWorkers > 0 || apWorkers > 0) {
		return fmt.Errorf("--workers cannot be combined with --tp-workers or --ap-workers")
	}
	if workMemKB > 0 && (tpWorkers > 0 || apWorkers > 0) {
		return fmt.Errorf("--work-mem cannot be combined with --tp-workers or --ap-workers")
	}
	if tpWorkers > 0 || apWorkers > 0 {
		if len(codes) != 1 || codes[0] != 103 {
			return fmt.Errorf("--tp-workers and --ap-workers require only scenario 103")
		}
		return nil
	}
	if len(codes) < 1 || len(codes) > 2 {
		return fmt.Errorf(
			"--workers/--work-mem require only scenario family 101/102 or 201/202",
		)
	}
	seen := make(map[ScenarioCode]struct{}, len(codes))
	cpuFamily := true
	memoryFamily := true
	for _, code := range codes {
		if _, duplicate := seen[code]; duplicate {
			return fmt.Errorf("worker overrides require unique scenarios")
		}
		seen[code] = struct{}{}
		if code != 101 && code != 102 {
			cpuFamily = false
		}
		if code != 201 && code != 202 {
			memoryFamily = false
		}
	}
	if workMemKB > 0 && !memoryFamily {
		return fmt.Errorf("--work-mem requires exactly one scenario: 201 or 202")
	}
	if workers > 0 && !cpuFamily && !memoryFamily {
		return fmt.Errorf(
			"--workers requires only scenario family 101/102 or 201/202",
		)
	}
	if memoryFamily && len(codes) != 1 {
		return fmt.Errorf(
			"memory worker/work_mem overrides require exactly one scenario: 201 or 202",
		)
	}
	return nil
}

func validateLockOverrideCompatibility(
	codes []ScenarioCode,
	sessions, chainDepth int,
) error {
	if sessions == 0 && chainDepth == 0 {
		return nil
	}
	if sessions < 0 || chainDepth < 0 {
		return fmt.Errorf("lock session counts and chain depth must be positive")
	}
	has501 := false
	for _, code := range codes {
		if code < 501 || code > 503 {
			return fmt.Errorf(
				"--sessions/--chain-depth require only scenarios 501-503",
			)
		}
		if code == 501 {
			has501 = true
		}
	}
	if sessions > 0 && sessions < 2 {
		return fmt.Errorf("--sessions must be at least 2")
	}
	if chainDepth > 5 {
		return fmt.Errorf("--chain-depth must be between 1 and 5")
	}
	if chainDepth > 0 && !has501 {
		return fmt.Errorf("--chain-depth requires scenario 501")
	}
	if sessions > 0 && chainDepth > 0 && sessions < chainDepth+1 {
		return fmt.Errorf(
			"scenario 501 requires sessions >= chain_depth + 1",
		)
	}
	return nil
}

func applyFixedWorkerOverrides(cfg *BenchConfig, overrides Overrides) error {
	if cfg == nil {
		return fmt.Errorf("benchmark config is required")
	}
	if err := validateFixedWorkerOverrideCompatibility(
		cfg.Run.ScenarioCodes,
		overrides.Workers,
		overrides.TPWorkers,
		overrides.APWorkers,
		overrides.WorkMemKB,
	); err != nil {
		return err
	}
	if overrides.Workers > 0 {
		for _, code := range cfg.Run.ScenarioCodes {
			switch code {
			case 101:
				cfg.FixedWorkers.TPWorkers = overrides.Workers
			case 102:
				cfg.FixedWorkers.APWorkers = overrides.Workers
			case 201:
				cfg.MemoryWorkloads.SortWorkers = overrides.Workers
			case 202:
				cfg.MemoryWorkloads.HashWorkers = overrides.Workers
			}
		}
	}
	if overrides.TPWorkers > 0 {
		cfg.FixedWorkers.MixedTPWorkers = overrides.TPWorkers
		cfg.FixedWorkers.MixedAPWorkers = overrides.APWorkers
	}
	if overrides.WorkMemKB > 0 {
		for _, code := range cfg.Run.ScenarioCodes {
			switch code {
			case 201:
				cfg.MemoryWorkloads.SortWorkMemKB = overrides.WorkMemKB
			case 202:
				cfg.MemoryWorkloads.HashWorkMemKB = overrides.WorkMemKB
			}
		}
	}
	return nil
}

func applyLockOverrides(cfg *BenchConfig, overrides Overrides) error {
	if cfg == nil {
		return fmt.Errorf("benchmark config is required")
	}
	if err := validateLockOverrideCompatibility(
		cfg.Run.ScenarioCodes,
		overrides.Sessions,
		overrides.ChainDepth,
	); err != nil {
		return err
	}
	if overrides.Sessions > 0 {
		for _, code := range cfg.Run.ScenarioCodes {
			switch code {
			case 501:
				cfg.LockWorkloads.RowChainSessions = overrides.Sessions
			case 502:
				cfg.LockWorkloads.TableExclusiveSessions = overrides.Sessions
			case 503:
				cfg.LockWorkloads.DDLWaitSessions = overrides.Sessions
			}
		}
	}
	if overrides.ChainDepth > 0 {
		cfg.LockWorkloads.RowChainDepth = overrides.ChainDepth
	}
	return nil
}

func (c BenchConfig) Validate() error {
	if !identifierRE.MatchString(c.Data.Schema) {
		return fmt.Errorf("data.schema %q is not a safe SQL identifier", c.Data.Schema)
	}
	if c.Database.Port < 1 || c.Database.Port > 65535 {
		return fmt.Errorf("database.port must be between 1 and 65535")
	}
	if c.Database.User == "" || c.Database.Database == "" {
		return fmt.Errorf("database.user and database.database are required")
	}
	if c.Run.Duration <= 0 || c.Run.RampInterval <= 0 || c.Safety.QueryTimeout <= 0 {
		return fmt.Errorf("duration, ramp_interval, and query_timeout must be positive")
	}
	if c.Run.Profile != "quick" && c.Run.Profile != "stress" {
		return fmt.Errorf("run.profile must be quick or stress")
	}
	if c.PoolTargets.ConnectionPercent < 1 ||
		c.PoolTargets.ConnectionPercent > 100 ||
		c.PoolTargets.ThreadPercent < 1 ||
		c.PoolTargets.ThreadPercent > 100 {
		return fmt.Errorf("pool target percentages must be between 1 and 100")
	}
	if len(c.Run.ScenarioCodes) == 0 {
		return fmt.Errorf("at least one scenario is required")
	}
	catalog := DefaultScenarioCatalog()
	seen := map[ScenarioCode]struct{}{}
	planChangeCount := 0
	fixedWorkerSelected := false
	unbudgetedSelected := false
	for _, code := range c.Run.ScenarioCodes {
		definition, err := catalog.LookupCode(code)
		if err != nil {
			return err
		}
		if _, duplicate := seen[code]; duplicate {
			return fmt.Errorf("duplicate scenario %q", definition.Name)
		}
		seen[code] = struct{}{}
		if definition.Category == CategoryPlan && code >= 601 && code <= 606 {
			planChangeCount++
		}
		switch code {
		case 101, 102, 103, 201, 202:
			fixedWorkerSelected = true
		case 501, 502, 503:
		default:
			unbudgetedSelected = true
		}
	}
	if fixedWorkerSelected && unbudgetedSelected {
		return fmt.Errorf(
			"fixed-worker scenarios 101-103 and 201-202 cannot be combined with other scenarios because their total concurrency cannot be bounded safely",
		)
	}
	if planChangeCount > 1 {
		return fmt.Errorf(
			"plan-change scenarios 601-606 must be run individually and serially",
		)
	}
	if c.Safety.MaxWorkers <= 0 || c.Safety.MaxConnections <= 0 || c.Data.MaxSizeGB <= 0 {
		return fmt.Errorf("worker, connection, and data size limits must be positive")
	}
	if c.FixedWorkers.TPWorkers <= 0 ||
		c.FixedWorkers.APWorkers <= 0 ||
		c.FixedWorkers.MixedTPWorkers <= 0 ||
		c.FixedWorkers.MixedAPWorkers <= 0 {
		return fmt.Errorf("fixed workers must be positive")
	}
	if c.MemoryWorkloads.SortWorkers <= 0 ||
		c.MemoryWorkloads.HashWorkers <= 0 {
		return fmt.Errorf("memory workload workers must be positive")
	}
	if c.MemoryWorkloads.SortWorkMemKB < minWorkMemKB ||
		c.MemoryWorkloads.HashWorkMemKB < minWorkMemKB {
		return fmt.Errorf("memory workload work_mem must be at least %dkB", minWorkMemKB)
	}
	if c.LockWorkloads.RowChainSessions < 2 ||
		c.LockWorkloads.TableExclusiveSessions < 2 ||
		c.LockWorkloads.DDLWaitSessions < 2 {
		return fmt.Errorf("lock workload sessions must be at least 2")
	}
	if c.LockWorkloads.RowChainDepth < 1 ||
		c.LockWorkloads.RowChainDepth > 5 {
		return fmt.Errorf("lock row chain depth must be between 1 and 5")
	}
	if c.LockWorkloads.RowChainSessions <
		c.LockWorkloads.RowChainDepth+1 {
		return fmt.Errorf(
			"scenario 501 requires sessions >= chain_depth + 1",
		)
	}
	if requiredRows := rowChainRequiredRows(
		c.LockWorkloads.RowChainSessions,
		c.LockWorkloads.RowChainDepth,
	); requiredRows > lockTargetRows {
		return fmt.Errorf(
			"scenario 501 requires %d unique lock_targets rows but only 10,000 are available",
			requiredRows,
		)
	}
	selectedFixedWorkers := 0
	selectedConnections := 0
	addFixedWorkers := func(workers int) error {
		if workers > c.Safety.MaxWorkers-selectedFixedWorkers {
			return fmt.Errorf(
				"selected fixed workers exceed safety.max_workers %d",
				c.Safety.MaxWorkers,
			)
		}
		if workers > c.Safety.MaxConnections-selectedConnections {
			return fmt.Errorf(
				"selected fixed workers exceed safety.max_connections %d",
				c.Safety.MaxConnections,
			)
		}
		selectedFixedWorkers += workers
		selectedConnections += workers
		return nil
	}
	addLockSessions := func(sessions int) error {
		if sessions > c.Safety.MaxConnections-selectedConnections {
			return fmt.Errorf(
				"selected workload sessions exceed safety.max_connections %d",
				c.Safety.MaxConnections,
			)
		}
		selectedConnections += sessions
		return nil
	}
	for _, code := range c.Run.ScenarioCodes {
		switch code {
		case 101:
			if err := addFixedWorkers(c.FixedWorkers.TPWorkers); err != nil {
				return err
			}
		case 102:
			if err := addFixedWorkers(c.FixedWorkers.APWorkers); err != nil {
				return err
			}
		case 103:
			if err := addFixedWorkers(c.FixedWorkers.MixedTPWorkers); err != nil {
				return err
			}
			if err := addFixedWorkers(c.FixedWorkers.MixedAPWorkers); err != nil {
				return err
			}
		case 201:
			if err := addFixedWorkers(c.MemoryWorkloads.SortWorkers); err != nil {
				return err
			}
		case 202:
			if err := addFixedWorkers(c.MemoryWorkloads.HashWorkers); err != nil {
				return err
			}
		case 501:
			if err := addLockSessions(c.LockWorkloads.RowChainSessions); err != nil {
				return err
			}
		case 502:
			if err := addLockSessions(c.LockWorkloads.TableExclusiveSessions); err != nil {
				return err
			}
		case 503:
			if err := addLockSessions(c.LockWorkloads.DDLWaitSessions); err != nil {
				return err
			}
		}
	}
	if c.Safety.ProfileCapGB < 1 || c.Safety.ProfileCapGB > 2048 {
		return fmt.Errorf("safety.profile_cap_gb must be between 1 and 2048")
	}
	if !c.Safety.RestoreOnExit {
		return fmt.Errorf("safety.restore_on_exit=false is unsupported; restoration is mandatory")
	}
	if c.Safety.RestoreOriginalRole {
		return fmt.Errorf("safety.restore_original_role=true is unsupported by the available provider")
	}
	if c.Data.MaxSizeGB > 2048 || c.Data.TargetBytes > maxDatasetBytes {
		return fmt.Errorf("data size limit must not exceed 2TB")
	}
	if c.Data.MinFreeDiskPercent < 10 || c.Data.MinFreeDiskPercent > 90 {
		return fmt.Errorf("min_free_disk_percent must be between 10 and 90")
	}
	if !stringInSet(
		defaultDatasetProvider(c.Data.CapacityProvider),
		"auto", "local_data_directory", "tablespace_quota",
	) {
		return fmt.Errorf(
			"data.capacity_provider must be auto, local_data_directory, or tablespace_quota",
		)
	}
	if c.Data.DataDirectoryHostRoot != "" &&
		!filepath.IsAbs(c.Data.DataDirectoryHostRoot) {
		return fmt.Errorf(
			"data.data_directory_host_root must be an absolute path",
		)
	}
	if !stringInSet(
		defaultDatasetProvider(c.Data.PhysicalSizeProvider),
		"auto", "catalog",
	) {
		return fmt.Errorf(
			"data.physical_size_provider must be auto or catalog",
		)
	}
	if c.Safety.AllowDatabaseRestart && strings.TrimSpace(c.Safety.RestartCommand) == "" {
		return fmt.Errorf("restart_command is required when allow_database_restart=true")
	}
	if err := validateFaultProviderConfig(c.FaultProvider); err != nil {
		return err
	}
	return nil
}

func resolveFaultProviderConfig(
	raw *baseconfig.Config,
	configPath string,
) (FaultProviderConfig, error) {
	config := FaultProviderConfig{
		Type: defaultFaultProviderType(
			raw.GetString("fault_provider.type", "none"),
		),
	}
	configuredPath := strings.TrimSpace(
		raw.GetString("fault_provider.ledger_path", ""),
	)
	if configuredPath == "" {
		path, err := defaultRecoveryLedgerPath(configPath)
		if err != nil {
			return FaultProviderConfig{}, err
		}
		config.LedgerPath = path
	} else {
		if !filepath.IsAbs(configuredPath) {
			configAbsolute, err := filepath.Abs(configPath)
			if err != nil {
				return FaultProviderConfig{}, fmt.Errorf(
					"fault_provider.ledger_path cannot be resolved",
				)
			}
			configuredPath = filepath.Join(
				filepath.Dir(configAbsolute),
				configuredPath,
			)
		}
		absolute, err := filepath.Abs(filepath.Clean(configuredPath))
		if err != nil {
			return FaultProviderConfig{}, fmt.Errorf(
				"fault_provider.ledger_path cannot be resolved",
			)
		}
		config.LedgerPath = absolute
	}
	if err := validateFaultProviderConfig(config); err != nil {
		return FaultProviderConfig{}, err
	}
	return config, nil
}

func defaultRecoveryLedgerPath(configPath string) (string, error) {
	configPath, err := filepath.Abs(filepath.Clean(configPath))
	if err != nil {
		return "", fmt.Errorf(
			"fault_provider.ledger_path default cannot resolve config identity",
		)
	}
	identity := configPath
	if evaluated, evalErr := filepath.EvalSymlinks(identity); evalErr == nil {
		identity = evaluated
	}
	identity = filepath.Clean(identity)
	digest := sha256.Sum256([]byte(identity))
	return filepath.Join(
		filepath.Dir(configPath),
		"logs",
		fmt.Sprintf("gsbench_%x_recovery.json", digest[:8]),
	), nil
}

func validateFaultProviderConfig(config FaultProviderConfig) error {
	providerType := defaultFaultProviderType(config.Type)
	if !supportedFaultProviderType(providerType) {
		return fmt.Errorf(
			"fault_provider.type must be none, local, ssh, or gaussdb_api",
		)
	}
	path, err := safeRecoveryLedgerPath(config.LedgerPath)
	if err != nil {
		return fmt.Errorf(
			"fault_provider.ledger_path must be a safe JSON file path",
		)
	}
	if err := rejectUnsafeLedgerTarget(path); err != nil {
		return fmt.Errorf(
			"fault_provider.ledger_path must name a non-symlink regular file: %w",
			err,
		)
	}
	return nil
}

func defaultDatasetProvider(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "auto"
	}
	return value
}

func stringInSet(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func scenarioCodes(definitions []ScenarioDefinition) []ScenarioCode {
	codes := make([]ScenarioCode, len(definitions))
	for i, definition := range definitions {
		codes[i] = definition.Code
	}
	return codes
}

func (c BenchConfig) DSN(database, appName string) string {
	if database == "" {
		database = c.Database.Database
	}
	if appName == "" {
		appName = c.Database.ApplicationName
	}
	parts := []string{
		dsnKV("host", c.Database.Host),
		dsnKV("port", strconv.Itoa(c.Database.Port)),
		dsnKV("dbname", database),
		dsnKV("user", c.Database.User),
		dsnKV("sslmode", c.Database.SSLMode),
		dsnKV("connect_timeout", strconv.Itoa(max(1, int(c.Database.ConnectTimeout/time.Second)))),
		dsnKV("application_name", appName),
	}
	if c.Database.Password != "" {
		parts = append(parts, dsnKV("password", c.Database.Password))
	}
	return strings.Join(parts, " ")
}

func (c BenchConfig) Redacted() string {
	return RedactDSN(c.DSN(c.Database.Database, c.Database.ApplicationName))
}

func RedactDSN(dsn string) string {
	return passwordRE.ReplaceAllString(dsn, "password='<redacted>'")
}

func dsnKV(key, value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `'`, `\'`)
	return key + "='" + escaped + "'"
}

func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
