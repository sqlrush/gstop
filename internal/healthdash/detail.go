package healthdash

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gstop/internal/config"
	"gstop/internal/dbconn"
	"gstop/internal/sqlshape"
)

type DetailStage string

const (
	StageCaptured  DetailStage = "captured-session"
	StageHistory   DetailStage = "history"
	StageRelocate  DetailStage = "relocation"
	StagePreflight DetailStage = "preflight-cache"
	StageEstimate  DetailStage = "estimate"
	StagePlan      DetailStage = "plan"
	StageCPU       DetailStage = "cpu"
	StageASH       DetailStage = "ash"
	StageCatalog   DetailStage = "catalog"
	StageComplete  DetailStage = "complete"
)

type StageState string

const (
	StageLoading   StageState = "loading"
	StageReady     StageState = "ready"
	StageTimeout   StageState = "timeout"
	StageError     StageState = "error"
	StageCancelled StageState = "cancelled"
	StageDone      StageState = "done"
)

type StageStatus struct {
	State   StageState
	Message string
}

type PlanQuality uint8

const (
	PlanQualityNone             PlanQuality = 0
	PlanQualityExplain          PlanQuality = 1
	PlanQualityPreflight        PlanQuality = 2
	PlanQualityRelocatedRuntime PlanQuality = 3
	PlanQualityHistory          PlanQuality = 4
	PlanQualityCapturedRuntime  PlanQuality = 5
)

const (
	PlanSourceCapturedRuntime  = "当前会话实时计划"
	PlanSourceHistory          = "历史真实计划"
	PlanSourceRelocatedRuntime = "重新定位会话实时计划"
	PlanSourcePreflight        = "gsbench 预检缓存计划"
	PlanSourceExplain          = "EXPLAIN估算计划"
	PlanSourceRuntime          = PlanSourceCapturedRuntime
)

type DetailTarget struct {
	RequestID                uint64
	SQLID                    int64
	SQLText                  string
	Databases                []string
	Users                    []string
	RepresentativePID        int64
	RepresentativeSessionID  string
	RepresentativeElapsedUS  float64
	RepresentativeQueryStart time.Time
	CapturedAt               time.Time
}

type PlanPublication struct {
	Quality  PlanQuality
	Revision uint64
	Identity string
	Source   string
	Lines    []string
}

type DetailPatch struct {
	RequestID         uint64
	Stage             DetailStage
	State             StageState
	Message           string
	SQLText           string
	RunningPID        int64
	RunningSessionID  string
	Plan              *PlanPublication
	CPU               *CPUSummary
	ASH               *ASHSummary
	Tables            []TableDiagnosis
	PlanRevision      uint64
	CatalogGeneration uint64
	CatalogSource     string
	Notices           []DiagnosticNotice
	Done              bool
}

type DetailEmitter func(DetailPatch) bool

// Detail is the scrollable SQL detail page's data.
type Detail struct {
	RequestID         uint64
	Target            DetailTarget
	SQLID             int64
	SQLText           string
	Databases         []string
	Users             []string
	Schemas           []string
	RunningPID        int64
	RunningSessionID  string
	PlanSource        string
	PlanQuality       PlanQuality
	PlanIdentity      string
	PlanRevision      uint64
	CatalogGeneration uint64
	CatalogSource     string
	PlanLines         []string
	Error             string
	Loading           bool
	Complete          bool
	Stages            map[DetailStage]StageStatus
	Plan              PlanAnalysis
	Tables            []TableDiagnosis
	Runtime           RuntimeEvidence
	Notices           []DiagnosticNotice
}

type DetailQueryer interface {
	Queryer
	QueryContext(context.Context, string) []dbconn.Row
}

type catalogDiagnoseFunc func(
	context.Context,
	[]TableAccess,
	RuntimeEvidence,
	PlanAnalysis,
	string,
) catalogDiagnosisResult

type catalogDiagnosisResult struct {
	Tables  []TableDiagnosis
	Notices []DiagnosticNotice
	Failed  bool
	Message string
}

