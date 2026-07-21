package app

import (
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
)

type healthViewState struct {
	selecting bool
	selected  int
	scroll    int
	detail    *healthdash.Detail
}

func (s *healthViewState) handleKey(key tui.Key, view *healthdash.View, viewportHeight int) healthAction {
	if s.detail != nil {
		switch {
		case key.Kind == tui.KeyEscape:
			s.detail = nil
			s.scroll = 0
		case key.Kind == tui.KeyUp:
			s.scroll = view.ClampScroll(s.scroll-1, viewportHeight)
		case key.Kind == tui.KeyDown:
			s.scroll = view.ClampScroll(s.scroll+1, viewportHeight)
		}
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

func (a *App) buildHealthDashboard(deps monitor.Deps) {
	if a.screen == nil {
		return
	}
	a.healthCollector = healthdash.NewCollector(deps.Cfg, deps.DB, deps.Logger, deps.Health)
	a.healthView = healthdash.NewView(model.MonitorWidth)
	a.healthDetailLoader = healthdash.NewDetailLoader(deps.DB)
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
	a.showHealthView = false
	a.healthState = healthViewState{selected: -1}
	a.setResidentVisible(true)
	a.screen.Clear()
}

func (a *App) drawHealthView() {
	if a.healthCollector == nil || a.healthView == nil {
		return
	}
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
	case healthOpenDetail:
		selections := a.healthView.SelectableSQL()
		if a.healthState.selected >= 0 && a.healthState.selected < len(selections) {
			selection := selections[a.healthState.selected]
			detail := a.healthDetailLoader.Load(selection.SQLID, selection.Query)
			a.healthState.detail = &detail
			a.healthState.scroll = 0
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
