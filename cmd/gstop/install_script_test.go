package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func projectFile(t *testing.T, parts ...string) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	return filepath.Join(append([]string{root}, parts...)...)
}

func TestInstallScriptValidatesV163SingleArchitecturePackage(t *testing.T) {
	data, err := os.ReadFile(projectFile(t, "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, required := range []string{
		`$PACKAGE_ROOT/bin/gstop`,
		`$PACKAGE_ROOT/scripts/run.sh`,
		`$PACKAGE_ROOT/configs/gstop.cfg`,
		`$PACKAGE_ROOT/VERSION`,
		`$PACKAGE_ROOT/SHA256SUMS`,
		`gstop v1.6.3`,
		`安装包版本/目录结构不匹配`,
		`sha256sum -c SHA256SUMS`,
		`shasum -a 256 -c SHA256SUMS`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("install.sh missing package-validation fragment %q", required)
		}
	}
	for _, obsolete := range []string{
		"binary not found next to this script",
		`linux-arm64/gstop`,
		`linux-x86_64/gstop`,
	} {
		if strings.Contains(script, obsolete) {
			t.Errorf("install.sh still contains obsolete layout fragment %q", obsolete)
		}
	}
}

func TestRunScriptUsesOnlyPackagedBinDirectory(t *testing.T) {
	data, err := os.ReadFile(projectFile(t, "scripts", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	if !strings.Contains(script, `$PACKAGE_ROOT/bin/gstop`) {
		t.Fatal("run.sh does not resolve bin/gstop")
	}
	for _, obsolete := range []string{`linux-arm64/gstop`, `linux-x86_64/gstop`} {
		if strings.Contains(script, obsolete) {
			t.Errorf("run.sh still contains unified-tree fallback %q", obsolete)
		}
	}
}

func TestInstallScriptRejectsIncompletePackageWithActionableError(t *testing.T) {
	root := t.TempDir()
	scripts := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(projectFile(t, "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	installer := filepath.Join(scripts, "install.sh")
	if err := os.WriteFile(installer, source, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", installer)
	cmd.Env = append(os.Environ(), "HOME="+filepath.Join(root, "home"))
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("incomplete package unexpectedly installed")
	}
	text := string(output)
	if !strings.Contains(text, "安装包版本/目录结构不匹配") ||
		!strings.Contains(text, filepath.Join(root, "bin", "gstop")) {
		t.Fatalf("non-actionable output:\n%s", text)
	}
}
