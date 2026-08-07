package gsbench

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/base32"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "gitcode.com/opengauss/openGauss-connector-go-pq"
)

const benchDriverName = "opengauss"

const (
	applicationNameMaxBytes        = 63
	applicationRunTokenMaxBytes    = 21
	applicationWorkerRoleBytes     = 7
	applicationTokenHashChars      = 10
	applicationCompressedTokenMark = "~"
)

var tagComponentRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

type Database struct {
	cfg                 BenchConfig
	ctx                 context.Context
	cancel              context.CancelFunc
	pool                *sql.DB
	openAdvisorySession advisoryLockSessionOpener

	mu     sync.Mutex
	tagged map[*TaggedConn]struct{}

	targetProduct Product
}

func (d *Database) setTargetProduct(product Product) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.targetProduct = product
}

func (d *Database) journalTargetProduct() Product {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.targetProduct
}

type TaggedConn struct {
	Conn *sql.Conn
	pool *sql.DB
	once sync.Once
	err  error
	db   *Database
}

func OpenDatabase(parent context.Context, cfg BenchConfig) (*Database, error) {
	return openDatabase(parent, cfg, true)
}

// OpenRestoreDatabase creates the control pool without requiring an initial
// ping. Recovery holds its local mutex first and can therefore undo a
// control-plane fault before retrying database connectivity.
func OpenRestoreDatabase(
	parent context.Context,
	cfg BenchConfig,
) (*Database, error) {
	return openDatabase(parent, cfg, false)
}

func openDatabase(
	parent context.Context,
	cfg BenchConfig,
	verifyReachability bool,
) (*Database, error) {
	ctx, cancel := context.WithCancel(parent)
	pool, err := sql.Open(benchDriverName, cfg.DSN(cfg.Database.Database, cfg.Database.ApplicationName+"/control"))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open database: %w", err)
	}
	pool.SetMaxOpenConns(4)
	pool.SetMaxIdleConns(4)
	db := &Database{
		cfg: cfg, ctx: ctx, cancel: cancel, pool: pool,
		openAdvisorySession: openAdvisoryLockSession,
		tagged:              map[*TaggedConn]struct{}{},
	}
	if !verifyReachability {
		return db, nil
	}
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
	prefix, err := taggedScenarioPrefix(runID, scenario)
	if err != nil {
		return "", err
	}
	workerToken, err := applicationToken(
		"worker id",
		workerID,
		applicationNameMaxBytes-len(prefix),
	)
	if err != nil {
		return "", err
	}
	return prefix + workerToken, nil
}

func taggedScenarioPattern(runID, scenario string) (string, error) {
	prefix, err := taggedScenarioPrefix(runID, scenario)
	if err != nil {
		return "", err
	}
	return taggedLIKEPattern(prefix), nil
}

func taggedScenarioPrefix(runID, scenario string) (string, error) {
	if err := validateTagComponent("run id", runID); err != nil {
		return "", err
	}
	if err := validateTagComponent("scenario", scenario); err != nil {
		return "", err
	}
	runToken := runID
	scenarioMaxBytes := applicationNameMaxBytes -
		len("gsbench///") -
		len(runToken) -
		applicationWorkerRoleBytes
	if scenarioMaxBytes <= 0 ||
		len(scenario) > scenarioMaxBytes &&
			scenarioMaxBytes < len(applicationCompressedTokenMark)+applicationTokenHashChars {
		var err error
		runToken, err = applicationToken("run id", runID, applicationRunTokenMaxBytes)
		if err != nil {
			return "", err
		}
		scenarioMaxBytes = applicationNameMaxBytes -
			len("gsbench///") -
			len(runToken) -
			applicationWorkerRoleBytes
	}
	scenarioToken, err := applicationToken(
		"scenario",
		scenario,
		scenarioMaxBytes,
	)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("gsbench/%s/%s/", runToken, scenarioToken), nil
}

