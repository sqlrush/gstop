package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"gstop/internal/dbcompat"
	"gstop/internal/dbconn"
	"gstop/internal/healthdash"
	"gstop/internal/tui"
)

func TestHealthStateSelectsSQLAndReturnsActions(t *testing.T) {
	view := healthdash.NewView(100)
	view.Render(healthdash.Snapshot{
		MemoryEnabled: false,
		AverageSQL: []healthdash.SQLMetric{
			{SQLID: 1, Query: "one"},
			{SQLID: 2, Query: "two"},
		},
	}, -1, false)
	state := healthViewState{selected: -1}

	if action := state.handleKey(tui.Key{Kind: tui.KeyRune, Rune: 's'}, view, 8); action != healthStay {
		t.Fatalf("s action = %v", action)
	}
	if !state.selecting || state.selected != 0 {
		t.Fatalf("selection state = %+v", state)
	}
	state.handleKey(tui.Key{Kind: tui.KeyDown}, view, 8)
	if state.selected != 1 {
		t.Fatalf("selected = %d, want 1", state.selected)
	}
	if action := state.handleKey(tui.Key{Kind: tui.KeyRune, Rune: 'p'}, view, 8); action != healthOpenDetail {
		t.Fatalf("p action = %v, want open detail", action)
	}
	if action := state.handleKey(tui.Key{Kind: tui.KeyRune, Rune: 'r'}, view, 8); action != healthRefreshSlow {
		t.Fatalf("r action = %v, want refresh slow", action)
	}
}

func TestHealthStateEscapeReturnsOneLevelAtATime(t *testing.T) {
	view := healthdash.NewView(100)
	view.Render(healthdash.Snapshot{}, -1, false)
	state := healthViewState{detail: &healthdash.Detail{SQLID: 1}, selected: -1}

	if action := state.handleKey(tui.Key{Kind: tui.KeyEscape}, view, 8); action != healthCloseDetail || state.detail != nil {
		t.Fatalf("escape from detail = action %v state %+v", action, state)
	}
	if action := state.handleKey(tui.Key{Kind: tui.KeyEscape}, view, 8); action != healthExit {
		t.Fatalf("escape from dashboard = %v, want exit health", action)
	}
}

func TestHealthStateArrowsScrollOutsideSelectionMode(t *testing.T) {
	view := healthdash.NewView(100)
	view.Render(healthdash.Snapshot{
		MemoryEnabled: false,
		AnalyzeHistory: []healthdash.AnalyzeRecord{
			{Database: "a"}, {Database: "b"}, {Database: "c"}, {Database: "d"},
		},
	}, -1, false)
	state := healthViewState{selected: -1}

	state.handleKey(tui.Key{Kind: tui.KeyDown}, view, 5)
	if state.scroll != 1 {
		t.Fatalf("scroll = %d, want 1", state.scroll)
	}
	state.handleKey(tui.Key{Kind: tui.KeyUp}, view, 5)
	if state.scroll != 0 {
		t.Fatalf("scroll = %d, want 0", state.scroll)
	}
}

func TestHealthStatePageAndEndpointNavigation(t *testing.T) {
	view := healthdash.NewView(100)
	view.Render(healthdash.Snapshot{
		AnalyzeHistory: []healthdash.AnalyzeRecord{
			{Database: "a"}, {Database: "b"}, {Database: "c"}, {Database: "d"}, {Database: "e"},
		},
	}, -1, false)
	state := healthViewState{selected: -1}
	viewport := 5

	state.handleKey(tui.Key{Kind: tui.KeyPageDown}, view, viewport)
	if state.scroll != 4 {
		t.Fatalf("page down scroll=%d, want 4", state.scroll)
	}
	state.handleKey(tui.Key{Kind: tui.KeyPageUp}, view, viewport)
	if state.scroll != 0 {
		t.Fatalf("page up scroll=%d, want 0", state.scroll)
	}
	state.handleKey(tui.Key{Kind: tui.KeyEnd}, view, viewport)
	if want := view.ClampScroll(view.Height(), viewport); state.scroll != want {
		t.Fatalf("end scroll=%d, want %d", state.scroll, want)
	}
	state.handleKey(tui.Key{Kind: tui.KeyHome}, view, viewport)
	if state.scroll != 0 {
		t.Fatalf("home scroll=%d, want 0", state.scroll)
	}
}

