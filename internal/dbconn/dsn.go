package dbconn

import (
	"strconv"
	"strings"

	"gstop/internal/config"
)

// buildDSN assembles a lib/pq keyword DSN for the given database from config.
//
// Fidelity to the original: the Python tool called psycopg2.connect with only
// dbname/port/user (and password when not password-free), letting libpq read
// the host/socket from the deploy user's environment (gauss_env_file). We mirror
// that by omitting host unless main.host is explicitly configured, so PGHOST and
// friends still drive a peer-authenticated unix-socket connection. sslmode and
// connect_timeout default sensibly and are overridable.
func buildDSN(cfg *config.Config, database string) string {
	parts := []string{
		kv("dbname", database),
		kv("port", strconv.Itoa(cfg.GetInt("main.port", 8000))),
		kv("user", cfg.GetString("main.user", "rdsAdmin")),
		kv("sslmode", cfg.GetString("main.sslmode", "disable")),
		kv("connect_timeout", strconv.Itoa(cfg.GetInt("main.connect_timeout", 5))),
	}

	if host := cfg.GetString("main.host", ""); host != "" {
		parts = append(parts, kv("host", host))
	}
	if !cfg.GetBool("main.password_free", true) {
		parts = append(parts, kv("password", cfg.GetString("main.db_password", "")))
	}
	return strings.Join(parts, " ")
}

// kv renders a single lib/pq keyword=value pair with the value single-quoted and
// backslashes/quotes escaped, so values containing spaces or special characters
// are transmitted verbatim.
func kv(key, value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `'`, `\'`)
	return key + "='" + escaped + "'"
}
