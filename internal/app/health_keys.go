package app

import (
	"context"

	"gstop/internal/config"
	"gstop/internal/emergency"
	"gstop/internal/healthdash"
	"gstop/internal/model"
	"gstop/internal/monitor"
	"gstop/internal/tui"
)

type healthAction int

const (
	healthStay healthAction = iota
	healthExit
	healthOpenDetail
	healthRefreshSlow
	healthCloseDetail
)

type healthDetailPatch struct {
	requestID uint64
	patch     healthdash.DetailPatch
}

type healthViewState struct {
	selecting bool
	selected  int
	scroll    int
	detail    *healthdash.Detail
}

func (s *healthViewState) handleKey(key tui.Key, view *healthdash.View, viewportHeight int) healthAction {
	if s.detail != nil {
		if key.Kind == tui.KeyEscape {
			s.detail = nil
			s.scroll = 0
			return healthCloseDetail
		} else {
			s.navigateDocument(key, view, viewportHeight, true)
		}
		return healthStay
	}
	if s.navigateDocument(key, view, viewportHeight, false) {
		return healthStay
	}

	selections := view.SelectableSQL()
	switch {
	case key.Kind == tui.KeyEscape:
		return healthExit
	case key.IsRune('r'):
		return healthRefreshSlow
	case key.IsRune('s'):
		s.selecting = !s.selecting
		if s.selecting {
			if len(selections) == 0 {
				s.selected = -1
				s.selecting = false
			} else if s.selected < 0 || s.selected >= len(selections) {
				s.selected = 0
			}
			s.scroll = view.EnsureVisible(s.selected, s.scroll, viewportHeight)
		}
	case key.Kind == tui.KeyUp:
		if s.selecting && s.selected > 0 {
			s.selected--
			s.scroll = view.EnsureVisible(s.selected, s.scroll, viewportHeight)
		} else if !s.selecting {
			s.scroll = view.ClampScroll(s.scroll-1, viewportHeight)
		}
	case key.Kind == tui.KeyDown:
		if s.selecting && s.selected+1 < len(selections) {
			s.selected++
			s.scroll = view.EnsureVisible(s.selected, s.scroll, viewportHeight)
		} else if !s.selecting {
			s.scroll = view.ClampScroll(s.scroll+1, viewportHeight)
		}
	case key.IsRune('p') && s.selecting && s.selected >= 0 && s.selected < len(selections):
		return healthOpenDetail
	}
	return healthStay
}

func (s *healthViewState) navigateDocument(key tui.Key, view *healthdash.View, viewportHeight int, arrows bool) bool {
	page := viewportHeight - 1
	if page < 1 {
		page = 1
	}
	switch key.Kind {
	case tui.KeyPageUp:
		s.scroll = view.ClampScroll(s.scroll-page, viewportHeight)
		return true
	case tui.KeyPageDown:
		s.scroll = view.ClampScroll(s.scroll+page, viewportHeight)
		return true
	case tui.KeyHome:
		s.scroll = 0
		return true
	case tui.KeyEnd:
		s.scroll = view.ClampScroll(view.Height(), viewportHeight)
		return true
	case tui.KeyUp:
		if arrows {
			s.scroll = view.ClampScroll(s.scroll-1, viewportHeight)
			return true
		}
	case tui.KeyDown:
		if arrows {
			s.scroll = view.ClampScroll(s.scroll+1, viewportHeight)
			return true
		}
	}
	return false
}

func (a *App) buildHealthDashboard(deps monitor.Deps) {
	if a.screen == nil {
		return
	}
	a.healthCollector = healthdash.NewCollector(deps.Cfg, deps.DB, deps.Logger, deps.Health, deps.OS)
	a.healthView = healthdash.NewView(model.MonitorWidth)
	a.healthDetailLoader = healthdash.NewDetailLoader(deps.DB, config.CollectTimeout(deps.Cfg))
	a.healthDetailPatches = make(chan healthDetailPatch, 32)
	a.healthState = healthViewState{selected: -1}
}

func (a *App) enterHealthView() {
	if a.healthCollector == nil {
		return
	}
	a.setResidentVisible(false)
	a.showHealthView = true
	a.healthState = healthViewState{selected: -1}
}

func (a *App) exitHealthView() {
	a.cancelHealthDetail()
	a.showHealthView = false
	a.healthState = healthViewState{selected: -1}
	a.setResidentVisible(true)
	if a.screen != nil {
		a.screen.Clear()
	}
}

