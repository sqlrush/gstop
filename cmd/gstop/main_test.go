package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gstop/internal/config"
	"gstop/internal/logging"
	"gstop/internal/oscmd"
)

func TestVersionRequested(t *testing.T) {
	for _, args := range [][]string{
		{"version"},
		{"-version"},
		{"--version"},
		{"-c", "/tmp/gstop.cfg", "--version"},
		{"--version", "-c=/tmp/gstop.cfg"},
	} {
		if !versionRequested(args) {
			t.Errorf("versionRequested(%q)=false", args)
		}
	}
	for _, args := range [][]string{nil, {"-d"}, {"version", "-d"}, {"-c", "--version"}} {
		if versionRequested(args) {
			t.Errorf("versionRequested(%q)=true", args)
		}
	}
}

func TestVersionIsV163(t *testing.T) {
	if version != "v1.6.3" {
		t.Fatalf("version=%q", version)
	}
}

func TestValidateDaemonInvocationRequiresExplicitCLIFlag(t *testing.T) {
	daemon := true
	tests := []struct {
		name    string
		cfg     *config.Config
		args    config.Args
		wantErr bool
	}{
		{
			name: "interactive config needs no daemon flag",
			cfg:  config.FromMap(map[string]any{"main": map[string]any{"daemon": false}}),
		},
		{
			name:    "config-only daemon is rejected",
			cfg:     config.FromMap(map[string]any{"main": map[string]any{"daemon": true}}),
			wantErr: true,
		},
		{
			name: "explicit daemon flag is accepted",
			cfg:  config.FromMap(map[string]any{"main": map[string]any{"daemon": true}}),
			args: config.Args{Daemon: &daemon},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDaemonInvocation(tt.cfg, tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateDaemonInvocation() error=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestDaemonUserLimitIgnoresNonGstopProcessesWithMatchingArguments(t *testing.T) {
	binDir := t.TempDir()
	psPath := filepath.Join(binDir, "ps")
	const fakePS = `#!/bin/sh
if [ "$*" = "aux" ]; then
	cat <<'EOF'
sqlrush 101 0.0 0.0 0 0 S 0:00 node --working-dir /Users/sqlrush/gstop
sqlrush 102 0.0 0.0 0 0 S 0:00 node --session-id deadbeef
sqlrush 103 0.0 0.0 0 0 S 0:00 /tmp/gstop -d
EOF
else
	cat <<'EOF'
101 S node /usr/bin/node --working-dir /Users/sqlrush/gstop
102 S node /usr/bin/node --session-id deadbeef
103 S gstop /tmp/gstop -d
EOF
fi
`
	if err := os.WriteFile(psPath, []byte(fakePS), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.FromMap(map[string]any{
		"main": map[string]any{"daemon": true},
	})
	runner := oscmd.New(logging.New("gstop-test", ""), time.Second)
	if !withinUserLimit(cfg, runner) {
		t.Fatal("non-gstop processes containing the workspace path or letter d counted as daemon instances")
	}
}

func TestDaemonUserLimitUsesExecutableNameRatherThanSpoofableArgv0(t *testing.T) {
	binDir := t.TempDir()
	psPath := filepath.Join(binDir, "ps")
	const fakePS = `#!/bin/sh
case "$*" in
	*ucomm*)
		cat <<'EOF'
201 S node /tmp/gstop -d
202 S python /tmp/gstop --daemon
EOF
		;;
	*)
		cat <<'EOF'
201 S /tmp/gstop -d
202 S /tmp/gstop --daemon
EOF
		;;
esac
`
	if err := os.WriteFile(psPath, []byte(fakePS), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.FromMap(map[string]any{
		"main": map[string]any{"daemon": true},
	})
	runner := oscmd.New(logging.New("gstop-test", ""), time.Second)
	if !withinUserLimit(cfg, runner) {
		t.Fatal("processes whose argv0 only looks like gstop counted as gstop executables")
	}
}

func TestUserLimitClassifiesExactGstopProcesses(t *testing.T) {
	tests := []struct {
		name      string
		psOutput  string
		daemon    bool
		maxUsers  int64
		wantAllow bool
	}{
		{
			name: "two daemon argument forms reach the daemon limit",
			psOutput: `201 S gstop /opt/gstop -d
202 S gstop /opt/gstop --daemon`,
			daemon:    true,
			wantAllow: false,
		},
		{
			name: "interactive gstop arguments do not masquerade as daemon flags",
			psOutput: `201 S gstop /opt/gstop -c daemon
202 S gstop /opt/gstop -u daemon`,
			daemon:    true,
			wantAllow: true,
		},
		{
			name: "zombie gstop is excluded",
			psOutput: `201 Z gstop /opt/gstop -d
202 S gstop /opt/gstop --daemon=true`,
			daemon:    true,
			wantAllow: true,
		},
		{
			name: "interactive limit counts exact gstop executables",
			psOutput: `201 S gstop /opt/gstop
202 S gstop /opt/gstop -c /tmp/gstop.cfg
203 S node /usr/bin/node /tmp/gstop`,
			maxUsers:  1,
			wantAllow: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := runnerWithPSOutput(t, tt.psOutput)
			cfg := config.FromMap(map[string]any{
				"main": map[string]any{
					"daemon":               tt.daemon,
					"max_concurrent_users": tt.maxUsers,
				},
			})
			if got := withinUserLimit(cfg, runner); got != tt.wantAllow {
				t.Fatalf("withinUserLimit()=%v, want %v", got, tt.wantAllow)
			}
		})
	}
}

func runnerWithPSOutput(t *testing.T, output string) *oscmd.Runner {
	t.Helper()
	binDir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncat <<'EOF'\n%s\nEOF\n", output)
	if err := os.WriteFile(filepath.Join(binDir, "ps"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return oscmd.New(logging.New("gstop-test", ""), time.Second)
}
