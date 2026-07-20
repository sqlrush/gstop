package emergency

import (
	"fmt"

	"gstop/internal/model"
)

const (
	memoryFullName    = "MemoryFull"
	memoryFullHeader  = "[EMER03 - MemoryFull] - switch to memory monitor panel to analyze"
	memoryFullPersist = 120
)

// Memory-full highlight modes, kept in lockstep with monitor.EmerNull /
// EmerDynamicGlobalMemoryFull / EmerSessionThreadMemoryFull. Duplicated here so
// the emergency package need not import monitor. The dispatcher reads
// MemoryFullType and forwards it to the memory dashboard for highlighting.
const (
	emerNull                    = 0
	emerDynamicGlobalMemoryFull = 1
	emerSessionThreadMemoryFull = 2
)

// MemoryFull detects two memory-exhaustion modes from the memory dashboard
// snapshot: a single dynamic global memory context dominating dynamic usage, or
// a high overall process memory usage percent. It never remediates from its own
// panel (the header directs the operator to the memory monitor); it only
// surfaces the top offenders and, when permitted, the quick-kill commands. Port
// of emergency/memory_full.py.
type MemoryFull struct {
	*Base
	memoryFullType int
}

// NewMemoryFull builds the memory-full scenario.
func NewMemoryFull(deps Deps) *MemoryFull {
	return &MemoryFull{Base: NewBase(memoryFullName, memoryFullHeader, deps, memoryFullPersist)}
}

// MemoryFullType exposes the current highlight mode so the dispatcher can tell
// the memory dashboard which panel to emphasise.
func (s *MemoryFull) MemoryFullType() int { return s.memoryFullType }

// Analyze resets the highlight mode, validates the four memory panels, then
// checks for a global dynamic-context full before falling back to a
// session/thread process-memory full.
func (s *MemoryFull) Analyze() {
	s.memoryFullType = emerNull

	if len(s.snap.Memory) == 0 {
		return
	}
	if len(s.snap.Memory) < 4 {
		s.deps.Logger.Error("invalid memory data structure: insufficient panels")
		return
	}

	panel0 := s.snap.Memory[0]
	if len(panel0.Value) == 0 || len(panel0.Value[0]) < 9 {
		s.deps.Logger.Error("invalid memory summary panel structure")
		return
	}
	// See memory.cfg: value[0] = [DATE, MEM_MAX, MEM_USED%, DYNAMIC_MAX,
	// DYNAMIC, DYNAMIC_USED%, SHARED_MAX, SHARED, SHARED_USED%].
	dynamicUsedCell := panel0.Value[0][4]
	processUsedPctCell := panel0.Value[0][2]

	if s.detectDynamicGlobalFull(s.snap.Memory[1], dynamicUsedCell) {
		return
	}
	s.detectSessionThreadFull(processUsedPctCell)
}

// detectDynamicGlobalFull fires when the summed dynamic context memory (the
// "SUM" column of panel 1) exceeds dynamic_ctx_pct_thresh of the current dynamic
// used memory. Returns whether it triggered.
func (s *MemoryFull) detectDynamicGlobalFull(panel1 MemPanel, dynamicUsedCell any) bool {
	if len(panel1.Value) == 0 {
		return false
	}
	totalRow := panel1.Value[0]
	dynamicUsedMemory := dbFloat(dynamicUsedCell)
	thresh := s.deps.Cfg.GetInt("emergency.memory_full.dynamic_ctx_pct_thresh", 80)

	for idx, name := range panel1.Header {
		if name != "SUM" {
			continue
		}
		if idx >= len(totalRow) {
			continue
		}
		dynamicCtxTotal := dbFloat(totalRow[idx])
		if dynamicCtxTotal > dynamicUsedMemory*float64(thresh)/100 {
			value := fmt.Sprintf("Gausstop检测到内存满（全局动态内存），当前动态内存占用%sM，全局动态内存占用%sM，阈值为%d%%",
				model.DisplayValue(dynamicUsedCell), model.DisplayValue(totalRow[idx]), thresh)
			s.triggerEmergency(emerDynamicGlobalMemoryFull, value)
			return true
		}
	}
	return false
}

