package healthdash

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ColumnUseKind controls the safe leading-column order of an index candidate.
type ColumnUseKind string

const (
	ColumnEquality ColumnUseKind = "equality"
	ColumnJoin     ColumnUseKind = "join"
	ColumnRange    ColumnUseKind = "range"
	ColumnOrder    ColumnUseKind = "order"
	ColumnGroup    ColumnUseKind = "group"
)

// ColumnUse is one plan-derived use of a relation column.
type ColumnUse struct {
	Column string
	Kind   ColumnUseKind
}

// ScanPredicate is a condition attached directly to one scan node. It is
// retained so unqualified identifiers can later be validated against the
// resolved table catalog without guessing across joins.
type ScanPredicate struct {
	Key        string
	Expression string
}

// TableAccess normalizes a plan scan and the predicates that refer to it.
type TableAccess struct {
	Schema               string
	Table                string
	Alias                string
	ScanType             string
	IndexName            string
	ReferencedIndexNames []string
	InHotspotSubtree     bool
	Columns              []ColumnUse
	Conditions           []string
	LocalPredicates      []ScanPredicate
}

func mergePlanIndexReferences(
	current []TableAccess,
	displayed []TableAccess,
) []TableAccess {
	result := append([]TableAccess(nil), current...)
	for i := range result {
		result[i].ReferencedIndexNames = append(
			[]string(nil), result[i].ReferencedIndexNames...,
		)
	}
	for _, historical := range displayed {
		if strings.TrimSpace(historical.IndexName) == "" {
			continue
		}
		var matches []int
		for i := range result {
			if sameTableAccess(result[i], historical) {
				matches = append(matches, i)
			}
		}
		if len(matches) != 1 {
			continue
		}
		index := matches[0]
		result[index].ReferencedIndexNames = appendUniqueFold(
			result[index].ReferencedIndexNames,
			historical.IndexName,
		)
	}
	return result
}

func sameTableAccess(left, right TableAccess) bool {
	if !strings.EqualFold(cleanIdentifier(left.Table), cleanIdentifier(right.Table)) {
		return false
	}
	if left.Schema != "" && right.Schema != "" &&
		!strings.EqualFold(cleanIdentifier(left.Schema), cleanIdentifier(right.Schema)) {
		return false
	}
	return left.Alias == "" || right.Alias == "" ||
		strings.EqualFold(cleanIdentifier(left.Alias), cleanIdentifier(right.Alias))
}

func appendUniqueFold(values []string, addition string) []string {
	addition = cleanIdentifier(addition)
	if addition == "" {
		return values
	}
	for _, value := range values {
		if strings.EqualFold(cleanIdentifier(value), addition) {
			return values
		}
	}
	return append(values, addition)
}

// IndexInfo is the catalog state of one index.
type IndexInfo struct {
	Schema     string
	Table      string
	Name       string
	Columns    []string
	Valid      bool
	Ready      bool
	Usable     bool
	Expression string
	Predicate  string
	Definition string
}

// ColumnStatistics contains the selectivity indicators used for conservative
// recommendations.
type ColumnStatistics struct {
	Available           bool
	NDistinct           float64
	NullFraction        float64
	MostCommonFrequency float64
}

// IndexAssessment is the dashboard's evidence classification.
type IndexAssessment string

const (
	IndexReasonable    IndexAssessment = "合理"
	IndexUnreasonable  IndexAssessment = "不合理"
	IndexVerify        IndexAssessment = "需要验证"
	IndexNotApplicable IndexAssessment = "不适用"
)

// IndexDiagnosis explains coverage and may include display-only example DDL.
type IndexDiagnosis struct {
	Assessment       IndexAssessment
	Reasons          []string
	Existing         []IndexInfo
	SuggestedColumns []string
	SuggestedDDL     string
}

// TableDiagnosis combines access, index, and optimizer-statistics evidence.
type TableDiagnosis struct {
	Access     TableAccess
	Index      IndexDiagnosis
	Statistics StatisticsAssessment
}

var qualifiedColumnPattern = regexp.MustCompile(
	`(?:"([^"]+)"|([A-Za-z_][A-Za-z0-9_$]*))\.(?:"([^"]+)"|([A-Za-z_][A-Za-z0-9_$]*))`,
)

var scanLocalComparisonPattern = regexp.MustCompile(
	`(?i)(?:"([^"]+)"|([A-Za-z_][A-Za-z0-9_$]*))\s*(<>|!=|<=|>=|=|<|>)`,
)