// DetailLoader resolves SQL text and the best available plan without executing
// the statement itself.
type DetailLoader struct {
	db              DetailQueryer
	now             func() time.Time
	stageTimeout    time.Duration
	catalogDiagnose catalogDiagnoseFunc
}

func NewDetailLoader(db DetailQueryer, timeout ...time.Duration) *DetailLoader {
	stageTimeout := config.DefaultCollectTimeout
	if len(timeout) > 0 && timeout[0] > 0 {
		stageTimeout = timeout[0]
	}
	loader := &DetailLoader{db: db, now: time.Now, stageTimeout: stageTimeout}
	loader.catalogDiagnose = func(
		ctx context.Context,
		accesses []TableAccess,
		runtime RuntimeEvidence,
		plan PlanAnalysis,
		sqlText string,
	) catalogDiagnosisResult {
		return loader.collectTableDiagnosesResult(ctx, accesses, runtime, plan, sqlText)
	}
	return loader
}

func NewLoadingDetail(target DetailTarget) Detail {
	return Detail{
		RequestID:  target.RequestID,
		Target:     target,
		SQLID:      target.SQLID,
		SQLText:    strings.TrimSpace(target.SQLText),
		Databases:  sortedUniqueStrings(target.Databases),
		Users:      sortedUniqueStrings(target.Users),
		RunningPID: target.RepresentativePID,
		Loading:    true,
		Stages: map[DetailStage]StageStatus{
			StageCaptured:  {State: StageLoading},
			StageHistory:   {State: StageLoading},
			StageRelocate:  {State: StageLoading},
			StagePreflight: {State: StageLoading},
			StageEstimate:  {State: StageLoading},
			StagePlan:      {State: StageLoading},
			StageCPU:       {State: StageLoading},
			StageASH:       {State: StageLoading},
			StageCatalog:   {State: StageLoading},
		},
	}
}

func MergeDetailPatch(detail *Detail, patch DetailPatch) bool {
	if detail == nil || patch.RequestID != detail.RequestID {
		return false
	}
	if patch.Stage == StageCatalog && patch.PlanRevision != 0 &&
		(patch.PlanRevision != detail.PlanRevision ||
			patch.CatalogGeneration < detail.CatalogGeneration) {
		return false
	}
	if patch.Tables != nil &&
		(patch.PlanRevision != detail.PlanRevision ||
			patch.CatalogGeneration < detail.CatalogGeneration) {
		return false
	}
	changed := false
	if detail.Stages == nil {
		detail.Stages = make(map[DetailStage]StageStatus)
	}
	if patch.Stage != "" {
		status := StageStatus{State: patch.State, Message: patch.Message}
		if detail.Stages[patch.Stage] != status {
			detail.Stages[patch.Stage] = status
			changed = true
		}
	}
	trimmedSQL := strings.TrimSpace(patch.SQLText)
	if trimmedSQL != "" && detail.SQLText != trimmedSQL {
		detail.SQLText = trimmedSQL
		changed = true
	}
	if patch.RunningPID != 0 && detail.RunningPID != patch.RunningPID {
		detail.RunningPID = patch.RunningPID
		changed = true
	}
	if patch.RunningSessionID != "" && detail.RunningSessionID != patch.RunningSessionID {
		detail.RunningSessionID = patch.RunningSessionID
		changed = true
	}
	if publication := patch.Plan; publication != nil {
		replace := publication.Quality > detail.PlanQuality ||
			(publication.Quality == detail.PlanQuality &&
				publication.Identity == detail.PlanIdentity &&
				publication.Revision > detail.PlanRevision)
		if replace {
			analysis := AnalyzePlan(publication.Lines)
			detail.PlanQuality = publication.Quality
			detail.PlanIdentity = publication.Identity
			detail.PlanRevision = publication.Revision
			detail.PlanSource = publication.Source
			detail.Plan = analysis
			detail.PlanLines = append([]string(nil), analysis.AnnotatedLines...)
			detail.Tables = nil
			detail.CatalogGeneration = 0
			detail.CatalogSource = ""
			detail.Notices = appendNotices(detail.Notices, analysis.Notices)
			detail.Stages[StagePlan] = StageStatus{State: StageReady}
			changed = true
		}
	}
	if patch.CPU != nil {
		detail.Runtime.CPU = *patch.CPU
		changed = true
	}
	if patch.ASH != nil {
		detail.Runtime.ASH = *patch.ASH
		changed = true
	}
	if patch.Stage == StageCatalog && patch.State == StageLoading &&
		patch.PlanRevision == detail.PlanRevision &&
		patch.CatalogGeneration > detail.CatalogGeneration {
		detail.CatalogGeneration = patch.CatalogGeneration
		changed = true
	}
	if patch.Stage == StageCatalog && patch.CatalogSource != "" &&
		patch.PlanRevision == detail.PlanRevision &&
		patch.CatalogGeneration >= detail.CatalogGeneration &&
		detail.CatalogSource != patch.CatalogSource {
		detail.CatalogSource = patch.CatalogSource
		changed = true
	}
	if patch.Tables != nil &&
		patch.PlanRevision == detail.PlanRevision &&
		patch.CatalogGeneration >= detail.CatalogGeneration {
		detail.Tables = append(
			make([]TableDiagnosis, 0, len(patch.Tables)),
			patch.Tables...,
		)
		schemas := make([]string, 0, len(patch.Tables))
		for _, table := range patch.Tables {
			schemas = append(schemas, table.Access.Schema)
		}
		detail.Schemas = sortedUniqueStrings(schemas)
		detail.CatalogGeneration = patch.CatalogGeneration
		changed = true
	}
	if len(patch.Notices) > 0 {
		before := len(detail.Notices)
		detail.Notices = appendNotices(detail.Notices, patch.Notices)
		changed = changed || len(detail.Notices) != before
	}
	if patch.Done && !detail.Complete {
		detail.Complete = true
		detail.Loading = false
		changed = true
	}
	return changed
}

