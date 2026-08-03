package healthdash

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"

	"gstop/internal/dbcompat"
	"gstop/internal/sqlshape"
)

type planCandidate struct {
	stage    DetailStage
	state    StageState
	message  string
	quality  PlanQuality
	identity string
	source   string
	lines    []string
	notices  []DiagnosticNotice
	sqlText  string
	pid      int64
	session  string
}

func catalogPlanRank(candidate planCandidate) int {
	switch {
	case candidate.source == PlanSourceCapturedRuntime:
		return 5
	case candidate.source == PlanSourceRelocatedRuntime:
		return 4
	case candidate.stage == StageRelocate && candidate.source == PlanSourceExplain:
		return 3
	case candidate.stage == StageEstimate && candidate.source == PlanSourceExplain:
		return 2
	case candidate.source == PlanSourceHistory,
		candidate.source == PlanSourcePreflight:
		return 1
	default:
		return 0
	}
}

func bindAccessSchemas(
	accesses []TableAccess,
	relations []sqlshape.RelationRef,
) []TableAccess {
	bound := append([]TableAccess(nil), accesses...)
	for accessIndex := range bound {
		access := &bound[accessIndex]
		*access = normalizePlanAccessIdentifiers(*access)
		if strings.TrimSpace(access.Schema) != "" || strings.TrimSpace(access.Table) == "" {
			continue
		}
		matchingSchemas := make(map[string]string)
		for _, relation := range relations {
			if !sqlIdentifierMatchesPlan(relation.Table, access.Table) ||
				!relationAliasMatchesPlan(relation, *access) {
				continue
			}
			matchingSchemas[relation.Schema.Value] = relation.Schema.Value
		}
		if len(matchingSchemas) != 1 {
			continue
		}
		for _, schema := range matchingSchemas {
			access.Schema = schema
		}
	}
	return bound
}

func bindAccessSchemasWithEvidence(
	accesses []TableAccess,
	evidence sqlshape.RelationEvidence,
) []TableAccess {
	bound := append([]TableAccess(nil), accesses...)
	for index := range bound {
		bound[index] = normalizePlanAccessIdentifiers(bound[index])
		if unqualifiedRelationMayMatchAccess(evidence.Unqualified, bound[index]) {
			continue
		}
		bound[index] = bindAccessSchemas(
			[]TableAccess{bound[index]}, evidence.Qualified,
		)[0]
	}
	return bound
}

func unqualifiedRelationMayMatchAccess(
	relations []sqlshape.RelationRef,
	access TableAccess,
) bool {
	for _, relation := range relations {
		if !sqlIdentifierMatchesPlan(relation.Table, access.Table) {
			continue
		}
		if strings.TrimSpace(access.Alias) == "" {
			return true
		}
		if relation.Alias != nil && sqlIdentifierMatchesPlan(*relation.Alias, access.Alias) {
			return true
		}
		if relation.Alias == nil && sqlIdentifierMatchesPlan(relation.Table, access.Alias) {
			return true
		}
	}
	return false
}

func relationAliasMatchesPlan(relation sqlshape.RelationRef, access TableAccess) bool {
	alias := strings.TrimSpace(access.Alias)
	if alias == "" {
		return relation.Alias == nil
	}
	if relation.Alias != nil {
		return sqlIdentifierMatchesPlan(*relation.Alias, alias)
	}
	return sqlIdentifierMatchesPlan(relation.Table, alias)
}

func sqlIdentifierMatchesPlan(identifier sqlshape.Identifier, planName string) bool {
	planName = normalizePlanIdentifierComponent(planName)
	if identifier.Quoted {
		return identifier.Value == planName
	}
	return strings.EqualFold(identifier.Value, planName)
}

type planIdentifierPart struct {
	value  string
	quoted bool
}