func resolveScanLocalColumnUses(
	access TableAccess,
	knownColumns map[string]bool,
) []ColumnUse {
	var result []ColumnUse
	for _, predicate := range access.LocalPredicates {
		for _, match := range scanLocalComparisonPattern.FindAllStringSubmatchIndex(
			predicate.Expression, -1,
		) {
			start := match[0]
			if scanIdentifierIsQualified(predicate.Expression, start) ||
				insideSingleQuotedLiteral(predicate.Expression, start) {
				continue
			}
			column := regexpGroup(predicate.Expression, match, 2, 4)
			column = cleanIdentifier(column)
			if !knownColumns[strings.ToLower(column)] {
				continue
			}
			operator := predicate.Expression[match[6]:match[7]]
			kind := ColumnEquality
			if strings.ContainsAny(operator, "<>") {
				kind = ColumnRange
			}
			result = append(result, ColumnUse{Column: column, Kind: kind})
		}
	}
	return orderAndDeduplicateColumns(result)
}

func scanIdentifierIsQualified(expression string, start int) bool {
	if start <= 0 {
		return false
	}
	previous := expression[start-1]
	return previous == '.' || previous == '"' || previous == '$' ||
		previous == '_' || previous >= '0' && previous <= '9' ||
		previous >= 'A' && previous <= 'Z' || previous >= 'a' && previous <= 'z'
}

func insideSingleQuotedLiteral(expression string, offset int) bool {
	quoted := false
	for i := 0; i < offset; i++ {
		if expression[i] != '\'' {
			continue
		}
		if i+1 < offset && expression[i+1] == '\'' {
			i++
			continue
		}
		quoted = !quoted
	}
	return quoted
}

// ExtractTableAccesses maps plan predicates, joins, ordering, and grouping back
// to resolved scan aliases. Ambiguous unqualified references are not invented.
func ExtractTableAccesses(plan PlanAnalysis) []TableAccess {
	var accesses []TableAccess
	aliasToAccess := make(map[string]int)
	nodeToAccess := make(map[*PlanNode]int)
	hotspotNodes := hotspotSubtree(plan)

	for _, node := range plan.Nodes {
		if node.Relation == "" || !strings.Contains(strings.ToLower(node.NodeType), "scan") {
			continue
		}
		schema, table := splitRelationName(node.Relation)
		access := TableAccess{
			Schema:           schema,
			Table:            table,
			Alias:            cleanIdentifier(node.Alias),
			ScanType:         node.NodeType,
			IndexName:        node.IndexName,
			InHotspotSubtree: hotspotNodes[node],
		}
		accesses = append(accesses, access)
		index := len(accesses) - 1
		nodeToAccess[node] = index
		for _, name := range []string{access.Alias, table, node.Relation} {
			if name != "" {
				aliasToAccess[strings.ToLower(cleanIdentifier(name))] = index
			}
		}
	}

	for _, node := range plan.Nodes {
		for key, values := range node.Metadata {
			if !isIndexEvidenceMetadata(key) {
				continue
			}
			for _, condition := range values {
				if index, ok := nodeToAccess[node]; ok && isScanLocalPredicate(key) {
					accesses[index].LocalPredicates = append(
						accesses[index].LocalPredicates,
						ScanPredicate{Key: key, Expression: strings.TrimSpace(condition)},
					)
				}
				kind := metadataColumnKind(key, condition)
				matches := qualifiedColumnPattern.FindAllStringSubmatchIndex(condition, -1)
				seenAccess := make(map[int]bool)
				for _, match := range matches {
					alias := regexpGroup(condition, match, 2, 4)
					column := regexpGroup(condition, match, 6, 8)
					index, ok := aliasToAccess[strings.ToLower(cleanIdentifier(alias))]
					if !ok {
						continue
					}
					columnKind := kind
					if key == "Filter" || key == "Index Cond" || key == "Recheck Cond" {
						columnKind = comparisonKindAfter(condition, match[1])
					}
					accesses[index].Columns = append(accesses[index].Columns, ColumnUse{
						Column: cleanIdentifier(column),
						Kind:   columnKind,
					})
					if !seenAccess[index] {
						accesses[index].Conditions = append(accesses[index].Conditions, strings.TrimSpace(condition))
						seenAccess[index] = true
					}
				}

				// A predicate attached directly to a scan may use an
				// unqualified column. Retain its raw condition for matching a
				// partial/expression index, but do not guess column names.
				if index, ok := nodeToAccess[node]; ok && !seenAccess[index] {
					accesses[index].Conditions = append(accesses[index].Conditions, strings.TrimSpace(condition))
				}
			}
		}
	}

	for i := range accesses {
		accesses[i].Columns = orderAndDeduplicateColumns(accesses[i].Columns)
		accesses[i].Conditions = deduplicateStrings(accesses[i].Conditions)
	}
	return accesses
}

func isIndexEvidenceMetadata(key string) bool {
	switch key {
	case "Filter", "Index Cond", "Recheck Cond", "Hash Cond", "Merge Cond",
		"Join Filter", "Sort Key", "Group Key":
		return true
	default:
		return false
	}
}

