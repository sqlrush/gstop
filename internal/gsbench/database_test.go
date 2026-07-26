package gsbench

import (
	"database/sql"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestApplicationNameUsesExactRunOwnershipTag(t *testing.T) {
	got, err := ApplicationName("run-1", "tp_cpu", "7")
	if err != nil {
		t.Fatal(err)
	}
	if got != "gsbench/run-1/tp_cpu/7" {
		t.Fatalf("tag = %q", got)
	}
}

func TestApplicationNameKeepsReadableIdentityWhenItFits(t *testing.T) {
	got, err := ApplicationName(
		"run-1",
		"memory_session_context_growth",
		"prepared-control",
	)
	if err != nil {
		t.Fatal(err)
	}
	const want = "gsbench/run-1/memory_session_context_growth/prepared-control"
	if got != want {
		t.Fatalf("tag = %q, want %q", got, want)
	}
}

func TestApplicationNameKeepsLegacyLongRunWhenIdentityFits(t *testing.T) {
	const runID = "1234567890123456789012"
	got, err := ApplicationName(runID, "tp_cpu", "worker")
	if err != nil {
		t.Fatal(err)
	}
	const want = "gsbench/1234567890123456789012/tp_cpu/worker"
	if got != want {
		t.Fatalf("tag = %q, want legacy-readable %q", got, want)
	}
}

func TestApplicationNameRejectsUnsafeComponents(t *testing.T) {
	if _, err := ApplicationName("run/other", "tp_cpu", "7"); err == nil {
		t.Fatal("expected unsafe run id error")
	}
}

func TestApplicationNameBoundsCatalogScenariosAndPreservesWorkerRoles(t *testing.T) {
	const runID = "20260726T123456-abcde"
	seen := make(map[string]string)
	for _, definition := range DefaultScenarioCatalog().Definitions() {
		for _, workerID := range []string{"blocker", "waiter", "chain-2"} {
			got, err := ApplicationName(runID, definition.Name, workerID)
			if err != nil {
				t.Fatalf("%s/%s: %v", definition.Name, workerID, err)
			}
			if len(got) > 63 {
				t.Errorf("%s/%s application name is %d bytes: %q", definition.Name, workerID, len(got), got)
			}
			if !strings.HasSuffix(got, "/"+workerID) {
				t.Errorf("%s/%s lost worker role: %q", definition.Name, workerID, got)
			}
			if previous, exists := seen[got]; exists {
				t.Errorf("%s/%s collided with %s: %q", definition.Name, workerID, previous, got)
			}
			seen[got] = definition.Name + "/" + workerID

			repeated, err := ApplicationName(runID, definition.Name, workerID)
			if err != nil {
				t.Fatalf("repeat %s/%s: %v", definition.Name, workerID, err)
			}
			if repeated != got {
				t.Errorf("%s/%s application name is unstable: %q then %q", definition.Name, workerID, got, repeated)
			}
		}
	}
}

func TestApplicationNameBoundsAndDistinguishesLongValidComponents(t *testing.T) {
	inputs := [][3]string{
		{strings.Repeat("r", 80) + "a", strings.Repeat("s", 80) + "a", strings.Repeat("w", 80) + "a"},
		{strings.Repeat("r", 80) + "b", strings.Repeat("s", 80) + "a", strings.Repeat("w", 80) + "a"},
		{strings.Repeat("r", 80) + "a", strings.Repeat("s", 80) + "b", strings.Repeat("w", 80) + "a"},
		{strings.Repeat("r", 80) + "a", strings.Repeat("s", 80) + "a", strings.Repeat("w", 80) + "b"},
	}
	seen := make(map[string]bool)
	for _, input := range inputs {
		got, err := ApplicationName(input[0], input[1], input[2])
		if err != nil {
			t.Fatal(err)
		}
		if len(got) > 63 {
			t.Errorf("application name is %d bytes: %q", len(got), got)
		}
		if seen[got] {
			t.Errorf("long valid components collided at %q", got)
		}
		seen[got] = true
	}
}

func TestCompressedRunTokenCannotAliasValidRawRunInput(t *testing.T) {
	longRunID := strings.Repeat("r", 80) + "a"
	applicationName, err := ApplicationName(longRunID, "tp_cpu", "worker")
	if err != nil {
		t.Fatal(err)
	}
	emittedRunToken := strings.Split(applicationName, "/")[1]
	alias, aliasErr := ApplicationName(emittedRunToken, "tp_cpu", "worker")
	if aliasErr == nil && alias == applicationName {
		t.Fatalf("long run %q aliases raw run input %q", longRunID, emittedRunToken)
	}
	if _, _, err := TaggedSessionPredicate(emittedRunToken); err == nil {
		t.Fatalf("compressed run token %q is accepted as a stored run ID", emittedRunToken)
	}
}

func TestCompressedScenarioTokenCannotAliasValidRawScenarioInput(t *testing.T) {
	const runID = "20260726T123456-abcde"
	longScenario := strings.Repeat("scenario", 10) + "a"
	applicationName, err := ApplicationName(runID, longScenario, "blocker")
	if err != nil {
		t.Fatal(err)
	}
	emittedScenarioToken := strings.Split(applicationName, "/")[2]
	alias, aliasErr := ApplicationName(runID, emittedScenarioToken, "blocker")
	if aliasErr == nil && alias == applicationName {
		t.Fatalf("long scenario %q aliases raw scenario input %q", longScenario, emittedScenarioToken)
	}
	if _, err := taggedScenarioPattern(runID, emittedScenarioToken); err == nil {
		t.Fatalf("compressed scenario token %q is accepted as a raw scenario", emittedScenarioToken)
	}
}

func TestTaggedSessionPredicateDoesNotMatchRunPrefixCollision(t *testing.T) {
	query, args, err := TaggedSessionPredicate("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "application_name LIKE $1") {
		t.Fatalf("query = %q", query)
	}
	arg := onlyStringArgument(t, args)
	if arg != "gsbench/run-1/%" {
		t.Fatalf("arg = %q", arg)
	}
	if strings.HasPrefix("gsbench/run-10/tp_cpu/1", strings.TrimSuffix(arg, "%")) {
		t.Fatal("run-1 ownership prefix also matched run-10")
	}
}

func TestTaggedSessionPredicateIncludesLegacyLongRunPrefix(t *testing.T) {
	const runID = "1234567890123456789012"
	query, args, err := TaggedSessionPredicate(runID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "application_name LIKE $1") ||
		!strings.Contains(query, "application_name LIKE $2") {
		t.Fatalf("query = %q", query)
	}
	if strings.Contains(query, runID) {
		t.Fatalf("query interpolates stored run ID: %q", query)
	}
	const legacyPattern = "gsbench/1234567890123456789012/%"
	var foundLegacy bool
	for _, arg := range args {
		if arg == legacyPattern {
			foundLegacy = true
		}
	}
	if !foundLegacy {
		t.Fatalf("predicate args = %q, want legacy pattern %q included", args, legacyPattern)
	}
	legacyApplicationName := "gsbench/1234567890123456789012/tp_cpu/worker"
	if !strings.HasPrefix(legacyApplicationName, strings.TrimSuffix(legacyPattern, "%")) {
		t.Fatalf("predicate %q does not discover legacy application %q", legacyPattern, legacyApplicationName)
	}
}

func TestTaggedSessionPredicateAlignsWithApplicationNamesAndEscapesLIKE(t *testing.T) {
	const runID = "20260726T123456_abcd"
	query, args, err := TaggedSessionPredicate(runID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, `ESCAPE E'\\'`) {
		t.Fatalf("query has no explicit LIKE escape: %q", query)
	}
	arg := onlyStringArgument(t, args)
	if arg != `gsbench/20260726T123456\_abcd/%` {
		t.Fatalf("arg = %q", arg)
	}
	prefix := literalLIKEPrefix(t, arg)
	for _, definition := range DefaultScenarioCatalog().Definitions() {
		got, err := ApplicationName(runID, definition.Name, "worker")
		if err != nil {
			t.Fatalf("%s: %v", definition.Name, err)
		}
		if !strings.HasPrefix(serverApplicationName(got), prefix) {
			t.Errorf("run predicate %q does not match %q", arg, got)
		}
	}
}

func TestScenarioPatternAlignsWithBoundedApplicationName(t *testing.T) {
	const (
		runID         = "20260726T123456-abcde"
		scenario      = "lockmode_shareupdateexclusive_accessexclusive"
		otherScenario = "lockmode_shareupdateexclusive_exclusive"
	)
	pattern, err := taggedScenarioPattern(runID, scenario)
	if err != nil {
		t.Fatal(err)
	}
	prefix := literalLIKEPrefix(t, pattern)
	got, err := ApplicationName(runID, scenario, "blocker")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(serverApplicationName(got), prefix) {
		t.Fatalf("scenario pattern %q does not match server application name %q", pattern, serverApplicationName(got))
	}
	other, err := ApplicationName(runID, otherScenario, "blocker")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(serverApplicationName(other), prefix) {
		t.Fatalf("scenario pattern %q also matches %q", pattern, serverApplicationName(other))
	}
}

func onlyStringArgument(t *testing.T, args []any) string {
	t.Helper()
	if len(args) != 1 {
		t.Fatalf("args = %q, want one string", args)
	}
	arg, ok := args[0].(string)
	if !ok {
		t.Fatalf("arg type = %T, want string", args[0])
	}
	return arg
}

func serverApplicationName(applicationName string) string {
	if len(applicationName) > 63 {
		return applicationName[:63]
	}
	return applicationName
}

func literalLIKEPrefix(t *testing.T, pattern string) string {
	t.Helper()
	if !strings.HasSuffix(pattern, "%") {
		t.Fatalf("LIKE pattern has no trailing wildcard: %q", pattern)
	}
	pattern = strings.TrimSuffix(pattern, "%")
	var prefix strings.Builder
	for index := 0; index < len(pattern); index++ {
		switch pattern[index] {
		case '\\':
			index++
			if index == len(pattern) {
				t.Fatalf("LIKE pattern ends in an escape: %q", pattern)
			}
			prefix.WriteByte(pattern[index])
		case '%', '_':
			t.Fatalf("LIKE pattern contains an unescaped wildcard: %q", pattern)
		default:
			prefix.WriteByte(pattern[index])
		}
	}
	return prefix.String()
}

func TestNormalizeConnectionCloseErrorIgnoresAlreadyClosedResources(t *testing.T) {
	for _, err := range []error{
		sql.ErrConnDone,
		net.ErrClosed,
		&net.OpError{Op: "write", Net: "tcp", Err: net.ErrClosed},
	} {
		if got := normalizeConnectionCloseError(err); got != nil {
			t.Errorf("normalizeConnectionCloseError(%v)=%v, want nil", err, got)
		}
	}

	unexpected := errors.New("close failed")
	if got := normalizeConnectionCloseError(unexpected); !errors.Is(got, unexpected) {
		t.Fatalf("normalizeConnectionCloseError(%v)=%v", unexpected, got)
	}
}
