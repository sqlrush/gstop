package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

type failingCPUSampler struct{ err error }

func (s failingCPUSampler) SampleCPU(context.Context) (float64, bool) {
	return 0, false
}

func (s failingCPUSampler) SampleCPUResult(context.Context) (float64, bool, error) {
	return 0, false, s.err
}

func TestCPUSamplePropagatesRealSamplerAndWorkerErrors(t *testing.T) {
	sentinel := errors.New("cpu catalog failed")
	sample := sampleCPU(context.Background(), failingCPUSampler{err: sentinel}, WorkerSnapshot{})
	if !errors.Is(sample.Err, sentinel) {
		t.Fatalf("sample=%+v", sample)
	}

	sample = sampleCPU(context.Background(), failingCPUSampler{}, WorkerSnapshot{
		Errors: 1, FirstError: "worker query failed",
	})
	if sample.Err == nil || sample.Errors != 1 {
		t.Fatalf("sample=%+v", sample)
	}
}

func TestContinuousControlStopCancelsAndJoinsController(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	a := &fakeActuator{}
	loop := &continuousControl{}
	loop.Start(context.Background(), Controller{
		Config: ControllerConfig{
			Target: 50, MinWorkers: 1, MaxWorkers: 2,
			RequiredSamples: 1, Interval: time.Nanosecond,
		},
		Actuator: a,
		Sample: func(ctx context.Context) Sample {
			close(entered)
			<-ctx.Done()
			close(exited)
			return Sample{}
		},
	})
	<-entered
	result := loop.Stop()
	select {
	case <-exited:
	default:
		t.Fatal("Stop returned before the controller sampler exited")
	}
	if result.Err != nil {
		t.Fatalf("result=%+v", result)
	}
}

func TestContinuousControlWaitDoesNotHideCallerCancellation(t *testing.T) {
	for range 50 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		loop := &continuousControl{}
		loop.Start(ctx, Controller{
			Config:   ControllerConfig{Target: 50, MinWorkers: 1, MaxWorkers: 1, Interval: time.Nanosecond},
			Actuator: &fakeActuator{},
			Sample:   func(context.Context) Sample { return Sample{} },
		})
		time.Sleep(time.Millisecond)
		_, err := loop.Wait(ctx, time.Second)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller cancellation was hidden: %v", err)
		}
	}
}

func TestContinuousControlWaitAndStopCanJoinConcurrently(t *testing.T) {
	entered := make(chan struct{})
	loop := &continuousControl{}
	loop.Start(context.Background(), Controller{
		Config:   ControllerConfig{Target: 50, MinWorkers: 1, MaxWorkers: 1, Interval: time.Nanosecond},
		Actuator: &fakeActuator{},
		Sample: func(ctx context.Context) Sample {
			close(entered)
			<-ctx.Done()
			return Sample{}
		},
	})
	<-entered
	waitDone := make(chan struct{})
	go func() {
		_, _ = loop.Wait(context.Background(), time.Second)
		close(waitDone)
	}()
	stopDone := make(chan struct{})
	go func() {
		_ = loop.Stop()
		close(stopDone)
	}()
	deadline := time.After(500 * time.Millisecond)
	for waitDone != nil || stopDone != nil {
		select {
		case <-waitDone:
			waitDone = nil
		case <-stopDone:
			stopDone = nil
		case <-deadline:
			t.Fatal("concurrent Wait/Stop did not both join")
		}
	}
}

func TestSQLWorkloadScalingDownClosesRetiredWorkerSession(t *testing.T) {
	state := &sessionCleanupTestState{}
	database := newSessionCleanupTestDatabase(t, state)
	taggedPool := sql.OpenDB(&sessionCleanupTestConnector{state: state})
	taggedPool.SetMaxOpenConns(1)
	taggedPool.SetMaxIdleConns(1)
	conn, err := taggedPool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tagged := &TaggedConn{Conn: conn, pool: taggedPool, db: database}
	database.mu.Lock()
	database.tagged[tagged] = struct{}{}
	database.mu.Unlock()

	started := make(chan struct{})
	workload := newSQLWorkload(
		context.Background(),
		&Runtime{Config: database.cfg},
		"scale-down",
		1,
		func(ctx context.Context, _ *sql.Conn, _ int) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	)
	workload.sessions[0] = tagged
	if err := workload.SetTarget(1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	if err := workload.SetTarget(0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		database.mu.Lock()
		remaining := len(database.tagged)
		database.mu.Unlock()
		if remaining == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	database.mu.Lock()
	remaining := len(database.tagged)
	database.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("scale-down left %d retired tagged sessions open", remaining)
	}
	if err := workload.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSQLWorkloadStopReportsRetiredSessionCloseFailure(t *testing.T) {
	state := &sessionCleanupTestState{failClose: true}
	database := newSessionCleanupTestDatabase(t, state)
	taggedPool := sql.OpenDB(&sessionCleanupTestConnector{state: state})
	conn, err := taggedPool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tagged := &TaggedConn{Conn: conn, pool: taggedPool, db: database}
	database.mu.Lock()
	database.tagged[tagged] = struct{}{}
	database.mu.Unlock()

	started := make(chan struct{})
	workload := newSQLWorkload(
		context.Background(),
		&Runtime{Config: database.cfg},
		"scale-down-close-error",
		1,
		func(ctx context.Context, _ *sql.Conn, _ int) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	)
	workload.sessions[0] = tagged
	if err := workload.SetTarget(1); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := workload.SetTarget(0); err != nil {
		t.Fatal(err)
	}
	if err := workload.Stop(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "close failed") {
		t.Fatalf("Stop error=%v, want retired close failure", err)
	}
}
