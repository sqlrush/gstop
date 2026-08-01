// Package sqlshape creates stable, non-executable identities for SQL shapes.
package sqlshape

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Signature returns a SHA-256 identity for Canonical(sql).
func Signature(sql string) string {
	sum := sha256.Sum256([]byte(Canonical(sql)))
	return hex.EncodeToString(sum[:])
}

// Canonical normalizes formatting and replaces literal and bind values with a
// common token. The result is used only for matching and must never be run.
func Canonical(sql string) string {
	tokens := make([]string, 0, len(sql)/3)
	for i := 0; i < len(sql); {
		switch {
		case isSpace(sql[i]):
			i++
		case i+1 < len(sql) && sql[i:i+2] == "--":
			i += 2
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case i+1 < len(sql) && sql[i:i+2] == "/*":
			i += 2
			for i+1 < len(sql) && sql[i:i+2] != "*/" {
				i++
			}
			if i+1 < len(sql) {
				i += 2
			}
		case sql[i] == '\'':
			i = skipQuoted(sql, i, '\'')
			tokens = append(tokens, "?")
		case sql[i] == '"':
			start := i
			i = skipQuoted(sql, i, '"')
			tokens = append(tokens, sql[start:i])
		case sql[i] == '?':
			tokens = append(tokens, "?")
			i++
		case sql[i] == '$' && i+1 < len(sql) && isDigit(sql[i+1]):
			i += 2
			for i < len(sql) && isDigit(sql[i]) {
				i++
			}
			tokens = append(tokens, "?")
		case sql[i] == ':' && i+1 < len(sql) && isWordStart(sql[i+1]):
			i += 2
			for i < len(sql) && isWordPart(sql[i]) {
				i++
			}
			tokens = append(tokens, "?")
		case isDigit(sql[i]) || sql[i] == '.' && i+1 < len(sql) && isDigit(sql[i+1]):
			i = skipNumber(sql, i)
			tokens = append(tokens, "?")
		case isWordStart(sql[i]):
			start := i
			i++
			for i < len(sql) && isWordPart(sql[i]) {
				i++
			}
			tokens = append(tokens, strings.ToLower(sql[start:i]))
		case sql[i] == ';':
			i++
		default:
			operator, width := scanOperator(sql[i:])
			tokens = append(tokens, operator)
			i += width
		}
	}
	return strings.Join(tokens, " ")
}

func skipQuoted(sql string, start int, quote byte) int {
	for i := start + 1; i < len(sql); i++ {
		if sql[i] != quote {
			continue
		}
		if i+1 < len(sql) && sql[i+1] == quote {
			i++
			continue
		}
		return i + 1
	}
	return len(sql)
}

func skipNumber(sql string, start int) int {
	i := start
	for i < len(sql) && (isDigit(sql[i]) || sql[i] == '.') {
		i++
	}
	if i < len(sql) && (sql[i] == 'e' || sql[i] == 'E') {
		i++
		if i < len(sql) && (sql[i] == '+' || sql[i] == '-') {
			i++
		}
		for i < len(sql) && isDigit(sql[i]) {
			i++
		}
	}
	return i
}

func scanOperator(sql string) (string, int) {
	for _, operator := range []string{"->>", "#>>", "::", "||", ">=", "<=", "<>", "!=", "->", "#>", ":="} {
		if strings.HasPrefix(sql, operator) {
			return operator, len(operator)
		}
	}
	return sql[:1], 1
}

func isSpace(value byte) bool {
	switch value {
	case ' ', '\t', '\r', '\n', '\f':
		return true
	default:
		return false
	}
}

func isDigit(value byte) bool { return value >= '0' && value <= '9' }

func isWordStart(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func isWordPart(value byte) bool {
	return isWordStart(value) || isDigit(value) || value == '$'
}
