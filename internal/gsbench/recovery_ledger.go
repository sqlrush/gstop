package gsbench

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
)

const (
	recoveryLedgerVersion    = 1
	recoveryLedgerMaxBytes   = 4 << 20
	recoveryLedgerMaxActions = 256
)

type RecoveryLedger interface {
	Put(context.Context, Action) error
	MarkRestored(context.Context, string, string) error
	Pending(context.Context, string) ([]Action, error)
}

type fileRecoveryLedger struct {
	path          string
	syncDirectory func(int) error
}

type recoveryLedgerFile struct {
	Version int                    `json:"version"`
	Actions []recoveryLedgerAction `json:"actions"`
}

// recoveryLedgerAction intentionally excludes LegacySQL. The local ledger is
// only a recovery mirror for external persistent actions, never a second SQL
// journal or a way to forge legacy provenance.
type recoveryLedgerAction struct {
	Sequence      int64           `json:"sequence"`
	RunID         string          `json:"run_id"`
	ScenarioCode  ScenarioCode    `json:"scenario_code"`
	Kind          ActionKind      `json:"action_kind"`
	TargetProduct Product         `json:"target_product"`
	Target        string          `json:"target"`
	Node          string          `json:"target_node"`
	Original      json.RawMessage `json:"original_state,omitempty"`
	Forward       json.RawMessage `json:"forward_action"`
	Inverse       json.RawMessage `json:"inverse_action"`
	Verify        json.RawMessage `json:"verify_action,omitempty"`
	State         MutationState   `json:"state"`
	LastError     string          `json:"last_error,omitempty"`
}

type recoveryLedgerIdentity struct {
	RunID  string
	Target string
}

type pinnedRecoveryLedgerDirectory struct {
	descriptor int
	targetName string
	label      string
}

var (
	recoveryLedgerPathLocks sync.Map
	canonicalNumberPattern  = regexp.MustCompile(
		`^(-?)(0|[1-9][0-9]*)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`,
	)
)

func NewFileRecoveryLedger(path string) RecoveryLedger {
	if absolute, err := filepath.Abs(filepath.Clean(path)); err == nil {
		path = canonicalDarwinSystemPath(absolute)
	}
	return &fileRecoveryLedger{
		path:          path,
		syncDirectory: syncRecoveryLedgerDirectory,
	}
}

func (l *fileRecoveryLedger) Put(ctx context.Context, action Action) error {
	if action.State == "" {
		action.State = MutationPlanned
	}
	if err := validateRecoveryLedgerPutAction(action); err != nil {
		return err
	}
	stored, err := canonicalRecoveryLedgerAction(action)
	if err != nil {
		return err
	}
	return l.withExclusiveLock(ctx, true, func(
		parent *pinnedRecoveryLedgerDirectory,
	) error {
		file, err := readRecoveryLedger(parent)
		if err != nil {
			return err
		}
		for index := range file.Actions {
			existing := file.Actions[index]
			if existing.RunID != stored.RunID || existing.Target != stored.Target {
				continue
			}
			if !sameRecoveryLedgerAction(existing, stored) {
				return fmt.Errorf(
					"recovery ledger action identity conflict for run and target",
				)
			}
			if !allowedRecoveryLedgerTransition(existing.State, stored.State) {
				if existing.State == MutationRestored {
					return fmt.Errorf(
						"recovery ledger action is already restored",
					)
				}
				return fmt.Errorf(
					"recovery ledger state regression from %q to %q is forbidden",
					existing.State,
					stored.State,
				)
			}
			if existing.State == stored.State &&
				existing.LastError != "" {
				switch stored.LastError {
				case "":
					stored.LastError = existing.LastError
				case existing.LastError:
				default:
					return fmt.Errorf(
						"recovery ledger same-state diagnostic conflict",
					)
				}
			}
			file.Actions[index].State = stored.State
			file.Actions[index].LastError = stored.LastError
			sortRecoveryLedgerActions(file.Actions)
			// Rewriting an idempotent update is deliberate: it canonicalizes
			// legacy v1 payload bytes and repairs an uncertain prior rename.
			return l.writeRecoveryLedger(parent, file)
		}
		if len(file.Actions) >= recoveryLedgerMaxActions {
			return fmt.Errorf(
				"recovery ledger action count limit %d exceeded",
				recoveryLedgerMaxActions,
			)
		}
		file.Actions = append(file.Actions, stored)
		sortRecoveryLedgerActions(file.Actions)
		return l.writeRecoveryLedger(parent, file)
	})
}