// normalizePlanAccessIdentifiers repairs the identifier representation left by
// the text-plan extractor. In particular, splitRelationName historically split
// on every dot, including dots inside quoted table names. The remaining `"."`
// boundary is lossless, so reconstruct and parse the original qualified name.
func normalizePlanAccessIdentifiers(access TableAccess) TableAccess {
	schema := strings.TrimSpace(access.Schema)
	table := strings.TrimSpace(access.Table)

	switch {
	case schema == "":
		if parts, ok := parsePlanIdentifierPath(table); ok {
			switch len(parts) {
			case 1:
				access.Table = parts[0].value
			case 2:
				access.Schema = parts[0].value
				access.Table = parts[1].value
			}
		}
	case strings.Contains(schema, `"."`):
		// splitRelationName removed the first and last quotes after splitting a
		// quoted table containing dots. Add them back before lexical parsing.
		reconstructed := `"` + schema + "." + table + `"`
		if parts, ok := parsePlanIdentifierPath(reconstructed); ok && len(parts) == 2 {
			access.Schema = parts[0].value
			access.Table = parts[1].value
		}
	default:
		qualified := schema + "." + table
		if parts, ok := parsePlanIdentifierPath(qualified); ok && len(parts) == 2 {
			access.Schema = parts[0].value
			access.Table = parts[1].value
		} else {
			access.Schema = normalizePlanIdentifierComponent(schema)
			access.Table = normalizePlanIdentifierComponent(table)
		}
	}

	access.Schema = normalizePlanIdentifierComponent(access.Schema)
	access.Table = normalizePlanIdentifierComponent(access.Table)
	access.Alias = normalizePlanIdentifierComponent(access.Alias)
	return access
}

func normalizePlanIdentifierComponent(value string) string {
	if parts, ok := parsePlanIdentifierPath(strings.TrimSpace(value)); ok && len(parts) == 1 {
		return parts[0].value
	}
	// ExtractTableAccesses has already removed the outer quotes in some
	// paths, but doubled quote escapes remain and can still be decoded.
	return strings.ReplaceAll(cleanIdentifier(value), `""`, `"`)
}

// parsePlanIdentifierPath parses the identifier subset emitted in text plans:
// one or more unquoted or SQL-double-quoted identifiers separated by dots.
// Dots inside quotes stay in the identifier and doubled quotes decode to one.
func parsePlanIdentifierPath(value string) ([]planIdentifierPart, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false
	}

	var parts []planIdentifierPart
	for offset := 0; offset < len(value); {
		for offset < len(value) && (value[offset] == ' ' || value[offset] == '\t') {
			offset++
		}
		if offset >= len(value) {
			return nil, false
		}

		part := planIdentifierPart{}
		var decoded strings.Builder
		if value[offset] == '"' {
			part.quoted = true
			offset++
			closed := false
			for offset < len(value) {
				if value[offset] != '"' {
					decoded.WriteByte(value[offset])
					offset++
					continue
				}
				if offset+1 < len(value) && value[offset+1] == '"' {
					decoded.WriteByte('"')
					offset += 2
					continue
				}
				offset++
				closed = true
				break
			}
			if !closed {
				return nil, false
			}
		} else {
			start := offset
			for offset < len(value) && value[offset] != '.' {
				if value[offset] == '"' {
					return nil, false
				}
				offset++
			}
			decoded.WriteString(strings.TrimSpace(value[start:offset]))
		}
		if decoded.Len() == 0 {
			return nil, false
		}
		part.value = decoded.String()
		parts = append(parts, part)

		for offset < len(value) && (value[offset] == ' ' || value[offset] == '\t') {
			offset++
		}
		if offset == len(value) {
			break
		}
		if value[offset] != '.' {
			return nil, false
		}
		offset++
	}
	return parts, true
}

type evidenceResult struct {
	stage   DetailStage
	state   StageState
	message string
	cpu     *CPUSummary
	ash     *ASHSummary
	notices []DiagnosticNotice
}

