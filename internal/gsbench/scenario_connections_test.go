package gsbench

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"
)

type connectionCleanupTestState struct {
	rollbackErr error
	closeErr    error
	rollbacks   atomic.Int64
	closes      atomic.Int64
}

type connectionCleanupTestConnector struct {
	state *connectionCleanupTestState
}

func (c connectionCleanupTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &connectionCleanupTestConn{state: c.state}, nil
}

func (connectionCleanupTestConnector) Driver() driver.Driver {
	return connectionCleanupTestDriver{}
}

type connectionCleanupTestDriver struct{}

func (connectionCleanupTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use connector")
}

type connectionCleanupTestConn struct {
	state *connectionCleanupTestState
}

func (*connectionCleanupTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (c *connectionCleanupTestConn) Close() error {
	c.state.closes.Add(1)
	return c.state.closeErr
}

func (c *connectionCleanupTestConn) Begin() (driver.Tx, error) {
	return &connectionCleanupTestTx{state: c.state}, nil
}

type connectionCleanupTestTx struct {
	state *connectionCleanupTestState
}

func (*connectionCleanupTestTx) Commit() error { return nil }

func (t *connectionCleanupTestTx) Rollback() error {
	t.state.rollbacks.Add(1)
	return t.state.rollbackErr
}

func connectionCleanupTestTaggedConn(
	t *testing.T,
	state *connectionCleanupTestState,
) (*TaggedConn, *sql.Tx) {
	t.Helper()
	pool := sql.OpenDB(connectionCleanupTestConnector{state: state})
	conn, err := pool.Conn(context.Background())
	if err != nil {
		_ = pool.Close()
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		_ = conn.Close()
		_ = pool.Close()
		t.Fatal(err)
	}
	return &TaggedConn{Conn: conn, pool: pool}, tx
}

func TestConnectionScenarioStopCleansResourcesAfterJoinTimeout(t *testing.T) {
	rollbackErr := errors.New("rollback failed")
	closeErr := errors.New("close failed")
	transactionState := &connectionCleanupTestState{rollbackErr: rollbackErr}
	connectionState := &connectionCleanupTestState{closeErr: closeErr}
	taggedWithTx, tx := connectionCleanupTestTaggedConn(t, transactionState)
	taggedToClose, unusedTx := connectionCleanupTestTaggedConn(t, connectionState)
	if err := unusedTx.Rollback(); err != nil {
		t.Fatal(err)
	}
	connectionState.rollbacks.Store(0)

	scenario := &ConnectionScenario{
		transactions: []*sql.Tx{tx},
		connections:  []*TaggedConn{taggedWithTx, taggedToClose},
	}
	scenario.activeWG.Add(1)
	defer scenario.activeWG.Done()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := scenario.Stop(ctx, nil)
	for _, want := range []error{context.Canceled, rollbackErr, closeErr} {
		if !errors.Is(err, want) {
			t.Errorf("Stop error %v does not include %v", err, want)
		}
	}
	if got := transactionState.rollbacks.Load(); got != 1 {
		t.Errorf("rollback calls=%d want=1", got)
	}
	if got := transactionState.closes.Load(); got != 1 {
		t.Errorf("transaction connection close calls=%d want=1", got)
	}
	if got := connectionState.closes.Load(); got != 1 {
		t.Errorf("plain connection close calls=%d want=1", got)
	}
	if scenario.transactions != nil || scenario.connections != nil {
		t.Errorf("resource state was not cleared: transactions=%v connections=%v",
			scenario.transactions, scenario.connections)
	}
}

func TestConnectionFrozenSampleNeverRequestsTopUp(t *testing.T) {
	scenario := &ConnectionScenario{budget: ConnectionBudget{
		UsableCapacity: 100,
		DesiredTotal:   90,
		WorkloadTarget: 10,
	}}
	if err := scenario.acceptRampSample(90, 10); err != nil {
		t.Fatal(err)
	}
	if scenario.liveTagged != 10 {
		t.Fatalf("live tagged=%d", scenario.liveTagged)
	}
	if err := scenario.acceptFrozenSample(75, 10); err != nil {
		t.Fatalf("external connection loss changed frozen injection: %v", err)
	}
	if !scenario.targetReached {
		t.Fatal("a later external loss erased successful target evidence")
	}
}

func TestConnectionFrozenSampleFailsWhenInjectedSessionIsLost(t *testing.T) {
	scenario := &ConnectionScenario{budget: ConnectionBudget{
		UsableCapacity: 100,
		WorkloadTarget: 10,
	}}
	if err := scenario.acceptFrozenSample(89, 9); err == nil {
		t.Fatal("lost tagged session was accepted")
	}
}

func TestConnectionRampSampleMustReachTargetOnce(t *testing.T) {
	scenario := &ConnectionScenario{budget: ConnectionBudget{
		UsableCapacity: 100,
		DesiredTotal:   90,
		WorkloadTarget: 10,
	}}
	if err := scenario.acceptRampSample(90, 10); err != nil {
		t.Fatal(err)
	}
	if err := scenario.acceptRampSample(89, 10); err == nil {
		t.Fatal("ramp accepted an unreached total target")
	}
}

func TestConnectionRampDeadlineRemainsFailureWithoutWrappingDeadline(
	t *testing.T,
) {
	err := connectionTargetRampError(context.DeadlineExceeded, 7, 10)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline target error=%v", err)
	}
}