func TestHealthStatePageNavigationWorksInDetailAndSelectionModes(t *testing.T) {
	view := healthdash.NewView(100)
	view.Render(healthdash.Snapshot{
		AverageSQL:     []healthdash.SQLMetric{{SQLID: 1, Query: "one"}, {SQLID: 2, Query: "two"}},
		AnalyzeHistory: []healthdash.AnalyzeRecord{{Database: "a"}, {Database: "b"}, {Database: "c"}},
	}, -1, false)
	state := healthViewState{selecting: true, selected: 0}
	state.handleKey(tui.Key{Kind: tui.KeyPageDown}, view, 5)
	if state.scroll != 4 || state.selected != 0 {
		t.Fatalf("selection page navigation state=%+v", state)
	}

	state.detail = &healthdash.Detail{SQLID: 1}
	state.handleKey(tui.Key{Kind: tui.KeyEnd}, view, 5)
	if state.scroll != view.ClampScroll(view.Height(), 5) {
		t.Fatalf("detail end state=%+v", state)
	}
	state.handleKey(tui.Key{Kind: tui.KeyHome}, view, 5)
	state.handleKey(tui.Key{Kind: tui.KeyPageDown}, view, 5)
	state.handleKey(tui.Key{Kind: tui.KeyPageUp}, view, 5)
	if state.scroll != 0 {
		t.Fatalf("detail page round trip state=%+v", state)
	}
}

type capturingStreamLoader struct {
	targets chan healthdash.DetailTarget
	stopped chan struct{}
}

func newCapturingStreamLoader() *capturingStreamLoader {
	return &capturingStreamLoader{
		targets: make(chan healthdash.DetailTarget, 1),
		stopped: make(chan struct{}),
	}
}

func (l *capturingStreamLoader) LoadStream(
	ctx context.Context,
	target healthdash.DetailTarget,
	emit healthdash.DetailEmitter,
) {
	defer close(l.stopped)
	select {
	case l.targets <- target:
	case <-ctx.Done():
		return
	}
	<-ctx.Done()
}

func (l *capturingStreamLoader) waitTarget(t *testing.T) healthdash.DetailTarget {
	t.Helper()
	select {
	case target := <-l.targets:
		return target
	case <-time.After(250 * time.Millisecond):
		t.Fatal("detail target was not delivered")
		return healthdash.DetailTarget{}
	}
}

func newHealthDetailTestApp(loader detailStreamer) *App {
	return &App{
		showHealthView:      true,
		healthDetailLoader:  loader,
		healthDetailPatches: make(chan healthDetailPatch, 32),
	}
}

func TestStartHealthDetailPassesCapturedRepresentativeIdentity(t *testing.T) {
	loader := newCapturingStreamLoader()
	app := newHealthDetailTestApp(loader)
	at := time.Unix(100, 0)
	queryStart := time.Unix(92, 0)
	app.startHealthDetail(healthdash.Selection{
		SQLID: 707, Query: "select 707",
		RepresentativePID:        7007,
		RepresentativeSessionID:  "session-707",
		RepresentativeElapsedUS:  8_000,
		RepresentativeQueryStart: queryStart,
		CapturedAt:               at,
		Databases:                []string{"postgres"},
		Users:                    []string{"bench", "gaussdb"},
	})
	target := loader.waitTarget(t)
	if target.SQLID != 707 || target.RepresentativePID != 7007 ||
		target.RepresentativeSessionID != "session-707" ||
		target.RepresentativeElapsedUS != 8_000 ||
		!target.RepresentativeQueryStart.Equal(queryStart) ||
		!target.CapturedAt.Equal(at) ||
		len(target.Databases) != 1 || target.Databases[0] != "postgres" ||
		len(target.Users) != 2 || target.Users[0] != "bench" || target.Users[1] != "gaussdb" {
		t.Fatalf("target=%+v", target)
	}
	app.cancelHealthDetail()
}

