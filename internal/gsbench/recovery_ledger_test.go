package gsbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func validLedgerAction(runID, target string) Action {
	return Action{
		Sequence:      7,
		RunID:         runID,
		ScenarioCode:  343,
		Kind:          ActionNetworkFirewall,
		TargetProduct: ProductGaussDB,
		Target:        target,
		Node:          "dn_1",
		Forward:       json.RawMessage(`{"operation":"add","rule":"gsbench-only"}`),
		Inverse:       json.RawMessage(`{"operation":"delete","rule":"gsbench-only"}`),
		Verify:        json.RawMessage(`{"operation":"absent","rule":"gsbench-only"}`),
		State:         MutationApplied,
	}
}

func TestFileRecoveryLedgerPersistsPendingAction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	var ledger RecoveryLedger = NewFileRecoveryLedger(path)
	action := validLedgerAction("run-1", "firewall-rule-1")

	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	got, err := ledger.Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("pending count = %d, want 1", len(got))
	}
	if got[0].RunID != "run-1" || got[0].Target != "firewall-rule-1" ||
		got[0].Kind != ActionNetworkFirewall {
		t.Fatalf("pending action = %+v", got[0])
	}
}

func TestFileRecoveryLedgerPendingExistingLedgerDoesNotCreateMissingLock(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	action := validLedgerAction("run-1", "firewall-rule-1")
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	lockPath := path + ".lock"
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}

	pending, err := ledger.Pending(context.Background(), action.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Target != action.Target {
		t.Fatalf("pending=%+v", pending)
	}
	if _, err := os.Lstat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only Pending created lock %q: %v", lockPath, err)
	}
}

func TestFileRecoveryLedgerSnapshotIncludesRestoredWithoutCreatingLock(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	action := validLedgerAction("run-1", "firewall-rule-1")
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if err := ledger.MarkRestored(
		context.Background(),
		action.RunID,
		action.Target,
	); err != nil {
		t.Fatal(err)
	}
	lockPath := path + ".lock"
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}

	snapshotter, ok := ledger.(interface {
		Snapshot(context.Context, string) ([]Action, error)
	})
	if !ok {
		t.Fatal("file recovery ledger has no read-only Snapshot")
	}
	actions, err := snapshotter.Snapshot(context.Background(), action.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].State != MutationRestored {
		t.Fatalf("snapshot=%+v", actions)
	}
	if _, err := os.Lstat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only Snapshot created lock %q: %v", lockPath, err)
	}
}

func TestFileRecoveryLedgerValidatesActionBeforePersistence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Action)
		want   string
	}{
		{
			name: "missing target product",
			mutate: func(action *Action) {
				action.TargetProduct = ProductUnknown
			},
			want: "target product",
		},
		{
			name: "missing node",
			mutate: func(action *Action) {
				action.Node = ""
			},
			want: "target node",
		},
		{
			name: "missing inverse",
			mutate: func(action *Action) {
				action.Inverse = nil
			},
			want: "inverse",
		},
		{
			name: "secret payload",
			mutate: func(action *Action) {
				action.Forward = json.RawMessage(
					`{"authorization":"Bearer do-not-persist"}`,
				)
			},
			want: "secret-bearing",
		},
		{
			name: "secret target",
			mutate: func(action *Action) {
				action.Target = "https://bench:do-not-persist@provider/rule"
			},
			want: "credential",
		},
		{
			name: "secret run ID",
			mutate: func(action *Action) {
				action.RunID = "run-password=do-not-persist"
			},
			want: "credential",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "recovery.json")
			action := validLedgerAction("run-1", "firewall-rule-1")
			tt.mutate(&action)

			err := NewFileRecoveryLedger(path).Put(context.Background(), action)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Put() error = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "do-not-persist") {
				t.Fatalf("Put() error leaked credential material: %q", err)
			}
			if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
				t.Fatalf("invalid action created ledger: %v", statErr)
			}
		})
	}
}

