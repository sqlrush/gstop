package gsbench

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

type MutationState string

const (
	MutationPlanned       MutationState = "planned"
	MutationApplied       MutationState = "applied"
	MutationRestoring     MutationState = "restoring"
	MutationRestored      MutationState = "restored"
	MutationRestoreFailed MutationState = "restore_failed"
)

// Mutation is the one-way compatibility input used by existing SQL scenario
// builders. Journal persistence and recovery are implemented only in terms of
// Action; SQLAction performs the conversion at this boundary.
type Mutation struct {
	RunID          string
	ScenarioCode   ScenarioCode
	ActionKind     string
	TargetProduct  Product
	TargetNode     string
	TargetEndpoint string
	OriginalState  string
	ForwardAction  string
	InverseAction  string
	VerifyAction   string
	LastError      string

	Scenario    string
	Kind        string
	Target      string
	Original    string
	ForwardSQL  string
	InverseSQL  string
	VerifySQL   string
	VerifyValue string
}

// JournalEntry remains as a source-compatibility data shape for callers that
// inspect old SQL mutations. The journal engine itself stores Action values.
type JournalEntry struct {
	ID int64
	Mutation
	State MutationState
	Error string
}

type Journal struct {
	store         ActionStore
	exec          ActionExecutor
	targetProduct Product
}

func NewJournal(
	store ActionStore,
	exec ActionExecutor,
	targetProduct ...Product,
) *Journal {
	journal := &Journal{store: store, exec: exec}
	if len(targetProduct) != 0 {
		journal.targetProduct = targetProduct[0]
	}
	return journal
}

func (j *Journal) Apply(ctx context.Context, mutation Mutation) error {
	action := SQLAction(mutation)
	if !action.TargetProduct.known() && j.targetProduct.known() {
		action.TargetProduct = j.targetProduct
	}
	return j.ApplyAction(ctx, action)
}

func (j *Journal) ApplyAction(ctx context.Context, action Action) error {
	if action.LegacySQL {
		return fmt.Errorf(
			"legacy SQL provenance is reserved for loaded journal rows",
		)
	}
	action.LastError = journalSafeErrorText(action.LastError)
	if err := action.Validate(); err != nil {
		return err
	}
	if err := j.exec.Preflight(ctx, action); err != nil {
		return fmt.Errorf("preflight action %s: %w", action.Target, err)
	}
	entry, err := j.store.InsertPlanned(ctx, action)
	if err != nil {
		return fmt.Errorf("record action before apply: %w", err)
	}
	if err := j.exec.Apply(ctx, entry); err != nil {
		stateErr := j.setActionState(
			ctx, entry, MutationPlanned, journalSafeErrorText(err.Error()),
		)
		return errors.Join(
			fmt.Errorf("apply action %s: %w", entry.Target, err),
			wrapActionStateError("retain planned action", stateErr),
		)
	}
	if err := j.setActionState(ctx, entry, MutationApplied, ""); err != nil {
		return fmt.Errorf("mark action applied: %w", err)
	}
	return nil
}

// restoreCoordinatorActions is intentionally package-private: only the
// universal coordinator backend may supply grouped actions and safety order.
func (j *Journal) restoreCoordinatorActions(
	ctx context.Context,
	actions []Action,
) error {
	return j.restoreActions(ctx, append([]Action(nil), actions...))
}

func sortActionsReverse(actions []Action) {
	sort.SliceStable(actions, func(i, k int) bool {
		return actions[i].Sequence > actions[k].Sequence
	})
}

func (j *Journal) restoreActions(ctx context.Context, actions []Action) error {
	var errs []error
	for _, action := range actions {
		if action.State == MutationRestored {
			continue
		}
		if err := j.exec.Preflight(ctx, action); err != nil {
			errs = append(errs, j.markRestoreFailed(
				ctx, action, "preflight restore", err,
			))
			continue
		}
		claimed, err := j.claimAction(ctx, action)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"mark action %d restoring: %w", action.Sequence, err,
			))
			continue
		}
		if !claimed {
			continue
		}
		if err := j.exec.Restore(ctx, action); err != nil {
			errs = append(errs, j.markRestoreFailed(ctx, action, "restore", err))
			continue
		}
		if err := j.exec.VerifyRestored(ctx, action); err != nil {
			errs = append(errs, j.markRestoreFailed(ctx, action, "verify restore", err))
			continue
		}
		if err := j.setActionState(
			ctx, action, MutationRestored, "",
		); err != nil {
			errs = append(errs, fmt.Errorf(
				"mark action %d restored: %w", action.Sequence, err,
			))
		}
	}
	return errors.Join(errs...)
}

type actionClaimStore interface {
	ClaimAction(context.Context, Action) (bool, error)
}

func (j *Journal) claimAction(
	ctx context.Context,
	action Action,
) (bool, error) {
	if store, ok := j.store.(actionClaimStore); ok {
		return store.ClaimAction(ctx, action)
	}
	if err := j.setActionState(
		ctx,
		action,
		MutationRestoring,
		"",
	); err != nil {
		return false, err
	}
	return true, nil
}

func (j *Journal) markRestoreFailed(
	ctx context.Context,
	action Action,
	operation string,
	actionErr error,
) error {
	stateErr := j.setActionState(
		ctx,
		action,
		MutationRestoreFailed,
		journalSafeErrorText(actionErr.Error()),
	)
	return errors.Join(
		fmt.Errorf("%s %s: %w", operation, action.Target, actionErr),
		wrapActionStateError("mark restore failed", stateErr),
	)
}

type actionStateStore interface {
	SetActionState(
		context.Context,
		Action,
		MutationState,
		string,
	) error
}

func (j *Journal) setActionState(
	ctx context.Context,
	action Action,
	state MutationState,
	detail string,
) error {
	if store, ok := j.store.(actionStateStore); ok {
		return store.SetActionState(ctx, action, state, detail)
	}
	return j.store.SetState(
		ctx,
		action.RunID,
		action.Sequence,
		state,
		detail,
	)
}

func wrapActionStateError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func normalizeMutationCompatibility(m Mutation) Mutation {
	if m.ScenarioCode == 0 {
		m.ScenarioCode = journalScenarioCode(m.Scenario)
	}
	if m.Scenario == "" && m.ScenarioCode != 0 {
		m.Scenario = journalScenarioName(m.ScenarioCode)
	}
	if m.ActionKind == "" {
		m.ActionKind = m.Kind
	}
	if m.Kind == "" {
		m.Kind = m.ActionKind
	}
	if m.TargetEndpoint == "" {
		m.TargetEndpoint = m.Target
	}
	if m.Target == "" {
		m.Target = m.TargetEndpoint
	}
	if m.OriginalState == "" {
		m.OriginalState = m.Original
	}
	if m.Original == "" {
		m.Original = m.OriginalState
	}
	if m.ForwardAction == "" {
		m.ForwardAction = m.ForwardSQL
	}
	if m.ForwardSQL == "" {
		m.ForwardSQL = m.ForwardAction
	}
	if m.InverseAction == "" {
		m.InverseAction = m.InverseSQL
	}
	if m.InverseSQL == "" {
		m.InverseSQL = m.InverseAction
	}
	if m.VerifyAction == "" {
		m.VerifyAction = m.VerifySQL
	}
	if m.VerifySQL == "" {
		m.VerifySQL = m.VerifyAction
	}
	return m
}

func (j *Journal) StaleRunIDs(ctx context.Context) ([]string, error) {
	return j.store.StaleRuns(ctx)
}
