package gsbench

import (
	"context"
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
	_, actions, mergeErr := prepareRestorePlan(discovery, filter.RunID)
	plan := RecoveryPlan{}
	if mergeErr != nil {
		plan.Items = append(plan.Items, RecoveryPlanItem{
			State:  RecoveryConflict,
			Target: "journal_identity",
			Detail: journalSafeErrorText(mergeErr.Error()),
		})
	}

	type plannedAction struct {
		action Action
		item   RecoveryPlanItem
	}
	planned := make([]plannedAction, 0, len(actions))
	for _, action := range actions {
		if filter.ScenarioCode != nil && action.ScenarioCode != *filter.ScenarioCode {
			continue
		}
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
		return nil, fmt.Sprintf(
			"%s node=%s target=%s",
			action.Kind,
			advisoryLogValue(action.Node),
			advisoryLogValue(action.Target),
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
	existingTargets := make(map[string]bool, len(plan.Items))
	for _, item := range plan.Items {
		if item.Target != "" {
			existingTargets[item.Target] = true
		}
	}
	for _, finding := range findings {
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
	return plan
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
		return []string{"RESTORE_PLAN_EMPTY"}
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