func TestFileRecoveryLedgerAcceptsOnlyExternalPersistentActions(t *testing.T) {
	disallowed := []ActionKind{
		ActionSQLMutation,
		ActionSessionSet,
		ActionSessionTransaction,
		ActionDataBaseline,
	}
	for _, kind := range disallowed {
		t.Run(string(kind), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "recovery.json")
			action := validLedgerAction("run-1", "target-1")
			action.Kind = kind
			action.Node = ""

			err := NewFileRecoveryLedger(path).Put(context.Background(), action)
			if err == nil || !strings.Contains(err.Error(), "external persistent") {
				t.Fatalf("Put() error = %v", err)
			}
		})
	}

	for _, kind := range []ActionKind{
		ActionGUCFileChange,
		ActionNetworkQDisc,
		ActionNetworkFirewall,
		ActionProcessState,
		ActionNodeRole,
		ActionCloudFaultJob,
	} {
		t.Run("accept_"+string(kind), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "recovery.json")
			action := validLedgerAction("run-1", "target-1")
			action.Kind = kind
			if err := NewFileRecoveryLedger(path).Put(
				context.Background(),
				action,
			); err != nil {
				t.Fatalf("Put() error = %v", err)
			}
		})
	}
}

func TestFileRecoveryLedgerPutIsIdempotentAndRejectsIdentityConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	action := validLedgerAction("run-1", "firewall-rule-1")

	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatalf("retry Put() error = %v", err)
	}
	got, err := ledger.Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("retry created %d actions, want 1", len(got))
	}

	conflict := action
	conflict.Inverse = json.RawMessage(`{"operation":"delete","rule":"other"}`)
	if err := ledger.Put(context.Background(), conflict); err == nil ||
		!strings.Contains(err.Error(), "conflict") {
		t.Fatalf("conflicting Put() error = %v", err)
	}
	got, err = ledger.Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var inverse struct {
		Rule string `json:"rule"`
	}
	if len(got) != 1 {
		t.Fatalf("conflict changed ledger action: %+v", got)
	}
	if err := json.Unmarshal(got[0].Inverse, &inverse); err != nil {
		t.Fatal(err)
	}
	if inverse.Rule != "gsbench-only" {
		t.Fatalf("conflict changed inverse rule to %q", inverse.Rule)
	}
}

func TestFileRecoveryLedgerKeepsRestoreFailedActionPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	action := validLedgerAction("run-1", "firewall-rule-1")
	action.State = MutationPlanned
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}

	action.State = MutationRestoreFailed
	action.LastError = "provider still reports rule present"
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	got, err := ledger.Pending(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != MutationRestoreFailed ||
		got[0].LastError != "provider still reports rule present" {
		t.Fatalf("pending = %+v", got)
	}
}

func TestFileRecoveryLedgerMarkRestoredUsesExactRunAndTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	for _, action := range []Action{
		validLedgerAction("run-1", "shared-target"),
		validLedgerAction("run-2", "shared-target"),
		validLedgerAction("run-1", "other-target"),
	} {
		if err := ledger.Put(context.Background(), action); err != nil {
			t.Fatal(err)
		}
	}

	if err := ledger.MarkRestored(
		context.Background(),
		"run-1",
		"shared-target",
	); err != nil {
		t.Fatal(err)
	}
	if err := ledger.MarkRestored(
		context.Background(),
		"run-1",
		"shared-target",
	); err != nil {
		t.Fatalf("idempotent MarkRestored() error = %v", err)
	}
	got, err := ledger.Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	gotKeys := make([]string, len(got))
	for i, action := range got {
		gotKeys[i] = action.RunID + "/" + action.Target
	}
	want := []string{"run-1/other-target", "run-2/shared-target"}
	if !reflect.DeepEqual(gotKeys, want) {
		t.Fatalf("pending keys = %v, want %v", gotKeys, want)
	}
}