func appendNotices(existing []DiagnosticNotice, additions []DiagnosticNotice) []DiagnosticNotice {
	seen := make(map[string]bool, len(existing)+len(additions))
	for _, notice := range existing {
		seen[notice.Area+"\x00"+notice.Message] = true
	}
	for _, notice := range additions {
		key := notice.Area + "\x00" + notice.Message
		if seen[key] {
			continue
		}
		seen[key] = true
		existing = append(existing, notice)
	}
	return existing
}

// Load is the blocking compatibility entry point used outside the TUI.
func (l *DetailLoader) Load(ctx context.Context, sqlID int64, query string) Detail {
	target := DetailTarget{RequestID: 1, SQLID: sqlID, SQLText: query}
	detail := NewLoadingDetail(target)
	l.LoadStream(ctx, target, func(patch DetailPatch) bool {
		MergeDetailPatch(&detail, patch)
		return true
	})
	if ctx != nil && ctx.Err() != nil {
		detail.Error = "SQL 详情加载已取消"
		detail.Loading = false
		detail.Notices = appendNotices(detail.Notices, []DiagnosticNotice{{
			Area: "detail", Message: detail.Error,
		}})
	} else if detail.PlanQuality == PlanQualityNone {
		for _, stage := range []DetailStage{
			StageEstimate, StagePreflight, StageHistory, StageCaptured, StageRelocate,
		} {
			if status := detail.Stages[stage]; status.Message != "" {
				detail.Error = status.Message
				break
			}
		}
	}
	return detail
}

func (l *DetailLoader) query(ctx context.Context, query string) []dbconn.Row {
	if ctx == nil || ctx.Err() != nil || l == nil || l.db == nil {
		return nil
	}
	return l.db.QueryContext(ctx, query)
}

func (l *DetailLoader) collectTableDiagnoses(
	ctx context.Context,
	accesses []TableAccess,
	runtime RuntimeEvidence,
	plan PlanAnalysis,
	detail *Detail,
) []TableDiagnosis {
	sqlText := ""
	if detail != nil {
		sqlText = detail.SQLText
	}
	result := l.collectTableDiagnosesResult(ctx, accesses, runtime, plan, sqlText)
	if detail != nil {
		detail.Notices = appendNotices(detail.Notices, result.Notices)
	}
	return result.Tables
}