func (l *fileRecoveryLedger) MarkRestored(
	ctx context.Context,
	runID string,
	target string,
) error {
	if strings.TrimSpace(runID) == "" {
		return fmt.Errorf("recovery ledger run ID is required")
	}
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("recovery ledger target is required")
	}
	if identityContainsControl(runID) || identityContainsControl(target) {
		return fmt.Errorf(
			"recovery ledger run ID and target must not contain a control character",
		)
	}
	if err := validateJournalStringField("run ID", runID); err != nil {
		return err
	}
	if err := validateJournalStringField("target", target); err != nil {
		return err
	}
	return l.withExclusiveLock(ctx, true, func(
		parent *pinnedRecoveryLedgerDirectory,
	) error {
		file, err := readRecoveryLedger(parent)
		if err != nil {
			return err
		}
		for index := range file.Actions {
			action := &file.Actions[index]
			if action.RunID != runID || action.Target != target {
				continue
			}
			if action.State == MutationRestored {
				return l.syncPinnedDirectory(parent)
			}
			action.State = MutationRestored
			action.LastError = ""
			sortRecoveryLedgerActions(file.Actions)
			return l.writeRecoveryLedger(parent, file)
		}
		// A no-op retry still repairs an uncertain prior directory sync.
		return l.syncPinnedDirectory(parent)
	})
}

func (l *fileRecoveryLedger) Pending(
	ctx context.Context,
	runID string,
) ([]Action, error) {
	actions, err := l.Snapshot(ctx, runID)
	if err != nil {
		return nil, err
	}
	pending := actions[:0]
	for _, action := range actions {
		if action.State != MutationRestored {
			pending = append(pending, action)
		}
	}
	return pending, nil
}

// Snapshot returns all matching ledger actions, including durable restored
// tombstones used to reconcile a database mirror after an offline inverse.
func (l *fileRecoveryLedger) Snapshot(
	ctx context.Context,
	runID string,
) ([]Action, error) {
	if runID != "" && strings.TrimSpace(runID) == "" {
		return nil, fmt.Errorf("recovery ledger run ID must not be blank")
	}
	if identityContainsControl(runID) {
		return nil, fmt.Errorf(
			"recovery ledger run ID must not contain a control character",
		)
	}
	if err := validateJournalStringField("run ID", runID); err != nil {
		return nil, err
	}
	var actions []Action
	err := l.withExclusiveLock(ctx, false, func(
		parent *pinnedRecoveryLedgerDirectory,
	) error {
		file, err := readRecoveryLedger(parent)
		if err != nil {
			return err
		}
		actions = make([]Action, 0, len(file.Actions))
		for _, stored := range file.Actions {
			if runID == "" || stored.RunID == runID {
				actions = append(actions, stored.action())
			}
		}
		return nil
	})
	return actions, err
}

func validateRecoveryLedgerPutAction(action Action) error {
	if action.State == MutationRestored {
		return fmt.Errorf(
			"restored recovery ledger state is reserved for MarkRestored",
		)
	}
	return validateRecoveryLedgerAction(action, false)
}

func validateRecoveryLedgerAction(action Action, allowRestored bool) error {
	if action.LegacySQL {
		return fmt.Errorf("legacy SQL provenance cannot be stored in recovery ledger")
	}
	if identityContainsControl(action.RunID) ||
		identityContainsControl(action.Target) {
		return fmt.Errorf(
			"recovery ledger run ID and target must not contain a control character",
		)
	}
	if err := validateJournalStringField("run ID", action.RunID); err != nil {
		return err
	}
	if err := action.Validate(); err != nil {
		return fmt.Errorf("validate recovery ledger action: %w", err)
	}
	if !externalPersistentActionKind(action.Kind) {
		return fmt.Errorf(
			"action kind %q is not an external persistent action",
			action.Kind,
		)
	}
	switch action.State {
	case MutationPlanned,
		MutationApplied,
		MutationRestoring,
		MutationRestoreFailed:
		return nil
	case MutationRestored:
		if allowRestored {
			return nil
		}
	}
	return fmt.Errorf(
		"recovery ledger action state %q is invalid",
		action.State,
	)
}

func identityContainsControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func externalPersistentActionKind(kind ActionKind) bool {
	switch kind {
	case ActionGUCFileChange,
		ActionNetworkQDisc,
		ActionNetworkFirewall,
		ActionProcessState,
		ActionNodeRole,
		ActionCloudFaultJob:
		return true
	default:
		return false
	}
}

func allowedRecoveryLedgerTransition(from, to MutationState) bool {
	if from == to {
		return true
	}
	switch from {
	case MutationPlanned:
		return to == MutationApplied ||
			to == MutationRestoring ||
			to == MutationRestoreFailed
	case MutationApplied:
		return to == MutationRestoring || to == MutationRestoreFailed
	case MutationRestoring:
		return to == MutationRestoreFailed
	case MutationRestoreFailed:
		return to == MutationRestoring
	default:
		return false
	}
}

func canonicalRecoveryLedgerAction(
	action Action,
) (recoveryLedgerAction, error) {
	canonicalPayload := func(
		name string,
		payload json.RawMessage,
	) (json.RawMessage, error) {
		if len(payload) == 0 {
			return nil, nil
		}
		canonical, err := canonicalRecoveryJSON(payload)
		if err != nil {
			return nil, fmt.Errorf(
				"canonicalize recovery ledger %s payload: %w",
				name,
				err,
			)
		}
		return canonical, nil
	}
	original, err := canonicalPayload("original", action.Original)
	if err != nil {
		return recoveryLedgerAction{}, err
	}
	forward, err := canonicalPayload("forward", action.Forward)
	if err != nil {
		return recoveryLedgerAction{}, err
	}
	inverse, err := canonicalPayload("inverse", action.Inverse)
	if err != nil {
		return recoveryLedgerAction{}, err
	}
	verify, err := canonicalPayload("verify", action.Verify)
	if err != nil {
		return recoveryLedgerAction{}, err
	}
	return recoveryLedgerAction{
		Sequence:      action.Sequence,
		RunID:         action.RunID,
		ScenarioCode:  action.ScenarioCode,
		Kind:          action.Kind,
		TargetProduct: action.TargetProduct,
		Target:        action.Target,
		Node:          action.Node,
		Original:      original,
		Forward:       forward,
		Inverse:       inverse,
		Verify:        verify,
		State:         action.State,
		LastError:     action.LastError,
	}, nil
}

func (a recoveryLedgerAction) action() Action {
	return Action{
		Sequence:      a.Sequence,
		RunID:         a.RunID,
		ScenarioCode:  a.ScenarioCode,
		Kind:          a.Kind,
		TargetProduct: a.TargetProduct,
		Target:        a.Target,
		Node:          a.Node,
		Original:      cloneRawMessage(a.Original),
		Forward:       cloneRawMessage(a.Forward),
		Inverse:       cloneRawMessage(a.Inverse),
		Verify:        cloneRawMessage(a.Verify),
		State:         a.State,
		LastError:     a.LastError,
	}
}

func cloneRawMessage(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), value...)
}

func sameRecoveryLedgerAction(left, right recoveryLedgerAction) bool {
	return left.Sequence == right.Sequence &&
		left.RunID == right.RunID &&
		left.ScenarioCode == right.ScenarioCode &&
		left.Kind == right.Kind &&
		left.TargetProduct == right.TargetProduct &&
		left.Target == right.Target &&
		left.Node == right.Node &&
		bytes.Equal(left.Original, right.Original) &&
		bytes.Equal(left.Forward, right.Forward) &&
		bytes.Equal(left.Inverse, right.Inverse) &&
		bytes.Equal(left.Verify, right.Verify)
}