func TestApplyHealthDetailPatchesPreservesScrollAndRejectsStaleRequest(t *testing.T) {
	target := healthdash.DetailTarget{RequestID: 2, SQLID: 2}
	detail := healthdash.NewLoadingDetail(target)
	app := &App{
		showHealthView: true, healthDetailRequestID: 2,
		healthDetailPatches: make(chan healthDetailPatch, 3),
		healthState:         healthViewState{scroll: 9, detail: &detail},
	}
	app.healthDetailPatches <- healthDetailPatch{
		requestID: 1,
		patch: healthdash.DetailPatch{RequestID: 1, Stage: healthdash.StagePlan,
			State: healthdash.StageReady, Done: true},
	}
	app.healthDetailPatches <- healthDetailPatch{
		requestID: 2,
		patch: healthdash.DetailPatch{RequestID: 2, Stage: healthdash.StageCPU,
			State: healthdash.StageReady, CPU: &healthdash.CPUSummary{Available: true}},
	}
	app.applyHealthDetailPatches()
	if app.healthState.scroll != 9 || !app.healthState.detail.Runtime.CPU.Available {
		t.Fatalf("state=%+v", app.healthState)
	}
}

func TestCancelHealthDetailInvalidatesLatePatches(t *testing.T) {
	target := healthdash.DetailTarget{RequestID: 4, SQLID: 4}
	detail := healthdash.NewLoadingDetail(target)
	app := &App{
		showHealthView: true, healthDetailRequestID: 4,
		healthDetailPatches: make(chan healthDetailPatch, 1),
		healthState:         healthViewState{detail: &detail},
	}
	app.cancelHealthDetail()
	app.healthDetailPatches <- healthDetailPatch{
		requestID: 4,
		patch: healthdash.DetailPatch{RequestID: 4, Stage: healthdash.StageComplete,
			State: healthdash.StageDone, Done: true},
	}
	app.applyHealthDetailPatches()
	if app.healthState.detail.Complete {
		t.Fatal("late patch was accepted after cancellation")
	}
}

func TestRequestExitPreventsBarrierReleasedHealthDetailStart(t *testing.T) {
	loader := newCapturingStreamLoader()
	app := newHealthDetailTestApp(loader)
	app.healthDetailMu.Lock()
	var unlock sync.Once
	defer unlock.Do(app.healthDetailMu.Unlock)
	exitDone := make(chan struct{})
	startDone := make(chan struct{})

	go func() {
		app.requestExit()
		close(exitDone)
	}()
	waitForExitRequest(t, app)
	go func() {
		app.startHealthDetail(healthdash.Selection{SQLID: 11, Query: "select 11"})
		close(startDone)
	}()
	unlock.Do(app.healthDetailMu.Unlock)
	<-startDone
	<-exitDone

	if app.healthState.detail != nil || app.healthDetailCancel != nil {
		t.Fatalf("detail request installed after exit: state=%+v cancel=%v",
			app.healthState, app.healthDetailCancel)
	}
	select {
	case target := <-loader.targets:
		t.Fatalf("detail stream started after exit: %+v", target)
	default:
	}
}