// detectSessionThreadFull fires when overall process memory usage exceeds
// process_memory_pct_thresh.
func (s *MemoryFull) detectSessionThreadFull(processUsedPctCell any) {
	thresh := s.deps.Cfg.GetInt("emergency.memory_full.process_memory_pct_thresh", 90)
	if dbFloat(processUsedPctCell) <= float64(thresh) {
		return
	}
	value := fmt.Sprintf("Gausstop检测到内存满（会话和线程），当前内存使用率为%s%%，阈值为%d%%",
		model.DisplayValue(processUsedPctCell), thresh)
	s.triggerEmergency(emerSessionThreadMemoryFull, value)
}

// triggerEmergency marks the scenario fired, records the highlight mode, renders
// the TOP3 session/thread memory lines, appends kill commands for the
// session/thread mode when permitted, then reports the alarm.
func (s *MemoryFull) triggerEmergency(memoryFullType int, alarmValue string) {
	s.Trigger()
	s.memoryFullType = memoryFullType

	sessionMemory, sessionIDs := memFullTopMemory(s.snap.Memory[2], "TOP3 SESSION MEMORY (SESSION_ID MEM):")
	threadMemory, threadIDs := memFullTopMemory(s.snap.Memory[3], "TOP3 THREAD MEMORY (THREAD_ID MEM):")

	s.AddInfo(sessionMemory)
	s.AddInfo(threadMemory)

	if memoryFullType == emerSessionThreadMemoryFull {
		alarmValue = s.appendTerminateCommands(alarmValue, sessionMemory, sessionIDs, threadMemory, threadIDs)
	}

	key := fmt.Sprintf("%s_%d", memoryFullName, s.memoryFullType)
	s.deps.Alarm.CheckAndReport(s.deps.Logger, key, alarmValue, true)
}

// appendTerminateCommands enriches the alarm with the top offenders and, when
// emergency commands are permitted, appends the quick-kill statements to both
// the alarm text and the info panel. Returns the (possibly extended) alarm text.
func (s *MemoryFull) appendTerminateCommands(alarmValue, sessionMemory string, sessionIDs []string, threadMemory string, threadIDs []string) string {
	support := s.deps.Cfg.GetBool("main.support_emergency_command", false)

	var terminateSession, terminateThread string
	if len(sessionIDs) != 0 {
		terminateSession = fmt.Sprintf("SELECT pg_terminate_session(pid, sessionid) FROM pg_stat_activity WHERE sessionid IN (%s);", quotedList(sessionIDs))
		if support {
			alarmValue = fmt.Sprintf("%s，内存占用较高的会话ID以及内存占用大小如下：%s，使用以下命令快速查杀内存占用高的会话：%s",
				alarmValue, sessionMemory, terminateSession)
		} else {
			alarmValue = fmt.Sprintf("%s，内存占用较高的会话ID以及内存占用大小如下：%s", alarmValue, sessionMemory)
		}
	}

	if len(threadIDs) != 0 {
		terminateThread = fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid IN (%s);", quotedList(threadIDs))
		if support {
			alarmValue = fmt.Sprintf("%s，内存占用较高的线程ID以及内存占用大小如下：%s，使用以下命令快速查杀内存占用高的线程：%s",
				alarmValue, threadMemory, terminateThread)
		} else {
			alarmValue = fmt.Sprintf("%s，内存占用较高的线程ID以及内存占用大小如下：%s", alarmValue, threadMemory)
		}
	}

	if (terminateSession != "" || terminateThread != "") && support {
		s.AddInfo("")
		if terminateSession != "" {
			s.AppendSplitString(terminateSession, "TERMINATE SESSION")
		}
		if terminateThread != "" {
			s.AppendSplitString(terminateThread, "TERMINATE THREAD")
		}
	}
	return alarmValue
}

// HandleCommand is intentionally empty: the memory-full panel offers no inline
// remediation and the header directs the operator to the memory monitor.
func (s *MemoryFull) HandleCommand(cmd *Command, line string) {}

// memFullTopMemory formats the top-3 rows of a memory panel (id, MB) into a
// single display line and returns the collected ids for the kill commands. Each
// value row is [id, sum_mb, ...] per the memory dashboard's session/thread
// panels.
func memFullTopMemory(panel MemPanel, header string) (line string, ids []string) {
	line = header
	for i, row := range panel.Value {
		if i >= 3 {
			break
		}
		if len(row) < 2 {
			continue
		}
		id := model.DisplayValue(row[0])
		mem := model.DisplayValue(row[1])
		ids = append(ids, id)
		line = fmt.Sprintf("%s  [%d] %s %sMB", line, i+1, id, mem)
	}
	return line, ids
}
