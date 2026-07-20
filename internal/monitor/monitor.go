// Package monitor implements gstop's five resident panels (database, instance,
// operating system, wait events, sessions) plus the memory dashboard, each a
// port of the corresponding module under monitor/ in the Python tool. Panels are
// driven by their .cfg files, refreshed on a background cadence, and drawn into
// an off-screen pad that the app layer blits to the terminal.
package monitor

import (
	"github.com/gdamore/tcell/v2"

	"gstop/internal/alarm"
	"gstop/internal/config"
	"gstop/internal/dbconn"
	"gstop/internal/health"
	"gstop/internal/logging"
	"gstop/internal/model"
	"gstop/internal/oscmd"
)

// Deps bundles the collaborators every monitor needs. Passing one struct keeps
// constructor signatures small and makes the wiring in the app layer explicit.
type Deps struct {
	Cfg       *config.Config
	DB        *dbconn.DB
	OS        *oscmd.Runner
	Logger    *logging.Logger
	Alarm     *alarm.Alarm
	Health    *health.Health
	ConfigDir string // directory holding monitor/*.cfg
}

// Monitor is the behaviour the app layer needs from every resident panel:
// lay out, refresh data off the UI thread, draw to the screen, and expose the
// last-drawn snapshot for persistence.
type Monitor interface {
	Name() string
	Height() int
	Init(beginX, beginY, width int)
	Refresh()
	Draw(screen tcell.Screen)
	DumpData() model.DumpData
	SetVisible(v bool)
}
