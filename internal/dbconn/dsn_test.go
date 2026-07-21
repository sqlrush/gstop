package dbconn

import (
	"strings"
	"testing"

	"gstop/internal/config"
	"gstop/internal/logging"
)

func TestDatabaseDSNSupportsPasswordAuthenticatedUserDatabases(t *testing.T) {
	cfg := config.FromMap(map[string]any{
		"main": map[string]any{
			"user":          "health_user",
			"port":          int64(5432),
			"password_free": false,
			"db_password":   "secret value",
		},
	})
	db := New(cfg, logging.New("dsn-test", ""))

	dsn := db.databaseDSN("application_db")

	for _, want := range []string{"dbname='application_db'", "user='health_user'", "password='secret value'"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("DSN %q does not contain %q", dsn, want)
		}
	}
}
