package emergency

import "fmt"

const (
	ioFullName    = "IOFull"
	ioFullHeader  = "[EMER05 - IOFull] - select the SQL id and press 'k' to terminate timed-out sessions"
	ioFullPersist = 120
)

// IOFull detects a saturated data-disk queue and surfaces the hottest waiting-IO
// SQL for termination. Port of emergency/io_full.py.
type IOFull struct {
	*Base
}

// NewIOFull builds the IO-full scenario.
func NewIOFull(deps Deps) *IOFull {
	return &IOFull{Base: NewBase(ioFullName, ioFullHeader, deps, ioFullPersist)}
}

// Analyze triggers when the data-disk average queue length (aqu-sz) is at or
// above the threshold.
func (s *IOFull) Analyze() {
	if len(s.snap.OS) <= 11 {
		s.deps.Logger.Error("invalid OS data structure for IO analysis")
		return
	}
	currAqu := dbFloat(s.osCell(11))
	if currAqu < s.deps.Cfg.GetFloat("emergency.io_full.io_aqu_sz_thresh", 20) {
		return
	}
	overtime := s.deps.Cfg.GetFloat("emergency.io_full.overtime_thresh", 50)
	s.analyzeSession("USR I/O", overtime)
	s.triggerEmergency(currAqu)
}

func (s *IOFull) triggerEmergency(currAqu float64) {
	if s.sortedTopSQL == nil {
		return
	}
	s.Trigger()
	topID := renderTopSQLTable(s.Base)
	s.reportAlarm(currAqu, topID)
}

func (s *IOFull) reportAlarm(currAqu float64, topID int64) {
	ioThresh := s.deps.Cfg.GetFloat("emergency.io_full.io_aqu_sz_thresh", 20)
	overtime := s.deps.Cfg.GetFloat("emergency.io_full.overtime_thresh", 50)

	var value string
	if len(s.overtimeSessList) != 0 {
		killCmd, ruleCmd, patchCmd := s.appendRemediationCommands(topID)
		if s.deps.Cfg.GetBool("main.support_emergency_command", false) {
			value = fmt.Sprintf("Gausstop检测到IO满，当前数据盘的IO平均队列长度：%v，IO满阈值：%v，使用以下命令快速查杀在事务内执行时间超过%vms且当前活跃会话数较多的占用IO高的会话：%s，如果查杀异常会话后又不断接入新的请求导致IO冲高，推荐使用SQL限流命令：%s，极端情况下可以使用Abort Patch阻断SQL的执行，对应命令：%s",
				pyFloat(currAqu), pyFloat(ioThresh), pyFloat(overtime), killCmd, ruleCmd, patchCmd)
		} else {
			value = fmt.Sprintf("Gausstop检测到IO满，当前数据盘的IO平均队列长度：%v，IO满阈值：%v，查杀在事务内执行时间超过%vms且当前活跃会话数较多的占用IO高的会话：%s，如果查杀异常会话后又不断接入新的请求导致IO冲高，推荐使用应用侧的SQL限流功能阻挡新流量进入，极端情况下可以使用Abort Patch阻断SQL的执行",
				pyFloat(currAqu), pyFloat(ioThresh), pyFloat(overtime), quotedList(s.overtimeSessList))
		}
	} else {
		value = fmt.Sprintf("Gausstop检测到IO满，当前数据盘的IO平均队列长度：%v，IO满阈值：%v，打开Gausstop查杀占用IO较多的会话",
			pyFloat(currAqu), pyFloat(ioThresh))
	}
	s.deps.Alarm.CheckAndReport(s.deps.Logger, ioFullName, value, true)
}

// HandleCommand runs the terminate sub-menu for the selected SQL id.
func (s *IOFull) HandleCommand(cmd *Command, line string) {
	overtime := s.deps.Cfg.GetInt("emergency.io_full.overtime_thresh", 50)
	handleFullCommand(s.Base, cmd, line, overtime)
}
