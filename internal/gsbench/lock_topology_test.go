package gsbench

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func businessLockDefinitionForTest(
	t *testing.T,
	code ScenarioCode,
) LockDefinition {
	t.Helper()
	definition, ok := lockDefinitionsByCode(
		BusinessLockDefinitions("gsbench", "run-1"),
	)[code]
	if !ok {
		t.Fatalf("lock definition %d is unavailable", code)
	}
	return definition
}

func TestConfigureRowChainDefinitionBuildsSharedRootBranches(t *testing.T) {
	for _, test := range []struct {
		name       string
		sessions   int
		depth      int
		branches   []int
		waiterTags []string
	}{
		{
			name:     "tail branch",
			sessions: 8,
			depth:    3,
			branches: []int{3, 3, 1},
			waiterTags: []string{
				"chain-1-1", "chain-1-2", "chain-1-3",
				"chain-2-1", "chain-2-2", "chain-2-3",
				"chain-3-1",
			},
		},
		{
			name:     "full branches",
			sessions: 10,
			depth:    3,
			branches: []int{3, 3, 3},
			waiterTags: []string{
				"chain-1-1", "chain-1-2", "chain-1-3",
				"chain-2-1", "chain-2-2", "chain-2-3",
				"chain-3-1", "chain-3-2", "chain-3-3",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			configured, err := configureLockDefinition(
				businessLockDefinitionForTest(t, 501),
				LockWorkloadConfig{
					RowChainSessions: test.sessions,
					RowChainDepth:    test.depth,
				},
				"gsbench",
				"run-1",
			)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(configured.BranchLengths, test.branches) {
				t.Fatalf("branches=%v want=%v", configured.BranchLengths, test.branches)
			}
			if configured.RequestedSessions != test.sessions ||
				configured.RequestedChainDepth != test.depth {
				t.Fatalf("configured=%+v", configured)
			}
			if len(configured.Waiters) != test.sessions-1 ||
				len(configured.ExpectedEdges) != test.sessions-1 {
				t.Fatalf(
					"waiters=%d edges=%d",
					len(configured.Waiters),
					len(configured.ExpectedEdges),
				)
			}
			if len(configured.HolderSQL) != len(test.branches) {
				t.Fatalf(
					"holder roots=%v want one per branch %v",
					configured.HolderSQL,
					test.branches,
				)
			}
			for index, rootID := range []string{"id=1", "id=5", "id=9"} {
				if !strings.Contains(configured.HolderSQL[index], rootID) {
					t.Fatalf(
						"holder SQL %d=%q want %s",
						index,
						configured.HolderSQL[index],
						rootID,
					)
				}
			}
			gotTags := make([]string, len(configured.Waiters))
			for index, waiter := range configured.Waiters {
				gotTags[index] = waiter.Tag
			}
			if !reflect.DeepEqual(gotTags, test.waiterTags) {
				t.Fatalf("tags=%v want=%v", gotTags, test.waiterTags)
			}
			if configured.ExpectedEdges[0].BlockerTag != "blocker" ||
				configured.ExpectedEdges[test.depth].BlockerTag != "blocker" {
				t.Fatalf("edges=%+v", configured.ExpectedEdges)
			}
		})
	}
}

func TestConfigureRowChainDefinitionAssignsUniqueOwnedAndUpstreamRows(t *testing.T) {
	configured, err := configureLockDefinition(
		businessLockDefinitionForTest(t, 501),
		LockWorkloadConfig{RowChainSessions: 8, RowChainDepth: 3},
		"gsbench",
		"run-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	wantSetup := []string{"id=2", "id=3", "id=4", "id=6", "id=7", "id=8", "id=10"}
	wantWait := []string{"id=1", "id=2", "id=3", "id=5", "id=6", "id=7", "id=9"}
	wantBlocker := []string{
		"blocker", "chain-1-1", "chain-1-2",
		"blocker", "chain-2-1", "chain-2-2", "blocker",
	}
	for index, waiter := range configured.Waiters {
		if len(waiter.SetupSQL) != 1 ||
			!strings.Contains(waiter.SetupSQL[0], wantSetup[index]) {
			t.Fatalf("waiter %d setup=%v", index, waiter.SetupSQL)
		}
		if len(waiter.WaitSQL) != 1 ||
			!strings.Contains(waiter.WaitSQL[0], wantWait[index]) {
			t.Fatalf("waiter %d wait=%v", index, waiter.WaitSQL)
		}
		if waiter.BlockerTag != wantBlocker[index] {
			t.Fatalf("waiter %d blocker=%q want=%q", index, waiter.BlockerTag, wantBlocker[index])
		}
	}
}

func TestConfigureTableExclusiveDefinitionBuildsEveryWaiter(t *testing.T) {
	configured, err := configureLockDefinition(
		businessLockDefinitionForTest(t, 502),
		LockWorkloadConfig{TableExclusiveSessions: 4},
		"gsbench",
		"run-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if configured.RequestedSessions != 4 || len(configured.Waiters) != 3 {
		t.Fatalf("configured=%+v", configured)
	}
	for index, waiter := range configured.Waiters {
		wantTag := "waiter-" + string(rune('1'+index))
		if waiter.Tag != wantTag || len(waiter.SetupSQL) != 0 ||
			!reflect.DeepEqual(waiter.WaitSQL, []string{
				"SELECT count(*) FROM gsbench.lock_table_targets",
			}) {
			t.Fatalf("waiter=%+v want_tag=%q", waiter, wantTag)
		}
	}
}

func TestConfigureDDLWaitDefinitionUsesUniqueSafeColumns(t *testing.T) {
	configured, err := configureLockDefinition(
		businessLockDefinitionForTest(t, 503),
		LockWorkloadConfig{DDLWaitSessions: 4},
		"gsbench",
		"run-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	columnRE := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	columns := map[string]struct{}{}
	for index, waiter := range configured.Waiters {
		if len(waiter.WaitSQL) != 1 {
			t.Fatalf("waiter=%+v", waiter)
		}
		fields := strings.Fields(waiter.WaitSQL[0])
		if len(fields) != 7 || fields[0] != "ALTER" || fields[1] != "TABLE" ||
			fields[3] != "ADD" || fields[4] != "COLUMN" || fields[6] != "integer" {
			t.Fatalf("DDL=%q fields=%v", waiter.WaitSQL[0], fields)
		}
		column := fields[5]
		if !columnRE.MatchString(column) || strings.Contains(column, "run-1") {
			t.Fatalf("unsafe DDL column=%q", column)
		}
		if _, duplicate := columns[column]; duplicate {
			t.Fatalf("duplicate DDL column=%q", column)
		}
		columns[column] = struct{}{}
		wantTag := "waiter-" + string(rune('1'+index))
		if waiter.Tag != wantTag {
			t.Fatalf("tag=%q want=%q", waiter.Tag, wantTag)
		}
	}
}

func TestConfigureRowChainDefinitionRejectsMoreSessionsThanRows(t *testing.T) {
	_, err := configureLockDefinition(
		businessLockDefinitionForTest(t, 501),
		LockWorkloadConfig{RowChainSessions: 10_001, RowChainDepth: 5},
		"gsbench",
		"run-1",
	)
	if err == nil || !strings.Contains(err.Error(), "10,000") {
		t.Fatalf("error=%v", err)
	}
}