func applicationToken(label, value string, maxBytes int) (string, error) {
	if err := validateTagComponent(label, value); err != nil {
		return "", err
	}
	if len(value) <= maxBytes {
		return value, nil
	}
	sum := sha256.Sum256([]byte(value))
	hash := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(
		sum[:],
	)
	hash = strings.ToLower(hash)
	hashBytes := maxBytes - len(applicationCompressedTokenMark)
	if hashBytes <= applicationTokenHashChars {
		return applicationCompressedTokenMark + hash[:hashBytes], nil
	}
	hash = hash[:applicationTokenHashChars]
	prefixBytes := maxBytes -
		len(applicationCompressedTokenMark) -
		applicationTokenHashChars -
		1
	return applicationCompressedTokenMark + value[:prefixBytes] + "-" + hash, nil
}

func validateTagComponent(label, value string) error {
	if value == "" || !tagComponentRE.MatchString(value) {
		return fmt.Errorf("unsafe %s %q", label, value)
	}
	return nil
}

func taggedLIKEPattern(prefix string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	).Replace(prefix)
	return escaped + "%"
}

func TaggedSessionPredicate(runID string) (query string, args []any, err error) {
	return taggedSessionPredicate(runID, "")
}

func taggedSessionPredicate(
	runID string,
	activityAlias string,
) (query string, args []any, err error) {
	columnPrefix := ""
	if activityAlias != "" {
		if !identifierRE.MatchString(activityAlias) {
			return "", nil, fmt.Errorf(
				"unsafe activity alias %q",
				activityAlias,
			)
		}
		columnPrefix = activityAlias + "."
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return columnPrefix + "application_name LIKE $1 ESCAPE E'\\\\'",
			[]any{taggedLIKEPattern("gsbench/")}, nil
	}
	runToken, err := applicationToken("run id", runID, applicationRunTokenMaxBytes)
	if err != nil {
		return "", nil, err
	}
	patterns := []string{taggedLIKEPattern("gsbench/" + runToken + "/")}
	if runToken != runID {
		patterns = append(patterns, taggedLIKEPattern("gsbench/"+runID+"/"))
	}
	conditions := make([]string, 0, len(patterns))
	args = make([]any, 0, len(patterns))
	for index, pattern := range patterns {
		conditions = append(
			conditions,
			fmt.Sprintf(
				"%sapplication_name LIKE $%d ESCAPE E'\\\\'",
				columnPrefix,
				index+1,
			),
		)
		args = append(args, pattern)
	}
	return "((" + strings.Join(conditions, " OR ") + ")" +
			" AND (COALESCE(" + columnPrefix + "sessionid,0)<>0 OR " +
			columnPrefix + "backend_start IS NOT NULL))",
		args, nil
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

// maintenanceContext links long-running maintenance work to both the command
// and database lifetimes without imposing the per-query workload timeout.
// Dataset DDL such as CREATE INDEX can legitimately run longer than that
// workload guard, especially when initializing large datasets.
func (d *Database) maintenanceContext(
	parent context.Context,
) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = d.ctx
	}
	ctx, cancel := context.WithCancel(d.ctx)
	stop := context.AfterFunc(parent, cancel)
	return ctx, func() {
		stop()
		cancel()
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
	c.once.Do(func() {
		if c.Conn != nil {
			c.err = normalizeConnectionCloseError(c.Conn.Close())
		}
		if err := normalizeConnectionCloseError(c.pool.Close()); c.err == nil {
			c.err = err
		}
		if c.db != nil {
			c.db.mu.Lock()
			delete(c.db.tagged, c)
			c.db.mu.Unlock()
		}
	})
	return c.err
}

