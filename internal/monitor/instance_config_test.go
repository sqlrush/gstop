package monitor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstanceSessionQueryJoinsOnUniqueThreadIdentity(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "configs", "monitor", instanceConfig)
	const uniqueJoin = "ON A.pid = W.tid AND A.sessionid = W.sessionid"

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
	if !strings.Contains(sessionLine, uniqueJoin) {
		t.Fatalf("%s SN query must use unique thread identity %q; query: %s", path, uniqueJoin, sessionLine)
	}
}
