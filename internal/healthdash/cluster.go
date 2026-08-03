package healthdash

import (
	"net"
	"strconv"
	"strings"

	"gstop/internal/dbconn"
)

func parseClusterNodes(rows []dbconn.Row) ([]ClusterNode, bool) {
	if len(rows) == 0 {
		return nil, false
	}
	nodes := make([]ClusterNode, 0, len(rows))
	hasCoordinator := false
	for _, row := range rows {
		typeName := strings.ToUpper(strings.TrimSpace(row.Str(1)))
		if typeName != "C" && typeName != "D" && typeName != "S" {
			continue
		}
		node := ClusterNode{
			Name: row.Str(0), Type: typeName, Host: row.Str(2), StandbyHost: row.Str(4),
			HostPrimary: rowBool(row, 6), NodePrimary: rowBool(row, 7),
			Preferred: rowBool(row, 8), Central: rowBool(row, 9),
		}
		node.Port, _ = row.Int(3)
		node.StandbyPort, _ = row.Int(5)
		node.ID, _ = row.Int(11)
		if typeName == "C" && strings.TrimSpace(node.Name) != "" {
			hasCoordinator = true
			node.ActiveKnown = true
			node.Active = rowBool(row, 10)
		}
		nodes = append(nodes, node)
	}
	if !hasCoordinator {
		return nil, false
	}
	return nodes, true
}

func parseCMStatus(output string) []ClusterComponent {
	var components []ClusterComponent
	section := ""
	for _, rawLine := range strings.Split(strings.ReplaceAll(output, "\r", ""), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = cmSectionKind(line)
			continue
		}
		if section == "" || isCMDecoration(line) {
			continue
		}
		if section == "CLUSTER" {
			if key, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(key), "cluster_state") {
				components = append(components, ClusterComponent{Kind: section, State: strings.TrimSpace(value), Raw: line})
			}
			continue
		}
		for _, segment := range strings.Split(line, "|") {
			if component, ok := parseCMComponent(section, strings.TrimSpace(segment)); ok {
				components = append(components, component)
			}
		}
	}
	return components
}

func cmSectionKind(header string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(strings.Trim(header, "[] ")), " "))
	switch normalized {
	case "cmserver state", "cm server state":
		return "CM SERVER"
	case "cm agent state", "cmagent state":
		return "CM AGENT"
	case "coordinator state", "central coordinator state":
		return "COORDINATOR"
	case "datanode state", "data node state":
		return "DATANODE"
	case "cluster state":
		return "CLUSTER"
	default:
		return ""
	}
}

func isCMDecoration(line string) bool {
	lower := strings.ToLower(line)
	if strings.HasPrefix(lower, "node ") || lower == "node" {
		return true
	}
	return strings.Trim(line, "- ") == ""
}

func parseCMComponent(kind, line string) (ClusterComponent, bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return ClusterComponent{}, false
	}
	if _, err := strconv.ParseInt(fields[0], 10, 64); err != nil {
		return ClusterComponent{}, false
	}
	component := ClusterComponent{Kind: kind, Node: fields[1], Raw: line}
	instanceIndex := -1
	for i := 2; i < len(fields); i++ {
		if net.ParseIP(fields[i]) != nil {
			component.Address = fields[i]
			continue
		}
		if _, err := strconv.ParseInt(fields[i], 10, 64); err == nil {
			component.Instance = fields[i]
			instanceIndex = i
			break
		}
	}
	if instanceIndex < 0 {
		return ClusterComponent{}, false
	}
	rest := append([]string(nil), fields[instanceIndex+1:]...)
	for len(rest) > 0 && strings.Contains(rest[0], "/") {
		rest = rest[1:]
	}
	if kind == "DATANODE" {
		marker := -1
		for i, value := range rest {
			if value == "P" || value == "S" || value == "C" {
				marker = i
				break
			}
		}
		if marker >= 0 {
			rest = rest[marker:]
			roleEnd := minInt(2, len(rest))
			component.Role = strings.Join(rest[:roleEnd], " ")
			component.State = strings.Join(rest[roleEnd:], " ")
		}
	} else if len(rest) > 0 {
		component.State = strings.Join(rest, " ")
		if kind == "CM SERVER" && (strings.EqualFold(rest[0], "primary") || strings.EqualFold(rest[0], "standby")) {
			component.Role = rest[0]
		}
	}
	return component, true
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