func sortRecoveryLedgerActions(actions []recoveryLedgerAction) {
	sort.Slice(actions, func(i, j int) bool {
		left, right := actions[i], actions[j]
		if left.RunID != right.RunID {
			return left.RunID < right.RunID
		}
		if left.Sequence != right.Sequence {
			return left.Sequence > right.Sequence
		}
		if left.Target != right.Target {
			return left.Target < right.Target
		}
		return left.Kind < right.Kind
	})
}

func canonicalRecoveryJSON(payload json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("multiple JSON values")
	}
	var output bytes.Buffer
	if err := appendCanonicalJSON(&output, value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func appendCanonicalJSON(output *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		if typed {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
	case string:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Errorf("encode JSON string")
		}
		output.Write(encoded)
	case json.Number:
		number, err := canonicalRecoveryJSONNumber(string(typed))
		if err != nil {
			return err
		}
		output.WriteString(number)
	case []any:
		output.WriteByte('[')
		for index, item := range typed {
			if index != 0 {
				output.WriteByte(',')
			}
			if err := appendCanonicalJSON(output, item); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for index, key := range keys {
			if index != 0 {
				output.WriteByte(',')
			}
			encodedKey, err := json.Marshal(key)
			if err != nil {
				return fmt.Errorf("encode JSON object key")
			}
			output.Write(encodedKey)
			output.WriteByte(':')
			if err := appendCanonicalJSON(output, typed[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value")
	}
	return nil
}

func canonicalRecoveryJSONNumber(value string) (string, error) {
	parts := canonicalNumberPattern.FindStringSubmatch(value)
	if parts == nil {
		return "", fmt.Errorf("invalid JSON number")
	}
	exponent := int64(0)
	if parts[4] != "" {
		parsed, err := strconv.ParseInt(parts[4], 10, 32)
		if err != nil {
			return "", fmt.Errorf("JSON number exponent is out of range")
		}
		exponent = parsed
	}
	digits := strings.TrimLeft(parts[2]+parts[3], "0")
	if digits == "" {
		return "0", nil
	}
	power := exponent - int64(len(parts[3]))
	for len(digits) > 1 && digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
		power++
	}
	scientificExponent := power + int64(len(digits)) - 1
	coefficient := digits[:1]
	if len(digits) > 1 {
		coefficient += "." + digits[1:]
	}
	if parts[1] == "-" {
		coefficient = "-" + coefficient
	}
	if scientificExponent == 0 {
		return coefficient, nil
	}
	return coefficient + "e" +
		strconv.FormatInt(scientificExponent, 10), nil
}

func readRecoveryLedger(
	parent *pinnedRecoveryLedgerDirectory,
) (recoveryLedgerFile, error) {
	file := recoveryLedgerFile{Version: recoveryLedgerVersion}
	descriptor, stat, exists, err := openTrustedLedgerFile(
		parent,
		parent.targetName,
		unix.O_RDONLY,
		"ledger",
	)
	if err != nil {
		return recoveryLedgerFile{}, err
	}
	if !exists {
		return file, nil
	}
	reader := os.NewFile(uintptr(descriptor), parent.label)
	defer reader.Close()
	if stat.Size > recoveryLedgerMaxBytes {
		return recoveryLedgerFile{}, fmt.Errorf(
			"recovery ledger %q exceeds size limit",
			parent.label,
		)
	}
	data, err := io.ReadAll(io.LimitReader(reader, recoveryLedgerMaxBytes+1))
	if err != nil {
		return recoveryLedgerFile{}, ledgerFileError(
			parent.label,
			"read",
			err,
		)
	}
	if len(data) > recoveryLedgerMaxBytes {
		return recoveryLedgerFile{}, fmt.Errorf(
			"recovery ledger %q exceeds size limit",
			parent.label,
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return recoveryLedgerFile{}, corruptLedgerError(parent.label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return recoveryLedgerFile{}, corruptLedgerError(parent.label, err)
	}
	if file.Version != recoveryLedgerVersion {
		return recoveryLedgerFile{}, fmt.Errorf(
			"recovery ledger %q has unsupported version %d",
			parent.label,
			file.Version,
		)
	}
	if len(file.Actions) > recoveryLedgerMaxActions {
		return recoveryLedgerFile{}, fmt.Errorf(
			"recovery ledger %q exceeds action count limit %d",
			parent.label,
			recoveryLedgerMaxActions,
		)
	}
	identities := make(
		map[recoveryLedgerIdentity]struct{},
		len(file.Actions),
	)
	for index, stored := range file.Actions {
		action := stored.action()
		if err := validateRecoveryLedgerAction(action, true); err != nil {
			return recoveryLedgerFile{}, corruptLedgerError(parent.label, err)
		}
		canonical, err := canonicalRecoveryLedgerAction(action)
		if err != nil {
			return recoveryLedgerFile{}, corruptLedgerError(parent.label, err)
		}
		file.Actions[index] = canonical
		identity := recoveryLedgerIdentity{
			RunID:  action.RunID,
			Target: action.Target,
		}
		if _, exists := identities[identity]; exists {
			return recoveryLedgerFile{}, corruptLedgerError(
				parent.label,
				fmt.Errorf("duplicate run and target identity"),
			)
		}
		identities[identity] = struct{}{}
	}
	sortRecoveryLedgerActions(file.Actions)
	return file, nil
}

func (l *fileRecoveryLedger) writeRecoveryLedger(
	parent *pinnedRecoveryLedgerDirectory,
	file recoveryLedgerFile,
) error {
	file.Version = recoveryLedgerVersion
	data, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("encode recovery ledger: %w", err)
	}
	data = append(data, '\n')
	if len(data) > recoveryLedgerMaxBytes {
		return fmt.Errorf(
			"recovery ledger size limit %d bytes exceeded",
			recoveryLedgerMaxBytes,
		)
	}
	temporaryName, temporaryDescriptor, err := createLedgerTemporary(parent)
	if err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = unix.Unlinkat(parent.descriptor, temporaryName, 0)
		}
	}()
	temporary := os.NewFile(
		uintptr(temporaryDescriptor),
		"recovery-ledger-temporary",
	)
	fail := func(operation string, operationErr error) error {
		_ = temporary.Close()
		return ledgerFileError(parent.label, operation, operationErr)
	}
	if _, err := temporary.Write(data); err != nil {
		return fail("write temporary file", err)
	}
	if err := temporary.Sync(); err != nil {
		return fail("sync temporary file", err)
	}
	if err := temporary.Close(); err != nil {
		return ledgerFileError(parent.label, "close temporary file", err)
	}
	existing, _, exists, err := openTrustedLedgerFile(
		parent,
		parent.targetName,
		unix.O_RDONLY,
		"ledger",
	)
	if err != nil {
		return err
	}
	if exists {
		_ = unix.Close(existing)
	}
	if err := unix.Renameat(
		parent.descriptor,
		temporaryName,
		parent.descriptor,
		parent.targetName,
	); err != nil {
		return ledgerFileError(parent.label, "atomically replace", err)
	}
	removeTemporary = false
	return l.syncPinnedDirectory(parent)
}

func createLedgerTemporary(
	parent *pinnedRecoveryLedgerDirectory,
) (string, int, error) {
	for attempt := 0; attempt < 128; attempt++ {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return "", -1, ledgerFileError(
				parent.label,
				"generate temporary name",
				err,
			)
		}
		name := "." + parent.targetName + ".tmp-" + fmt.Sprintf("%x", random)
		descriptor, err := unix.Openat(
			parent.descriptor,
			name,
			unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|
				unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0o600,
		)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", -1, ledgerFileError(
				parent.label,
				"create same-directory temporary file",
				err,
			)
		}
		if err := unix.Fchmod(descriptor, 0o600); err != nil {
			_ = unix.Close(descriptor)
			_ = unix.Unlinkat(parent.descriptor, name, 0)
			return "", -1, ledgerFileError(
				parent.label,
				"set new temporary file mode",
				err,
			)
		}
		if _, err := validateTrustedLedgerDescriptor(
			descriptor,
			parent.label,
			"temporary file",
		); err != nil {
			_ = unix.Close(descriptor)
			_ = unix.Unlinkat(parent.descriptor, name, 0)
			return "", -1, err
		}
		return name, descriptor, nil
	}
	return "", -1, fmt.Errorf(
		"create recovery ledger %q temporary file: name attempts exhausted",
		parent.label,
	)
}

