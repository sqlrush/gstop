// Package emergency implements gstop's one-click emergency subsystem: nine
// fault scenarios (plan change, CPU/IO/memory/thread-pool/connections full, slow
// SQL, performance jitter, inspection) that are analysed every refresh cycle,
// rendered under the session panel, and — when the terminate switch is on —
// remediated from the keyboard. It is a port of the emergency/ package.
//
// The subsystem depends only on model, dbconn, tui, and the support packages; it
// receives a Snapshot of the current monitor values built by the app, so it does
// not import the monitor package.
package emergency

import (
	"time"

	"gstop/internal/dbconn"
)

// MemPanel is one memory-dashboard panel, decoupled from the monitor package's
// MemPanelData so emergency need not import monitor. The app converts between
// them when assembling a Snapshot.
type MemPanel struct {
	Title  string
	Header []string
	Width  []int
	Value  [][]any
}

// Snapshot is the per-cycle view of every monitor the scenarios read, replacing
// the Python monitors_value dict plus full_session. Column indices follow the
// panels' display order (e.g. OS[1]=%CPU, Instance[12]=CONNECTION,
// Instance[13]=THREADPOOL, Instance[9]=P80).
type Snapshot struct {
	SnapID   int
	SnapTS   time.Time
	DB       []string
	OS       []string
	Instance []string
	Event    []string
	Memory   []MemPanel
	Session  []dbconn.Row // full_session: raw 21-column rows
}

// Scenario is one emergency module. Analyze inspects the injected snapshot and
// records whether the fault is triggered plus its display lines and remediation
// targets; HandleCommand runs the interactive 'k' remediation for a selected
// display line. Shared state and helpers live on the embedded Base, reachable
// via Base().
type Scenario interface {
	// Common exposes the shared emergency state (triggered flag, info lines,
	// sql-id/pid targets, injected snapshot, persistence).
	Common() *Base
	// Analyze runs the scenario's detection and remediation planning.
	Analyze()
	// HandleCommand performs the keyboard remediation for the given display line.
	HandleCommand(cmd *Command, line string)
}

// Command carries the interactive context a scenario needs to prompt and confirm
// a remediation from the keyboard, decoupling scenarios from the concrete screen.
type Command struct {
	// Confirm shows the second-confirmation prompt and returns the answer.
	Confirm func() bool
	// InputNumber reads a bounded count from the user (e.g. "top X").
	InputNumber func() int
	// ShowMenu renders a sub-menu prompt and returns the chosen key rune.
	ShowMenu func(lines []string) rune
}
