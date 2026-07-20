package monitor

import (
	"fmt"

	"gstop/internal/model"
)

const blockSplit = "**********************************************************"

// analyzeBlockStatus assigns the BLK column (H/W/H&W), reports an alarm for each
// lock holder, and logs any deadlock cycles. It mutates and returns rows. Port
// of session.analyze_block_status.
func (m *SessionMonitor) analyzeBlockStatus(rows []model.SessionRow, blockers []any) []model.SessionRow {
	m.deps.Logger.Warning("%s PRINT BLOCK START %s", blockSplit, blockSplit)

	blockingMap := map[int64]int64{}
	for _, row := range rows {
		m.classifyRow(row, blockers, blockingMap)
	}
	m.logDeadlocks(blockingMap)

	m.deps.Logger.Warning("%s PRINT BLOCK END %s", blockSplit, blockSplit)
	return rows
}

// classifyRow sets one row's BLK status and records its blocking edge.
func (m *SessionMonitor) classifyRow(row model.SessionRow, blockers []any, blockingMap map[int64]int64) {
	pid := row.Get(model.SIdxPID)
	blocker := row.Get(model.SIdxBlocker)
	sessionID := row.Get(model.SIdxSessionID)

	if blocker != nil {
		row[model.SIdxBLK] = lockWaiter
		if pidInBlockers(pid, blockers) {
			row[model.SIdxBLK] = lockHolderWaiter
		}
		if bID, ok := sInt64(blocker); ok {
			if pID, ok2 := sInt64(pid); ok2 && pID != bID {
				blockingMap[pID] = bID
			}
		}
	} else if pid != nil && pidInBlockers(pid, blockers) {
		row[model.SIdxBLK] = lockHolder
		m.reportBlocker(pid, sessionID)
	}

	if blk := row.Display(model.SIdxBLK); blk != "" {
		m.deps.Logger.Warning("BLK: %s SESS: %v PID: %v BLOCKER: %v STATE: %v QUERY_START: %v XACT_START: %v UNIQUE_SQL_ID: %v QUERY: %v",
			blk, model.DisplayValue(sessionID), model.DisplayValue(pid), model.DisplayValue(blocker),
			model.DisplayValue(row.Get(model.SIdxState)), model.DisplayValue(row.Get(model.SIdxQueryStart)),
			model.DisplayValue(row.Get(model.SIdxXactStart)), model.DisplayValue(row.Get(model.SIdxSQLID)),
			model.DisplayValue(row.Get(model.SIdxSQL)))
	}
}

// reportBlocker raises the lock-holder alarm, embedding a quick-kill command only
// when emergency commands are permitted.
func (m *SessionMonitor) reportBlocker(pid, sessionID any) {
	key := fmt.Sprintf("BLOCKER_%v", model.DisplayValue(sessionID))
	var value string
	if m.deps.Cfg.GetBool("main.support_emergency_command", false) {
		command := model.TerminateSessionCmd(pid, sessionID)
		value = fmt.Sprintf("Gausstop检测到锁阻塞，阻塞源头会话ID：%v，线程ID：%v，使用以下命令快速查杀会话：%s",
			model.DisplayValue(sessionID), model.DisplayValue(pid), command)
	} else {
		value = fmt.Sprintf("Gausstop检测到锁阻塞，阻塞源头会话ID：%v，线程ID：%v",
			model.DisplayValue(sessionID), model.DisplayValue(pid))
	}
	m.deps.Alarm.CheckAndReport(m.deps.Logger, key, value, true)
}

// logDeadlocks finds and logs cycles in the blocking graph. Port of the nested
// find_cycle DFS.
func (m *SessionMonitor) logDeadlocks(blockingMap map[int64]int64) {
	visitedAll := map[int64]bool{}
	var deadlocks [][]int64
	for pid := range blockingMap {
		if visitedAll[pid] {
			continue
		}
		if cycle := findCycle(pid, blockingMap, visitedAll); cycle != nil {
			deadlocks = append(deadlocks, cycle)
			for _, node := range cycle[:len(cycle)-1] {
				visitedAll[node] = true
			}
		}
	}

	if len(deadlocks) == 0 {
		return
	}
	m.deps.Logger.Warning("FOUND %d DEADLOCKS", len(deadlocks))
	for i, cycle := range deadlocks {
		for j := 0; j < len(cycle)-1; j++ {
			m.deps.Logger.Warning("DEADLOCK[%d]: %d -> %d", i+1, cycle[j], cycle[j+1])
		}
	}
}

// findCycle walks the blocking edges from start until it revisits a node (a
// cycle) or leaves the graph. It returns the cycle nodes with the start repeated
// at the end, or nil. Matches the Python step-ordered reconstruction.
func findCycle(start int64, blockingMap map[int64]int64, visitedAll map[int64]bool) []int64 {
	visitedLocal := map[int64]int{}
	current := start
	step := 0
	for {
		next, inMap := blockingMap[current]
		if !inMap {
			return nil
		}
		if startIdx, seen := visitedLocal[current]; seen {
			return reconstructCycle(visitedLocal, startIdx)
		}
		if visitedAll[current] {
			return nil
		}
		visitedLocal[current] = step
		step++
		current = next
	}
}

// reconstructCycle rebuilds the cycle nodes ordered by their visit step, from the
// cycle start index onward, then appends the first node to close the loop.
func reconstructCycle(visitedLocal map[int64]int, startIdx int) []int64 {
	byStep := map[int]int64{}
	maxStep := startIdx
	for node, idx := range visitedLocal {
		if idx >= startIdx {
			byStep[idx] = node
			if idx > maxStep {
				maxStep = idx
			}
		}
	}
	var cycle []int64
	for idx := startIdx; idx <= maxStep; idx++ {
		if node, ok := byStep[idx]; ok {
			cycle = append(cycle, node)
		}
	}
	if len(cycle) > 0 {
		cycle = append(cycle, cycle[0])
	}
	return cycle
}
