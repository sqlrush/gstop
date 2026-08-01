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
	CPUTargetPercent             int
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
	Path          string
	Database      DatabaseConfig
	Run           RunConfig
	Data          DataConfig
	Safety        SafetyConfig
	FaultProvider FaultProviderConfig
	Raw           *baseconfig.Config
}

func LoadConfig(path string, overrides Overrides) (BenchConfig, error) {
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
	cfg := BenchConfig{
		Path: path,
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
			CPUTargetPercent:             raw.GetInt("safety.cpu_target_percent", 95),
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
	if len(c.Run.ScenarioCodes) == 0 {
		return fmt.Errorf("at least one scenario is required")
	}
	catalog := DefaultScenarioCatalog()
	seen := map[ScenarioCode]struct{}{}
	for _, code := range c.Run.ScenarioCodes {
		definition, err := catalog.LookupCode(code)
		if err != nil {
			return err
		}
		if _, duplicate := seen[code]; duplicate {
			return fmt.Errorf("duplicate scenario %q", definition.Name)
		}
		seen[code] = struct{}{}
	}
	if c.Safety.CPUTargetPercent < 1 || c.Safety.CPUTargetPercent > 100 {
		return fmt.Errorf("cpu_target_percent must be between 1 and 100")
	}
	if c.Safety.MaxWorkers <= 0 || c.Safety.MaxConnections <= 0 || c.Data.MaxSizeGB <= 0 {
		return fmt.Errorf("worker, connection, and data size limits must be positive")
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
	identity, err := filepath.Abs(configPath)
	if err != nil {
		return "", fmt.Errorf(
			"fault_provider.ledger_path default cannot resolve config identity",
		)
	}
	if evaluated, evalErr := filepath.EvalSymlinks(identity); evalErr == nil {
		identity = evaluated
	}
	identity = filepath.Clean(identity)
	digest := sha256.Sum256([]byte(identity))
	workingDirectory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf(
			"fault_provider.ledger_path default cannot resolve logs directory",
		)
	}
	return filepath.Join(
		workingDirectory,
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
