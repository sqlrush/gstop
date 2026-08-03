package gsbench

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestPlanTrafficCandidateSelectionIsDeterministic(t *testing.T) {
	for _, test := range []struct {
		worker int
		want   []int
	}{
		{worker: 0, want: []int{0, 1, 2, 0, 1, 2}},
		{worker: 1, want: []int{1, 2, 0, 1, 2, 0}},
	} {
		var got []int
		for operation := range len(test.want) {
			got = append(got, planCandidateIndex(
				test.worker,
				operation,
				3,
			))
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("worker=%d got=%v want=%v", test.worker, got, test.want)
		}
	}
}

type planTrafficTestState struct {
	mu       sync.Mutex
	connects map[int]int
	queries  map[int][]string
}

type planTrafficTestConnector struct {
	state  *planTrafficTestState
	worker int
}

func (c *planTrafficTestConnector) Connect(context.Context) (driver.Conn, error) {
	c.state.mu.Lock()
	c.state.connects[c.worker]++
	c.state.mu.Unlock()
	return &planTrafficTestConn{state: c.state, worker: c.worker}, nil
}

func (c *planTrafficTestConnector) Driver() driver.Driver {
	return planTrafficTestDriver{connector: c}
}

type planTrafficTestDriver struct {
	connector *planTrafficTestConnector
}

func (d planTrafficTestDriver) Open(string) (driver.Conn, error) {
	return d.connector.Connect(context.Background())
}

type planTrafficTestConn struct {
	state  *planTrafficTestState
	worker int
}

func (*planTrafficTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (*planTrafficTestConn) Close() error { return nil }

func (*planTrafficTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *planTrafficTestConn) QueryContext(
	_ context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	c.state.mu.Lock()
	if len(c.state.queries[c.worker]) < 12 {
		c.state.queries[c.worker] = append(c.state.queries[c.worker], query)
	}
	c.state.mu.Unlock()
	return &advisoryLockBoolRows{value: true}, nil
}

func TestPlanTrafficKeepsFixedSessionsAndSQLMixUntilDuration(t *testing.T) {
	state := &planTrafficTestState{
		connects: map[int]int{},
		queries:  map[int][]string{},
	}
	runtime := &Runtime{
		Config: BenchConfig{Safety: SafetyConfig{QueryTimeout: time.Second}},
		RunID:  "workload-1",
	}
	definition := PlanScenarioDefinition{
		Code:       601,
		Name:       "planchange_stats_target",
		Candidates: []string{"SQL-1", "SQL-2", "SQL-3"},
	}
	traffic, err := newPlanTraffic(
		context.Background(), runtime, definition, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	traffic.workload.sessionOpener = func(
		ctx context.Context,
		workerID int,
	) (*TaggedConn, error) {
		pool := sql.OpenDB(&planTrafficTestConnector{
			state: state, worker: workerID,
		})
		pool.SetMaxOpenConns(1)
		pool.SetMaxIdleConns(1)
		conn, err := pool.Conn(ctx)
		if err != nil {
			_ = pool.Close()
			return nil, err
		}
		return &TaggedConn{Conn: conn, pool: pool}, nil
	}

	snapshot, err := traffic.Run(context.Background(), 40*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Started != 2 || snapshot.PeakActive != 2 || snapshot.Operations == 0 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	for worker := 0; worker < 2; worker++ {
		if state.connects[worker] != 1 {
			t.Fatalf("worker=%d connects=%d", worker, state.connects[worker])
		}
		if len(state.queries[worker]) < 6 {
			t.Fatalf("worker=%d queries=%v", worker, state.queries[worker])
		}
		want := []string{"SQL-1", "SQL-2", "SQL-3", "SQL-1", "SQL-2", "SQL-3"}
		if worker == 1 {
			want = []string{"SQL-2", "SQL-3", "SQL-1", "SQL-2", "SQL-3", "SQL-1"}
		}
		if !reflect.DeepEqual(state.queries[worker][:6], want) {
			t.Fatalf(
				"worker=%d queries=%v want=%v",
				worker,
				state.queries[worker][:6],
				want,
			)
		}
	}
}
