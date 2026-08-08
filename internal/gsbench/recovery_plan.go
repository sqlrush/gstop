package gsbench

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type RecoveryPlanState string

const (
	RecoveryPending         RecoveryPlanState = "PENDING"
	RecoveryAlreadyRestored RecoveryPlanState = "ALREADY_RESTORED"
	RecoveryUnverified      RecoveryPlanState = "UNVERIFIED"
	RecoveryConflict        RecoveryPlanState = "CONFLICT"
)

type RecoveryPlanItem struct {
	RunID        string
	Sequence     int64
	ScenarioCode ScenarioCode
	Kind         ActionKind
	Target       string
	State        RecoveryPlanState
	Statements   []string
	ManualAction string
	Detail       string
}

type RecoveryPlan struct {
	Items []RecoveryPlanItem
}

type RecoveryPlanFilter struct {
	RunID        string
	ScenarioCode *ScenarioCode
}

type RecoveryVerifyFunc func(context.Context, Action) (bool, error)

func recoveryVerifierWithPlanLiveAuthority(
	base RecoveryVerifyFunc,
) RecoveryVerifyFunc {
	return func(ctx context.Context, action Action) (bool, error) {
		if action.ScenarioCode == ScenarioCode(601) ||
			action.ScenarioCode == ScenarioCode(602) {
			return false, fmt.Errorf(
				"scenario %03d recovery authority is the complete live catalog inspection",
				action.ScenarioCode,
			)
		}
		if base == nil {
			return false, fmt.Errorf("recovery verifier is unavailable")
		}
		return base(ctx, action)
	}
}

func recoveryDiscoveryErrorAllowsFallback(err error) bool {
	return err != nil &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded)
}

func recoveryBaselineErrorAllowsFinding(err error) bool {
	return recoveryDiscoveryErrorAllowsFallback(err)
}

type recoveryReadOnlyDatabase interface {
	ScanReadOnly(context.Context, string, []any, ...any) error
}

func newRecoveryActionVerifier(
	database actionSQLDatabase,
	cfg BenchConfig,
) RecoveryVerifyFunc {
	return func(ctx context.Context, action Action) (bool, error) {
		readOnly, ok := database.(recoveryReadOnlyDatabase)
		if !ok {
			return false, fmt.Errorf("database read-only verifier is unavailable")
		}
		payload, err := trustedRecoveryVerifyPayload(cfg, action)
		if err != nil {
			return false, err
		}
		var actual string
		if err := readOnly.ScanReadOnly(ctx, payload.SQL, nil, &actual); err != nil {
			return false, fmt.Errorf("run trusted read-only verifier: %w", err)
		}
		switch payload.Comparison {
		case "":
			return databaseValuesEqual(actual, payload.Expected), nil
		case sqlVerifyComparisonIndexDDL:
			return datasetIndexMatches(actual, payload.Expected), nil
		default:
			return false, fmt.Errorf(
				"trusted verifier comparison %q is unsupported",
				payload.Comparison,
			)
		}
	}
}

func trustedRecoveryVerifyPayload(
	cfg BenchConfig,
	action Action,
) (sqlActionPayload, error) {
	if action.Kind != ActionSQLMutation && action.Kind != ActionDataBaseline {
		return sqlActionPayload{}, fmt.Errorf(
			"action kind %q has no SQL recovery verifier",
			action.Kind,
		)
	}
	candidates, err := canonicalRecoveryActions(cfg, action.ScenarioCode)
	if err != nil {
		return sqlActionPayload{}, err
	}
	for _, candidate := range candidates {
		if candidate.Target != action.Target ||
			!recoveryActionsHaveSameDesiredState(candidate, action) {
			continue
		}
		trusted, err := decodeSQLActionPayload(candidate.Verify)
		if err != nil {
			return sqlActionPayload{}, fmt.Errorf(
				"decode trusted recovery verifier: %w",
				err,
			)
		}
		recorded, err := decodeSQLActionPayload(action.Verify)
		if err != nil {
			return sqlActionPayload{}, fmt.Errorf(
				"decode recorded recovery verifier: %w",
				err,
			)
		}
		if trusted.SQL != recorded.SQL ||
			trusted.Expected != recorded.Expected ||
			trusted.Comparison != recorded.Comparison ||
			trusted.SkipInverseWhenRestored != recorded.SkipInverseWhenRestored {
			return sqlActionPayload{}, fmt.Errorf(
				"recorded recovery verifier does not match the canonical definition",
			)
		}
		return trusted, nil
	}
	return sqlActionPayload{}, fmt.Errorf(
		"no trusted recovery verifier for scenario %03d target %s",
		action.ScenarioCode,
		advisoryLogValue(action.Target),
	)
}