type catalogQueryState struct {
	failed  bool
	message string
}

type catalogStatementClass uint8

const (
	catalogStatementUnknown catalogStatementClass = iota
	catalogStatementOrdinary
	catalogStatementMaintenance
)

func classifyCatalogStatement(sqlText string) catalogStatementClass {
	keyword, err := sqlshape.LeadingKeyword(sqlText)
	if err != nil || keyword == "" {
		return catalogStatementUnknown
	}
	if keyword == "ANALYZE" || keyword == "VACUUM" {
		return catalogStatementMaintenance
	}
	return catalogStatementOrdinary
}

func (s *catalogQueryState) query(
	l *DetailLoader,
	ctx context.Context,
	query,
	failureMessage string,
) []dbconn.Row {
	if s.failed || ctx.Err() != nil {
		return nil
	}
	rows := l.query(ctx, query)
	if rows == nil && ctx.Err() == nil {
		s.failed = true
		s.message = failureMessage
	}
	return rows
}

func (s *catalogQueryState) probeColumns(
	l *DetailLoader,
	ctx context.Context,
	schema,
	relation,
	failureMessage string,
) map[string]bool {
	result := make(map[string]bool)
	query := "SELECT a.attname FROM pg_catalog.pg_attribute a " +
		"JOIN pg_catalog.pg_class c ON c.oid = a.attrelid " +
		"JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace " +
		"WHERE n.nspname = " + sqlLiteral(schema) +
		" AND c.relname = " + sqlLiteral(relation) +
		" AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum;"
	for _, row := range s.query(l, ctx, query, failureMessage) {
		result[strings.ToLower(row.Str(0))] = true
	}
	return result
}

