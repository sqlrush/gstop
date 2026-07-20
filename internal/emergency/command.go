package emergency

import (
	"gstop/internal/model"
	"gstop/internal/tui"
)

// newCommand builds the interactive primitives a scenario uses to prompt for and
// confirm a remediation, rendering at the emergency panel's origin row.
func newCommand(screen *tui.Screen, baseY int) *Command {
	return &Command{
		Confirm: func() bool {
			return tui.TerminateConfirmPassed(screen, 0, baseY)
		},
		InputNumber: func() int {
			screen.SetString(0, baseY, "Please input the number and press Enter: ", model.Style{Pair: model.PairConfirmYellow, Bold: true})
			screen.Show()
			return tui.GetInputNumber(screen, 40, baseY)
		},
		ShowMenu: func(lines []string) rune {
			for i, line := range lines {
				screen.SetString(0, baseY+i, line+spaces(80), model.Normal)
			}
			screen.Show()
			for {
				key, ok := screen.GetKey(tui.BlockForever)
				if !ok {
					continue
				}
				screen.FlushInput()
				if key.Kind == tui.KeyRune {
					return key.Rune
				}
				return 0
			}
		},
	}
}
