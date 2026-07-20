package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// loadFile parses an INI file into a nested tree. Section names containing dots
// are expanded into nested maps (e.g. [emergency.plan_change] -> emergency ->
// plan_change). Option keys are lower-cased to match Python's configparser
// (optionxform = str.lower); section names are preserved verbatim.
//
// A missing file returns an empty tree and no error, matching the original
// tool, which logs and continues on defaults. Structural errors (a key line
// outside any section, an unterminated section header) are reported.
func loadFile(path string) (map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("open config %q: %w", path, err)
	}
	defer file.Close()

	root := map[string]any{}
	var section string
	haveSection := false

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("config %q line %d: malformed section header %q", path, lineNo, line)
			}
			section = strings.TrimSpace(line[1 : len(line)-1])
			if section == "" {
				return nil, fmt.Errorf("config %q line %d: empty section header", path, lineNo)
			}
			haveSection = true
			continue
		}

		if !haveSection {
			return nil, fmt.Errorf("config %q line %d: key %q outside any section", path, lineNo, line)
		}

		key, rawValue, ok := splitOption(line)
		if !ok {
			return nil, fmt.Errorf("config %q line %d: not a key=value line: %q", path, lineNo, line)
		}
		root = setPath(root, section+"."+strings.ToLower(key), parseValue(rawValue))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	return root, nil
}

// splitOption splits a line on the first '=' or ':' delimiter, whichever comes
// first, matching configparser's default delimiters. The key and value are
// trimmed of surrounding whitespace.
func splitOption(line string) (key, value string, ok bool) {
	eq := strings.IndexByte(line, '=')
	colon := strings.IndexByte(line, ':')
	idx := eq
	if idx < 0 || (colon >= 0 && colon < idx) {
		idx = colon
	}
	if idx <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]), true
}