func (a *App) drawHealthView() {
	if a.healthCollector == nil || a.healthView == nil {
		return
	}
	a.applyHealthDetailPatches()
	a.screen.Clear()
	if a.healthState.detail != nil {
		a.healthView.DrawDetail(a.screen.Raw(), *a.healthState.detail, a.healthState.scroll)
		return
	}
	a.healthView.Draw(a.screen.Raw(), a.healthCollector.Snapshot(), a.healthState.selected,
		a.healthState.scroll, a.healthState.selecting)
}

func (a *App) handleHealthViewKey(key tui.Key) {
	_, height := a.screen.Size()
	action := a.healthState.handleKey(key, a.healthView, height)
	switch action {
	case healthExit:
		a.exitHealthView()
	case healthRefreshSlow:
		a.healthCollector.RequestSlowRefresh()
	case healthCloseDetail:
		a.cancelHealthDetail()
	case healthOpenDetail:
		selections := a.healthView.SelectableSQL()
		if a.healthState.selected >= 0 && a.healthState.selected < len(selections) {
			a.startHealthDetail(selections[a.healthState.selected])
		}
	}
}

func (a *App) startHealthDetail(selection healthdash.Selection) {
	if a.healthDetailLoader == nil {
		return
	}
	a.healthDetailMu.Lock()
	previousCancel := a.cancelHealthDetailLocked()
	if a.exitRequested.Load() {
		a.healthDetailMu.Unlock()
		if previousCancel != nil {
			previousCancel()
		}
		return
	}
	if a.healthDetailPatches == nil {
		a.healthDetailPatches = make(chan healthDetailPatch, 32)
	}
	a.healthDetailRequestID++
	requestID := a.healthDetailRequestID
	target := healthdash.DetailTarget{
		RequestID:                requestID,
		SQLID:                    selection.SQLID,
		SQLText:                  selection.Query,
		Databases:                append([]string(nil), selection.Databases...),
		Users:                    append([]string(nil), selection.Users...),
		RepresentativePID:        selection.RepresentativePID,
		RepresentativeSessionID:  selection.RepresentativeSessionID,
		RepresentativeElapsedUS:  selection.RepresentativeElapsedUS,
		RepresentativeQueryStart: selection.RepresentativeQueryStart,
		CapturedAt:               selection.CapturedAt,
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.healthDetailCancel = cancel
	detail := healthdash.NewLoadingDetail(target)
	a.healthState.detail = &detail
	a.healthState.scroll = 0
	loader := a.healthDetailLoader
	patches := a.healthDetailPatches
	a.healthDetailMu.Unlock()

	if previousCancel != nil {
		previousCancel()
	}
	go loader.LoadStream(ctx, target, func(patch healthdash.DetailPatch) bool {
		select {
		case patches <- healthDetailPatch{requestID: requestID, patch: patch}:
			return true
		case <-ctx.Done():
			return false
		}
	})
}

func (a *App) cancelHealthDetail() {
	a.healthDetailMu.Lock()
	cancel := a.cancelHealthDetailLocked()
	a.healthDetailMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *App) cancelHealthDetailLocked() context.CancelFunc {
	a.healthDetailRequestID++
	cancel := a.healthDetailCancel
	a.healthDetailCancel = nil
	return cancel
}

func (a *App) applyHealthDetailPatches() {
	if a.healthDetailPatches == nil {
		return
	}
	for {
		select {
		case result := <-a.healthDetailPatches:
			a.healthDetailMu.Lock()
			if result.requestID != a.healthDetailRequestID ||
				a.exitRequested.Load() || !a.showHealthView ||
				a.healthState.detail == nil {
				a.healthDetailMu.Unlock()
				continue
			}
			merged := healthdash.MergeDetailPatch(a.healthState.detail, result.patch)
			if merged && result.patch.Done {
				a.healthDetailCancel = nil
			}
			a.healthDetailMu.Unlock()
		default:
			return
		}
	}
}

func healthPlanEvents(events []emergency.PlanChangeEvent) []healthdash.PlanChangeEvent {
	out := make([]healthdash.PlanChangeEvent, len(events))
	for i, event := range events {
		out[i] = healthdash.PlanChangeEvent{
			SQLID: event.SQLID, Query: event.Query, FirstSeen: event.FirstSeen, LastSeen: event.LastSeen,
			RecoveredAt: event.RecoveredAt, PreviousAcs: event.PreviousAcs, CurrentAcs: event.CurrentAcs,
			PreviousLatUS: event.PreviousLatUS, CurrentLatUS: event.CurrentLatUS, Recovered: event.Recovered,
		}
	}
	return out
}
