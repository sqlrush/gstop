package emergency

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gstop/internal/dbconn"
	"gstop/internal/model"
)

// snapshotData is one retained snapshot: the wall-clock time, every panel's
// screen dump, and the full session set. Port of the snapshot_dict values.
type snapshotData struct {
	snapTS      time.Time
	dumpData    []model.DumpData
	fullSession []dbconn.Row
}

// sessionCSVHeader lists the columns persist_session_to_log writes.
const sessionCSVHeader = "snap_id,snap_ts,pid,session_id,unique_sql_id,state,wait_status,wait_event,block_session,xact_start,xact_start_diff,query_start,query_start_diff"

// Persist writes the retained snapshots for this scenario while it is triggered,
// opening per-trigger log files on first fire and renaming them once the
// configured number of snapshots has been captured. Port of Emergency.persist.
func (b *Base) Persist(snapshotDict map[int]snapshotData) {
	if b.triggered && !b.needPersist {
		b.openPersistLogs()
	}
	if !b.needPersist {
		return
	}
	if len(b.persistSnapIDs) >= b.snapshotPersistNumber {
		b.finishPersist()
		return
	}
	b.writePendingSnapshots(snapshotDict)
}

// openPersistLogs creates the dump and session log files on the first trigger.
func (b *Base) openPersistLogs() {
	b.needPersist = true
	b.startPersistTime = b.now().Format("20060102150405")

	dir := b.deps.Cfg.GetString("emergency.emergency_log_base_dir", "logs_emergency")
	_ = os.MkdirAll(dir, 0o755)
	maxBytes := int64(b.deps.Cfg.GetInt("emergency.log_file_max_size", 10)) * 1024 * 1024

	base := "gstop_emergency_" + b.name + "_" + b.startPersistTime
	b.persistWriter = newRawLog(filepath.Join(dir, base+".log"), maxBytes)
	b.sessionPersistWriter = newRawLog(filepath.Join(dir, base+"_session.log"), maxBytes)
	b.deps.Logger.Info("Emergency triggered: module = %s, start_persist_snap_id = %d, snapshot_persist_num = %d",
		b.name, b.startPersistSnapID, b.snapshotPersistNumber)
}

// finishPersist renames the completed log file with an end timestamp and resets
// the persistence state once enough snapshots have been captured.
func (b *Base) finishPersist() {
	b.deps.Logger.Info("Emergency recovered: module = %s, stop persist gstop info", b.name)
	dir := b.deps.Cfg.GetString("emergency.emergency_log_base_dir", "logs_emergency")
	end := b.now().Format("20060102150405")
	oldName := filepath.Join(dir, "gstop_emergency_"+b.name+"_"+b.startPersistTime+".log")
	newName := filepath.Join(dir, "gstop_emergency_"+b.name+"_"+b.startPersistTime+"_"+end+".log")
	if err := os.Rename(oldName, newName); err != nil {
		b.deps.Logger.Error("Rename failed: %v", err)
	}

	if b.persistWriter != nil {
		b.persistWriter.Close()
	}
	if b.sessionPersistWriter != nil {
		b.sessionPersistWriter.Close()
	}
	b.needPersist = false
	b.startPersistSnapID = 0
	b.startPersistTime = ""
	b.persistSnapIDs = map[int]bool{}
	b.persistWriter = nil
	b.sessionPersistWriter = nil
}

// writePendingSnapshots persists any not-yet-written snapshots at or after the
// start id, in id order.
func (b *Base) writePendingSnapshots(snapshotDict map[int]snapshotData) {
	ids := make([]int, 0, len(snapshotDict))
	for id := range snapshotDict {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	for _, id := range ids {
		if id < b.startPersistSnapID || b.persistSnapIDs[id] {
			continue
		}
		b.persistSnapIDs[id] = true
		data := snapshotDict[id]
		b.persistDumps(data.dumpData)
		b.persistSessions(id, data.snapTS, data.fullSession)
	}
}

// persistDumps writes each panel's screen dump followed by a separator.
func (b *Base) persistDumps(dumps []model.DumpData) {
	for _, dump := range dumps {
		if len(dump) == 0 {
			continue
		}
		b.persistWriter.Info(dumpToText(dump))
		b.persistWriter.Info("")
	}
	b.persistWriter.Info(strings.Repeat("=", 150))
	b.persistWriter.Info("")
}

// persistSessions writes the full session set as CSV.
func (b *Base) persistSessions(snapID int, snapTS time.Time, fullSession []dbconn.Row) {
	b.sessionPersistWriter.Info(sessionCSVHeader)
	ts := snapTS.Format("2006-01-02 15:04:05")
	for _, row := range fullSession {
		b.sessionPersistWriter.Info(sessionCSVLine(snapID, ts, row))
	}
	b.sessionPersistWriter.Info(strings.Repeat("=", 150))
}

// sessionCSVLine formats one raw session row into the CSV column order.
func sessionCSVLine(snapID int, ts string, row dbconn.Row) string {
	fields := []string{
		itoa(snapID), ts,
		model.DisplayValue(row.Col(0)),  // pid
		model.DisplayValue(row.Col(3)),  // session_id
		model.DisplayValue(row.Col(4)),  // unique_sql_id
		model.DisplayValue(row.Col(9)),  // state
		model.DisplayValue(row.Col(11)), // wait_status
		model.DisplayValue(row.Col(19)), // wait_event
		model.DisplayValue(row.Col(20)), // block_session
		model.DisplayValue(row.Col(16)), // xact_start
		model.DisplayValue(row.Col(18)), // xact_start_diff
		model.DisplayValue(row.Col(17)), // query_start
		model.DisplayValue(row.Col(8)),  // query_start_diff
	}
	return strings.Join(fields, ",")
}

// dumpToText renders a screen dump to text, padding each row to the global max
// column, matching the persist_to_log reconstruction.
func dumpToText(dump model.DumpData) string {
	maxY, maxX := 0, 0
	for y, row := range dump {
		if y > maxY {
			maxY = y
		}
		for x := range row {
			if x > maxX {
				maxX = x
			}
		}
	}
	lines := make([]string, 0, maxY+1)
	for y := 0; y <= maxY; y++ {
		row, ok := dump[y]
		if !ok {
			lines = append(lines, "")
			continue
		}
		buf := make([]rune, maxX+1)
		for x := 0; x <= maxX; x++ {
			if r, ok := row[x]; ok {
				buf[x] = r
			} else {
				buf[x] = ' '
			}
		}
		lines = append(lines, string(buf))
	}
	return strings.Join(lines, "\n")
}

// now returns the current time; the Base has no injected clock so it uses the
// wall clock (persistence timestamps are not exercised by unit tests).
func (b *Base) now() time.Time { return time.Now() }

func itoa(n int) string { return model.DisplayValue(int64(n)) }
