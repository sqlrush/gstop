package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	_ "gitcode.com/opengauss/openGauss-connector-go-pq"
)

const benchDriverName = "opengauss"

var tagComponentRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

type Database struct {
	cfg    BenchConfig
	ctx    context.Context
	cancel context.CancelFunc
	pool   *sql.DB

	mu     sync.Mutex
	tagged map[*TaggedConn]struct{}
}

type TaggedConn struct {
	Conn *sql.Conn
	pool *sql.DB
	once sync.Once
	db   *Database
}

func OpenDatabase(parent context.Context, cfg BenchConfig) (*Database, error) {
	ctx, cancel := context.WithCancel(parent)
	pool, err := sql.Open(benchDriverName, cfg.DSN(cfg.Database.Database, cfg.Database.ApplicationName+"/control"))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open database: %w", err)
	}
	pool.SetMaxOpenConns(4)
	pool.SetMaxIdleConns(4)
	db := &Database{cfg: cfg, ctx: ctx, cancel: cancel, pool: pool, tagged: map[*TaggedConn]struct{}{}}
	opCtx, opCancel := db.operationContext(ctx)
	defer opCancel()
	if err := pool.PingContext(opCtx); err != nil {
		pool.Close()
		cancel()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return db, nil
}

func ApplicationName(runID, scenario, workerID string) (string, error) {
	for label, value := range map[string]string{"run id": runID, "scenario": scenario, "worker id": workerID} {
		if value == "" || !tagComponentRE.MatchString(value) {
			return "", fmt.Errorf("unsafe %s %q", label, value)
		}
	}
	return fmt.Sprintf("gsbench/%s/%s/%s", runID, scenario, workerID), nil
}

func TaggedSessionPredicate(runID string) (query, arg string, err error) {
	if runID == "" || !tagComponentRE.MatchString(runID) {
		return "", "", fmt.Errorf("unsafe run id %q", runID)
	}
	return "application_name LIKE $1", "gsbench/" + runID + "/%", nil
}

func (d *Database) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := d.cfg.Safety.QueryTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if parent == nil {
		parent = d.ctx
	}
	ctx, cancelParent := context.WithCancel(d.ctx)
	stop := context.AfterFunc(parent, cancelParent)
	ctx, cancelTimeout := context.WithTimeout(ctx, timeout)
	return ctx, func() {
		stop()
		cancelTimeout()
		cancelParent()
	}
}

func (d *Database) OpenTagged(parent context.Context, runID, scenario, workerID string) (*TaggedConn, error) {
	appName, err := ApplicationName(runID, scenario, workerID)
	if err != nil {
		return nil, err
	}
	pool, err := sql.Open(benchDriverName, d.cfg.DSN(d.cfg.Database.Database, appName))
	if err != nil {
		return nil, fmt.Errorf("open tagged connection: %w", err)
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	ctx, cancel := d.operationContext(parent)
	defer cancel()
	conn, err := pool.Conn(ctx)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect tagged session: %w", err)
	}
	tagged := &TaggedConn{Conn: conn, pool: pool, db: d}
	d.mu.Lock()
	d.tagged[tagged] = struct{}{}
	d.mu.Unlock()
	return tagged, nil
}

func (c *TaggedConn) Close() error {
	var result error
	c.once.Do(func() {
		if c.Conn != nil {
			result = c.Conn.Close()
		}
		if err := c.pool.Close(); result == nil {
			result = err
		}
		if c.db != nil {
			c.db.mu.Lock()
			delete(c.db.tagged, c)
			c.db.mu.Unlock()
		}
	})
	return result
}

func (d *Database) Exec(parent context.Context, query string, args ...any) (sql.Result, error) {
	ctx, cancel := d.operationContext(parent)
	defer cancel()
	return d.pool.ExecContext(ctx, query, args...)
}

func (d *Database) Ping(parent context.Context) error {
	ctx, cancel := d.operationContext(parent)
	defer cancel()
	return d.pool.PingContext(ctx)
}

func (d *Database) Query(parent context.Context, query string, args ...any) (*Rows, error) {
	ctx, cancel := d.operationContext(parent)
	rows, err := d.pool.QueryContext(ctx, query, args...)
	if err != nil {
		cancel()
		return nil, err
	}
	return &Rows{Rows: rows, cancel: cancel}, nil
}

// QueryRow uses the caller context directly because QueryRowContext defers its
// error until Scan; callers should use Database.Probe or Exec for bounded
// one-row operations. It remains useful to scenario code with its own deadline.
func (d *Database) QueryRow(parent context.Context, query string, args ...any) *sql.Row {
	return d.pool.QueryRowContext(parent, query, args...)
}

func (d *Database) Scan(parent context.Context, query string, args []any, dest ...any) error {
	ctx, cancel := d.operationContext(parent)
	defer cancel()
	return d.pool.QueryRowContext(ctx, query, args...).Scan(dest...)
}

func (d *Database) Probe(parent context.Context, _, query string) (string, error) {
	ctx, cancel := d.operationContext(parent)
	defer cancel()
	var value any
	if err := d.pool.QueryRowContext(ctx, query).Scan(&value); err != nil {
		return "", err
	}
	return fmt.Sprint(value), nil
}

func (d *Database) CancelTagged(parent context.Context, runID string) error {
	predicate, arg, err := TaggedSessionPredicate(runID)
	if err != nil {
		return err
	}
	query := "SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE " + predicate + " AND pid <> pg_backend_pid()"
	rows, err := d.pool.QueryContext(parent, query, arg)
	if err != nil {
		return err
	}
	return rows.Close()
}

func (d *Database) Close() error {
	if d == nil {
		return nil
	}
	d.cancel()
	d.mu.Lock()
	tagged := make([]*TaggedConn, 0, len(d.tagged))
	for conn := range d.tagged {
		tagged = append(tagged, conn)
	}
	d.mu.Unlock()
	var errs []error
	for _, conn := range tagged {
		if err := conn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := d.pool.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

type Rows struct {
	*sql.Rows
	cancel context.CancelFunc
}

func (r *Rows) Close() error {
	err := r.Rows.Close()
	r.cancel()
	return err
}
