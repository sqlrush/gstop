package emergency

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

type layoutScenario struct {
	*Base
	handled bool
	line    string
}

func newLayoutScenario() *layoutScenario {
	return &layoutScenario{Base: NewBase("layout", "[LAYOUT]", Deps{}, 0)}
}

func (s *layoutScenario) Analyze() {}

func (s *layoutScenario) HandleCommand(_ *Command, line string) {
	s.handled = true
	s.line = line
}

func triggeredLayoutEmergency(t *testing.T, height int) (*EmergencyMain, *layoutScenario, tcell.SimulationScreen) {
	t.Helper()
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(151, height)
	scenario := newLayoutScenario()
	main := NewEmergencyMain(Deps{}, 0, 43, 151, []Scenario{scenario}, false)
	main.triggered = true
	main.results = []scenarioResult{{scenario: scenario, header: "[LAYOUT]", info: []string{"action"}, triggered: true}}
	return main, scenario, screen
}

func TestEmergencyDrawOverlaysBottomOfFortyRowScreen(t *testing.T) {
	main, _, screen := triggeredLayoutEmergency(t, 40)
	defer screen.Fini()

	main.Draw(screen, -1)

	if got := main.DisplayBeginY(); got != 20 {
		t.Fatalf("display begin y = %d, want 20", got)
	}
	if got, _, _, _ := screen.GetContent(0, 20); got != 'E' {
		t.Fatalf("row 20 starts with %q, want emergency banner", got)
	}
}

func TestEmergencyDrawPreservesConfiguredOriginOnTallScreen(t *testing.T) {
	main, _, screen := triggeredLayoutEmergency(t, 63)
	defer screen.Fini()

	main.Draw(screen, -1)

	if got := main.DisplayBeginY(); got != 43 {
		t.Fatalf("display begin y = %d, want 43", got)
	}
}

func TestEmergencyCommandUsesDisplayedOrigin(t *testing.T) {
	main, scenario, screen := triggeredLayoutEmergency(t, 40)
	defer screen.Fini()

	main.Draw(screen, 21)
	main.HandleCommand(nil, 21)

	if !scenario.handled || scenario.line != "[LAYOUT]" {
		t.Fatalf("handled=%v line=%q, want first scenario row", scenario.handled, scenario.line)
	}
}

func TestRecoveredEmergencyDoesNotBlankResidentOverlayArea(t *testing.T) {
	main, _, screen := triggeredLayoutEmergency(t, 40)
	defer screen.Fini()
	screen.SetContent(0, 20, 'R', nil, tcell.StyleDefault)
	main.triggered = false

	main.Draw(screen, -1)

	if got, _, _, _ := screen.GetContent(0, 20); got != 'R' {
		t.Fatalf("resident cell = %q, want R", got)
	}
}