type catalogResult struct {
	serial     uint64
	revision   uint64
	generation uint64
	source     string
	state      StageState
	message    string
	tables     []TableDiagnosis
	notices    []DiagnosticNotice
}

type detailPipelineEvent struct {
	candidate *planCandidate
	evidence  *evidenceResult
	catalog   *catalogResult
	done      bool
}

func (l *DetailLoader) stageContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, l.stageTimeout)
}

func (l *DetailLoader) stageOutcome(ctx context.Context, emptyMessage string) (StageState, string) {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return StageTimeout, "查询超过" + l.stageTimeout.String()
	case errors.Is(ctx.Err(), context.Canceled):
		return StageCancelled, "查询已取消"
	default:
		return StageDone, emptyMessage
	}
}

func (l *DetailLoader) explainPlanResult(
	ctx context.Context,
	statement string,
) ([]string, bool) {
	rows := l.query(ctx, "EXPLAIN (VERBOSE) "+statement)
	lines := planLines(rows)
	if len(lines) > 0 {
		return lines, false
	}
	if ctx.Err() != nil {
		return nil, rows == nil
	}
	rows = l.query(ctx, "EXPLAIN "+statement)
	return planLines(rows), rows == nil
}

func emitPatch(ctx context.Context, emit DetailEmitter, patch DetailPatch) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	return emit != nil && emit(patch)
}

func pipelineSend(ctx context.Context, events chan<- detailPipelineEvent, event detailPipelineEvent) {
	select {
	case events <- event:
	case <-ctx.Done():
	}
}

