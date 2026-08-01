package monitor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstanceSessionQueryExcludesThreadPoolWorkerRows(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "configs", "monitor", instanceConfig)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var sessionLine string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "SN:") {
			sessionLine = line
			break
		}
	}
	if sessionLine == "" {
		t.Fatalf("%s has no SN query", path)
	}
	for _, want := range []string{
		"ON A.pid = W.tid AND A.sessionid = W.sessionid",
		"COALESCE(A.sessionid,0) <> 0 OR A.backend_start IS NOT NULL",
	} {
		if !strings.Contains(sessionLine, want) {
			t.Fatalf("%s SN query missing %q: %s", path, want, sessionLine)
		}
	}
}
