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

func TestConfigLoadsDefaultsAndDurations(t *testing.T) {
	cfg, err := LoadConfig(writeTestConfig(t, minimalConfig()), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Run.Duration != 10*time.Minute {
		t.Fatalf("duration = %v", cfg.Run.Duration)
	}
	if cfg.Safety.CPUTargetPercent != 95 || cfg.Safety.MaxWorkers != 256 {
		t.Fatalf("safety defaults = %+v", cfg.Safety)
	}
	if got := cfg.Run.ScenarioCodes; !reflect.DeepEqual(got, []ScenarioCode{101}) {
		t.Fatalf("scenario codes = %v", got)
	}
	if cfg.Data.CapacityProvider != "auto" ||
		cfg.Data.PhysicalSizeProvider != "auto" {
		t.Fatalf("dataset providers=%+v", cfg.Data)
	}
}

func TestConfigParsesExplicitDatasetProviders(t *testing.T) {
	body := strings.Replace(
		minimalConfig(),
		"schema = gsbench",
		"schema = gsbench\ncapacity_provider = tablespace_quota\nphysical_size_provider = catalog",
		1,
	)
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Data.CapacityProvider != "tablespace_quota" ||
		cfg.Data.PhysicalSizeProvider != "catalog" {
		t.Fatalf("dataset providers=%+v", cfg.Data)
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

func TestSafetyConfigParsesFaultAuthorizationSettings(t *testing.T) {
	body := minimalConfig() + `
[safety]
restore_timeout = 7m
profile_cap_gb = 128
allow_admin_mutation = true
allow_infrastructure_fault = true
restore_original_role = true
`
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Safety.RestoreTimeout != 7*time.Minute || cfg.Safety.ProfileCapGB != 128 ||
		!cfg.Safety.AllowAdminMutation || !cfg.Safety.AllowInfrastructureFault || !cfg.Safety.RestoreOriginalRole {
		t.Fatalf("safety=%+v", cfg.Safety)
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
	body := strings.Replace(minimalConfig(), "scenarios = tp_cpu", "scenarios = 101,ap_cpu,501", 1)
	cfg, err := LoadConfig(writeTestConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Run.ScenarioCodes, []ScenarioCode{101, 102, 501}; !reflect.DeepEqual(got, want) {
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
		filepath.Base(filepath.Dir(first.FaultProvider.LedgerPath)) != "logs" {
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