func (l *DetailLoader) LoadStream(ctx context.Context, target DetailTarget, emit DetailEmitter) {
	if ctx == nil {
		ctx = context.Background()
	}
	if emit == nil {
		return
	}
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	for _, stage := range []DetailStage{
		StageCaptured, StageHistory, StageRelocate, StagePreflight, StageEstimate,
		StageCPU, StageASH, StageCatalog,
	} {
		if !emitPatch(runCtx, emit, DetailPatch{
			RequestID: target.RequestID,
			Stage:     stage,
			State:     StageLoading,
		}) {
			return
		}
	}

	var captured validatedSession
	capturedCtx, capturedCancel := l.stageContext(runCtx)
	capturedResult := l.validateSessionResult(capturedCtx, target)
	captured, capturedOK := capturedResult.Session, capturedResult.Found
	if capturedOK {
		if !emitPatch(runCtx, emit, DetailPatch{
			RequestID:        target.RequestID,
			Stage:            StageCaptured,
			State:            StageReady,
			SQLText:          captured.Query,
			RunningPID:       captured.PID,
			RunningSessionID: captured.SessionID,
		}) {
			capturedCancel()
			return
		}
	} else {
		state, message := l.stageOutcome(capturedCtx, "采集时会话已失效或未提供")
		if capturedResult.Failed {
			state, message = StageError, "采集时会话校验查询失败或无权限"
		}
		if !emitPatch(runCtx, emit, DetailPatch{
			RequestID: target.RequestID,
			Stage:     StageCaptured,
			State:     state,
			Message:   message,
		}) {
			capturedCancel()
			return
		}
	}

	events := make(chan detailPipelineEvent, 32)
	var sourceWG sync.WaitGroup
	var catalogWG sync.WaitGroup
	var catalogCancel context.CancelFunc
	defer func() {
		if catalogCancel != nil {
			catalogCancel()
		}
		runCancel()
		sourceWG.Wait()
		catalogWG.Wait()
	}()

	var acceptedQuality PlanQuality
	var acceptedIdentity string
	var planRevision uint64
	var sequence uint64
	var currentPlan PlanAnalysis
	var catalogPlan PlanAnalysis
	var catalogPlanSource string
	var catalogPlanIdentity string
	var catalogSQLText string
	var acceptedCatalogRank int
	var currentRuntime RuntimeEvidence
	var catalogGeneration uint64
	var catalogSerial uint64
	latestCatalogDone := true
	fallbackSQL := strings.TrimSpace(target.SQLText)
	if fallbackSQL == "" && capturedOK {
		fallbackSQL = strings.TrimSpace(captured.Query)
	}

	launchCatalog := func() bool {
		if len(catalogPlan.Nodes) == 0 {
			latestCatalogDone = true
			return true
		}
		if catalogCancel != nil {
			catalogCancel()
		}
		catalogSerial++
		serial := catalogSerial
		revision := planRevision
		generation := catalogGeneration
		plan := catalogPlan
		displayPlan := currentPlan
		source := catalogPlanSource
		sqlText := catalogSQLText
		runtime := currentRuntime
		if !emitPatch(runCtx, emit, DetailPatch{
			RequestID: target.RequestID,
			Stage:     StageCatalog, State: StageLoading,
			PlanRevision: revision, CatalogGeneration: generation,
			CatalogSource: source, Tables: []TableDiagnosis{},
		}) {
			latestCatalogDone = true
			return false
		}
		catalogCtx, cancel := l.stageContext(runCtx)
		catalogCancel = cancel
		latestCatalogDone = false
		catalogWG.Add(1)
		go func() {
			defer catalogWG.Done()
			defer cancel()
			accesses := mergePlanIndexReferences(
				ExtractTableAccesses(plan),
				ExtractTableAccesses(displayPlan),
			)
			if evidence, err := sqlshape.RelationEvidenceFor(sqlText); err == nil {
				accesses = bindAccessSchemasWithEvidence(accesses, evidence)
			}
			result := l.catalogDiagnose(catalogCtx, accesses, runtime, plan, sqlText)
			state, message := l.stageOutcome(catalogCtx, "未发现可诊断的表访问")
			if result.Failed && catalogCtx.Err() == nil {
				state, message = StageError, result.Message
			} else if len(result.Tables) > 0 && catalogCtx.Err() == nil {
				state, message = StageReady, ""
			}
			pipelineSend(runCtx, events, detailPipelineEvent{catalog: &catalogResult{
				serial: serial, revision: revision, generation: generation,
				source: source,
				state:  state, message: message,
				tables: result.Tables, notices: result.Notices,
			}})
		}()
		return true
	}

	acceptCandidate := func(candidate planCandidate) bool {
		sequence++
		if len(candidate.lines) == 0 {
			return emitPatch(runCtx, emit, DetailPatch{
				RequestID:        target.RequestID,
				Stage:            candidate.stage,
				State:            candidate.state,
				Message:          candidate.message,
				SQLText:          candidate.sqlText,
				RunningPID:       candidate.pid,
				RunningSessionID: candidate.session,
				Notices:          candidate.notices,
			})
		}
		displayReplace := candidate.quality > acceptedQuality ||
			(candidate.quality == acceptedQuality &&
				candidate.identity == acceptedIdentity)
		candidatePlan := AnalyzePlan(candidate.lines)
		catalogRank := catalogPlanRank(candidate)
		catalogReplace := catalogRank > acceptedCatalogRank ||
			(catalogRank > 0 && catalogRank == acceptedCatalogRank &&
				candidate.identity == catalogPlanIdentity)
		if !displayReplace {
			if !emitPatch(runCtx, emit, DetailPatch{
				RequestID:        target.RequestID,
				Stage:            candidate.stage,
				State:            candidate.state,
				Message:          candidate.message,
				SQLText:          candidate.sqlText,
				RunningPID:       candidate.pid,
				RunningSessionID: candidate.session,
				Notices:          candidate.notices,
			}) {
				return false
			}
		} else {
			acceptedQuality = candidate.quality
			acceptedIdentity = candidate.identity
			planRevision++
			currentPlan = candidatePlan
			if !emitPatch(runCtx, emit, DetailPatch{
				RequestID:        target.RequestID,
				Stage:            candidate.stage,
				State:            StageReady,
				SQLText:          candidate.sqlText,
				RunningPID:       candidate.pid,
				RunningSessionID: candidate.session,
				Plan: &PlanPublication{
					Quality: candidate.quality, Revision: planRevision,
					Identity: candidate.identity, Source: candidate.source,
					Lines: append([]string(nil), candidate.lines...),
				},
				Notices: candidate.notices,
			}) {
				return false
			}
		}
		if catalogReplace {
			if catalogSerial > 0 && !displayReplace {
				catalogGeneration++
			}
			acceptedCatalogRank = catalogRank
			catalogPlanIdentity = candidate.identity
			catalogPlanSource = candidate.source
			catalogPlan = candidatePlan
			catalogSQLText = strings.TrimSpace(candidate.sqlText)
			if catalogSQLText == "" {
				catalogSQLText = fallbackSQL
			}
		}
		if displayReplace || catalogReplace {
			return launchCatalog()
		}
		return true
	}

	if capturedOK && dbSupportsRuntimePlan(l) {
		lines, failed := l.runtimePlanResult(capturedCtx, captured)
		state, message := l.stageOutcome(capturedCtx, "实时计划查询失败或无权限")
		if failed {
			state, message = StageError, "实时计划查询失败或无权限"
		}
		if len(lines) > 0 && capturedCtx.Err() == nil {
			state, message = StageReady, ""
		}
		if !acceptCandidate(planCandidate{
			stage: StageCaptured, state: state, message: message,
			quality:  PlanQualityCapturedRuntime,
			identity: detailSessionIdentity(captured), source: PlanSourceCapturedRuntime,
			lines: lines, sqlText: captured.Query, pid: captured.PID, session: captured.SessionID,
		}) {
			capturedCancel()
			return
		}
	}
	capturedCancel()

	startSource := func(work func(context.Context) detailPipelineEvent) {
		sourceWG.Add(1)
		go func() {
			defer sourceWG.Done()
			stageCtx, cancel := l.stageContext(runCtx)
			defer cancel()
			event := work(stageCtx)
			event.done = true
			pipelineSend(runCtx, events, event)
		}()
	}

	startSource(func(stageCtx context.Context) detailPipelineEvent {
		result := l.loadHistoricalPlanResult(stageCtx, target.SQLID)
		state, message := l.stageOutcome(stageCtx, "最近 60 分钟没有可用历史计划")
		if result.Failed {
			state, message = StageError, result.Message
		}
		if len(result.Lines) > 0 && stageCtx.Err() == nil {
			state, message = StageReady, ""
		}
		return detailPipelineEvent{candidate: &planCandidate{
			stage: StageHistory, state: state, message: message,
			quality:  PlanQualityHistory,
			identity: "history/" + strconv.FormatInt(target.SQLID, 10),
			source:   PlanSourceHistory, lines: result.Lines, notices: result.Notices,
			sqlText: fallbackSQL,
		}}
	})

	startSource(func(stageCtx context.Context) detailPipelineEvent {
		result := l.relocateSessionResult(stageCtx, target.SQLID)
		session, ok := result.Session, result.Found
		state, message := l.stageOutcome(stageCtx, "未找到仍在执行的代表性会话")
		if result.Failed {
			state, message = StageError, "活跃会话重新定位查询失败或无权限"
		}
		candidate := &planCandidate{stage: StageRelocate, state: state, message: message}
		if !ok || stageCtx.Err() != nil {
			return detailPipelineEvent{candidate: candidate}
		}
		candidate.sqlText = session.Query
		candidate.pid = session.PID
		candidate.session = session.SessionID
		if dbSupportsRuntimePlan(l) {
			var failed bool
			candidate.lines, failed = l.runtimePlanResult(stageCtx, session)
			candidate.quality = PlanQualityRelocatedRuntime
			candidate.identity = detailSessionIdentity(session)
			candidate.source = PlanSourceRelocatedRuntime
			if failed {
				candidate.state, candidate.message = StageError, "实时计划查询失败或无权限"
			}
		} else {
			statement, err := safeExplainStatement(session.Query)
			if err != nil {
				candidate.notices = []DiagnosticNotice{{Area: "plan", Message: err.Error()}}
			} else {
				var failed bool
				candidate.lines, failed = l.explainPlanResult(stageCtx, statement)
				candidate.quality = PlanQualityExplain
				candidate.identity = "explain/" + statement
				candidate.source = PlanSourceExplain
				if failed && stageCtx.Err() == nil {
					candidate.state, candidate.message = StageError, "执行计划查询失败或无权限"
				}
			}
		}
		state, message = l.stageOutcome(stageCtx, "执行计划查询失败或无权限")
		if candidate.state == StageError {
			state, message = candidate.state, candidate.message
		}
		if len(candidate.lines) > 0 && stageCtx.Err() == nil {
			state, message = StageReady, ""
		}
		candidate.state, candidate.message = state, message
		return detailPipelineEvent{candidate: candidate}
	})

	startSource(func(stageCtx context.Context) detailPipelineEvent {
		result := l.loadCachedWorkloadPlanResult(stageCtx, fallbackSQL)
		state, message := l.stageOutcome(stageCtx, "未匹配到 gsbench 场景预检计划")
		if result.Failed {
			state, message = StageError, result.Message
		}
		if len(result.Lines) > 0 && stageCtx.Err() == nil {
			state, message = StageReady, ""
		}
		return detailPipelineEvent{candidate: &planCandidate{
			stage:    StagePreflight,
			state:    state,
			message:  message,
			quality:  PlanQualityPreflight,
			identity: "preflight/" + sqlshape.Signature(fallbackSQL),
			source:   PlanSourcePreflight,
			lines:    result.Lines,
			sqlText:  fallbackSQL,
		}}
	})

	startSource(func(stageCtx context.Context) detailPipelineEvent {
		sqlText := fallbackSQL
		if sqlText == "" {
			textSQL := "SELECT query FROM dbe_perf.statement WHERE unique_sql_id = " +
				strconv.FormatInt(target.SQLID, 10) +
				" AND query IS NOT NULL AND BTRIM(query) <> '' LIMIT 1;"
			rows := l.query(stageCtx, textSQL)
			if rows == nil && stageCtx.Err() == nil {
				return detailPipelineEvent{candidate: &planCandidate{
					stage: StageEstimate, state: StageError,
					message: "SQL 文本查询失败或无权限",
				}}
			}
			if len(rows) > 0 {
				sqlText = strings.TrimSpace(rows[0].Str(0))
			}
		}
		candidate := &planCandidate{
			stage: StageEstimate, quality: PlanQualityExplain,
			identity: "explain/" + sqlText, source: PlanSourceExplain,
			sqlText: sqlText,
		}
		statement, err := safeExplainStatement(sqlText)
		if err != nil {
			candidate.notices = []DiagnosticNotice{{Area: "plan", Message: err.Error()}}
			candidate.state, candidate.message = l.stageOutcome(stageCtx, err.Error())
			return detailPipelineEvent{candidate: candidate}
		}
		candidate.identity = "explain/" + statement
		var failed bool
		candidate.lines, failed = l.explainPlanResult(stageCtx, statement)
		candidate.state, candidate.message = l.stageOutcome(stageCtx, "执行计划查询失败或无权限")
		if failed && stageCtx.Err() == nil {
			candidate.state, candidate.message = StageError, "执行计划查询失败或无权限"
		}
		if len(candidate.lines) > 0 && stageCtx.Err() == nil {
			candidate.state, candidate.message = StageReady, ""
		}
		return detailPipelineEvent{candidate: candidate}
	})

	startSource(func(stageCtx context.Context) detailPipelineEvent {
		result := l.collectCPUEvidenceResult(stageCtx, target.SQLID)
		cpu, notices := result.Summary, result.Notices
		state, message := l.stageOutcome(stageCtx, "CPU history unavailable")
		if result.Failed {
			state, message = StageError, result.Message
		}
		if cpu.Available && stageCtx.Err() == nil {
			state, message = StageReady, ""
		}
		return detailPipelineEvent{evidence: &evidenceResult{
			stage: StageCPU, state: state, message: message, cpu: &cpu, notices: notices,
		}}
	})

	startSource(func(stageCtx context.Context) detailPipelineEvent {
		result := l.collectASHEvidenceResult(stageCtx, target.SQLID)
		ash, notices := result.Summary, result.Notices
		state, message := l.stageOutcome(stageCtx, "ASH inference unavailable")
		if result.Failed {
			state, message = StageError, result.Message
		}
		if ash.Available && stageCtx.Err() == nil {
			state, message = StageReady, ""
		}
		return detailPipelineEvent{evidence: &evidenceResult{
			stage: StageASH, state: state, message: message, ash: &ash, notices: notices,
		}}
	})

	remainingSources := 6
	for remainingSources > 0 || !latestCatalogDone {
		select {
		case <-runCtx.Done():
			return
		case event := <-events:
			if event.done {
				remainingSources--
			}
			switch {
			case event.candidate != nil:
				if !acceptCandidate(*event.candidate) {
					return
				}
			case event.evidence != nil:
				evidence := event.evidence
				if evidence.cpu != nil {
					currentRuntime.CPU = *evidence.cpu
				}
				if evidence.ash != nil {
					currentRuntime.ASH = *evidence.ash
				}
				catalogGeneration++
				if !emitPatch(runCtx, emit, DetailPatch{
					RequestID: target.RequestID,
					Stage:     evidence.stage, State: evidence.state, Message: evidence.message,
					CPU: evidence.cpu, ASH: evidence.ash, Notices: evidence.notices,
				}) {
					return
				}
				if planRevision > 0 {
					if !launchCatalog() {
						return
					}
				}
			case event.catalog != nil:
				result := event.catalog
				if result.serial != catalogSerial ||
					result.revision != planRevision ||
					result.generation != catalogGeneration {
					continue
				}
				latestCatalogDone = true
				tables := result.tables
				if result.state != StageReady {
					tables = []TableDiagnosis{}
				}
				if !emitPatch(runCtx, emit, DetailPatch{
					RequestID: target.RequestID,
					Stage:     StageCatalog, State: result.state, Message: result.message,
					Tables: tables, PlanRevision: result.revision,
					CatalogGeneration: result.generation,
					CatalogSource:     result.source, Notices: result.notices,
				}) {
					return
				}
			}
		}
	}

	if planRevision == 0 {
		if !emitPatch(runCtx, emit, DetailPatch{
			RequestID: target.RequestID,
			Stage:     StagePlan, State: StageDone, Message: "没有可用执行计划",
		}) {
			return
		}
	}
	if catalogSerial == 0 {
		if !emitPatch(runCtx, emit, DetailPatch{
			RequestID: target.RequestID,
			Stage:     StageCatalog, State: StageDone, Message: "没有可诊断的执行计划",
		}) {
			return
		}
	}
	emitPatch(runCtx, emit, DetailPatch{
		RequestID: target.RequestID,
		Stage:     StageComplete, State: StageDone, Done: true,
	})
}

func detailSessionIdentity(session validatedSession) string {
	return strconv.FormatInt(session.PID, 10) + "/" + session.SessionID + "/" +
		strconv.FormatInt(session.SQLID, 10)
}

func dbSupportsRuntimePlan(l *DetailLoader) bool {
	return l != nil && l.db != nil && dbcompat.SupportsGsGetExplain(l.db.Kind())
}