func (l *DetailLoader) collectTableDiagnosesResult(
	ctx context.Context,
	accesses []TableAccess,
	runtime RuntimeEvidence,
	plan PlanAnalysis,
	sqlText string,
) catalogDiagnosisResult {
	result := make([]TableDiagnosis, 0, len(accesses))
	notices := make([]DiagnosticNotice, 0)
	state := &catalogQueryState{}
	statementClass := classifyCatalogStatement(sqlText)
	maintenanceStatement := statementClass == catalogStatementMaintenance
	for _, original := range accesses {
		if ctx.Err() != nil {
			break
		}
		access := original
		if access.Schema == "" {
			resolveSQL := "SELECT n.nspname FROM pg_catalog.pg_class c " +
				"JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace " +
				"WHERE c.relname = " + sqlLiteral(access.Table) +
				" AND c.relkind IN ('r','p','f','m') " +
				"AND n.nspname NOT IN ('pg_catalog','information_schema') " +
				"ORDER BY CASE WHEN n.nspname = current_schema THEN 0 ELSE 1 END, n.nspname LIMIT 2;"
			rows := state.query(
				l, ctx, resolveSQL, "表名目录解析查询失败或无权限",
			)
			if state.failed {
				break
			}
			if len(rows) != 1 {
				reason := "无法唯一解析执行计划中的表名"
				if len(rows) == 0 {
					reason = "无法从当前目录解析执行计划中的表名"
				}
				indexDiagnosis := IndexDiagnosis{
					Assessment: IndexVerify,
					Reasons:    []string{reason},
				}
				if maintenanceStatement {
					indexDiagnosis = IndexDiagnosis{
						Assessment: IndexNotApplicable,
						Reasons: []string{
							"维护语句的表扫描不适用索引优化建议",
							reason,
						},
					}
				}
				result = append(result, TableDiagnosis{
					Access:     access,
					Index:      indexDiagnosis,
					Statistics: StatisticsAssessment{State: FreshnessVerify, Reasons: []string{reason}},
				})
				notices = appendNotice(notices, DiagnosticNotice{
					Area: "catalog", Message: reason + ": " + access.Table,
				})
				continue
			}
			access.Schema = rows[0].Str(0)
		}
		if statementClass == catalogStatementOrdinary && len(access.LocalPredicates) > 0 {
			knownColumns := state.probeColumns(
				l, ctx, access.Schema, access.Table,
				"表字段目录查询失败或无权限",
			)
			if state.failed || ctx.Err() != nil {
				break
			}
			access.Columns = orderAndDeduplicateColumns(append(
				access.Columns,
				resolveScanLocalColumnUses(access, knownColumns)...,
			))
		}

		var indexes []IndexInfo
		var indexKnown bool
		if maintenanceStatement {
			indexState := &catalogQueryState{}
			indexes, _ = l.loadIndexesCatalog(ctx, access, indexState)
			if ctx.Err() != nil {
				break
			}
			if indexState.failed {
				notices = appendNotice(notices, DiagnosticNotice{
					Area: "catalog", Message: indexState.message,
				})
			}
		} else {
			indexes, indexKnown = l.loadIndexesCatalog(ctx, access, state)
			if state.failed || ctx.Err() != nil {
				break
			}
		}
		var indexDiagnosis IndexDiagnosis
		switch statementClass {
		case catalogStatementMaintenance:
			indexDiagnosis = IndexDiagnosis{
				Assessment: IndexNotApplicable,
				Existing:   indexes,
				Reasons:    []string{"维护语句的表扫描不适用索引优化建议"},
			}
		case catalogStatementUnknown:
			indexDiagnosis = IndexDiagnosis{
				Assessment: IndexVerify,
				Existing:   indexes,
				Reasons:    []string{"无法可靠解析 SQL 语句类型，已抑制索引创建建议"},
			}
		default:
			columnStats := l.loadColumnStatisticsCatalog(ctx, access, state)
			if state.failed || ctx.Err() != nil {
				break
			}
			indexDiagnosis = AssessIndexes(access, indexes, columnStats)
			if !indexKnown {
				indexDiagnosis.Assessment = IndexVerify
				indexDiagnosis.SuggestedDDL = ""
				indexDiagnosis.Reasons = []string{"索引目录字段不可用，不能可靠判断索引策略"}
			}
		}

		statsEvidence := l.loadStatisticsEvidenceCatalog(ctx, access, state)
		if state.failed || ctx.Err() != nil {
			break
		}
		signals := PerformanceSignals{
			InHotspotSubtree: access.InHotspotSubtree,
			CPUToDBAvailable: runtime.CPU.CPUToDBAvailable,
			CPUToDBRatio:     runtime.CPU.CPUToDBRatio,
			ASHAvailable:     runtime.ASH.Available,
			ASHCPUShare:      runtime.ASH.OnCPUShare,
		}
		if plan.Hotspot != nil {
			signals.HotspotCostShare = plan.Hotspot.CostShare
		}
		statsAssessment := ClassifyStatistics(l.now(), statsEvidence, signals)
		result = append(result, TableDiagnosis{
			Access: access, Index: indexDiagnosis, Statistics: statsAssessment,
		})
	}
	if state.failed {
		notices = appendNotice(notices, DiagnosticNotice{
			Area: "catalog", Message: state.message,
		})
	}
	return catalogDiagnosisResult{
		Tables: result, Notices: notices,
		Failed: state.failed, Message: state.message,
	}
}

func (l *DetailLoader) loadIndexes(ctx context.Context, access TableAccess) ([]IndexInfo, bool) {
	return l.loadIndexesCatalog(ctx, access, &catalogQueryState{})
}