func TestFileRecoveryLedgerPendingOrderIsDeterministic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	actions := []Action{
		validLedgerAction("run-b", "target-z"),
		validLedgerAction("run-a", "target-z"),
		validLedgerAction("run-a", "target-a"),
	}
	actions[0].Sequence = 2
	actions[1].Sequence = 1
	actions[2].Sequence = 3
	for _, action := range actions {
		if err := ledger.Put(context.Background(), action); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ledger.Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	gotKeys := make([]string, len(got))
	for i, action := range got {
		gotKeys[i] = fmt.Sprintf("%s/%d/%s", action.RunID, action.Sequence, action.Target)
	}
	want := []string{
		"run-a/3/target-a",
		"run-a/1/target-z",
		"run-b/2/target-z",
	}
	if !reflect.DeepEqual(gotKeys, want) {
		t.Fatalf("pending order = %v, want %v", gotKeys, want)
	}
}

func TestFileRecoveryLedgerUsesVersionedJSONAndMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	if err := NewFileRecoveryLedger(path).Put(
		context.Background(),
		validLedgerAction("run-1", "target-1"),
	); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("ledger mode = %04o, want 0600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Version int               `json:"version"`
		Actions []json.RawMessage `json:"actions"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("ledger is not complete JSON: %v", err)
	}
	if envelope.Version != 1 || len(envelope.Actions) != 1 {
		t.Fatalf("ledger envelope = %+v", envelope)
	}
	if bytes.Contains(data, []byte("LegacySQL")) ||
		bytes.Contains(data, []byte("legacy_sql")) {
		t.Fatalf("ledger persisted internal legacy provenance: %s", data)
	}
}

func TestFileRecoveryLedgerRejectsPermissiveExistingLedgerWithoutChmod(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	action := validLedgerAction("run-1", "target-1")
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ledger.Pending(context.Background(), ""); err == nil ||
		!strings.Contains(err.Error(), "0600") {
		t.Fatalf("Pending() error = %v", err)
	}
	if err := ledger.Put(context.Background(), action); err == nil ||
		!strings.Contains(err.Error(), "0600") {
		t.Fatalf("idempotent Put() error = %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("untrusted ledger mode changed to %04o", got)
	}
}

func TestFileRecoveryLedgerRejectsUntrustedParentAndSymlinkAncestor(t *testing.T) {
	t.Run("group writable parent", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "ledger-parent")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0o770); err != nil {
			t.Fatal(err)
		}
		err := NewFileRecoveryLedger(
			filepath.Join(parent, "recovery.json"),
		).Put(context.Background(), validLedgerAction("run-1", "target-1"))
		if err == nil || !strings.Contains(err.Error(), "group/world-writable") {
			t.Fatalf("Put() error = %v", err)
		}
	})

	t.Run("symlink ancestor", func(t *testing.T) {
		root := t.TempDir()
		realParent := filepath.Join(root, "real", "child")
		if err := os.MkdirAll(realParent, 0o700); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(root, "alias")
		if err := os.Symlink(filepath.Join(root, "real"), alias); err != nil {
			t.Fatal(err)
		}
		err := NewFileRecoveryLedger(
			filepath.Join(alias, "child", "recovery.json"),
		).Put(context.Background(), validLedgerAction("run-1", "target-1"))
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("Put() error = %v", err)
		}
	})
}

func TestFileRecoveryLedgerRejectsUntrustedLockWithoutChangingIt(t *testing.T) {
	t.Run("permissive mode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "recovery.json")
		ledger := NewFileRecoveryLedger(path)
		if err := ledger.Put(
			context.Background(),
			validLedgerAction("run-1", "target-1"),
		); err != nil {
			t.Fatal(err)
		}
		lockPath := path + ".lock"
		if err := os.Chmod(lockPath, 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := ledger.Pending(context.Background(), ""); err == nil ||
			!strings.Contains(err.Error(), "lock") ||
			!strings.Contains(err.Error(), "0600") {
			t.Fatalf("Pending() error = %v", err)
		}
		info, err := os.Lstat(lockPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Fatalf("untrusted lock mode changed to %04o", got)
		}
	})

	t.Run("multiple hard links", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "recovery.json")
		ledger := NewFileRecoveryLedger(path)
		if err := ledger.Put(
			context.Background(),
			validLedgerAction("run-1", "target-1"),
		); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(path+".lock", path+".lock-copy"); err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.Pending(context.Background(), ""); err == nil ||
			!strings.Contains(err.Error(), "link count") {
			t.Fatalf("Pending() error = %v", err)
		}
	})
}

func TestFileRecoveryLedgerRejectsHardLinkedLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	if err := ledger.Put(
		context.Background(),
		validLedgerAction("run-1", "target-1"),
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, path+".copy"); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Pending(context.Background(), ""); err == nil ||
		!strings.Contains(err.Error(), "link count") {
		t.Fatalf("Pending() error = %v", err)
	}
}

func TestFileRecoveryLedgerIdempotentPutRepairsDirectorySyncFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path).(*fileRecoveryLedger)
	first := validLedgerAction("run-1", "target-1")
	second := validLedgerAction("run-1", "target-2")
	if err := ledger.Put(context.Background(), first); err != nil {
		t.Fatal(err)
	}

	ledger.syncDirectory = func(int) error {
		return errors.New("injected post-rename directory sync failure")
	}
	err := ledger.Put(context.Background(), second)
	if err == nil || !strings.Contains(err.Error(), "injected post-rename") {
		t.Fatalf("Put() error = %v", err)
	}

	retrySyncs := 0
	ledger.syncDirectory = func(descriptor int) error {
		retrySyncs++
		return syncRecoveryLedgerDirectory(descriptor)
	}
	if err := ledger.Put(context.Background(), second); err != nil {
		t.Fatalf("retry Put() error = %v", err)
	}
	if retrySyncs == 0 {
		t.Fatal("idempotent retry did not sync the pinned parent")
	}
	got, err := ledger.Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("pending count = %d, want 2", len(got))
	}
}

func TestFileRecoveryLedgerMarkRestoredUsesDurableTombstone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path).(*fileRecoveryLedger)
	action := validLedgerAction("run-1", "target-1")
	action.State = MutationRestoreFailed
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}

	ledger.syncDirectory = func(int) error {
		return errors.New("injected tombstone directory sync failure")
	}
	err := ledger.MarkRestored(context.Background(), "run-1", "target-1")
	if err == nil || !strings.Contains(err.Error(), "injected tombstone") {
		t.Fatalf("MarkRestored() error = %v", err)
	}

	retrySyncs := 0
	ledger.syncDirectory = func(descriptor int) error {
		retrySyncs++
		return syncRecoveryLedgerDirectory(descriptor)
	}
	if err := ledger.MarkRestored(
		context.Background(),
		"run-1",
		"target-1",
	); err != nil {
		t.Fatalf("retry MarkRestored() error = %v", err)
	}
	if retrySyncs == 0 {
		t.Fatal("tombstone retry did not sync the pinned parent")
	}
	got, err := ledger.Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("restored tombstone returned as pending: %+v", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"state":"restored"`)) {
		t.Fatalf("restored tombstone not persisted: %s", data)
	}
}