func (l *fileRecoveryLedger) withExclusiveLock(
	ctx context.Context,
	createParent bool,
	operation func(*pinnedRecoveryLedgerDirectory) error,
) error {
	path, err := safeRecoveryLedgerPath(l.path)
	if err != nil {
		return err
	}
	processLock := recoveryLedgerMutex(path)
	processLock.Lock()
	defer processLock.Unlock()

	parent, err := l.openPinnedParent(path, createParent)
	if errors.Is(err, os.ErrNotExist) && !createParent {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(parent.descriptor)

	// A read of a missing ledger is an empty snapshot. Return before creating
	// the coordination lock so dry-run/status/doctor remain strictly
	// read-only when no local recovery state exists.
	if !createParent {
		descriptor, _, exists, err := openTrustedLedgerFile(
			parent,
			parent.targetName,
			unix.O_RDONLY,
			"file",
		)
		if err != nil {
			return err
		}
		if exists {
			_ = unix.Close(descriptor)
		} else {
			return nil
		}
		lockDescriptor, _, lockExists, err := openTrustedLedgerFile(
			parent,
			parent.targetName+".lock",
			unix.O_RDONLY,
			"lock",
		)
		if err != nil {
			return err
		}
		if lockExists {
			_ = unix.Close(lockDescriptor)
		}
		// Writers replace the ledger atomically in the same pinned directory.
		// Reading the trusted target while holding the process mutex therefore
		// gives a complete old-or-new snapshot without creating or depending on
		// cross-process coordination state.
		return operation(parent)
	}

	lockDescriptor, err := l.openTrustedLock(parent)
	if err != nil {
		return err
	}
	defer unix.Close(lockDescriptor)
	if err := lockLedgerFile(ctx, lockDescriptor); err != nil {
		return err
	}
	defer unix.Flock(lockDescriptor, unix.LOCK_UN)
	if err := verifyPinnedLockName(parent, lockDescriptor); err != nil {
		return err
	}
	return operation(parent)
}

func (l *fileRecoveryLedger) openPinnedParent(
	path string,
	create bool,
) (*pinnedRecoveryLedgerDirectory, error) {
	parentPath := filepath.Dir(path)
	components := strings.Split(
		strings.TrimPrefix(parentPath, string(filepath.Separator)),
		string(filepath.Separator),
	)
	current, err := unix.Open(
		string(filepath.Separator),
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return nil, ledgerFileError(
			filepath.Base(path),
			"open filesystem root",
			err,
		)
	}
	for _, component := range components {
		if component == "" {
			continue
		}
		next, openErr := unix.Openat(
			current,
			component,
			unix.O_RDONLY|unix.O_DIRECTORY|
				unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if errors.Is(openErr, unix.ENOENT) && create {
			if mkdirErr := unix.Mkdirat(current, component, 0o700); mkdirErr != nil &&
				!errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(current)
				return nil, ledgerFileError(
					filepath.Base(path),
					"create trusted parent directory",
					mkdirErr,
				)
			}
			if mkdirErr := l.syncDescriptor(current); mkdirErr != nil {
				_ = unix.Close(current)
				return nil, ledgerFileError(
					filepath.Base(path),
					"sync newly created parent directory entry",
					mkdirErr,
				)
			}
			next, openErr = unix.Openat(
				current,
				component,
				unix.O_RDONLY|unix.O_DIRECTORY|
					unix.O_CLOEXEC|unix.O_NOFOLLOW,
				0,
			)
		}
		if openErr != nil {
			_ = unix.Close(current)
			if errors.Is(openErr, unix.ELOOP) ||
				errors.Is(openErr, unix.ENOTDIR) {
				return nil, fmt.Errorf(
					"recovery ledger parent contains a symlink or non-directory ancestor",
				)
			}
			if errors.Is(openErr, unix.ENOENT) {
				return nil, os.ErrNotExist
			}
			return nil, ledgerFileError(
				filepath.Base(path),
				"open trusted parent directory",
				openErr,
			)
		}
		_ = unix.Close(current)
		current = next
	}
	var stat unix.Stat_t
	if err := unix.Fstat(current, &stat); err != nil {
		_ = unix.Close(current)
		return nil, ledgerFileError(
			filepath.Base(path),
			"inspect pinned parent directory",
			err,
		)
	}
	if uint32(stat.Mode)&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(current)
		return nil, fmt.Errorf(
			"recovery ledger parent must be a directory",
		)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		_ = unix.Close(current)
		return nil, fmt.Errorf(
			"recovery ledger parent must be owned by the current user",
		)
	}
	if uint32(stat.Mode)&0o022 != 0 {
		_ = unix.Close(current)
		return nil, fmt.Errorf(
			"recovery ledger parent must not be group/world-writable",
		)
	}
	return &pinnedRecoveryLedgerDirectory{
		descriptor: current,
		targetName: filepath.Base(path),
		label:      filepath.Base(path),
	}, nil
}