func (l *DetailLoader) loadIndexesCatalog(
	ctx context.Context,
	access TableAccess,
	state *catalogQueryState,
) ([]IndexInfo, bool) {
	columns := state.probeColumns(
		l, ctx, "pg_catalog", "pg_index",
		"pg_index 字段能力查询失败或无权限",
	)
	if state.failed || ctx.Err() != nil {
		return nil, false
	}
	if !containsColumns(columns, "indisvalid", "indisready") {
		return nil, false
	}
	usableExpression := "TRUE"
	if columns["indisusable"] {
		usableExpression = "i.indisusable"
	}
	query := "SELECT s.schemaname, s.relname, s.indexrelname, " +
		"i.indisvalid, i.indisready, " + usableExpression + ", " +
		"pg_get_indexdef(i.indexrelid), " +
		"COALESCE(pg_get_expr(i.indexprs, i.indrelid), ''), " +
		"COALESCE(pg_get_expr(i.indpred, i.indrelid), '') " +
		"FROM pg_stat_user_indexes s JOIN pg_catalog.pg_index i ON i.indexrelid = s.indexrelid " +
		"WHERE s.schemaname = " + sqlLiteral(access.Schema) +
		" AND s.relname = " + sqlLiteral(access.Table) + " ORDER BY s.indexrelname;"
	rows := state.query(l, ctx, query, "索引目录查询失败或无权限")
	indexes := make([]IndexInfo, 0, len(rows))
	for _, row := range rows {
		definition := row.Str(6)
		indexes = append(indexes, IndexInfo{
			Schema: row.Str(0), Table: row.Str(1), Name: row.Str(2),
			Valid: parseDatabaseBool(row.Col(3)), Ready: parseDatabaseBool(row.Col(4)),
			Usable: parseDatabaseBool(row.Col(5)), Columns: parseIndexColumns(definition),
			Definition: definition, Expression: row.Str(7), Predicate: row.Str(8),
		})
	}
	return indexes, true
}

func (l *DetailLoader) loadColumnStatistics(ctx context.Context, access TableAccess) map[string]ColumnStatistics {
	return l.loadColumnStatisticsCatalog(ctx, access, &catalogQueryState{})
}

func (l *DetailLoader) loadColumnStatisticsCatalog(
	ctx context.Context,
	access TableAccess,
	state *catalogQueryState,
) map[string]ColumnStatistics {
	result := make(map[string]ColumnStatistics)
	if len(access.Columns) == 0 {
		return result
	}
	columns := state.probeColumns(
		l, ctx, "pg_catalog", "pg_stats",
		"pg_stats 字段能力查询失败或无权限",
	)
	if state.failed || ctx.Err() != nil {
		return result
	}
	if !containsColumns(columns, "schemaname", "tablename", "attname", "n_distinct", "null_frac", "most_common_freqs") {
		return result
	}
	names := make([]string, 0, len(access.Columns))
	for _, use := range access.Columns {
		names = append(names, sqlLiteral(use.Column))
	}
	query := "SELECT attname, n_distinct, null_frac, most_common_freqs::text FROM pg_catalog.pg_stats " +
		"WHERE schemaname = " + sqlLiteral(access.Schema) +
		" AND tablename = " + sqlLiteral(access.Table) +
		" AND attname IN (" + strings.Join(names, ",") + ");"
	for _, row := range state.query(
		l, ctx, query, "列统计信息查询失败或无权限",
	) {
		nDistinct, nOK := row.Float(1)
		nullFraction, nullOK := row.Float(2)
		result[row.Str(0)] = ColumnStatistics{
			Available: nOK && nullOK, NDistinct: nDistinct, NullFraction: nullFraction,
			MostCommonFrequency: maximumArrayNumber(row.Str(3)),
		}
	}
	return result
}

func (l *DetailLoader) loadStatisticsEvidence(ctx context.Context, access TableAccess) StatisticsEvidence {
	return l.loadStatisticsEvidenceCatalog(ctx, access, &catalogQueryState{})
}

