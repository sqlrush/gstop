package gsbench

import (
	"fmt"
	"os"
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

var validScenarios = map[string]struct{}{
	"tp_cpu": {}, "ap_cpu": {}, "mixed_cpu": {},
	"connection_pool": {}, "thread_pool": {}, "dynamic_memory": {},
	"plan_regression": {}, "lock_storm": {}, "vacuum_pressure": {},
}

type Overrides struct {
	Scenarios []string
	Duration  time.Duration
	Profile   string
	DryRun    *bool
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
	Scenarios    []string
	Duration     time.Duration
	RampInterval time.Duration
	Profile      string
	DryRun       bool
}

type DataConfig struct {
	Schema             string
	MaxSizeGB          int
	MinFreeDiskPercent int
	ReuseExisting      bool
}

type SafetyConfig struct {
	CPUTargetPercent             int
	MaxConnections               int
	MaxWorkers                   int
	QueryTimeout                 time.Duration
	RestoreOnExit                bool
	AllowInstanceParameterChange bool
	AllowDatabaseRestart         bool
	RestartCommand               string
}

type BenchConfig struct {
	Path     string
	Database DatabaseConfig
	Run      RunConfig
	Data     DataConfig
	Safety   SafetyConfig
	Raw      *baseconfig.Config
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
	passwordEnv := raw.GetString("database.password_env", "GSBENCH_PASSWORD")
	cfg := BenchConfig{
		Path: path,
		Database: DatabaseConfig{
			Host:            raw.GetString("database.host", "127.0.0.1"),
			Port:            raw.GetInt("database.port", 5432),
			Database:        raw.GetString("database.database", "postgres"),
			User:            raw.GetString("database.user", "bench"),
			PasswordEnv:     passwordEnv,
			Password:        os.Getenv(passwordEnv),
			SSLMode:         raw.GetString("database.sslmode", "disable"),
			ApplicationName: raw.GetString("database.application_name", "gsbench"),
			ConnectTimeout:  connectTimeout,
		},
		Run: RunConfig{
			Scenarios:    configuredScenarios(raw),
			Duration:     duration,
			RampInterval: ramp,
			Profile:      strings.ToLower(raw.GetString("run.profile", "quick")),
			DryRun:       raw.GetBool("run.dry_run", false),
		},
		Data: DataConfig{
			Schema:             raw.GetString("data.schema", "gsbench"),
			MaxSizeGB:          raw.GetInt("data.max_size_gb", 5),
			MinFreeDiskPercent: raw.GetInt("data.min_free_disk_percent", 20),
			ReuseExisting:      raw.GetBool("data.reuse_existing", true),
		},
		Safety: SafetyConfig{
			CPUTargetPercent:             raw.GetInt("safety.cpu_target_percent", 95),
			MaxConnections:               raw.GetInt("safety.max_connections", 500),
			MaxWorkers:                   raw.GetInt("safety.max_workers", 256),
			QueryTimeout:                 queryTimeout,
			RestoreOnExit:                raw.GetBool("safety.restore_on_exit", true),
			AllowInstanceParameterChange: raw.GetBool("safety.allow_instance_parameter_change", true),
			AllowDatabaseRestart:         raw.GetBool("safety.allow_database_restart", false),
			RestartCommand:               raw.GetString("safety.restart_command", ""),
		},
		Raw: raw,
	}
	if !raw.Has("data.max_size_gb") && cfg.Run.Profile == "stress" {
		cfg.Data.MaxSizeGB = 20
	}
	if len(overrides.Scenarios) > 0 {
		cfg.Run.Scenarios = append([]string(nil), overrides.Scenarios...)
	}
	if overrides.Duration > 0 {
		cfg.Run.Duration = overrides.Duration
	}
	if overrides.Profile != "" {
		cfg.Run.Profile = strings.ToLower(overrides.Profile)
	}
	if overrides.DryRun != nil {
		cfg.Run.DryRun = *overrides.DryRun
	}
	if err := cfg.Validate(); err != nil {
		return BenchConfig{}, err
	}
	return cfg, nil
}

func configuredScenarios(raw *baseconfig.Config) []string {
	value := raw.Get("run.scenarios")
	if value == nil {
		value = "tp_cpu"
	}
	return normalizeScenarios(splitList(fmt.Sprint(value)))
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
	if len(c.Run.Scenarios) == 0 {
		return fmt.Errorf("at least one scenario is required")
	}
	seen := map[string]struct{}{}
	for _, name := range c.Run.Scenarios {
		if _, ok := validScenarios[name]; !ok {
			return fmt.Errorf("unknown scenario %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate scenario %q", name)
		}
		seen[name] = struct{}{}
	}
	if c.Safety.CPUTargetPercent < 1 || c.Safety.CPUTargetPercent > 100 {
		return fmt.Errorf("cpu_target_percent must be between 1 and 100")
	}
	if c.Safety.MaxWorkers <= 0 || c.Safety.MaxConnections <= 0 || c.Data.MaxSizeGB <= 0 {
		return fmt.Errorf("worker, connection, and data size limits must be positive")
	}
	if c.Data.MinFreeDiskPercent < 10 || c.Data.MinFreeDiskPercent > 90 {
		return fmt.Errorf("min_free_disk_percent must be between 10 and 90")
	}
	if c.Safety.AllowDatabaseRestart && strings.TrimSpace(c.Safety.RestartCommand) == "" {
		return fmt.Errorf("restart_command is required when allow_database_restart=true")
	}
	return nil
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
