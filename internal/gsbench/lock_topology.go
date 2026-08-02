package gsbench

import "fmt"

const lockTargetRows = 10_000

type LockWaiterRole struct {
	Tag           string
	SetupSQL      []string
	WaitSQL       []string
	Transactional bool
	BlockerTag    string
	Branch        int
	Depth         int
}

type LockExpectedEdge struct {
	BlockerTag string
	WaiterTag  string
	Branch     int
	Depth      int
}

func configureLockDefinition(
	definition LockDefinition,
	config LockWorkloadConfig,
	schema, runID string,
) (LockDefinition, error) {
	if !identifierRE.MatchString(schema) {
		return LockDefinition{}, fmt.Errorf("unsafe lock schema %q", schema)
	}
	if !tagComponentRE.MatchString(runID) {
		return LockDefinition{}, fmt.Errorf("unsafe lock run ID %q", runID)
	}
	sessions, depth, ok := config.For(definition.Code)
	if !ok {
		return LockDefinition{}, fmt.Errorf(
			"lock definition %d does not support configurable sessions",
			definition.Code,
		)
	}
	if sessions < 2 {
		return LockDefinition{}, fmt.Errorf("lock workload sessions must be at least 2")
	}
	if definition.Code == 501 {
		if depth < 1 || depth > 5 {
			return LockDefinition{}, fmt.Errorf("lock row chain depth must be between 1 and 5")
		}
		if sessions < depth+1 {
			return LockDefinition{}, fmt.Errorf(
				"scenario 501 requires sessions >= chain_depth + 1",
			)
		}
		requiredRows := rowChainRequiredRows(sessions, depth)
		if requiredRows > lockTargetRows {
			return LockDefinition{}, fmt.Errorf(
				"scenario 501 requires %d unique lock_targets rows but only 10,000 are available",
				requiredRows,
			)
		}
	}

	definition.RequestedSessions = sessions
	definition.RequestedChainDepth = depth
	definition.Waiters = nil
	definition.ExpectedEdges = nil
	definition.BranchLengths = nil

	switch definition.Code {
	case 501:
		configureRowChainWaiters(&definition, schema, sessions, depth)
	case 502:
		configureDirectWaiters(&definition, sessions, definition.WaiterSQL)
	case 503:
		configureDDLWaiters(&definition, schema, runID, sessions)
	default:
		return LockDefinition{}, fmt.Errorf(
			"lock definition %d does not support configurable sessions",
			definition.Code,
		)
	}
	return definition, nil
}

func rowChainRequiredRows(sessions, depth int) int {
	waiters := sessions - 1
	branches := (waiters + depth - 1) / depth
	return waiters + branches
}

func configureRowChainWaiters(
	definition *LockDefinition,
	schema string,
	sessions, maxDepth int,
) {
	definition.HolderSQL = nil
	remainingWaiters := sessions - 1
	nextRow := 1
	for branch := 1; remainingWaiters > 0; branch++ {
		branchLength := maxDepth
		if remainingWaiters < branchLength {
			branchLength = remainingWaiters
		}
		rootRow := nextRow
		nextRow++
		definition.HolderSQL = append(
			definition.HolderSQL,
			rowUpdate(schema, "lock_targets", rootRow),
		)
		definition.BranchLengths = append(
			definition.BranchLengths,
			branchLength,
		)
		blockerRow := rootRow
		blockerTag := definition.HolderTag
		for depth := 1; depth <= branchLength; depth++ {
			row := nextRow
			nextRow++
			tag := fmt.Sprintf("chain-%d-%d", branch, depth)
			definition.Waiters = append(definition.Waiters, LockWaiterRole{
				Tag:           tag,
				SetupSQL:      []string{rowUpdate(schema, "lock_targets", row)},
				WaitSQL:       []string{rowUpdate(schema, "lock_targets", blockerRow)},
				Transactional: true,
				BlockerTag:    blockerTag,
				Branch:        branch,
				Depth:         depth,
			})
			definition.ExpectedEdges = append(
				definition.ExpectedEdges,
				LockExpectedEdge{
					BlockerTag: blockerTag,
					WaiterTag:  tag,
					Branch:     branch,
					Depth:      depth,
				},
			)
			blockerRow = row
			blockerTag = tag
		}
		remainingWaiters -= branchLength
	}
}

func configureDirectWaiters(
	definition *LockDefinition,
	sessions int,
	waitSQL []string,
) {
	for index := 1; index < sessions; index++ {
		tag := fmt.Sprintf("waiter-%d", index)
		definition.Waiters = append(definition.Waiters, LockWaiterRole{
			Tag:           tag,
			WaitSQL:       append([]string(nil), waitSQL...),
			Transactional: definition.WaiterTransactional,
			BlockerTag:    definition.HolderTag,
			Branch:        index,
			Depth:         1,
		})
		definition.ExpectedEdges = append(
			definition.ExpectedEdges,
			LockExpectedEdge{
				BlockerTag: definition.HolderTag,
				WaiterTag:  tag,
				Branch:     index,
				Depth:      1,
			},
		)
	}
}

func configureDDLWaiters(
	definition *LockDefinition,
	schema, runID string,
	sessions int,
) {
	token := lockRunToken(runID)
	for index := 1; index < sessions; index++ {
		tag := fmt.Sprintf("waiter-%d", index)
		column := fmt.Sprintf("ddl_%s_%d", token, index)
		definition.Waiters = append(definition.Waiters, LockWaiterRole{
			Tag: tag,
			WaitSQL: []string{
				addColumnSQL(schema, "lock_ddl_targets", column),
			},
			Transactional: definition.WaiterTransactional,
			BlockerTag:    definition.HolderTag,
			Branch:        index,
			Depth:         1,
		})
		definition.ExpectedEdges = append(
			definition.ExpectedEdges,
			LockExpectedEdge{
				BlockerTag: definition.HolderTag,
				WaiterTag:  tag,
				Branch:     index,
				Depth:      1,
			},
		)
	}
}