func normalizeConnectionCloseError(err error) error {
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (d *Database) Exec(parent context.Context, query string, args ...any) (sql.Result, error) {
	ctx, cancel := d.operationContext(parent)
	defer cancel()
	return d.pool.ExecContext(ctx, query, args...)
}

func (d *Database) execMaintenance(
	parent context.Context,
	query string,
	args ...any,
) (sql.Result, error) {
	ctx, cancel := d.maintenanceContext(parent)
	defer cancel()
	return d.pool.ExecContext(ctx, query, args...)
}

func (d *Database) ExecSession(parent context.Context, statements ...string) error {
	ctx, cancel := d.operationContext(parent)
	defer cancel()
	return d.execSession(ctx, statements...)
}

func (d *Database) ExecMaintenanceSession(
	parent context.Context,
	statements ...string,
) error {
	ctx, cancel := d.maintenanceContext(parent)
	defer cancel()
	return d.execSession(ctx, statements...)
}

func (d *Database) execSession(ctx context.Context, statements ...string) error {
	conn, err := d.pool.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	for index, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			executionErr := fmt.Errorf(
				"execute session statement %d: %w", index+1, err,
			)
			if cleanupErr := d.cleanupFailedSession(conn); cleanupErr != nil {
				discardErr := discardSessionConnection(conn)
				return errors.Join(
					executionErr,
					fmt.Errorf("clean failed session: %w", cleanupErr),
					discardErr,
				)
			}
			return executionErr
		}
	}
	return nil
}

func (d *Database) cleanupFailedSession(conn *sql.Conn) error {
	// The work context may already be canceled (for example after an ANALYZE
	// timeout). Cleanup therefore gets its own bounded context rooted at the
	// database lifetime, while still using the exact same physical session.
	ctx, cancel := d.operationContext(nil)
	defer cancel()
	var cleanupErrors []error
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		cleanupErrors = append(
			cleanupErrors, fmt.Errorf("rollback failed session: %w", err),
		)
	}
	if _, err := conn.ExecContext(ctx, "RESET ALL"); err != nil {
		cleanupErrors = append(
			cleanupErrors, fmt.Errorf("reset failed session: %w", err),
		)
	}
	return errors.Join(cleanupErrors...)
}

func discardSessionConnection(conn *sql.Conn) error {
	// Returning driver.ErrBadConn from Raw tells database/sql to close the
	// underlying driver connection instead of placing it back in the idle pool.
	err := conn.Raw(func(any) error { return driver.ErrBadConn })
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("discard failed session connection: %w", err)
	}
	return nil
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
	predicate, args, err := TaggedSessionPredicate(runID)
	if err != nil {
		return err
	}
	query := "SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE " + predicate + " AND pid <> pg_backend_pid()"
	return d.signalTagged(parent, query, args, "cancel")
}

func (d *Database) TerminateTagged(parent context.Context, runID string) error {
	query, args, err := StopTaggedSQL(runID)
	if err != nil {
		return err
	}
	return d.signalTagged(parent, query, args, "terminate")
}

func (d *Database) signalTagged(
	parent context.Context,
	query string,
	args []any,
	operation string,
) error {
	rows, err := d.Query(parent, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	rejected := 0
	for rows.Next() {
		var accepted bool
		if err := rows.Scan(&accepted); err != nil {
			return fmt.Errorf("scan %s signal result: %w", operation, err)
		}
		if !accepted {
			rejected++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read %s signal results: %w", operation, err)
	}
	if rejected != 0 {
		return fmt.Errorf("%d tagged session %s signals were rejected", rejected, operation)
	}
	return nil
}

func (d *Database) TaggedSessionState(
	parent context.Context,
	runID string,
) (sessions int, locks int, err error) {
	query, args, err := taggedSessionStateSQL(runID)
	if err != nil {
		return 0, 0, err
	}
	err = d.Scan(
		parent,
		query,
		args,
		&sessions,
		&locks,
	)
	return sessions, locks, err
}

func taggedSessionStateSQL(
	runID string,
) (query string, args []any, err error) {
	predicate, args, err := taggedSessionPredicate(runID, "a")
	if err != nil {
		return "", nil, err
	}
	return "SELECT count(DISTINCT a.pid),count(l.pid) " +
		"FROM pg_stat_activity a LEFT JOIN pg_locks l ON l.pid=a.pid " +
		"WHERE " + predicate, args, nil
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