func (l *DetailLoader) loadStatisticsEvidenceCatalog(
	ctx context.Context,
	access TableAccess,
	state *catalogQueryState,
) StatisticsEvidence {
	columns := state.probeColumns(
		l, ctx, "pg_catalog", "pg_stat_user_tables",
		"pg_stat_user_tables 字段能力查询失败或无权限",
	)
	if state.failed || ctx.Err() != nil {
		return StatisticsEvidence{}
	}
	baseRequired := []string{"schemaname", "relname", "last_analyze", "last_autoanalyze", "n_live_tup"}
	if !containsColumns(columns, baseRequired...) {
		return StatisticsEvidence{}
	}
	var partial StatisticsEvidence
	if columns["n_mod_since_analyze"] {
		query := "SELECT last_analyze, last_autoanalyze, n_live_tup, n_mod_since_analyze " +
			"FROM pg_catalog.pg_stat_user_tables WHERE schemaname = " + sqlLiteral(access.Schema) +
			" AND relname = " + sqlLiteral(access.Table) + " LIMIT 1;"
		rows := state.query(
			l, ctx, query, "表统计信息修改量查询失败或无权限",
		)
		if state.failed || ctx.Err() != nil {
			return StatisticsEvidence{}
		}
		if len(rows) > 0 {
			partial = modifiedStatisticsEvidence(rows[0])
			threshold, scale, settingsOK := l.loadAnalyzeSettingsCatalog(ctx, state)
			if state.failed || ctx.Err() != nil {
				return StatisticsEvidence{}
			}
			if settingsOK {
				partial.AnalyzeThreshold = threshold
				partial.AnalyzeScaleFactor = scale
				return partial
			}
			partial.Available = false
		}
	}
	if !columns["last_data_changed"] {
		return partial
	}
	query := "SELECT last_analyze, last_autoanalyze, n_live_tup, last_data_changed " +
		"FROM pg_catalog.pg_stat_user_tables WHERE schemaname = " + sqlLiteral(access.Schema) +
		" AND relname = " + sqlLiteral(access.Table) + " LIMIT 1;"
	rows := state.query(
		l, ctx, query, "表统计信息时间查询失败或无权限",
	)
	if state.failed || ctx.Err() != nil {
		return StatisticsEvidence{}
	}
	if len(rows) == 0 {
		return StatisticsEvidence{}
	}
	lastAnalyze, analyzeOK := rowTimestamp(rows[0], 0)
	lastAutoAnalyze, autoOK := rowTimestamp(rows[0], 1)
	if lastAutoAnalyze.After(lastAnalyze) {
		lastAnalyze = lastAutoAnalyze
	}
	liveTuples, liveOK := rows[0].Float(2)
	lastDataChanged, _ := rowTimestamp(rows[0], 3)
	return StatisticsEvidence{
		Available:             liveOK,
		LastAnalyze:           lastAnalyze,
		LastDataChanged:       lastDataChanged,
		AnalyzeTimestampKnown: (analyzeOK || rows[0].IsNull(0)) && (autoOK || rows[0].IsNull(1)),
		TimestampOnly:         true,
		LiveTuples:            liveTuples,
	}
}

func modifiedStatisticsEvidence(row dbconn.Row) StatisticsEvidence {
	lastAnalyze, analyzeOK := rowTimestamp(row, 0)
	lastAutoAnalyze, autoOK := rowTimestamp(row, 1)
	if lastAutoAnalyze.After(lastAnalyze) {
		lastAnalyze = lastAutoAnalyze
	}
	liveTuples, liveOK := row.Float(2)
	modified, modifiedOK := row.Float(3)
	return StatisticsEvidence{
		Available:             liveOK && modifiedOK,
		LastAnalyze:           lastAnalyze,
		AnalyzeTimestampKnown: (analyzeOK || row.IsNull(0)) && (autoOK || row.IsNull(1)),
		LiveTuples:            liveTuples,
		ModifiedSinceAnalyze:  modified,
	}
}

func (l *DetailLoader) loadAnalyzeSettings(ctx context.Context) (float64, float64, bool) {
	return l.loadAnalyzeSettingsCatalog(ctx, &catalogQueryState{})
}

func (l *DetailLoader) loadAnalyzeSettingsCatalog(
	ctx context.Context,
	state *catalogQueryState,
) (float64, float64, bool) {
	columns := state.probeColumns(
		l, ctx, "pg_catalog", "pg_settings",
		"pg_settings 字段能力查询失败或无权限",
	)
	if state.failed || ctx.Err() != nil {
		return 0, 0, false
	}
	if !containsColumns(columns, "name", "setting") {
		return 0, 0, false
	}
	query := "SELECT name, setting FROM pg_catalog.pg_settings " +
		"WHERE name IN ('autovacuum_analyze_threshold','autovacuum_analyze_scale_factor') " +
		"ORDER BY name;"
	var threshold, scale float64
	var thresholdKnown, scaleKnown bool
	for _, row := range state.query(
		l, ctx, query, "统计参数查询失败或无权限",
	) {
		value, ok := row.Float(1)
		if !ok {
			continue
		}
		switch row.Str(0) {
		case "autovacuum_analyze_threshold":
			threshold, thresholdKnown = value, true
		case "autovacuum_analyze_scale_factor":
			scale, scaleKnown = value, true
		}
	}
	return threshold, scale, thresholdKnown && scaleKnown
}

