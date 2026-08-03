package monitor

import "gstop/internal/dbconn"

// rowsHaveWidth rejects result sets associated with a different concurrent
// query before their positional fields reach a monitor snapshot.
func rowsHaveWidth(rows []dbconn.Row, width int) bool {
	for _, row := range rows {
		if len(row) != width {
			return false
		}
	}
	return true
}