func TestFileRecoveryLedgerNoopMarkRestoredSyncsParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path).(*fileRecoveryLedger)
	if err := ledger.Put(
		context.Background(),
		validLedgerAction("run-1", "target-1"),
	); err != nil {
		t.Fatal(err)
	}
	ledger.syncDirectory = func(int) error {
		return errors.New("injected no-op directory sync failure")
	}
	err := ledger.MarkRestored(context.Background(), "run-1", "missing-target")
	if err == nil || !strings.Contains(err.Error(), "injected no-op") {
		t.Fatalf("MarkRestored() error = %v", err)
	}
}

func TestFileRecoveryLedgerRejectsStaleStateRegression(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	action := validLedgerAction("run-1", "target-1")
	for _, state := range []MutationState{
		MutationPlanned,
		MutationApplied,
		MutationRestoring,
	} {
		action.State = state
		if err := ledger.Put(context.Background(), action); err != nil {
			t.Fatalf("Put(%s) error = %v", state, err)
		}
	}
	for _, stale := range []MutationState{MutationPlanned, MutationApplied} {
		action.State = stale
		if err := ledger.Put(context.Background(), action); err == nil ||
			!strings.Contains(err.Error(), "state regression") {
			t.Fatalf("stale Put(%s) error = %v", stale, err)
		}
	}
	got, err := ledger.Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != MutationRestoring {
		t.Fatalf("pending after stale updates = %+v", got)
	}
}