func canonicalRecoveryActions(
	cfg BenchConfig,
	code ScenarioCode,
) ([]Action, error) {
	var mutations []Mutation
	var err error
	switch {
	case isPlanChangeCode(code):
		definition, lookupErr := DefaultScenarioCatalog().LookupCode(code)
		if lookupErr != nil {
			return nil, lookupErr
		}
		mutations, err = PlanMutationSet(
			"recovery-verifier", cfg.Data.Schema, definition.Name,
		)
	case code == 625:
		mutations, err = HardParseInvalidationMutations(
			"recovery-verifier", cfg.Data.Schema,
		)
	case code == 801:
		if _, ok := quoteDatasetSchema(cfg.Data.Schema); !ok {
			return nil, fmt.Errorf("unsafe dataset schema %q", cfg.Data.Schema)
		}
		table := cfg.Data.Schema + ".vacuum_targets"
		mutations = []Mutation{{
			RunID: "recovery-verifier", ScenarioCode: 801,
			Target: table,
			InverseSQL: "UPDATE " + table +
				" SET version=0,payload=repeat('v',900),updated_at=current_timestamp",
			VerifySQL:   "SELECT count(*) FROM " + table + " WHERE version<>0",
			VerifyValue: "0",
		}}
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	actions := make([]Action, 0, len(mutations))
	for _, mutation := range mutations {
		actions = append(actions, SQLAction(mutation))
	}
	return actions, nil
}

func recoveryActionsHaveSameDesiredState(left, right Action) bool {
	leftStatements, leftManual, leftErr := renderRecoveryAction(left)
	rightStatements, rightManual, rightErr := renderRecoveryAction(right)
	if leftErr != nil || rightErr != nil || leftManual != rightManual ||
		len(leftStatements) != len(rightStatements) {
		return false
	}
	for index := range leftStatements {
		if leftStatements[index] != rightStatements[index] {
			return false
		}
	}
	return true
}

func BuildRecoveryPlan(
	ctx context.Context,
	discovery RestoreDiscovery,
	filter RecoveryPlanFilter,
	verify RecoveryVerifyFunc,
) (RecoveryPlan, error) {
	filter.RunID = strings.TrimSpace(filter.RunID)
	if filter.RunID != "" {
		if err := validateTagComponent("restore run ID", filter.RunID); err != nil {
			return RecoveryPlan{}, err
		}
	}
	actions, conflicts := scopedRecoveryActions(discovery, filter)
	plan := RecoveryPlan{Items: conflicts}

	type plannedAction struct {
		action Action
		item   RecoveryPlanItem
	}
	planned := make([]plannedAction, 0, len(actions))
	for _, action := range actions {
		item := RecoveryPlanItem{
			RunID: action.RunID, Sequence: action.Sequence,
			ScenarioCode: action.ScenarioCode, Kind: action.Kind,
			Target: action.Target, State: RecoveryUnverified,
		}
		if verify != nil {
			restored, err := verify(ctx, action)
			switch {
			case err != nil:
				item.Detail = journalSafeErrorText(err.Error())
			case restored:
				item.State = RecoveryAlreadyRestored
			case !restored:
				item.State = RecoveryPending
			}
		}
		if item.State != RecoveryAlreadyRestored {
			statements, manual, err := renderRecoveryAction(action)
			if err != nil {
				item.State = RecoveryUnverified
				item.Detail = journalSafeErrorText(err.Error())
			} else {
				item.Statements = statements
				item.ManualAction = manual
			}
		}
		planned = append(planned, plannedAction{action: action, item: item})
	}

	// The coordinator ordering already separates session, external,
	// configuration, structural DDL, and ANALYZE stages. Keep that ordering but
	// make inverse sequence order deterministic across semantically deduplicated
	// rows from older journal formats.
	sort.SliceStable(planned, func(i, j int) bool {
		leftStage, leftPriority := restoreActionOrder(planned[i].action)
		rightStage, rightPriority := restoreActionOrder(planned[j].action)
		if leftStage != rightStage {
			return leftStage < rightStage
		}
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		if planned[i].action.RunID == planned[j].action.RunID &&
			planned[i].action.Sequence != planned[j].action.Sequence {
			return planned[i].action.Sequence > planned[j].action.Sequence
		}
		return false
	})

	seen := make(map[string]int)
	for _, candidate := range planned {
		key := recoverySemanticKey(candidate.item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = len(plan.Items)
		plan.Items = append(plan.Items, candidate.item)
	}
	return plan, nil
}

func ReconcilePlanRecoveryWithLiveState(
	plan RecoveryPlan,
	inspection PlanFaultInspection,
	schema string,
) (RecoveryPlan, error) {
	if inspection.Code != ScenarioCode(601) &&
		inspection.Code != ScenarioCode(602) {
		return RecoveryPlan{}, fmt.Errorf(
			"scenario %03d has no live plan recovery reconciliation",
			inspection.Code,
		)
	}
	if inspection.State == PlanFaultUnavailable {
		return plan, nil
	}
	statements, err := planFaultRecoveryStatements(schema, inspection)
	if err != nil {
		return RecoveryPlan{}, err
	}
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return RecoveryPlan{}, fmt.Errorf("unsafe dataset schema %q", schema)
	}

	runID := "baseline"
	var sequence int64
	items := make([]RecoveryPlanItem, 0, len(plan.Items)+1)
	for _, item := range plan.Items {
		if item.ScenarioCode != inspection.Code {
			items = append(items, item)
			continue
		}
		candidate := strings.TrimSpace(item.RunID)
		if candidate != "" && candidate != "baseline" && runID == "baseline" {
			runID = candidate
		}
		if item.Sequence > sequence {
			sequence = item.Sequence
		}
	}

	target := quotedSchema + ".plan_data_lookup_idx"
	if inspection.Code == ScenarioCode(602) {
		target = quotedSchema + ".plan_data.lookup_key"
	}
	state := RecoveryPending
	if inspection.State == PlanFaultRestored {
		state = RecoveryAlreadyRestored
	}
	items = append(items, RecoveryPlanItem{
		RunID:        runID,
		Sequence:     sequence,
		ScenarioCode: inspection.Code,
		Kind:         ActionSQLMutation,
		Target:       target,
		State:        state,
		Statements:   statements,
		Detail: "source=live_catalog state=" +
			string(inspection.State) + " " + inspection.Detail,
	})
	sortRecoveryPlanItems(items)
	return RecoveryPlan{Items: items}, nil
}

func planFaultRecoveryStatements(
	schema string,
	inspection PlanFaultInspection,
) ([]string, error) {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return nil, fmt.Errorf("unsafe dataset schema %q", schema)
	}
	if inspection.State == PlanFaultRestored ||
		inspection.State == PlanFaultUnavailable {
		return nil, nil
	}
	if inspection.State != PlanFaultPresent &&
		inspection.State != PlanFaultDrifted {
		return nil, fmt.Errorf(
			"unsupported live plan fault state %q",
			inspection.State,
		)
	}

	switch inspection.Code {
	case ScenarioCode(601):
		definition, ok := planIndexDefinitionByName("plan_data_lookup_idx")
		if !ok {
			return nil, fmt.Errorf(
				"canonical plan index plan_data_lookup_idx is unavailable",
			)
		}
		create, err := planIndexDDL(schema, definition, false)
		if err != nil {
			return nil, err
		}
		statements := []string{recoverySQLStatement(create)}
		if inspection.State == PlanFaultDrifted {
			statements = append([]string{recoverySQLStatement(
				"DROP INDEX IF EXISTS " + quotedSchema + "." + definition.Name,
			)}, statements...)
		}
		return statements, nil
	case ScenarioCode(602):
		table := quotedSchema + ".plan_data"
		return []string{
			recoverySQLStatement(
				"ALTER TABLE " + table +
					" ALTER COLUMN lookup_key RESET (n_distinct)",
			),
			recoverySQLStatement("ANALYZE " + table + "(lookup_key)"),
		}, nil
	default:
		return nil, fmt.Errorf(
			"scenario %03d has no canonical plan fault recovery",
			inspection.Code,
		)
	}
}

