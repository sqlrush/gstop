package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"gstop/internal/config"
	"gstop/internal/health"
	"gstop/internal/logging"
	"gstop/internal/model"
	"gstop/internal/monitor"
)

type controlledMonitor struct {
	name       string
	started    chan struct{}
	release    chan error
	stateMu    sync.Mutex
	state      monitor.RefreshState
	calls      atomic.Int32
	concurrent atomic.Int32
	maxActive  atomic.Int32
}

func (m *controlledMonitor) Name() string             { return m.name }
func (m *controlledMonitor) Height() int              { return 1 }
func (m *controlledMonitor) Init(int, int, int)       {}
func (m *controlledMonitor) Draw(tcell.Screen)        {}
func (m *controlledMonitor) DumpData() model.DumpData { return model.DumpData{} }
func (m *controlledMonitor) SetVisible(bool)          {}
func (m *controlledMonitor) Refresh()                 {}
func (m *controlledMonitor) RefreshWithContext(ctx context.Context) error {
	m.calls.Add(1)
	active := m.concurrent.Add(1)
	defer m.concurrent.Add(-1)
	for {
		old := m.maxActive.Load()
		if active <= old || m.maxActive.CompareAndSwap(old, active) {
			break
		}
	}
	select {
	case m.started <- struct{}{}:
	default:
	}
	select {
	case err := <-m.release:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *controlledMonitor) SetRefreshState(state monitor.RefreshState) {
	m.stateMu.Lock()
	m.state = state
	m.stateMu.Unlock()
}
func (m *controlledMonitor) RefreshState() monitor.RefreshState {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.state
}

func newControlledMonitor(name string) *controlledMonitor {
	return &controlledMonitor{
		name: name, started: make(chan struct{}, 8), release: make(chan error, 8),
		state: monitor.RefreshState{Phase: monitor.RefreshLoading},
	}
}

func newTestRefresher(mon *controlledMonitor) *Refresher {
	cfg := config.FromMap(map[string]any{
		"main": map[string]any{"collect_timeout": 1.0, "interval": int64(60)},
	})
	return NewRefresher(
		[]monitor.Monitor{mon}, health.New(cfg), cfg,
		logging.New("refresher-test", ""),
	)
}

func waitStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("monitor refresh did not start")
	}
}

func waitForPhase(t *testing.T, mon *controlledMonitor, want monitor.RefreshPhase) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if mon.RefreshState().Phase == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("phase=%s, want %s", mon.RefreshState().Phase, want)
}

func TestRefresherFastModuleCompletesWithoutSlowBarrier(t *testing.T) {
	fast := newControlledMonitor("fast")
	slow := newControlledMonitor("slow")
	r := NewRefresher([]monitor.Monitor{fast, slow}, health.New(config.FromMap(map[string]any{})),
		config.FromMap(map[string]any{"main": map[string]any{"collect_timeout": 1.0}}),
		logging.New("refresher-test", ""))
	r.triggerIdle()
	waitStarted(t, fast.started)
	waitStarted(t, slow.started)
	fast.release <- nil
	waitForPhase(t, fast, monitor.RefreshFresh)
	if got := slow.RefreshState().Phase; got != monitor.RefreshLoading &&
		got != monitor.RefreshRefreshing {
		t.Fatalf("slow state=%s", got)
	}
	r.RequestStop()
	slow.release <- context.Canceled
	r.Stop()
}

func TestRefresherSkipsBusyModuleInsteadOfOverlappingOrQueueing(t *testing.T) {
	slow := newControlledMonitor("slow")
	r := newTestRefresher(slow)
	r.triggerIdle()
	waitStarted(t, slow.started)
	r.triggerIdle()
	r.triggerIdle()
	if got := slow.calls.Load(); got != 1 {
		t.Fatalf("calls while busy=%d, want 1", got)
	}
	slow.release <- nil
	waitForPhase(t, slow, monitor.RefreshFresh)
	select {
	case <-slow.started:
		t.Fatal("busy-slot triggers queued a second refresh")
	case <-time.After(50 * time.Millisecond):
	}
	if got := slow.calls.Load(); got != 1 {
		t.Fatalf("calls after first completion=%d, want 1", got)
	}
	if got := slow.maxActive.Load(); got != 1 {
		t.Fatalf("max concurrent=%d, want 1", got)
	}

	r.triggerIdle()
	waitStarted(t, slow.started)
	if got := slow.calls.Load(); got != 2 {
		t.Fatalf("calls after explicit idle trigger=%d, want 2", got)
	}
	slow.release <- nil
	waitForPhase(t, slow, monitor.RefreshFresh)
	if got := slow.maxActive.Load(); got != 1 {
		t.Fatalf("max concurrent after second refresh=%d, want 1", got)
	}
	r.Stop()
}

func TestRefresherTriggerReturnsBeforeModuleCompletes(t *testing.T) {
	slow := newControlledMonitor("slow")
	r := newTestRefresher(slow)
	returned := make(chan struct{})
	go func() { r.triggerIdle(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("trigger waited for slow module")
	}
	r.RequestStop()
	slow.release <- context.Canceled
	r.Stop()
}

func TestRefresherStopWaitsForAfterRefreshHook(t *testing.T) {
	r := NewRefresher(nil, nil, config.FromMap(map[string]any{}), logging.New("refresher-test", ""))
	entered := make(chan struct{})
	release := make(chan struct{})
	r.SetAfterRefresh(func() {
		close(entered)
		<-release
	})
	r.runAfterRefreshAsync()
	<-entered

	stopped := make(chan struct{})
	go func() {
		r.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while after-refresh hook was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after hook completed")
	}
}

func TestRefresherGivesEachModuleOneCollectionDeadline(t *testing.T) {
	mon := newControlledMonitor("deadline")
	cfg := config.FromMap(map[string]any{"main": map[string]any{"collect_timeout": 0.05}})
	r := NewRefresher([]monitor.Monitor{mon}, nil, cfg, logging.New("refresher-test", ""))
	r.RefreshOnce()

	waitStarted(t, mon.started)
	waitForPhase(t, mon, monitor.RefreshTimeout)
	if got := mon.calls.Load(); got != 1 {
		t.Fatalf("calls=%d, want 1", got)
	}
	r.Stop()
}