func TestFileRecoveryLedgerStaleSameStatePutCannotClearOrReplaceDiagnostic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	action := validLedgerAction("run-1", "target-1")
	action.State = MutationRestoreFailed
	action.LastError = "newer restore diagnostic"
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}

	stale := action
	stale.LastError = ""
	if err := ledger.Put(context.Background(), stale); err != nil {
		t.Fatalf("blank stale retry error = %v", err)
	}
	conflicting := action
	conflicting.LastError = "different stale diagnostic"
	if err := ledger.Put(context.Background(), conflicting); err == nil ||
		!strings.Contains(err.Error(), "diagnostic conflict") {
		t.Fatalf("conflicting diagnostic Put() error = %v", err)
	}

	got, err := ledger.Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].LastError != "newer restore diagnostic" {
		t.Fatalf("pending diagnostic = %+v", got)
	}
}

func TestFileRecoveryLedgerAllowsRestoreRetryThenRejectsTombstoneResurrection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	action := validLedgerAction("run-1", "target-1")
	for _, state := range []MutationState{
		MutationApplied,
		MutationRestoreFailed,
		MutationRestoring,
	} {
		action.State = state
		if err := ledger.Put(context.Background(), action); err != nil {
			t.Fatalf("Put(%s) error = %v", state, err)
		}
	}
	if err := ledger.MarkRestored(
		context.Background(),
		action.RunID,
		action.Target,
	); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []MutationState{
		MutationPlanned,
		MutationApplied,
		MutationRestoreFailed,
	} {
		action.State = stale
		if err := ledger.Put(context.Background(), action); err == nil ||
			!strings.Contains(err.Error(), "restored") {
			t.Fatalf("resurrection Put(%s) error = %v", stale, err)
		}
	}
	got, err := ledger.Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("restored action resurrected: %+v", got)
	}
}

func TestFileRecoveryLedgerCanonicalizesObjectOrderAndNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	action := validLedgerAction("run-1", "target-1")
	action.Forward = json.RawMessage(
		`{"operation":"add","count":1,"nested":{"b":2,"a":1}}`,
	)
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	retry := action
	retry.Forward = json.RawMessage(
		`{"nested":{"a":1.0,"b":2.0},"count":1.0,"operation":"add"}`,
	)
	if err := ledger.Put(context.Background(), retry); err != nil {
		t.Fatalf("semantic retry Put() error = %v", err)
	}
	got, err := ledger.Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("pending count = %d, want 1", len(got))
	}
	want := `{"count":1,"nested":{"a":1,"b":2},"operation":"add"}`
	if string(got[0].Forward) != want {
		t.Fatalf("canonical forward = %s, want %s", got[0].Forward, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(want)) ||
		bytes.Contains(data, []byte("1.0")) {
		t.Fatalf("ledger does not contain canonical payload: %s", data)
	}
}

