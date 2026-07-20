// Package dbconn manages gstop's connection to GaussDB/openGauss and exposes the
// query executors the monitors and emergency modules rely on. It uses the
// openGauss driver (a lib/pq fork that speaks openGauss's SHA256 auth), matching
// the original tool which linked psycopg2 against the openGauss libpq.
//
// The original kept a single autocommit connection, throttled reconnects to at
// most one attempt per second, and returned "no rows" rather than blocking when
// the connection was down, so the refresh loop degrades gracefully during an
// outage. This package preserves that behaviour: a failed query marks the
// connection unhealthy, and further queries are skipped for one second before a
// reconnect is attempted.
package dbconn

import (
	"database/sql"
	"sync"
	"time"

	_ "gitcode.com/opengauss/openGauss-connector-go-pq" // registers the "opengauss" driver

	"gstop/internal/config"
	"gstop/internal/dbcompat"
	"gstop/internal/logging"
	"gstop/internal/timing"
)

const driverName = "opengauss"

// maxPoolConns bounds the shared connection pool. Enough for the five resident
// monitors plus the emergency scenarios to run concurrently, small enough to be
// a negligible session count on the server.
const maxPoolConns = 8

// DB is the shared database gateway. It is safe for concurrent use.
type DB struct {
	cfg    *config.Config
	logger *logging.Logger
	now    func() time.Time

	mu           sync.Mutex
	pool         *sql.DB
	healthy      bool
	lastAttempt  time.Time
	kind         dbcompat.Kind
	kindDetected bool
}

// New builds a DB bound to cfg. The underlying pool is created lazily on first
// use, so New never blocks on the network.
func New(cfg *config.Config, logger *logging.Logger) *DB {
	return &DB{cfg: cfg, logger: logger, now: time.Now}
}

func (d *DB) slowThreshold() time.Duration {
	return time.Duration(d.cfg.GetFloat("main.sql_command_time_thresh", 3) * float64(time.Second))
}

// ensure returns a usable pool or nil. It enforces the one-attempt-per-second
// reconnect throttle. The caller must hold d.mu.
func (d *DB) ensure() *sql.DB {
	if d.pool != nil && d.healthy {
		return d.pool
	}
	if !d.lastAttempt.IsZero() && d.now().Sub(d.lastAttempt) <= time.Second {
		return nil
	}
	d.lastAttempt = d.now()

	if d.pool == nil {
		pool, err := sql.Open(driverName, buildDSN(d.cfg, d.cfg.GetString("main.database", "postgres")))
		if err != nil {
			d.logger.Error("open database failed: %v", err)
			return nil
		}
		// Allow a small pool so the concurrently-refreshed monitors and the
		// concurrently-analysed emergency scenarios do not serialise on a single
		// connection (a slow query would otherwise stall all monitoring).
		pool.SetMaxOpenConns(maxPoolConns)
		pool.SetMaxIdleConns(maxPoolConns)
		d.pool = pool
	}
	if err := d.pool.Ping(); err != nil {
		d.logger.Error("database ping failed: %v", err)
		d.healthy = false
		return nil
	}
	d.healthy = true
	return d.pool
}

// markUnhealthy forces a reconnect (subject to the throttle) on the next call.
func (d *DB) markUnhealthy() {
	d.mu.Lock()
	d.healthy = false
	d.mu.Unlock()
}

// Kind returns the detected database family (GaussDB or openGauss), used to route
// diverging queries. It reads Unknown until the first successful query completes
// detection; Unknown behaves as GaussDB at the call sites.
func (d *DB) Kind() dbcompat.Kind {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.kind
}

// detectKind classifies the server from version() once, retrying on failure. It
// must be called without holding d.mu.
func (d *DB) detectKind(pool *sql.DB) {
	d.mu.Lock()
	done := d.kindDetected
	d.mu.Unlock()
	if done {
		return
	}
	var version string
	if err := pool.QueryRow("select version();").Scan(&version); err != nil {
		return // leave undetected; retried on the next query
	}
	kind := dbcompat.Detect(version)
	d.mu.Lock()
	d.kind = kind
	d.kindDetected = true
	d.mu.Unlock()
	d.logger.Info("Detected database kind: %s", kind)
}

// Query runs a SELECT and returns its rows, or nil on any failure (connection
// down, SQL error), logging the cause. Mirrors util.execute_query.
func (d *DB) Query(query string) []Row {
	var out []Row
	timing.LogSlow(d.logger, "query", query, d.slowThreshold(), func() {
		out = d.doQuery(query)
	})
	return out
}

