package monitor

import (
	"fmt"

	"gstop/internal/dbconn"
	"gstop/internal/model"
)

// pg_lock_status() column positions, matching the comments in session.py.
const (
	lockColType     = 0
	lockColPID      = 12
	lockColSession  = 13
	lockColMode     = 14
	lockColGranted  = 15
	lockColLocktag  = 17
	waitStatusBlock = 14 // block_sessionid in pg_thread_wait_status
	waitStatusTag   = 12 // locktag in pg_thread_wait_status
)

// lockRecord is one row of pg_lock_status() relevant to the block tree.
type lockRecord struct {
	locktype  string
	pid       any
	sessionID any
	mode      string
	granted   bool
}

// printBlockedTree renders the upstream (blocking-this-session) and downstream
// (blocked-by-this-session) lock trees and returns the next row, the lock holder
// pid (or ""), and the session ids collected for the "print SQL by number" menu.
// Port of session.print_blocked_tree.
func (m *SessionMonitor) printBlockedTree(w *lineWriter, targetPID any) (int, string, []any) {
	w.line("[THE BLOCK TREE]", model.Normal)
	var sqlSessionIDs []any
	lockHolder := ""

	lockHolder = m.printUpstreamTree(w, targetPID, &sqlSessionIDs)
	lockHolder = m.printDownstreamTree(w, targetPID, lockHolder, &sqlSessionIDs)

	return w.y, lockHolder, sqlSessionIDs
}

// printUpstreamTree renders the threads holding and waiting on the lock that
// blocks the target session.
func (m *SessionMonitor) printUpstreamTree(w *lineWriter, targetPID any, sqlIDs *[]any) string {
	blockInfo := m.blockInfoByPID(targetPID)
	if blockInfo == nil || blockInfo.IsNull(waitStatusBlock) {
		return ""
	}
	lockTag := blockInfo.Str(waitStatusTag)
	lockInfo := m.lockInfoByLockTag(lockTag)
	if lockInfo == nil {
		return ""
	}

	lockHolder := ""
	var waiters []lockRecord
	for _, row := range lockInfo {
		rec := toLockRecord(row)
		if !rec.granted {
			waiters = append(waiters, rec)
			continue
		}
		lockHolder = model.DisplayValue(rec.pid)
		sql := sTruncate(m.sqlTextByPID(rec.pid), 140)
		w.line(fmt.Sprintf("PID: %v  SESSION_ID: %v", model.DisplayValue(rec.pid), model.DisplayValue(rec.sessionID)), model.Normal)
		w.line(fmt.Sprintf("Lock type: %s  Lock mode: %s  Lock tag: %s", rec.locktype, rec.mode, lockTag), model.Normal)
		w.line(fmt.Sprintf("SQL[%d]: %s", len(*sqlIDs), sql), model.Normal)
		*sqlIDs = append(*sqlIDs, rec.sessionID)
	}

	m.printWaiters(w, waiters, targetPID, sqlIDs)
	w.line("", model.Normal)
	return lockHolder
}

// printWaiters renders up to five waiter branches, collapsing the rest.
func (m *SessionMonitor) printWaiters(w *lineWriter, waiters []lockRecord, targetPID any, sqlIDs *[]any) {
	for i, waiter := range waiters {
		if i == 5 {
			w.prefixed("    |    ", "", model.Normal)
			w.prefixed("    |────", fmt.Sprintf("<all %d threads>", len(waiters)), model.Normal)
			return
		}
		sql := sTruncate(m.sqlTextByPID(waiter.pid), 80)
		style := model.Normal
		if samePID(waiter.pid, targetPID) {
			style = model.Style{Pair: model.PairConfirmYellow}
		}
		w.prefixed("    |    ", "", model.Normal)
		w.prefixed("    |────", fmt.Sprintf("PID: %v  Lock mode: %s  SQL[%d]: %s",
			model.DisplayValue(waiter.pid), waiter.mode, len(*sqlIDs), sql), style)
		*sqlIDs = append(*sqlIDs, waiter.sessionID)
	}
}

// printDownstreamTree renders the locks held by the target that block others.
func (m *SessionMonitor) printDownstreamTree(w *lineWriter, targetPID any, lockHolder string, sqlIDs *[]any) string {
	lockInfo := m.lockInfoByPID(targetPID)
	if lockInfo == nil {
		return lockHolder
	}

	for _, tag := range orderedLockTags(lockInfo) {
		records := tag.records
		holder, blocksOthers := splitHolder(records)
		if !blocksOthers || holder == nil {
			continue
		}
		if lockHolder == "" {
			lockHolder = model.DisplayValue(holder.pid)
		}
		m.printHolder(w, *holder, tag.tag, targetPID, sqlIDs)
		m.printHeldWaiters(w, records, sqlIDs)
		w.line("", model.Normal)
	}
	return lockHolder
}

