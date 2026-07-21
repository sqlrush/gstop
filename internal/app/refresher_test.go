package app

import (
	"context"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"gstop/internal/config"
	"gstop/internal/logging"
	"gstop/internal/model"
	"gstop/internal/monitor"
)

type deadlineMonitor struct {
	usedContext chan struct{}
	release     chan struct{}
}

func (m *deadlineMonitor) Name() string                       { return "deadline" }
func (m *deadlineMonitor) Height() int                        { return 1 }
func (m *deadlineMonitor) Init(int, int, int)                 {}
func (m *deadlineMonitor) Draw(tcell.Screen)                  {}
func (m *deadlineMonitor) DumpData() model.DumpData           { return model.DumpData{} }
func (m *deadlineMonitor) SetVisible(bool)                    {}
func (m *deadlineMonitor) Refresh()                           { <-m.release }
func (m *deadlineMonitor) RefreshContext(ctx context.Context) { close(m.usedContext); <-ctx.Done() }

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
	mon := &deadlineMonitor{usedContext: make(chan struct{}), release: make(chan struct{})}
	cfg := config.FromMap(map[string]any{"main": map[string]any{"collect_timeout": 0.05}})
	r := NewRefresher([]monitor.Monitor{mon}, nil, cfg, logging.New("refresher-test", ""))
	done := make(chan struct{})
	go func() {
		r.RefreshOnce()
		close(done)
	}()

	select {
	case <-mon.usedContext:
	case <-time.After(time.Second):
		close(mon.release)
		t.Fatal("refresher did not use the monitor context path")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		close(mon.release)
		t.Fatal("module refresh exceeded collect_timeout")
	}
}