func TestFileRecoveryLedgerRejectsControlCharactersInIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Action)
	}{
		{
			name: "run ID NUL",
			mutate: func(action *Action) {
				action.RunID = "run\x00target"
			},
		},
		{
			name: "target NUL",
			mutate: func(action *Action) {
				action.Target = "target\x00suffix"
			},
		},
		{
			name: "run ID newline",
			mutate: func(action *Action) {
				action.RunID = "run\nother"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action := validLedgerAction("run-1", "target-1")
			tt.mutate(&action)
			err := NewFileRecoveryLedger(
				filepath.Join(t.TempDir(), "recovery.json"),
			).Put(context.Background(), action)
			if err == nil || !strings.Contains(err.Error(), "control character") {
				t.Fatalf("Put() error = %v", err)
			}
		})
	}
}

func TestFileRecoveryLedgerCorruptionIsActionableAndNeverOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	original := []byte(`{"version":1,"actions":[`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := NewFileRecoveryLedger(path)

	if _, err := ledger.Pending(context.Background(), ""); err == nil ||
		!strings.Contains(err.Error(), "corrupt") ||
		!strings.Contains(err.Error(), filepath.Base(path)) {
		t.Fatalf("Pending() error = %v", err)
	}
	if err := ledger.Put(
		context.Background(),
		validLedgerAction("run-1", "target-1"),
	); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("Put() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("corrupt ledger was overwritten: %q", got)
	}
}