func (l *fileRecoveryLedger) openTrustedLock(
	parent *pinnedRecoveryLedgerDirectory,
) (int, error) {
	name := parent.targetName + ".lock"
	descriptor, err := unix.Openat(
		parent.descriptor,
		name,
		unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|
			unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		descriptor, err = unix.Openat(
			parent.descriptor,
			name,
			unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return -1, fmt.Errorf(
				"recovery ledger lock for %q must not be a symlink",
				parent.label,
			)
		}
		return -1, ledgerFileError(parent.label, "open lock file", err)
	}
	if created {
		if err := unix.Fchmod(descriptor, 0o600); err != nil {
			_ = unix.Close(descriptor)
			return -1, ledgerFileError(
				parent.label,
				"set new lock file mode",
				err,
			)
		}
	}
	if _, err := validateTrustedLedgerDescriptor(
		descriptor,
		parent.label,
		"lock",
	); err != nil {
		_ = unix.Close(descriptor)
		return -1, err
	}
	if created {
		if err := l.syncPinnedDirectory(parent); err != nil {
			_ = unix.Close(descriptor)
			return -1, err
		}
	}
	return descriptor, nil
}

func openTrustedLedgerFile(
	parent *pinnedRecoveryLedgerDirectory,
	name string,
	flags int,
	kind string,
) (int, unix.Stat_t, bool, error) {
	descriptor, err := unix.Openat(
		parent.descriptor,
		name,
		flags|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if errors.Is(err, unix.ENOENT) {
		return -1, unix.Stat_t{}, false, nil
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return -1, unix.Stat_t{}, false, fmt.Errorf(
				"recovery ledger %s %q must not be a symlink",
				kind,
				parent.label,
			)
		}
		return -1, unix.Stat_t{}, false, ledgerFileError(
			parent.label,
			"open "+kind,
			err,
		)
	}
	stat, err := validateTrustedLedgerDescriptor(
		descriptor,
		parent.label,
		kind,
	)
	if err != nil {
		_ = unix.Close(descriptor)
		return -1, unix.Stat_t{}, false, err
	}
	return descriptor, stat, true, nil
}

