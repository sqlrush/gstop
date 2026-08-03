package healthdash

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var (
	planCostPattern = regexp.MustCompile(
		`^(.*?)\s+\(cost=([0-9]+(?:\.[0-9]+)?)\.\.([0-9]+(?:\.[0-9]+)?)\s+rows=([0-9]+)\s+width=[0-9]+\)`,
	)
	indexScanPattern = regexp.MustCompile(
		`^(Index Only Scan|Index Scan|Bitmap Index Scan|Tid Scan)\s+using\s+(\S+)\s+on\s+(\S+)(?:\s+(\S+))?$`,
	)
	relationScanPattern = regexp.MustCompile(
		`^(Seq Scan|CStore Scan|Column Store Scan|Bitmap Heap Scan|Foreign Scan|Subquery Scan)\s+on\s+(\S+)(?:\s+(\S+))?$`,
	)
)

// DiagnosticNotice records a non-fatal diagnostic limitation.
type DiagnosticNotice struct {
	Area    string
	Message string
}

// PlanNode is one parsed text-plan operation. SelfCost is the node's
// incremental cost after subtracting the total costs of its direct children.
type PlanNode struct {
	Line          int
	Depth         int
	NodeType      string
	Relation      string
	Alias         string
	IndexName     string
	StartupCost   float64
	TotalCost     float64
	SelfCost      float64
	EstimatedRows int64
	Metadata      map[string][]string
	Children      []*PlanNode
}

// PlanHotspot describes the operation with the largest incremental self cost.
type PlanHotspot struct {
	Line        int
	Depth       int
	NodeType    string
	Relation    string
	StartupCost float64
	TotalCost   float64
	SelfCost    float64
	CostShare   float64
	Explanation string
}

// PlanAnalysis is the best-effort interpretation of a text execution plan.
type PlanAnalysis struct {
	Roots          []*PlanNode
	Nodes          []*PlanNode
	Hotspot        *PlanHotspot
	AnnotatedLines []string
	Notices        []DiagnosticNotice
}

type planStackItem struct {
	indent int
	node   *PlanNode
}

// AnalyzePlan parses PostgreSQL/openGauss/GaussDB text plans without executing
// the statement. Unknown operators are preserved rather than discarded.
func AnalyzePlan(lines []string) PlanAnalysis {
	result := PlanAnalysis{AnnotatedLines: append([]string(nil), lines...)}
	var stack []planStackItem

	for lineNumber, original := range lines {
		indent, content := planLineContent(original)
		match := planCostPattern.FindStringSubmatch(content)
		if match == nil {
			attachPlanMetadata(stack, content)
			continue
		}

		startupCost, _ := strconv.ParseFloat(match[2], 64)
		totalCost, _ := strconv.ParseFloat(match[3], 64)
		estimatedRows, _ := strconv.ParseInt(match[4], 10, 64)
		nodeType, relation, alias, indexName := parsePlanOperation(strings.TrimSpace(match[1]))
		node := &PlanNode{
			Line:          lineNumber,
			NodeType:      nodeType,
			Relation:      relation,
			Alias:         alias,
			IndexName:     indexName,
			StartupCost:   startupCost,
			TotalCost:     totalCost,
			EstimatedRows: estimatedRows,
			Metadata:      make(map[string][]string),
		}

		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		node.Depth = len(stack)
		if len(stack) == 0 {
			result.Roots = append(result.Roots, node)
		} else {
			parent := stack[len(stack)-1].node
			parent.Children = append(parent.Children, node)
		}
		result.Nodes = append(result.Nodes, node)
		stack = append(stack, planStackItem{indent: indent, node: node})
	}

	if len(result.Nodes) == 0 {
		result.Notices = append(result.Notices, DiagnosticNotice{
			Area:    "plan",
			Message: "无法解析执行计划节点，已保留原始计划文本",
		})
		return result
	}

	var totalSelfCost float64
	for _, node := range result.Nodes {
		childCost := 0.0
		for _, child := range node.Children {
			childCost += child.TotalCost
		}
		node.SelfCost = math.Max(0, node.TotalCost-childCost)
		totalSelfCost += node.SelfCost
	}

	hot := result.Nodes[0]
	for _, node := range result.Nodes[1:] {
		if planNodeHotter(node, hot) {
			hot = node
		}
	}
	share := 0.0
	if totalSelfCost > 0 {
		share = hot.SelfCost / totalSelfCost
	}
	result.Hotspot = &PlanHotspot{
		Line:        hot.Line,
		Depth:       hot.Depth,
		NodeType:    hot.NodeType,
		Relation:    hot.Relation,
		StartupCost: hot.StartupCost,
		TotalCost:   hot.TotalCost,
		SelfCost:    hot.SelfCost,
		CostShare:   share,
		Explanation: explainPlanOperation(hot),
	}
	result.AnnotatedLines[hot.Line] = "→ HOT " + result.AnnotatedLines[hot.Line]
	return result
}

func planLineContent(line string) (int, string) {
	indent := len(line) - len(strings.TrimLeft(line, " \t"))
	content := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(content, "->") {
		content = strings.TrimSpace(strings.TrimPrefix(content, "->"))
	}
	return indent, content
}

func attachPlanMetadata(stack []planStackItem, content string) {
	if len(stack) == 0 {
		return
	}
	key, value, ok := strings.Cut(strings.TrimSpace(content), ":")
	if !ok {
		return
	}
	switch key {
	case "Filter", "Index Cond", "Recheck Cond", "Hash Cond", "Merge Cond",
		"Join Filter", "Rows Removed by Filter", "Sort Key", "Group Key",
		"Output", "Distribute Key", "One-Time Filter":
		node := stack[len(stack)-1].node
		node.Metadata[key] = append(node.Metadata[key], strings.TrimSpace(value))
	}
}

func parsePlanOperation(header string) (nodeType, relation, alias, indexName string) {
	if match := indexScanPattern.FindStringSubmatch(header); match != nil {
		return match[1], match[3], match[4], match[2]
	}
	if match := relationScanPattern.FindStringSubmatch(header); match != nil {
		return match[1], match[2], match[3], ""
	}
	return header, "", "", ""
}

func planNodeHotter(candidate, current *PlanNode) bool {
	if candidate.SelfCost != current.SelfCost {
		return candidate.SelfCost > current.SelfCost
	}
	if candidate.TotalCost != current.TotalCost {
		return candidate.TotalCost > current.TotalCost
	}
	if candidate.Depth != current.Depth {
		return candidate.Depth > current.Depth
	}
	return candidate.Line < current.Line
}

func explainPlanOperation(node *PlanNode) string {
	switch {
	case strings.Contains(node.NodeType, "Seq Scan"):
		return fmt.Sprintf("全表扫描 %s", node.Relation)
	case strings.Contains(node.NodeType, "Index"):
		return fmt.Sprintf("索引访问 %s", node.Relation)
	case strings.Contains(node.NodeType, "Sort"):
		return "排序操作"
	case strings.Contains(node.NodeType, "Join"):
		return "连接操作"
	case strings.Contains(node.NodeType, "Hash"):
		return "哈希构建或聚合"
	case strings.Contains(node.NodeType, "Aggregate"):
		return "聚合操作"
	case strings.Contains(node.NodeType, "Stream"):
		return "分布式数据重分布"
	default:
		return node.NodeType
	}
}