func TestFileRecoveryLedgerRejectsUnsupportedVersionWithoutOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	original := []byte(`{"version":99,"actions":[]}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	err := NewFileRecoveryLedger(path).Put(
		context.Background(),
		validLedgerAction("run-1", "target-1"),
	)
	if err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Fatalf("Put() error = %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("unsupported ledger was overwritten: %q", got)
	}
}

func TestFileRecoveryLedgerRejectsSymlinkAndNonRegularTarget(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		realPath := filepath.Join(dir, "real.json")
		original := []byte(`{"version":1,"actions":[]}`)
		if err := os.WriteFile(realPath, original, 0o600); err != nil {
			t.Fatal(err)
		}
		linkPath := filepath.Join(dir, "recovery.json")
		if err := os.Symlink(realPath, linkPath); err != nil {
			t.Fatal(err)
		}
		err := NewFileRecoveryLedger(linkPath).Put(
			context.Background(),
			validLedgerAction("run-1", "target-1"),
		)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("Put() error = %v", err)
		}
		got, readErr := os.ReadFile(realPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(got, original) {
			t.Fatalf("symlink target changed: %q", got)
		}
	})

	t.Run("directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "recovery.json")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		err := NewFileRecoveryLedger(path).Put(
			context.Background(),
			validLedgerAction("run-1", "target-1"),
		)
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("Put() error = %v", err)
		}
	})
}

func TestFileRecoveryLedgerRejectsUnsafeBroadPath(t *testing.T) {
	paths := []string{"", ".", string(filepath.Separator)}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, home)
	}
	for _, path := range paths {
		t.Run(strings.ReplaceAll(path, string(filepath.Separator), "_"), func(t *testing.T) {
			err := NewFileRecoveryLedger(path).Put(
				context.Background(),
				validLedgerAction("run-1", "target-1"),
			)
			if err == nil || !strings.Contains(err.Error(), "unsafe recovery ledger path") {
				t.Fatalf("Put(%q) error = %v", path, err)
			}
		})
	}
}

func TestFileRecoveryLedgerFailedOversizePutPreservesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	if err := ledger.Put(
		context.Background(),
		validLedgerAction("run-1", "target-1"),
	); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	oversize := validLedgerAction("run-2", "target-2")
	oversize.Forward = json.RawMessage(
		`{"blob":"` + strings.Repeat("a", 5<<20) + `"}`,
	)
	if err := ledger.Put(context.Background(), oversize); err == nil ||
		!strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversize Put() error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("failed Put changed the existing ledger")
	}
}

func TestFileRecoveryLedgerBoundsPendingActionCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	for i := 0; i < 256; i++ {
		action := validLedgerAction("run-1", fmt.Sprintf("target-%03d", i))
		action.Sequence = int64(i)
		if err := ledger.Put(context.Background(), action); err != nil {
			t.Fatalf("Put(%d) error = %v", i, err)
		}
	}
	err := ledger.Put(
		context.Background(),
		validLedgerAction("run-1", "target-over-limit"),
	)
	if err == nil || !strings.Contains(err.Error(), "action count limit") {
		t.Fatalf("over-limit Put() error = %v", err)
	}
	got, pendingErr := ledger.Pending(context.Background(), "")
	if pendingErr != nil {
		t.Fatal(pendingErr)
	}
	if len(got) != 256 {
		t.Fatalf("pending count = %d, want 256", len(got))
	}
}

func TestFileRecoveryLedgerConcurrentGoroutinePutsDoNotLoseUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	const writers = 32
	start := make(chan struct{})
	errs := make(chan error, writers)
	var group sync.WaitGroup
	for i := 0; i < writers; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			action := validLedgerAction(
				"run-1",
				fmt.Sprintf("goroutine-target-%02d", index),
			)
			action.Sequence = int64(index)
			errs <- ledger.Put(context.Background(), action)
		}(i)
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Put() error = %v", err)
		}
	}

	got, err := ledger.Pending(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != writers {
		t.Fatalf("pending count = %d, want %d", len(got), writers)
	}
}

func TestFileRecoveryLedgerConcurrentProcessPutsDoNotLoseUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	const processes = 4
	const writesPerProcess = 8
	gate := filepath.Join(t.TempDir(), "start")
	commands := make([]*exec.Cmd, 0, processes)
	outputs := make([]*bytes.Buffer, 0, processes)
	for i := 0; i < processes; i++ {
		ready := gate + ".ready." + strconv.Itoa(i)
		command := exec.Command(
			os.Args[0],
			"-test.run=^TestRecoveryLedgerProcessHelper$",
			"-test.count=1",
		)
		command.Env = append(os.Environ(),
			"GSBENCH_LEDGER_HELPER=1",
			"GSBENCH_LEDGER_PATH="+path,
			"GSBENCH_LEDGER_GATE="+gate,
			"GSBENCH_LEDGER_READY="+ready,
			"GSBENCH_LEDGER_PROCESS="+strconv.Itoa(i),
			"GSBENCH_LEDGER_WRITES="+strconv.Itoa(writesPerProcess),
		)
		output := &bytes.Buffer{}
		command.Stdout = output
		command.Stderr = output
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
		outputs = append(outputs, output)
	}
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; i < processes; {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for helper processes")
		}
		if _, err := os.Stat(gate + ".ready." + strconv.Itoa(i)); err == nil {
			i++
			continue
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("helper failed: %v\n%s", err, outputs[i].Bytes())
		}
	}

	got, err := NewFileRecoveryLedger(path).Pending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if want := processes * writesPerProcess; len(got) != want {
		t.Fatalf("pending count = %d, want %d", len(got), want)
	}
}

func TestRecoveryLedgerProcessHelper(t *testing.T) {
	if os.Getenv("GSBENCH_LEDGER_HELPER") != "1" {
		t.Skip("helper process only")
	}
	path := os.Getenv("GSBENCH_LEDGER_PATH")
	gate := os.Getenv("GSBENCH_LEDGER_GATE")
	ready := os.Getenv("GSBENCH_LEDGER_READY")
	processIndex, err := strconv.Atoi(os.Getenv("GSBENCH_LEDGER_PROCESS"))
	if err != nil {
		t.Fatal(err)
	}
	writes, err := strconv.Atoi(os.Getenv("GSBENCH_LEDGER_WRITES"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for process gate")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ledger := NewFileRecoveryLedger(path)
	for i := 0; i < writes; i++ {
		action := validLedgerAction(
			"run-process",
			fmt.Sprintf("process-%d-target-%d", processIndex, i),
		)
		action.Sequence = int64(processIndex*writes + i)
		if err := ledger.Put(context.Background(), action); err != nil {
			t.Fatal(err)
		}
	}
}
