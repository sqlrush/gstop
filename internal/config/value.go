package config

import (
	"strconv"
	"strings"
)

// parseValue mirrors the Python Config._parse_value semantics:
//   - a value wrapped in matching single or double quotes -> the inner string
//   - a run of ASCII digits -> int64
//   - a decimal with a single dot (not leading/trailing) -> float64
//   - "true"/"false" (case-insensitive) -> bool
//   - anything else -> the raw string
//
// It returns a newly derived value and never mutates its input.
func parseValue(raw string) any {
	value := strings.TrimSpace(raw)

	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return value[1 : len(value)-1]
		}
	}

	if isDigits(value) {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			return n
		}
	}

	if isDecimal(value) {
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	}

	switch strings.ToLower(value) {
	case "true":
		return true
	case "false":
		return false
	}

	return value
}

// isDigits reports whether value is non-empty and consists only of ASCII digits,
// matching Python's str.isdigit for the inputs this tool encounters.
func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isDecimal reports whether value is a decimal number containing exactly one dot
// that is neither the first nor the last character, with digits on both sides.
func isDecimal(value string) bool {
	if !strings.Contains(value, ".") {
		return false
	}
	if strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	return isDigits(strings.Replace(value, ".", "", 1))
}
