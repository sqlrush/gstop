package gsbench

import (
	"fmt"
	"strings"
	"sync"
	"unicode"
)

const advisoryValueMaxRunes = 256

type PrecheckWarning struct {
	ScenarioCode ScenarioCode
	Scenario     string
	Check        string
	Object       string
	Actual       string
	Expected     string
	Impact       string
}

func (w PrecheckWarning) LogLine() string {
	return fmt.Sprintf(
		"PRECHECK_WARN scenario=%03d name=%s check=%s object=%s actual=%s expected=%s impact=%s",
		w.ScenarioCode,
		advisoryLogValue(w.Scenario),
		advisoryLogValue(w.Check),
		advisoryLogValue(w.Object),
		advisoryLogValue(w.Actual),
		advisoryLogValue(w.Expected),
		advisoryLogValue(w.Impact),
	)
}

func (w PrecheckWarning) Evidence() Evidence {
	return Evidence{
		Metric:    "precheck_warning",
		Actual:    1,
		Available: true,
		Details: map[string]any{
			"check":    advisoryLogValue(w.Check),
			"object":   advisoryLogValue(w.Object),
			"actual":   advisoryLogValue(w.Actual),
			"expected": advisoryLogValue(w.Expected),
			"impact":   advisoryLogValue(w.Impact),
		},
	}
}

func advisoryLogValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "none"
	}
	if journalStringContainsCredentialMaterial(value) {
		return "redacted"
	}
	value = strings.Join(strings.FieldsFunc(value, func(character rune) bool {
		return unicode.IsControl(character) || unicode.IsSpace(character)
	}), "_")
	runes := []rune(value)
	if len(runes) > advisoryValueMaxRunes {
		value = string(runes[:advisoryValueMaxRunes-1]) + "…"
	}
	return value
}

type AdvisoryCollector struct {
	mu       sync.Mutex
	warnings []PrecheckWarning
}

func deprecatedConfigWarnings(cfg BenchConfig) []PrecheckWarning {
	values := []struct {
		object string
		actual any
	}{
		{"run.validation_enabled", cfg.Run.ValidationEnabled},
		{"safety.restore_on_exit", cfg.Safety.RestoreOnExit},
		{"safety.restore_original_role", cfg.Safety.RestoreOriginalRole},
		{"safety.max_workers", cfg.Safety.MaxWorkers},
		{"safety.max_connections", cfg.Safety.MaxConnections},
		{"safety.profile_cap_gb", cfg.Safety.ProfileCapGB},
		{"data.max_size_gb", cfg.Data.MaxSizeGB},
		{"data.min_free_disk_percent", cfg.Data.MinFreeDiskPercent},
	}
	warnings := make([]PrecheckWarning, 0, len(values))
	for _, value := range values {
		warnings = append(warnings, PrecheckWarning{
			Check:    "deprecated_config",
			Object:   value.object,
			Actual:   fmt.Sprint(value.actual),
			Expected: "advisory_only",
			Impact:   "accepted_but_not_enforced",
		})
	}
	return warnings
}

func logDeprecatedConfigWarnings(cfg BenchConfig, log *RunLog) {
	for _, warning := range deprecatedConfigWarnings(cfg) {
		log.Warn("%s", warning.LogLine())
	}
}

func runtimeWarn(runtime *Runtime, warning PrecheckWarning) {
	if runtime == nil {
		return
	}
	if runtime.ReportWarning != nil {
		runtime.ReportWarning(warning)
	}
	if runtime.Log != nil {
		runtime.Log.Warn("%s", warning.LogLine())
	}
}

func (c *AdvisoryCollector) Report(warning PrecheckWarning) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.warnings = append(c.warnings, warning)
	c.mu.Unlock()
}

func (c *AdvisoryCollector) Scenario(code ScenarioCode) []PrecheckWarning {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var warnings []PrecheckWarning
	for _, warning := range c.warnings {
		if warning.ScenarioCode == code {
			warnings = append(warnings, warning)
		}
	}
	return warnings
}
