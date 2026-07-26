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

func TestApplicationNameRejectsUnsafeComponents(t *testing.T) {
	if _, err := ApplicationName("run/other", "tp_cpu", "7"); err == nil {
		t.Fatal("expected unsafe run id error")
	}
}

func TestTaggedSessionPredicateDoesNotMatchRunPrefixCollision(t *testing.T) {
	query, arg, err := TaggedSessionPredicate("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "application_name LIKE $1") {
		t.Fatalf("query = %q", query)
	}
	if arg != "gsbench/run-1/%" {
		t.Fatalf("arg = %q", arg)
	}
	if strings.HasPrefix("gsbench/run-10/tp_cpu/1", strings.TrimSuffix(arg, "%")) {
		t.Fatal("run-1 ownership prefix also matched run-10")
	}
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
