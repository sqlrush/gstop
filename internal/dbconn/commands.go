package dbconn

import "fmt"

// BuildWorkloadRuleCmd returns the SQL that rate-limits a statement by its
// Unique SQL ID via gs_add_workload_rule. Mirrors util.build_workload_rule_cmd,
// including the literal '10' max-concurrency argument used by the original (the
// doc comment there notes '0' would fully forbid the statement).
func BuildWorkloadRuleCmd(uniqueSQLID string) string {
	ruleName := "Rule_" + uniqueSQLID
	ruleParameter := fmt.Sprintf("{id=%s}", uniqueSQLID)
	return fmt.Sprintf(
		"SELECT gs_add_workload_rule('sqlid', '%s', '', '', '', '10', '%s');",
		ruleName, ruleParameter,
	)
}

// BuildAbortPatchCmd returns the SQL that creates an abort SQL patch for the
// given Unique SQL ID. Mirrors util.build_abort_patch_cmd.
func BuildAbortPatchCmd(uniqueSQLID string) string {
	patchName := "Patch_" + uniqueSQLID
	return fmt.Sprintf(
		"SELECT * FROM dbe_sql_util.create_abort_sql_patch('%s', %s);",
		patchName, uniqueSQLID,
	)
}