func validateTrustedLedgerDescriptor(
	descriptor int,
	label string,
	kind string,
) (unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(descriptor, &stat); err != nil {
		return unix.Stat_t{}, ledgerFileError(
			label,
			"inspect "+kind,
			err,
		)
	}
	if uint32(stat.Mode)&unix.S_IFMT != unix.S_IFREG {
		return unix.Stat_t{}, fmt.Errorf(
			"recovery ledger %s for %q must be a regular file",
			kind,
			label,
		)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return unix.Stat_t{}, fmt.Errorf(
			"recovery ledger %s for %q must be owned by the current user",
			kind,
			label,
		)
	}
	if uint64(stat.Nlink) != 1 {
		return unix.Stat_t{}, fmt.Errorf(
			"recovery ledger %s for %q must have link count 1",
			kind,
			label,
		)
	}
	if uint32(stat.Mode)&0o777 != 0o600 {
		return unix.Stat_t{}, fmt.Errorf(
			"recovery ledger %s for %q must have mode 0600",
			kind,
			label,
		)
	}
	return stat, nil
}

func verifyPinnedLockName(
	parent *pinnedRecoveryLedgerDirectory,
	descriptor int,
) error {
	pinned, err := validateTrustedLedgerDescriptor(
		descriptor,
		parent.label,
		"lock",
	)
	if err != nil {
		return err
	}
	var named unix.Stat_t
	if err := unix.Fstatat(
		parent.descriptor,
		parent.targetName+".lock",
		&named,
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return ledgerFileError(
			parent.label,
			"verify pinned lock name",
			err,
		)
	}
	if named.Dev != pinned.Dev || named.Ino != pinned.Ino {
		return fmt.Errorf(
			"recovery ledger lock for %q changed while acquiring lock",
			parent.label,
		)
	}
	return nil
}

