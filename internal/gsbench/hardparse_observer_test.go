package gsbench

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestHardParseDeltaUsesDirectCounters(t *testing.T) {
	before := HardParseSample{Available: true, Hard: 10, Soft: 90, ParseUS: 1000, PlanUS: 800}
	after := HardParseSample{Available: true, Hard: 90, Soft: 110, ParseUS: 9000, PlanUS: 7000}
	got := hardParseDelta(before, after)
	if !got.Available || got.Hard != 80 || got.Soft != 20 || got.ParseUS != 8000 || got.PlanUS != 6200 || got.Ratio != .8 {
		t.Fatalf("delta=%+v", got)
	}
}

func TestHardParseDeltaFailsClosedWithoutCounters(t *testing.T) {
	got := hardParseDelta(HardParseSample{}, HardParseSample{Hard: 20})
	if got.Available {
		t.Fatalf("missing direct counter must not be available: %+v", got)
	}
}

type hardParseObserverTestDriver struct{}

var registerHardParseObserverTestDriver sync.Once

func (hardParseObserverTestDriver) Open(string) (driver.Conn, error) {
	return hardParseObserverTestConn{}, nil
}

type hardParseObserverTestConn struct{}

func (hardParseObserverTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}
func (hardParseObserverTestConn) Close() error { return nil }
func (hardParseObserverTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin is not supported")
}
func (hardParseObserverTestConn) QueryContext(
	_ context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	if len(args) == 0 {
		return &hardParseObserverTestRows{
			columns: []string{"node"},
			values:  [][]driver.Value{{"cn-1"}},
		}, nil
	}
	pattern, _ := args[0].Value.(string)
	var hard int64
	switch pattern {
	case "gsbench/run-1/%", "gsbench/run-1/hardparse_literal_flood/%",
		"gsbench/run-1/hardparse_ddl_invalidation/%":
		hard = 11
	case `gsbench/run-1/hardparse\_ddl\_invalidation/%`:
		hard = 0
	default:
		return nil, errors.New("unexpected statement-history application prefix")
	}
	return &hardParseObserverTestRows{
		columns: []string{"hard", "soft", "parse_us", "plan_us"},
		values:  [][]driver.Value{{hard, int64(0), hard * 100, hard * 80}},
	}, nil
}

type hardParseObserverTestRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *hardParseObserverTestRows) Columns() []string { return r.columns }
func (r *hardParseObserverTestRows) Close() error      { return nil }
func (r *hardParseObserverTestRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

func TestHardParseObserverDoesNotAttributeAnotherScenarioTag(t *testing.T) {
	const driverName = "gsbench-hardparse-observer-test"
	registerHardParseObserverTestDriver.Do(func() {
		sql.Register(driverName, hardParseObserverTestDriver{})
	})
	pool, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx := context.Background()
	rt := &Runtime{
		RunID: "run-1",
		Database: &Database{
			cfg:  BenchConfig{Safety: SafetyConfig{QueryTimeout: time.Second}},
			ctx:  ctx,
			pool: pool,
		},
	}
	sample, err := (databaseHardParseObserver{}).Sample(
		ctx, rt, "hardparse_ddl_invalidation",
	)
	if err != nil {
		t.Fatal(err)
	}
	if sample.Node != "cn-1" || sample.Hard != 0 {
		t.Fatalf("another scenario's 11 hard parses were attributed: %+v", sample)
	}
}
