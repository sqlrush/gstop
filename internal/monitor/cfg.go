package monitor

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cfgPath resolves a monitor config file (e.g. "db.cfg") under the configured
// directory, matching the layout the original tool shipped (monitor/*.cfg).
func cfgPath(deps Deps, name string) string {
	return filepath.Join(deps.ConfigDir, "monitor", name)
}

// readCfgLines returns the trimmed, colon-bearing lines of a monitor config
// file, mirroring the Python parse loops that skip lines without a ':'.
func readCfgLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open monitor config %q: %w", path, err)
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.Contains(line, ":") {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read monitor config %q: %w", path, err)
	}
	return lines, nil
}

// atoiOrZero parses an integer field, returning 0 for an empty or invalid value,
// matching the Python `int(x) if x != ” else 0` idiom used across the parsers.
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