func isScanLocalPredicate(key string) bool {
	switch key {
	case "Filter", "Index Cond", "Recheck Cond":
		return true
	default:
		return false
	}
}

// AssessIndexes uses valid leading-prefix coverage only. SuggestedDDL is
// presentation data; this package never submits it to a database.
func AssessIndexes(access TableAccess, indexes []IndexInfo, stats map[string]ColumnStatistics) IndexDiagnosis {
	result := IndexDiagnosis{Existing: append([]IndexInfo(nil), indexes...)}
	missingReasons := missingPlanIndexReasons(access, indexes)
	ordered := orderAndDeduplicateColumns(access.Columns)
	for _, use := range ordered {
		result.SuggestedColumns = append(result.SuggestedColumns, use.Column)
	}

	var uncertainSpecial bool
	for _, index := range indexes {
		if !index.Valid || !index.Ready || !index.Usable {
			continue
		}
		nameMatchesPlan := access.IndexName != "" && strings.EqualFold(access.IndexName, index.Name)
		prefixMatches := leadingPrefixMatches(index.Columns, result.SuggestedColumns)
		special := index.Expression != "" || index.Predicate != ""
		specialRelevant := special && specialIndexMentionsCandidates(index, result.SuggestedColumns)
		if !nameMatchesPlan && !prefixMatches && !specialRelevant {
			continue
		}
		if special && !specialIndexExactlyMatches(index, access.Conditions) {
			uncertainSpecial = true
			continue
		}
		result.Assessment = IndexReasonable
		result.Reasons = appendReasons(
			missingReasons,
			fmt.Sprintf("可用索引 %s 的前导列覆盖当前访问条件", index.Name),
		)
		result.SuggestedColumns = nil
		return result
	}

	if uncertainSpecial {
		result.Assessment = IndexVerify
		result.Reasons = appendReasons(
			missingReasons,
			"表达式或部分索引与计划条件不能确定为精确匹配",
		)
		result.SuggestedColumns = nil
		return result
	}

	if len(result.SuggestedColumns) == 0 {
		if strings.Contains(strings.ToLower(access.ScanType), "seq scan") {
			result.Assessment = IndexUnreasonable
			result.Reasons = appendReasons(
				missingReasons,
				"全表扫描且没有可安全提取的索引列；应先收敛扫描范围或调整访问路径",
			)
		} else {
			result.Assessment = IndexVerify
			result.Reasons = appendReasons(missingReasons, "缺少可验证的索引条件")
		}
		return result
	}

	if len(result.SuggestedColumns) == 1 && evidentlyLowSelectivity(stats[result.SuggestedColumns[0]]) {
		result.Assessment = IndexVerify
		result.Reasons = appendReasons(
			missingReasons,
			"候选前导列选择性较低，不建议仅凭当前证据创建单列 B-tree 索引",
		)
		result.SuggestedColumns = nil
		return result
	}

	result.Assessment = IndexUnreasonable
	result.Reasons = appendReasons(
		missingReasons,
		"没有可用索引以前导列顺序覆盖当前全表扫描条件",
	)
	result.SuggestedDDL = buildIndexDDL(access, result.SuggestedColumns)
	return result
}

func missingPlanIndexReasons(access TableAccess, indexes []IndexInfo) []string {
	var names []string
	names = appendUniqueFold(names, access.IndexName)
	for _, name := range access.ReferencedIndexNames {
		names = appendUniqueFold(names, name)
	}
	var reasons []string
	for _, name := range names {
		found := false
		for _, index := range indexes {
			if strings.EqualFold(cleanIdentifier(index.Name), cleanIdentifier(name)) {
				found = true
				break
			}
		}
		if !found {
			reasons = append(reasons, fmt.Sprintf("执行计划引用的索引 %s 当前不存在", name))
		}
	}
	return reasons
}

func appendReasons(prefix []string, reasons ...string) []string {
	result := append([]string(nil), prefix...)
	return append(result, reasons...)
}

