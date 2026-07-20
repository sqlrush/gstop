package emergency

import "fmt"

const (
	cpuFullName    = "CPUFull"
	cpuFullHeader  = "[EMER04 - CPUFull] - select the SQL id and press 'k' to terminate timed-out sessions"
	cpuFullPersist = 120
)

// CPUFull detects sustained high OS CPU and surfaces the hottest on-CPU SQL for
// termination. Port of emergency/cpu_full.py.
type CPUFull struct {
	*Base
}

// NewCPUFull builds the CPU-full scenario.
func NewCPUFull(deps Deps) *CPUFull {
	return &CPUFull{Base: NewBase(cpuFullName, cpuFullHeader, deps, cpuFullPersist)}
}

// Analyze triggers when %CPU is at or above the threshold, then aggregates the
// over-threshold on-CPU sessions.
func (s *CPUFull) Analyze() {
	if len(s.snap.OS) < 2 {
		s.deps.Logger.Error("invalid OS data structure")
		return
	}
	currCPU := dbFloat(s.osCell(1))
	if currCPU < s.deps.Cfg.GetFloat("emergency.cpu_full.os_cpu_thresh", 80) {
		return
	}
	overtime := s.deps.Cfg.GetFloat("emergency.cpu_full.overtime_thresh", 50)
	s.analyzeSession("ON CPU", overtime)
	s.triggerEmergency(currCPU)
}

// triggerEmergency renders the hot-SQL table and reports the alarm.
func (s *CPUFull) triggerEmergency(currCPU float64) {
	if s.sortedTopSQL == nil {
		return
	}
	s.Trigger()
	topID := renderTopSQLTable(s.Base)
	s.reportAlarm(currCPU, topID)
}

// reportAlarm builds and reports the CPU-full alarm, including remediation
// commands when permitted.
func (s *CPUFull) reportAlarm(currCPU float64, topID int64) {
	cpuThresh := s.deps.Cfg.GetFloat("emergency.cpu_full.os_cpu_thresh", 80)
	overtime := s.deps.Cfg.GetFloat("emergency.cpu_full.overtime_thresh", 50)

	var value string
	if len(s.overtimeSessList) != 0 {
		killCmd, ruleCmd, patchCmd := s.appendRemediationCommands(topID)
		if s.deps.Cfg.GetBool("main.support_emergency_command", false) {
			value = fmt.Sprintf("Gausstop检测到CPU满，当前CPU利用率：%v%%，CPU满阈值：%v%%，使用以下命令快速查杀在事务内执行时间超过%vms且当前活跃会话数较多的占用CPU高的会话：%s，如果查杀异常会话后又不断接入新的请求导致CPU冲高，推荐使用SQL限流命令：%s，极端情况下可以使用Abort Patch阻断SQL的执行，对应命令：%s",
				pyFloat(currCPU), pyFloat(cpuThresh), pyFloat(overtime), killCmd, ruleCmd, patchCmd)
		} else {
			value = fmt.Sprintf("Gausstop检测到CPU满，当前CPU利用率：%v%%，CPU满阈值：%v%%，查杀在事务内执行时间超过%vms且当前活跃会话数较多的占用CPU高的会话：%s，如果查杀异常会话后又不断接入新的请求导致CPU冲高，推荐使用应用侧的SQL限流功能阻挡新流量进入，极端情况下可以使用Abort Patch阻断SQL的执行",
				pyFloat(currCPU), pyFloat(cpuThresh), pyFloat(overtime), quotedList(s.overtimeSessList))
		}
	} else {
		value = fmt.Sprintf("Gausstop检测到CPU满，当前CPU利用率：%v%%，CPU满阈值：%v%%，打开Gausstop查杀占用CPU较多的会话",
			pyFloat(currCPU), pyFloat(cpuThresh))
	}
	s.deps.Alarm.CheckAndReport(s.deps.Logger, cpuFullName, value, true)
}

// HandleCommand runs the terminate sub-menu for the selected SQL id.
func (s *CPUFull) HandleCommand(cmd *Command, line string) {
	overtime := s.deps.Cfg.GetInt("emergency.cpu_full.overtime_thresh", 50)
	handleFullCommand(s.Base, cmd, line, overtime)
}