func recoveryLedgerMutex(path string) *sync.Mutex {
	value, _ := recoveryLedgerPathLocks.LoadOrStore(path, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func lockLedgerFile(ctx context.Context, descriptor int) error {
	for {
		err := unix.Flock(descriptor, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) &&
			!errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("lock recovery ledger: %w", err)
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("lock recovery ledger: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (l *fileRecoveryLedger) syncPinnedDirectory(
	parent *pinnedRecoveryLedgerDirectory,
) error {
	if err := l.syncDescriptor(parent.descriptor); err != nil {
		return ledgerFileError(parent.label, "sync parent directory", err)
	}
	return nil
}

func (l *fileRecoveryLedger) syncDescriptor(descriptor int) error {
	syncDirectory := l.syncDirectory
	if syncDirectory == nil {
		syncDirectory = syncRecoveryLedgerDirectory
	}
	return syncDirectory(descriptor)
}

func syncRecoveryLedgerDirectory(descriptor int) error {
	return unix.Fsync(descriptor)
}

func safeRecoveryLedgerPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" ||
		journalStringContainsCredentialMaterial(path) {
		return "", fmt.Errorf("unsafe recovery ledger path")
	}
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("unsafe recovery ledger path")
	}
	absolute = canonicalDarwinSystemPath(absolute)
	root := filepath.VolumeName(absolute) + string(filepath.Separator)
	home, homeErr := os.UserHomeDir()
	if absolute == root ||
		homeErr == nil && sameCleanPath(absolute, home) ||
		strings.ToLower(filepath.Ext(absolute)) != ".json" {
		return "", fmt.Errorf("unsafe recovery ledger path")
	}
	return absolute, nil
}

func canonicalDarwinSystemPath(path string) string {
	if runtime.GOOS != "darwin" {
		return path
	}
	for _, alias := range []string{"/var", "/tmp", "/etc"} {
		if path != alias &&
			!strings.HasPrefix(path, alias+string(filepath.Separator)) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(alias)
		if err == nil && filepath.IsAbs(resolved) {
			return resolved + strings.TrimPrefix(path, alias)
		}
	}
	return path
}

func sameCleanPath(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(filepath.Clean(left))
	rightAbsolute, rightErr := filepath.Abs(filepath.Clean(right))
	return leftErr == nil && rightErr == nil && leftAbsolute == rightAbsolute
}

// rejectUnsafeLedgerTarget is a non-mutating early config validation. The
// ledger itself repeats all trust checks from pinned directory descriptors.
func rejectUnsafeLedgerTarget(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return ledgerFileError(path, "inspect", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(
			"recovery ledger %q must not be a symlink",
			filepath.Base(path),
		)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf(
			"recovery ledger %q must be a regular file",
			filepath.Base(path),
		)
	}
	return nil
}

func corruptLedgerError(path string, err error) error {
	return fmt.Errorf(
		"recovery ledger %q is corrupt; repair or move it before retrying: %w",
		filepath.Base(path),
		err,
	)
}

func ledgerFileError(path, operation string, err error) error {
	return fmt.Errorf(
		"%s recovery ledger %q: %w",
		operation,
		filepath.Base(path),
		err,
	)
}