func (l *DetailLoader) probeColumns(ctx context.Context, schema, relation string) map[string]bool {
	columns, _ := l.probeColumnsResult(ctx, schema, relation)
	return columns
}

func (l *DetailLoader) probeColumnsResult(
	ctx context.Context,
	schema,
	relation string,
) (map[string]bool, bool) {
	result := make(map[string]bool)
	query := "SELECT a.attname FROM pg_catalog.pg_attribute a " +
		"JOIN pg_catalog.pg_class c ON c.oid = a.attrelid " +
		"JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace " +
		"WHERE n.nspname = " + sqlLiteral(schema) +
		" AND c.relname = " + sqlLiteral(relation) +
		" AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum;"
	rows := l.query(ctx, query)
	if rows == nil && ctx.Err() == nil {
		return result, true
	}
	for _, row := range rows {
		result[strings.ToLower(row.Str(0))] = true
	}
	return result, false
}

func planLines(rows []dbconn.Row) []string {
	if rows == nil {
		return nil
	}
	var lines []string
	for _, row := range rows {
		for _, line := range strings.Split(row.Str(0), "\n") {
			if strings.TrimSpace(line) != "" {
				lines = append(lines, line)
			}
		}
	}
	return lines
}

func containsColumns(columns map[string]bool, names ...string) bool {
	for _, name := range names {
		if !columns[strings.ToLower(name)] {
			return false
		}
	}
	return true
}

func sqlLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func rowTimestamp(row dbconn.Row, column int) (time.Time, bool) {
	if value, ok := row.Time(column); ok {
		return value, true
	}
	text := strings.TrimSpace(row.Str(column))
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
	} {
		if value, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			return value, true
		}
	}
	return time.Time{}, false
}

func parseDatabaseBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return typed == "t" || strings.EqualFold(typed, "true") || typed == "1"
	case []byte:
		return parseDatabaseBool(string(typed))
	case int64:
		return typed != 0
	case int:
		return typed != 0
	}
	return false
}

func parseIndexColumns(definition string) []string {
	open := strings.Index(definition, "(")
	if open < 0 {
		return nil
	}
	depth := 0
	end := -1
	for i := open; i < len(definition); i++ {
		switch definition[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
				i = len(definition)
			}
		}
	}
	if end <= open {
		return nil
	}
	var columns []string
	var part strings.Builder
	depth = 0
	flush := func() {
		value := strings.TrimSpace(part.String())
		part.Reset()
		if value == "" {
			return
		}
		fields := strings.Fields(value)
		columns = append(columns, cleanIdentifier(fields[0]))
	}
	for _, r := range definition[open+1 : end] {
		switch r {
		case '(':
			depth++
			part.WriteRune(r)
		case ')':
			depth--
			part.WriteRune(r)
		case ',':
			if depth == 0 {
				flush()
			} else {
				part.WriteRune(r)
			}
		default:
			part.WriteRune(r)
		}
	}
	flush()
	return columns
}

var numberPattern = regexp.MustCompile(`[-+]?[0-9]*\.?[0-9]+`)

func maximumArrayNumber(value string) float64 {
	var numbers []float64
	for _, raw := range numberPattern.FindAllString(value, -1) {
		if number, err := strconv.ParseFloat(raw, 64); err == nil {
			numbers = append(numbers, number)
		}
	}
	if len(numbers) == 0 {
		return 0
	}
	sort.Float64s(numbers)
	return numbers[len(numbers)-1]
}

func appendNotice(notices []DiagnosticNotice, notice DiagnosticNotice) []DiagnosticNotice {
	for _, existing := range notices {
		if existing.Area == notice.Area && existing.Message == notice.Message {
			return notices
		}
	}
	return append(notices, notice)
}
