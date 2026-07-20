package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseValue(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{`"rdsAdmin"`, "rdsAdmin"},
		{`'quoted'`, "quoted"},
		{`8000`, int64(8000)},
		{`0.3`, 0.3},
		{`true`, true},
		{`False`, false},
		{`logs`, "logs"},
		{`00:00,08:00,600`, "00:00,08:00,600"}, // not numeric, stays string
		{`.5`, ".5"},                           // leading dot -> string
		{`5.`, "5."},                           // trailing dot -> string
		{`-1`, "-1"},                           // Python isdigit is false for '-1'
	}
	for _, tc := range cases {
		if got := parseValue(tc.in); got != tc.want {
			t.Errorf("parseValue(%q) = %v (%T), want %v (%T)", tc.in, got, got, tc.want, tc.want)
		}
	}
}

const sampleCfg = `
[main]
interval = 3
log_interval = 0
user = "rdsAdmin"
password_free = true
port = 8000
database = "postgres"

[alarm]
%CPU = 80

[emergency.plan_change]
os_cpu_thresh = 60
sql_acs_ins_pct_thresh = 0.3

[emergency.slow_sql]
strategy0 = "00:00,08:00,600,180,300"
`

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gstop.cfg")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadBasic(t *testing.T) {
	c, err := Load(writeCfg(t, sampleCfg), Args{})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.GetInt("main.interval", -1); got != 3 {
		t.Errorf("main.interval = %d, want 3", got)
	}
	if got := c.GetString("main.user", ""); got != "rdsAdmin" {
		t.Errorf("main.user = %q, want rdsAdmin", got)
	}
	if got := c.GetBool("main.password_free", false); got != true {
		t.Errorf("main.password_free = %v, want true", got)
	}
	if got := c.GetInt("emergency.plan_change.os_cpu_thresh", -1); got != 60 {
		t.Errorf("nested os_cpu_thresh = %d, want 60", got)
	}
	if got := c.GetFloat("emergency.plan_change.sql_acs_ins_pct_thresh", -1); got != 0.3 {
		t.Errorf("nested float = %v, want 0.3", got)
	}
	// configparser lower-cases option keys: %CPU -> %cpu.
	if got := c.GetInt("alarm.%cpu", -1); got != 80 {
		t.Errorf("alarm.%%cpu = %d, want 80", got)
	}
	// strategy value keeps its embedded colons intact.
	if got := c.GetString("emergency.slow_sql.strategy0", ""); got != "00:00,08:00,600,180,300" {
		t.Errorf("strategy0 = %q", got)
	}
}

func TestArgsOverride(t *testing.T) {
	interval := 10
	user := "override"
	c, err := Load(writeCfg(t, sampleCfg), Args{Interval: &interval, User: &user})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.GetInt("main.interval", -1); got != 10 {
		t.Errorf("overridden interval = %d, want 10", got)
	}
	if got := c.GetString("main.user", ""); got != "override" {
		t.Errorf("overridden user = %q, want override", got)
	}
}

func TestPostProcessClampsInterval(t *testing.T) {
	// log_interval > 0 and smaller than interval clamps interval down.
	body := "[main]\ninterval = 9\nlog_interval = 4\n"
	c, err := Load(writeCfg(t, body), Args{})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.GetInt("main.interval", -1); got != 4 {
		t.Errorf("clamped interval = %d, want 4", got)
	}
}

func TestPostProcessKeepsIntervalWhenLogZero(t *testing.T) {
	body := "[main]\ninterval = 3\nlog_interval = 0\n"
	c, err := Load(writeCfg(t, body), Args{})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.GetInt("main.interval", -1); got != 3 {
		t.Errorf("interval = %d, want 3 (log_interval=0 disables clamp)", got)
	}
}

func TestWithIsImmutable(t *testing.T) {
	c, err := Load(writeCfg(t, sampleCfg), Args{})
	if err != nil {
		t.Fatal(err)
	}
	c2 := c.With("main.support_terminate", false)
	if c.Has("main.support_terminate") {
		t.Error("With mutated the receiver")
	}
	if c2.GetBool("main.support_terminate", true) != false {
		t.Error("With did not set the value on the returned config")
	}
	// unrelated keys survive on the derived config
	if c2.GetInt("main.interval", -1) != 3 {
		t.Error("With dropped unrelated keys")
	}
}

func TestMissingFileIsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.cfg"), Args{})
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if c.Has("main.interval") {
		t.Error("expected empty config for missing file")
	}
	if got := c.GetInt("main.interval", 3); got != 3 {
		t.Errorf("default fallback = %d, want 3", got)
	}
}