func (d *DB) doQuery(query string) []Row {
	d.mu.Lock()
	pool := d.ensure()
	d.mu.Unlock()
	if pool == nil {
		d.logger.Warning("Connection is None when exec query: %s", query)
		return nil
	}
	d.detectKind(pool)

	rows, err := pool.Query(query)
	if err != nil {
		d.logger.Error("Exec query '%s' failed: %v", query, err)
		d.markUnhealthy()
		return nil
	}
	defer rows.Close()

	out, err := scanRows(rows)
	if err != nil {
		d.logger.Error("Scan query '%s' failed: %v", query, err)
		d.markUnhealthy()
		return nil
	}
	return out
}

// NoReturn runs a statement whose result set is irrelevant, returning whether it
// succeeded. Mirrors util.execute_noreturn_query.
func (d *DB) NoReturn(query string) bool {
	ok := false
	timing.LogSlow(d.logger, "query", query, d.slowThreshold(), func() {
		d.mu.Lock()
		pool := d.ensure()
		d.mu.Unlock()
		if pool == nil {
			d.logger.Warning("Connection is None when exec query: %s", query)
			return
		}
		if _, err := pool.Exec(query); err != nil {
			d.logger.Error("Exec query '%s' failed: %v", query, err)
			d.markUnhealthy()
			return
		}
		ok = true
	})
	return ok
}

// ExecuteOnUserDB runs query against every user database (datdba <> 10, i.e.
// excluding system-owned databases), opening a short-lived connection per
// database, and returns a map of database name to its rows. Mirrors
// util.execute_query_on_user_db.
func (d *DB) ExecuteOnUserDB(query string) map[string][]Row {
	d.mu.Lock()
	pool := d.ensure()
	d.mu.Unlock()
	if pool == nil {
		d.logger.Warning("Connection is None when exec query: %s", query)
		return nil
	}

	dbRows, err := pool.Query("SELECT datname FROM pg_database WHERE datdba <> 10;")
	if err != nil {
		d.logger.Error("list user databases failed: %v", err)
		d.markUnhealthy()
		return nil
	}
	names, err := scanRows(dbRows)
	dbRows.Close()
	if err != nil {
		d.logger.Error("scan user databases failed: %v", err)
		return nil
	}

	result := make(map[string][]Row, len(names))
	for _, row := range names {
		name := Row(row).Str(0)
		if name == "" {
			continue
		}
		result[name] = d.queryOnDatabase(name, query)
	}
	return result
}

func (d *DB) queryOnDatabase(database, query string) []Row {
	if !d.cfg.GetBool("main.password_free", true) {
		return nil // create_connection only supports password-free connections
	}
	pool, err := sql.Open(driverName, buildDSN(d.cfg, database))
	if err != nil {
		d.logger.Warning("Create connection to database %s failed: %v", database, err)
		return nil
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)

	rows, err := pool.Query(query)
	if err != nil {
		d.logger.Error("Exec query '%s' in database '%s' failed: %v", query, database, err)
		return nil
	}
	defer rows.Close()
	out, err := scanRows(rows)
	if err != nil {
		d.logger.Error("Scan query '%s' in database '%s' failed: %v", query, database, err)
		return nil
	}
	return out
}

// BackgroundQuery runs a statement on a detached goroutine, used by emergency
// modules to fire off remediation without blocking the loop. Mirrors
// util.run_query_background.
func (d *DB) BackgroundQuery(query string) {
	go func() {
		d.logger.Warning("Exec background query: %s", query)
		if d.NoReturn(query) {
			d.logger.Warning("Exec background query finished")
		}
	}()
}

// Close releases the pool.
func (d *DB) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pool != nil {
		d.pool.Close()
		d.pool = nil
		d.healthy = false
	}
}

func scanRows(rows *sql.Rows) ([]Row, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	// A non-nil empty slice distinguishes an empty result set from a failed
	// query (which returns nil), matching psycopg2's []-vs-None. Monitors treat
	// nil as "connection down / abort" and an empty slice as "no rows".
	out := make([]Row, 0)
	for rows.Next() {
		values := make([]any, len(cols))
		pointers := make([]any, len(cols))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		out = append(out, Row(values))
	}
	return out, rows.Err()
}
