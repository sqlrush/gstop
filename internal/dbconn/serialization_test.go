package dbconn

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gstop/internal/config"
	"gstop/internal/logging"
)

var serializationDriverID atomic.Uint64

func TestPrimaryGatewaySerializesConcurrentQueries(t *testing.T) {
	tracker := newSerializationTracker()
	db := newSerializationTestDB(t, tracker)

	done := make(chan struct{}, 2)
	go func() {
		db.Query("first")
		done <- struct{}{}
	}()
	waitForSerializationEntry(t, tracker.entered)
	go func() {
		db.Query("second")
		done <- struct{}{}
	}()

	concurrent := false
	select {
	case <-tracker.entered:
		concurrent = true
	case <-time.After(100 * time.Millisecond):
	}
	close(tracker.release)
	waitForSerializationEntry(t, done)
	waitForSerializationEntry(t, done)
	if concurrent || tracker.maxConcurrent() != 1 {
		t.Fatalf("same gateway max concurrent queries=%d, want 1", tracker.maxConcurrent())
	}
}

func TestPrimaryGatewaySerializesEntireResultStreams(t *testing.T) {
	tracker := newSerializationTracker()
	tracker.blockInRows = true
	db := newSerializationTestDB(t, tracker)

	done := make(chan struct{}, 2)
	go func() {
		db.Query("first stream")
		done <- struct{}{}
	}()
	waitForSerializationEntry(t, tracker.entered)
	go func() {
		db.Query("second stream")
		done <- struct{}{}
	}()

	concurrent := false
	select {
	case <-tracker.entered:
		concurrent = true
	case <-time.After(100 * time.Millisecond):
	}
	close(tracker.release)
	waitForSerializationEntry(t, done)
	waitForSerializationEntry(t, done)
	if concurrent || tracker.maxConcurrent() != 1 {
		t.Fatalf("same gateway max concurrent result streams=%d, want 1", tracker.maxConcurrent())
	}
}

func TestPrimaryGatewaySerializesQueryAndNoReturn(t *testing.T) {
	tracker := newSerializationTracker()
	db := newSerializationTestDB(t, tracker)

	queryDone := make(chan struct{})
	go func() {
		db.Query("blocking query")
		close(queryDone)
	}()
	waitForSerializationEntry(t, tracker.entered)

	execDone := make(chan bool, 1)
	go func() {
		execDone <- db.NoReturn("queued statement")
	}()
	concurrent := false
	select {
	case <-tracker.entered:
		concurrent = true
	case <-time.After(100 * time.Millisecond):
	}
	close(tracker.release)
	waitForSerializationEntry(t, queryDone)
	if ok := <-execDone; !ok {
		t.Fatal("serialized NoReturn failed")
	}
	if concurrent || tracker.maxConcurrent() != 1 {
		t.Fatalf("query/NoReturn max concurrency=%d, want 1", tracker.maxConcurrent())
	}
}

func newSerializationTestDB(t *testing.T, tracker *serializationTracker) *DB {
	t.Helper()
	name := fmt.Sprintf("gstop-serialization-%d", serializationDriverID.Add(1))
	sql.Register(name, &serializationDriver{tracker: tracker})
	pool, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open serialization test pool: %v", err)
	}
	pool.SetMaxOpenConns(maxPoolConns)
	pool.SetMaxIdleConns(maxPoolConns)

	db := New(config.FromMap(map[string]any{}), logging.New("db-serialization-test", ""))
	db.mu.Lock()
	db.pool = pool
	db.healthy = true
	db.kindDetected = true
	db.mu.Unlock()
	t.Cleanup(db.Close)
	return db
}

func waitForSerializationEntry(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for database operation")
	}
}

type serializationTracker struct {
	mu          sync.Mutex
	current     int
	maximum     int
	entered     chan struct{}
	release     chan struct{}
	blockInRows bool
}

func newSerializationTracker() *serializationTracker {
	return &serializationTracker{
		entered: make(chan struct{}, 4),
		release: make(chan struct{}),
	}
}

func (t *serializationTracker) enter() {
	t.mu.Lock()
	t.current++
	if t.current > t.maximum {
		t.maximum = t.current
	}
	t.mu.Unlock()
	t.entered <- struct{}{}
}

func (t *serializationTracker) leave() {
	t.mu.Lock()
	t.current--
	t.mu.Unlock()
}

func (t *serializationTracker) maxConcurrent() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.maximum
}

type serializationDriver struct {
	tracker *serializationTracker
}

func (d *serializationDriver) Open(string) (driver.Conn, error) {
	return &serializationConn{tracker: d.tracker}, nil
}

type serializationConn struct {
	tracker *serializationTracker
}

func (*serializationConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (*serializationConn) Close() error { return nil }
func (*serializationConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin is not supported")
}

func (c *serializationConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	if c.tracker.blockInRows {
		return &blockingSerializationRows{ctx: ctx, tracker: c.tracker}, nil
	}
	c.tracker.enter()
	defer c.tracker.leave()
	select {
	case <-c.tracker.release:
		return emptySerializationRows{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *serializationConn) ExecContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	c.tracker.enter()
	defer c.tracker.leave()
	select {
	case <-c.tracker.release:
		return driver.RowsAffected(1), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type blockingSerializationRows struct {
	ctx     context.Context
	tracker *serializationTracker
	done    bool
}

func (*blockingSerializationRows) Columns() []string { return []string{"value"} }
func (*blockingSerializationRows) Close() error      { return nil }

func (r *blockingSerializationRows) Next([]driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	r.tracker.enter()
	defer r.tracker.leave()
	select {
	case <-r.tracker.release:
		return io.EOF
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

type emptySerializationRows struct{}

func (emptySerializationRows) Columns() []string         { return []string{"value"} }
func (emptySerializationRows) Close() error              { return nil }
func (emptySerializationRows) Next([]driver.Value) error { return io.EOF }
