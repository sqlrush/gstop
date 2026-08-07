package gsbench

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

type sessionLockTestState struct {
	queries       []string
	argumentCount []int
	connectCount  int
	physicalClose int
}

type advisoryLockSQLStateError struct {
	state string
}

func (e advisoryLockSQLStateError) Error() string    { return "sqlstate " + e.state }
func (e advisoryLockSQLStateError) SQLState() string { return e.state }

type sessionLockTestConnector struct {
	state *sessionLockTestState
}

func (c sessionLockTestConnector) Connect(context.Context) (driver.Conn, error) {
	c.state.connectCount++
	return &sessionLockTestConn{state: c.state}, nil
}

func (sessionLockTestConnector) Driver() driver.Driver {
	return sessionLockTestDriver{}
}

type sessionLockTestDriver struct{}

func (sessionLockTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use connector")
}

type sessionLockTestConn struct {
	state *sessionLockTestState
}

func (*sessionLockTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (c *sessionLockTestConn) Close() error {
	c.state.physicalClose++
	return nil
}

func (*sessionLockTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *sessionLockTestConn) QueryContext(
	_ context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	c.state.queries = append(c.state.queries, query)
	c.state.argumentCount = append(c.state.argumentCount, len(args))
	return &sessionLockTestBoolRows{}, nil
}

type sessionLockTestBoolRows struct {
	read bool
}

func (*sessionLockTestBoolRows) Columns() []string { return []string{"locked"} }
func (*sessionLockTestBoolRows) Close() error      { return nil }
func (r *sessionLockTestBoolRows) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	dest[0] = true
	return nil
}

func TestSessionAdvisoryLockQueryUsesSafeSimpleSQL(t *testing.T) {
	query, err := sessionAdvisoryLockQuery(
		advisoryTryLock,
		`gsbench/restore/postgres/schema'\name`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(query, "$1") ||
		!strings.Contains(query, "pg_try_advisory_lock") ||
		!strings.Contains(query, "schema''") ||
		!strings.Contains(query, `\\name`) {
		t.Fatalf("query=%q", query)
	}
}

func TestSessionAdvisoryLockQueryRejectsUnknownOperation(t *testing.T) {
	if _, err := sessionAdvisoryLockQuery(
		advisoryLockOperation("drop"),
		"key",
	); err == nil {
		t.Fatal("unknown advisory operation accepted")
	}
}

func TestSQLAdvisoryLockSessionUsesZeroArgumentQueriesAndClosesOnce(
	t *testing.T,
) {
	state := &sessionLockTestState{}
	pool := sql.OpenDB(sessionLockTestConnector{state: state})
	db := &Database{
		cfg: BenchConfig{Safety: SafetyConfig{QueryTimeout: time.Second}},
		ctx: context.Background(),
	}
	session, err := newSQLAdvisoryLockSession(
		context.Background(),
		db,
		pool,
	)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := session.TryLock(context.Background(), "lock-key")
	if err != nil || !acquired {
		t.Fatalf("acquired=%v error=%v", acquired, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if state.connectCount != 1 || state.physicalClose != 1 {
		t.Fatalf(
			"connects=%d physical closes=%d",
			state.connectCount,
			state.physicalClose,
		)
	}
	if len(state.argumentCount) != 1 || state.argumentCount[0] != 0 {
		t.Fatalf("advisory query argument counts=%v want=[0]", state.argumentCount)
	}
}

func TestSQLAdvisoryLockSessionDiscardThenCloseIsIdempotent(t *testing.T) {
	state := &sessionLockTestState{}
	pool := sql.OpenDB(sessionLockTestConnector{state: state})
	db := &Database{
		cfg: BenchConfig{Safety: SafetyConfig{QueryTimeout: time.Second}},
		ctx: context.Background(),
	}
	session, err := newSQLAdvisoryLockSession(
		context.Background(),
		db,
		pool,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if state.physicalClose != 1 {
		t.Fatalf("physical closes=%d want=1", state.physicalClose)
	}
}

func TestRetryableAdvisoryLockErrorClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "bad connection", err: driver.ErrBadConn, want: true},
		{name: "closed connection", err: sql.ErrConnDone, want: true},
		{name: "network failure", err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}, want: true},
		{name: "connection failure", err: advisoryLockSQLStateError{state: "08006"}, want: true},
		{name: "out of memory", err: advisoryLockSQLStateError{state: "53200"}, want: true},
		{name: "too many connections", err: advisoryLockSQLStateError{state: "53300"}, want: true},
		{name: "admin shutdown", err: advisoryLockSQLStateError{state: "57P01"}, want: true},
		{name: "openGauss memory", err: errors.New("memory is temporarily unavailable"), want: true},
		{name: "permission", err: advisoryLockSQLStateError{state: "42501"}},
		{name: "syntax", err: advisoryLockSQLStateError{state: "42601"}},
		{name: "unknown", err: errors.New("unexpected query failure")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isRetryableAdvisoryLockError(test.err); got != test.want {
				t.Fatalf("retryable=%v want=%v error=%v", got, test.want, test.err)
			}
		})
	}
}