func hotspotSubtree(plan PlanAnalysis) map[*PlanNode]bool {
	result := make(map[*PlanNode]bool)
	if plan.Hotspot == nil {
		return result
	}
	var root *PlanNode
	for _, node := range plan.Nodes {
		if node.Line == plan.Hotspot.Line {
			root = node
			break
		}
	}
	var walk func(*PlanNode)
	walk = func(node *PlanNode) {
		if node == nil {
			return
		}
		result[node] = true
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(root)
	return result
}

func splitRelationName(relation string) (string, string) {
	relation = strings.TrimSpace(relation)
	parts := strings.Split(relation, ".")
	if len(parts) < 2 {
		return "", cleanIdentifier(relation)
	}
	return cleanIdentifier(strings.Join(parts[:len(parts)-1], ".")), cleanIdentifier(parts[len(parts)-1])
}

func cleanIdentifier(value string) string {
	return strings.Trim(strings.TrimSpace(value), `"`)
}

func regexpGroup(source string, indexes []int, firstStart, secondStart int) string {
	if indexes[firstStart] >= 0 {
		return source[indexes[firstStart]:indexes[firstStart+1]]
	}
	return source[indexes[secondStart]:indexes[secondStart+1]]
}

func metadataColumnKind(key, condition string) ColumnUseKind {
	switch key {
	case "Hash Cond", "Merge Cond", "Join Filter":
		return ColumnJoin
	case "Sort Key":
		return ColumnOrder
	case "Group Key":
		return ColumnGroup
	}
	if strings.ContainsAny(condition, "<>") {
		return ColumnRange
	}
	return ColumnEquality
}

func comparisonKindAfter(condition string, columnEnd int) ColumnUseKind {
	end := len(condition)
	if offset := strings.Index(strings.ToUpper(condition[columnEnd:]), " AND "); offset >= 0 {
		end = columnEnd + offset
	}
	fragment := condition[columnEnd:end]
	if strings.ContainsAny(fragment, "<>") {
		return ColumnRange
	}
	return ColumnEquality
}

func orderAndDeduplicateColumns(columns []ColumnUse) []ColumnUse {
	priority := map[ColumnUseKind]int{
		ColumnEquality: 0,
		ColumnJoin:     1,
		ColumnRange:    2,
		ColumnOrder:    3,
		ColumnGroup:    4,
	}
	best := make(map[string]ColumnUse)
	first := make(map[string]int)
	for i, column := range columns {
		key := strings.ToLower(cleanIdentifier(column.Column))
		if key == "" {
			continue
		}
		current, exists := best[key]
		if !exists || priority[column.Kind] < priority[current.Kind] {
			best[key] = ColumnUse{Column: cleanIdentifier(column.Column), Kind: column.Kind}
		}
		if !exists {
			first[key] = i
		}
	}
	result := make([]ColumnUse, 0, len(best))
	for _, column := range best {
		result = append(result, column)
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := priority[result[i].Kind], priority[result[j].Kind]
		if left != right {
			return left < right
		}
		return first[strings.ToLower(result[i].Column)] < first[strings.ToLower(result[j].Column)]
	})
	return result
}

func deduplicateStrings(values []string) []string {
	var result []string
	seen := make(map[string]bool)
	for _, value := range values {
		key := normalizeExpression(value)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, value)
	}
	return result
}

func leadingPrefixMatches(indexColumns, candidates []string) bool {
	if len(indexColumns) == 0 || len(candidates) == 0 {
		return false
	}
	limit := len(indexColumns)
	if len(candidates) < limit {
		limit = len(candidates)
	}
	for i := 0; i < limit; i++ {
		if !strings.EqualFold(cleanIdentifier(indexColumns[i]), cleanIdentifier(candidates[i])) {
			return false
		}
	}
	return limit > 0
}

func specialIndexExactlyMatches(index IndexInfo, conditions []string) bool {
	for _, required := range []string{index.Expression, index.Predicate} {
		if strings.TrimSpace(required) == "" {
			continue
		}
		match := false
		needle := normalizeExpression(required)
		for _, condition := range conditions {
			if strings.Contains(normalizeExpression(condition), needle) {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}

func specialIndexMentionsCandidates(index IndexInfo, candidates []string) bool {
	definition := normalizeExpression(index.Definition + index.Expression + index.Predicate)
	for _, candidate := range candidates {
		if strings.Contains(definition, normalizeExpression(candidate)) {
			return true
		}
	}
	return false
}

func normalizeExpression(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Join(strings.Fields(value), "")
}

func evidentlyLowSelectivity(stat ColumnStatistics) bool {
	if !stat.Available {
		return false
	}
	return stat.MostCommonFrequency >= .20 || stat.NDistinct > 0 && stat.NDistinct <= 10
}

func buildIndexDDL(access TableAccess, columns []string) string {
	if len(columns) == 0 {
		return ""
	}
	schema := access.Schema
	if schema == "" {
		schema = "public"
	}
	nameParts := append([]string{"gstop", access.Table}, columns...)
	name := strings.Join(nameParts, "_")
	quotedColumns := make([]string, 0, len(columns))
	for _, column := range columns {
		quotedColumns = append(quotedColumns, quoteIdentifier(column))
	}
	return fmt.Sprintf(
		"CREATE INDEX %s ON %s.%s (%s);",
		quoteIdentifier(name),
		quoteIdentifier(schema),
		quoteIdentifier(access.Table),
		strings.Join(quotedColumns, ", "),
	)
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
