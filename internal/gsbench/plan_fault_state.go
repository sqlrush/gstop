package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type PlanFaultLiveState string

const (
	PlanFaultRestored    PlanFaultLiveState = "RESTORED"
	PlanFaultPresent     PlanFaultLiveState = "FAULT_PRESENT"
	PlanFaultDrifted     PlanFaultLiveState = "DRIFTED"
	PlanFaultUnavailable PlanFaultLiveState = "UNAVAILABLE"
)

type PlanFaultInspection struct {
	Code   ScenarioCode
	State  PlanFaultLiveState
	Object string
	Detail string
}

type planFaultCatalog interface {
	ScanReadOnly(context.Context, string, []any, ...any) error
}

func InspectPlanFaultState(
	ctx context.Context,
	db planFaultCatalog,
	schema string,
	code ScenarioCode,
) (PlanFaultInspection, error) {
	quotedSchema, catalogSchema, err := planFaultSchemaNames(schema)
	if err != nil {
		return PlanFaultInspection{}, err
	}
	if db == nil {
		return PlanFaultInspection{}, fmt.Errorf("plan fault catalog is unavailable")
	}

	switch code {
	case ScenarioCode(601):
		return inspectPlanFault601(ctx, db, quotedSchema, catalogSchema)
	case ScenarioCode(602):
		return inspectPlanFault602(ctx, db, quotedSchema)
	default:
		return PlanFaultInspection{}, fmt.Errorf(
			"scenario %03d has no live plan fault inspection", code,
		)
	}
}

func inspectPlanFault601(
	ctx context.Context,
	db planFaultCatalog,
	quotedSchema,
	catalogSchema string,
) (PlanFaultInspection, error) {
	const code = ScenarioCode(601)
	definition, ok := planIndexDefinitionByName("plan_data_lookup_idx")
	if !ok {
		return PlanFaultInspection{}, fmt.Errorf(
			"canonical plan index plan_data_lookup_idx is unavailable",
		)
	}
	expected, err := planIndexDDL(quotedSchema, definition, false)
	if err != nil {
		return PlanFaultInspection{}, err
	}
	inspection := PlanFaultInspection{
		Code:   code,
		Object: quotedSchema + "." + definition.Name,
	}

	var actual string
	err = db.ScanReadOnly(
		ctx,
		planIndexDefinitionQuery,
		[]any{catalogSchema, definition.Name},
		&actual,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		inspection.State = PlanFaultPresent
		inspection.Detail = "canonical index is absent"
		return inspection, nil
	case err != nil:
		inspection.State = PlanFaultUnavailable
		inspection.Detail = "index definition probe: " + journalSafeErrorText(err.Error())
		return inspection, nil
	case !datasetIndexMatches(actual, expected):
		inspection.State = PlanFaultDrifted
		inspection.Detail = "index definition differs from canonical shape"
		return inspection, nil
	}

	var usable int
	err = db.ScanReadOnly(
		ctx,
		`SELECT count(*) FROM pg_index WHERE indexrelid='`+
			quotedSchema+`.`+definition.Name+
			`'::regclass AND indisusable AND indisready AND indisvalid`,
		nil,
		&usable,
	)
	if err != nil {
		inspection.State = PlanFaultUnavailable
		inspection.Detail = "index usability probe: " + journalSafeErrorText(err.Error())
		return inspection, nil
	}
	if usable != 1 {
		inspection.State = PlanFaultDrifted
		inspection.Detail = "index is not usable, ready, and valid"
		return inspection, nil
	}
	inspection.State = PlanFaultRestored
	inspection.Detail = "canonical index is present and usable"
	return inspection, nil
}

func inspectPlanFault602(
	ctx context.Context,
	db planFaultCatalog,
	quotedSchema string,
) (PlanFaultInspection, error) {
	const code = ScenarioCode(602)
	inspection := PlanFaultInspection{
		Code:   code,
		Object: quotedSchema + ".plan_data.lookup_key",
	}
	var options string
	err := db.ScanReadOnly(
		ctx,
		`SELECT COALESCE(array_to_string(attoptions,','),'')
			FROM pg_attribute
			WHERE attrelid='`+quotedSchema+`.plan_data'::regclass
			  AND attname='lookup_key'`,
		nil,
		&options,
	)
	if err != nil {
		inspection.State = PlanFaultUnavailable
		inspection.Detail = "column options probe: " + journalSafeErrorText(err.Error())
		return inspection, nil
	}

	nDistinct, found := planFaultNDistinctOption(options)
	switch {
	case !found:
		inspection.State = PlanFaultRestored
		inspection.Detail = "n_distinct override is absent"
	case nDistinct == "1":
		inspection.State = PlanFaultPresent
		inspection.Detail = "n_distinct override is 1"
	default:
		inspection.State = PlanFaultDrifted
		inspection.Detail = "n_distinct override differs from fault value: " + nDistinct
	}
	return inspection, nil
}

func planFaultSchemaNames(schema string) (quoted string, catalog string, err error) {
	quoted, ok := quoteDatasetSchema(schema)
	if !ok {
		return "", "", fmt.Errorf("unsafe dataset schema %q", schema)
	}
	catalog = schema
	if strings.HasPrefix(schema, `"`) && strings.HasSuffix(schema, `"`) {
		catalog = schema[1 : len(schema)-1]
	}
	return quoted, catalog, nil
}

func planFaultNDistinctOption(options string) (string, bool) {
	for _, option := range strings.Split(options, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(option), "=")
		if found && strings.EqualFold(strings.TrimSpace(key), "n_distinct") {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}
