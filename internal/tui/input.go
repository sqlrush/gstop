package tui

import (
	"strconv"

	"gstop/internal/model"
)

// confirmPrompt is the exact wording the original used before any terminate.
const confirmPrompt = "Confirm again whether you need to execute the terminate command (y/n): "

// GetInputNumber reads a non-negative integer, echoing digits at (x, y).
// Enter confirms, Backspace deletes; non-digits are ignored. An empty entry
// yields 0. Port of util.get_input_number.
func GetInputNumber(s *Screen, x, y int) int {
	input := ""
	for {
		k, ok := s.GetKey(BlockForever)
		if !ok {
			continue
		}
		switch {
		case k.Kind == KeyEnter:
			n, err := strconv.Atoi(input)
			if err != nil {
				return 0
			}
			return n
		case k.Kind == KeyBackspace:
			if input != "" {
				input = input[:len(input)-1]
			}
		case k.Kind == KeyRune && k.Rune >= '0' && k.Rune <= '9':
			input += string(k.Rune)
		}
		s.SetString(x, y, input+" ", model.Normal)
		s.Show()
	}
}

// TerminateConfirmPassed shows the second-confirmation prompt at (x, y) and
// returns true only if the user answers 'y' before pressing Enter. Port of
// util.terminate_confirm_passed.
func TerminateConfirmPassed(s *Screen, x, y int) bool {
	style := model.Style{Pair: model.PairConfirmYellow, Bold: true}
	s.SetString(x, y, confirmPrompt, style)
	s.Show()

	result := false
	for {
		k, ok := s.GetKey(BlockForever)
		if !ok {
			continue
		}
		s.FlushInput()
		if k.Kind == KeyEnter {
			return result
		}
		result = k.IsRune('y')
		s.SetContent(x+len(confirmPrompt), y, echoRune(k), style)
		s.Show()
	}
}

// echoRune returns a printable representation of a key for the confirm echo.
func echoRune(k Key) rune {
	if k.Kind == KeyRune {
		return k.Rune
	}
	return '?'
}