func planRecoveryDiscoveryCanUseLiveState(
	code ScenarioCode,
	inspection PlanFaultInspection,
) bool {
	if inspection.Code != code ||
		(code != ScenarioCode(601) && code != ScenarioCode(602)) {
		return false
	}
	switch inspection.State {
	case PlanFaultRestored, PlanFaultPresent, PlanFaultDrifted:
		return true
	default:
		return false
	}
}

func scopedRecoveryActions(
	discovery RestoreDiscovery,
	filter RecoveryPlanFilter,
) ([]Action, []RecoveryPlanItem) {
	groups := make(map[string][]Action)
	order := make([]string, 0)
	add := func(action Action) {
		if filter.RunID != "" && action.RunID != filter.RunID {
			return
		}
		if filter.ScenarioCode != nil && action.ScenarioCode != *filter.ScenarioCode {
			return
		}
		var key string
		if action.Sequence > 0 {
			key = fmt.Sprintf("state\x00%s\x00%d", action.RunID, action.Sequence)
		} else {
			identity := restoreIdentity(action)
			key = fmt.Sprintf(
				"action\x00%s\x00%s\x00%s",
				identity.runID, identity.kind, identity.target,
			)
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		for _, existing := range groups[key] {
			if sameRestoreAction(existing, action) {
				return
			}
		}
		groups[key] = append(groups[key], action)
	}
	for _, action := range discovery.DatabaseActions {
		add(action)
	}
	for _, action := range discovery.LocalActions {
		add(action)
	}

	var actions []Action
	var conflicts []RecoveryPlanItem
	for _, key := range order {
		group := groups[key]
		if len(group) == 1 {
			actions = append(actions, group[0])
			continue
		}
		for _, action := range group {
			conflicts = append(conflicts, RecoveryPlanItem{
				RunID: action.RunID, Sequence: action.Sequence,
				ScenarioCode: action.ScenarioCode, Kind: action.Kind,
				Target: action.Target, State: RecoveryConflict,
				Detail: recoveryConflictDetail(action),
			})
		}
	}
	sort.SliceStable(conflicts, func(i, j int) bool {
		if conflicts[i].RunID != conflicts[j].RunID {
			return conflicts[i].RunID < conflicts[j].RunID
		}
		if conflicts[i].Sequence != conflicts[j].Sequence {
			return conflicts[i].Sequence < conflicts[j].Sequence
		}
		return conflicts[i].Target < conflicts[j].Target
	})
	return actions, conflicts
}

func recoveryConflictDetail(action Action) string {
	statements, manual, err := renderRecoveryAction(action)
	if err != nil {
		return journalSafeErrorText(
			"conflicting desired state; SQL suppressed: " + err.Error(),
		)
	}
	desired := manual
	if desired == "" {
		desired = strings.Join(statements, " ")
	}
	return journalSafeErrorText(
		"conflicting desired state; SQL suppressed: " + desired,
	)
}

func renderRecoveryAction(action Action) ([]string, string, error) {
	if err := validateJournalStringField("target", action.Target); err != nil {
		return nil, "", err
	}
	if err := validateJournalStringField("target node", action.Node); err != nil {
		return nil, "", err
	}
	switch action.Kind {
	case ActionSQLMutation, ActionDataBaseline:
		if err := preflightSQLActionPayload(
			"inverse",
			action.Inverse,
			action.LegacySQL,
		); err != nil {
			return nil, "", err
		}
		payload, err := decodeSQLActionPayload(action.Inverse)
		if err != nil {
			return nil, "", fmt.Errorf("decode inverse SQL: %w", err)
		}
		var statements []string
		if len(payload.SessionSQL) != 0 {
			statements = append(statements, payload.SessionSQL...)
		} else if action.LegacySQL {
			statements, err = legacySQLStatements(payload.SQL)
			if err != nil {
				return nil, "", err
			}
		} else {
			statements = []string{payload.SQL}
		}
		for index, statement := range statements {
			if journalStringContainsCredentialMaterial(statement) {
				return nil, "", fmt.Errorf("inverse SQL contains credential material")
			}
			if workloadBindMarkerRE.MatchString(statement) {
				return nil, "", fmt.Errorf("inverse SQL contains unresolved bind marker")
			}
			statements[index] = recoverySQLStatement(statement)
		}
		return statements, "", nil
	default:
		if err := validateActionPayload("inverse", action.Inverse, true); err != nil {
			return nil, "", err
		}
		if journalStringContainsCredentialMaterial(string(action.Inverse)) {
			return nil, "", fmt.Errorf("manual inverse contains credential material")
		}
		inverse, err := canonicalRecoveryJSON(action.Inverse)
		if err != nil {
			return nil, "", fmt.Errorf("canonicalize manual inverse: %w", err)
		}
		return nil, fmt.Sprintf(
			"%s node=%s target=%s inverse=%s",
			action.Kind,
			advisoryLogValue(action.Node),
			advisoryLogValue(action.Target),
			inverse,
		), nil
	}
}

func recoverySQLStatement(statement string) string {
	statement = strings.TrimSpace(statement)
	for strings.HasSuffix(statement, ";") {
		statement = strings.TrimSpace(strings.TrimSuffix(statement, ";"))
	}
	return statement + ";"
}

func recoverySemanticKey(item RecoveryPlanItem) string {
	return fmt.Sprintf(
		"%03d\x00%s\x00%s\x00%s\x00%s",
		item.ScenarioCode,
		item.Kind,
		item.Target,
		strings.Join(item.Statements, "\x00"),
		item.ManualAction,
	)
}

func MergePlanBaselineFindings(
	plan RecoveryPlan,
	findings []PlanBaselineFinding,
	filter RecoveryPlanFilter,
) RecoveryPlan {
	runScopedScenarios := make(map[ScenarioCode]bool)
	if strings.TrimSpace(filter.RunID) != "" {
		for _, item := range plan.Items {
			if item.RunID == filter.RunID {
				runScopedScenarios[item.ScenarioCode] = true
			}
		}
	}
	existingTargets := make(map[string]bool, len(plan.Items))
	for _, item := range plan.Items {
		if item.Target != "" {
			existingTargets[item.Target] = true
		}
	}
	for _, finding := range findings {
		if strings.TrimSpace(filter.RunID) != "" {
			matched := false
			for _, code := range finding.ScenarioCodes {
				if runScopedScenarios[code] {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		code, selected := selectBaselineFindingScenario(
			finding.ScenarioCodes,
			filter.ScenarioCode,
		)
		if !selected || existingTargets[finding.Target] {
			continue
		}
		item := RecoveryPlanItem{
			RunID: "baseline", ScenarioCode: code,
			Kind: ActionSQLMutation, Target: finding.Target,
			State:  RecoveryPending,
			Detail: finding.Detail,
		}
		for _, statement := range finding.Statements {
			if journalStringContainsCredentialMaterial(statement) ||
				workloadBindMarkerRE.MatchString(statement) {
				item.State = RecoveryUnverified
				item.Statements = nil
				item.Detail = "baseline suggestion could not be rendered safely"
				break
			}
			item.Statements = append(
				item.Statements,
				recoverySQLStatement(statement),
			)
		}
		if len(item.Statements) == 0 {
			item.State = RecoveryUnverified
		}
		plan.Items = append(plan.Items, item)
		existingTargets[finding.Target] = true
	}
	sortRecoveryPlanItems(plan.Items)
	return plan
}

func sortRecoveryPlanItems(items []RecoveryPlanItem) {
	rank := func(item RecoveryPlanItem) int {
		if item.State == RecoveryConflict {
			return 0
		}
		hasAnalyze := false
		analyzeOnlyWithSessionSettings := true
		for _, statement := range item.Statements {
			normalized := strings.ToUpper(strings.TrimSpace(statement))
			switch {
			case strings.HasPrefix(normalized, "ANALYZE "):
				hasAnalyze = true
			case strings.HasPrefix(normalized, "SET "),
				strings.HasPrefix(normalized, "RESET "):
			default:
				analyzeOnlyWithSessionSettings = false
			}
		}
		if hasAnalyze && analyzeOnlyWithSessionSettings {
			return 2
		}
		return 1
	}
	sort.SliceStable(items, func(i, j int) bool {
		return rank(items[i]) < rank(items[j])
	})
}

func selectBaselineFindingScenario(
	codes []ScenarioCode,
	filter *ScenarioCode,
) (ScenarioCode, bool) {
	if filter != nil {
		for _, code := range codes {
			if code == *filter {
				return code, true
			}
		}
		return 0, false
	}
	if len(codes) == 0 {
		return 0, true
	}
	return codes[0], true
}

func RecoveryPlanLines(plan RecoveryPlan) []string {
	if len(plan.Items) == 0 {
		return []string{"RESTORE_PLAN_EMPTY state=ALREADY_RESTORED"}
	}
	lines := []string{"RECOVERY_PLAN display_only=true execution=operator_controlled"}
	for _, item := range plan.Items {
		lines = append(lines, fmt.Sprintf(
			"RECOVERY_ITEM state=%s run_id=%s scenario=%03d kind=%s target=%s detail=%s",
			item.State,
			advisoryLogValue(item.RunID),
			item.ScenarioCode,
			item.Kind,
			advisoryLogValue(item.Target),
			advisoryLogValue(item.Detail),
		))
		for _, statement := range item.Statements {
			lines = append(lines, "SQL "+statement)
		}
		if item.ManualAction != "" {
			lines = append(lines, "MANUAL_ACTION "+item.ManualAction)
		}
	}
	return lines
}