func (m *SessionMonitor) printHolder(w *lineWriter, holder lockRecord, tag string, targetPID any, sqlIDs *[]any) {
	style := model.Normal
	if samePID(holder.pid, targetPID) {
		style = model.Style{Pair: model.PairConfirmYellow}
	}
	sql := sTruncate(m.sqlTextByPID(holder.pid), 140)
	w.line(fmt.Sprintf("PID: %v  SESSION_ID: %v", model.DisplayValue(holder.pid), model.DisplayValue(holder.sessionID)), style)
	w.line(fmt.Sprintf("Lock type: %s  Lock mode: %s  Lock tag: %s", holder.locktype, holder.mode, tag), style)
	w.line(fmt.Sprintf("SQL[%d]: %s", len(*sqlIDs), sql), style)
	*sqlIDs = append(*sqlIDs, holder.sessionID)
}

func (m *SessionMonitor) printHeldWaiters(w *lineWriter, records []lockRecord, sqlIDs *[]any) {
	printed := 0
	for _, rec := range records {
		if rec.granted {
			continue
		}
		if printed == 5 {
			w.prefixed("    |    ", "", model.Normal)
			w.prefixed("    |────", fmt.Sprintf("<all %d threads>", len(records)-1), model.Normal)
			return
		}
		sql := sTruncate(m.sqlTextByPID(rec.pid), 80)
		w.prefixed("    |    ", "", model.Normal)
		w.prefixed("    |────", fmt.Sprintf("PID: %v  Lock mode: %s  SQL[%d]: %s",
			model.DisplayValue(rec.pid), rec.mode, len(*sqlIDs), sql), model.Normal)
		printed++
		*sqlIDs = append(*sqlIDs, rec.sessionID)
	}
}

// terminateBlockerSession terminates the lock holder blocking session_pid. Port
// of terminate_blocker_session.
func (m *SessionMonitor) terminateBlockerSession(sessionPID any, lockHolder string) bool {
	m.deps.Logger.Warning("start terminate lock holder: session_pid=%v lock_holder=%s.", model.DisplayValue(sessionPID), lockHolder)
	if model.DisplayValue(sessionPID) == lockHolder {
		return m.deps.DB.NoReturn(fmt.Sprintf("select pg_terminate_backend(%s);", lockHolder))
	}
	blocker := m.blockerByPID(sessionPID)
	if blocker == "" || blocker != lockHolder {
		return false
	}
	return m.deps.DB.NoReturn(fmt.Sprintf("select pg_terminate_backend(%s);", blocker))
}

// --- query helpers ---

func (m *SessionMonitor) blockInfoByPID(pid any) dbconn.Row {
	rows := m.deps.DB.Query(fmt.Sprintf("SELECT * FROM pg_thread_wait_status WHERE tid='%v';", model.DisplayValue(pid)))
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

func (m *SessionMonitor) blockerByPID(pid any) string {
	rows := m.deps.DB.Query(fmt.Sprintf(
		"SELECT pid FROM pg_stat_activity WHERE sessionid IN (SELECT block_sessionid FROM pg_thread_wait_status WHERE tid='%v');",
		model.DisplayValue(pid)))
	if len(rows) == 0 {
		return ""
	}
	return rows[0].Str(0)
}

func (m *SessionMonitor) lockInfoByLockTag(tag string) []dbconn.Row {
	return m.deps.DB.Query(fmt.Sprintf("SELECT * FROM pg_lock_status() WHERE locktag='%s';", tag))
}

func (m *SessionMonitor) lockInfoByPID(pid any) []dbconn.Row {
	return m.deps.DB.Query(fmt.Sprintf(
		"SELECT * FROM pg_lock_status() WHERE locktag IN (SELECT locktag FROM pg_lock_status() WHERE pid = '%v' AND granted = true);",
		model.DisplayValue(pid)))
}

// --- helpers ---

func toLockRecord(row dbconn.Row) lockRecord {
	return lockRecord{
		locktype:  row.Str(lockColType),
		pid:       row.Col(lockColPID),
		sessionID: row.Col(lockColSession),
		mode:      row.Str(lockColMode),
		granted:   isGranted(row.Col(lockColGranted)),
	}
}

// taggedRecords keeps lock records grouped by tag in first-seen order.
type taggedRecords struct {
	tag     string
	records []lockRecord
}

// orderedLockTags groups lock rows by locktag, preserving first-seen order to
// match Python dict iteration.
func orderedLockTags(rows []dbconn.Row) []taggedRecords {
	var order []taggedRecords
	index := map[string]int{}
	for _, row := range rows {
		tag := row.Str(lockColLocktag)
		rec := toLockRecord(row)
		if pos, ok := index[tag]; ok {
			order[pos].records = append(order[pos].records, rec)
		} else {
			index[tag] = len(order)
			order = append(order, taggedRecords{tag: tag, records: []lockRecord{rec}})
		}
	}
	return order
}

// splitHolder returns the granted holder record (if any) and whether any waiter
// is present.
func splitHolder(records []lockRecord) (*lockRecord, bool) {
	var holder *lockRecord
	blocksOthers := false
	for i := range records {
		if !records[i].granted {
			blocksOthers = true
		} else {
			holder = &records[i]
		}
	}
	return holder, blocksOthers
}

func samePID(a, b any) bool {
	ai, aok := sInt64(a)
	bi, bok := sInt64(b)
	return aok && bok && ai == bi
}

// isGranted reports whether a pg_lock_status granted value is true.
func isGranted(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "t" || x == "true" || x == "True"
	case []byte:
		s := string(x)
		return s == "t" || s == "true"
	}
	return false
}