func TestRequestExitCancelsHealthDetailStartedBeforeExit(t *testing.T) {
	loader := newCapturingStreamLoader()
	app := newHealthDetailTestApp(loader)
	app.startHealthDetail(healthdash.Selection{SQLID: 12, Query: "select 12"})
	loader.waitTarget(t)

	exitDone := make(chan struct{})
	go func() {
		app.requestExit()
		close(exitDone)
	}()
	select {
	case <-exitDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("requestExit did not cancel the installed detail context")
	}
	select {
	case <-loader.stopped:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("detail stream did not observe exit cancellation")
	}
}

func TestRequestExitRejectsBarrierReleasedHealthDetailPatch(t *testing.T) {
	target := healthdash.DetailTarget{RequestID: 14, SQLID: 14}
	detail := healthdash.NewLoadingDetail(target)
	app := &App{
		showHealthView: true, healthDetailRequestID: 14,
		healthDetailPatches: make(chan healthDetailPatch, 1),
		healthState:         healthViewState{detail: &detail},
	}
	app.healthDetailPatches <- healthDetailPatch{
		requestID: 14,
		patch: healthdash.DetailPatch{
			RequestID: 14, Stage: healthdash.StageComplete,
			State: healthdash.StageDone, Done: true,
		},
	}
	app.healthDetailMu.Lock()
	var unlock sync.Once
	defer unlock.Do(app.healthDetailMu.Unlock)
	exitDone := make(chan struct{})
	applyDone := make(chan struct{})
	go func() {
		app.requestExit()
		close(exitDone)
	}()
	waitForExitRequest(t, app)
	go func() {
		app.applyHealthDetailPatches()
		close(applyDone)
	}()
	unlock.Do(app.healthDetailMu.Unlock)
	<-applyDone
	<-exitDone

	if app.healthState.detail.Complete {
		t.Fatal("detail patch merged after exit cancellation")
	}
}

func waitForExitRequest(t *testing.T, app *App) {
	t.Helper()
	deadline := time.Now().Add(250 * time.Millisecond)
	for !app.exitRequested.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !app.exitRequested.Load() {
		t.Fatal("requestExit did not record exit before the barrier deadline")
	}
}

func TestHealthDetailEscapeAndExitCancelCurrentRequest(t *testing.T) {
	var cancelled int
	app := &App{
		showHealthView:      true,
		healthDetailCancel:  func() { cancelled++ },
		healthDetailPatches: make(chan healthDetailPatch, 1),
		healthState: healthViewState{
			detail: &healthdash.Detail{SQLID: 1, Loading: true},
		},
	}
	app.cancelHealthDetail()
	if cancelled != 1 || app.healthDetailCancel != nil {
		t.Fatalf("cancelled=%d cancel=%v", cancelled, app.healthDetailCancel)
	}

	app.healthDetailCancel = func() { cancelled++ }
	app.exitHealthView()
	if cancelled != 2 || app.showHealthView || app.healthState.detail != nil {
		t.Fatalf("exit state=%+v cancelled=%d", app.healthState, cancelled)
	}
}

type blockingHealthDetailQueryer struct {
	started  chan struct{}
	done     chan struct{}
	once     sync.Once
	doneOnce sync.Once
}

func (b *blockingHealthDetailQueryer) Query(string) []dbconn.Row { return nil }
func (b *blockingHealthDetailQueryer) ExecuteOnUserDB(string) map[string][]dbconn.Row {
	return nil
}
func (b *blockingHealthDetailQueryer) Kind() dbcompat.Kind { return dbcompat.KindGaussDB }
func (b *blockingHealthDetailQueryer) QueryContext(ctx context.Context, query string) []dbconn.Row {
	b.once.Do(func() {
		b.done = make(chan struct{})
		close(b.started)
	})
	<-ctx.Done()
	b.doneOnce.Do(func() { close(b.done) })
	return nil
}
func (b *blockingHealthDetailQueryer) wait(t *testing.T) {
	t.Helper()
	if b.done == nil {
		t.Fatal("query never started")
	}
	select {
	case <-b.done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("detail query did not stop after cancellation")
	}
}
