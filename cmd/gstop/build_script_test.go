package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildScriptUsesRunnablePackageLayout(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test source path")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "scripts", "build.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, required := range []string{
		`"$out/bin"`,
		`"$out/scripts"`,
		`-o "$out/bin/gstop"`,
		`-o "$out/bin/gsbench"`,
		`"$out/scripts/"`,
		`SHA256SUMS`,
		`gstop %s`,
		`scripts/run.sh scripts/install.sh`,
		`COPYFILE_DISABLE=1`,
		`--no-xattrs`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("build.sh missing runnable-package fragment %q", required)
		}
	}
	for _, obsolete := range []string{
		`-o "$out/gstop"`,
		`-o "$out/gsbench"`,
		`"$out/run.sh"`,
	} {
		if strings.Contains(script, obsolete) {
			t.Errorf("build.sh still contains flat-layout fragment %q", obsolete)
		}
	}
}
