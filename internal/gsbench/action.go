package gsbench

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

type ActionKind string

const (
	ActionSQLMutation        ActionKind = "SQL_MUTATION"
	ActionSessionSet         ActionKind = "SESSION_SET"
	ActionSessionTransaction ActionKind = "SESSION_TRANSACTION"
	ActionGUCFileChange      ActionKind = "GUC_FILE_CHANGE"
	ActionNetworkQDisc       ActionKind = "NETWORK_QDISC"
	ActionNetworkFirewall    ActionKind = "NETWORK_FIREWALL"
	ActionProcessState       ActionKind = "PROCESS_STATE"
	ActionNodeRole           ActionKind = "NODE_ROLE"
	ActionCloudFaultJob      ActionKind = "CLOUD_FAULT_JOB"
	ActionDataBaseline       ActionKind = "DATA_BASELINE"
)

type Action struct {
	Sequence      int64
	RunID         string
	ScenarioCode  ScenarioCode
	Kind          ActionKind
	TargetProduct Product
	Target        string
	Node          string
	Original      json.RawMessage
	Forward       json.RawMessage
	Inverse       json.RawMessage
	Verify        json.RawMessage
	State         MutationState
	LastError     string
	LegacySQL     bool
}

type ActionStore interface {
	InsertPlanned(context.Context, Action) (Action, error)
	SetState(context.Context, string, int64, MutationState, string) error
	Pending(context.Context, string) ([]Action, error)
	StaleRuns(context.Context) ([]string, error)
}

type ActionExecutor interface {
	Preflight(context.Context, Action) error
	Apply(context.Context, Action) error
	Restore(context.Context, Action) error
	VerifyRestored(context.Context, Action) error
}

func (a Action) Validate() error {
	if strings.TrimSpace(a.RunID) == "" {
		return fmt.Errorf("action run ID is required")
	}
	if a.ScenarioCode == 0 {
		return fmt.Errorf("action scenario code is required")
	}
	if !a.Kind.valid() {
		return fmt.Errorf("action kind %q is invalid", a.Kind)
	}
	if !a.TargetProduct.known() {
		return fmt.Errorf("action target product is required and must be known")
	}
	if strings.TrimSpace(a.Target) == "" {
		return fmt.Errorf("action target is required")
	}
	if a.Kind.requiresNode() && strings.TrimSpace(a.Node) == "" {
		return fmt.Errorf("action target node is required for kind %q", a.Kind)
	}
	for name, value := range map[string]string{
		"target":      a.Target,
		"target node": a.Node,
		"last error":  a.LastError,
	} {
		if err := validateJournalStringField(name, value); err != nil {
			return err
		}
	}
	if err := validateActionPayload("forward", a.Forward, true); err != nil {
		return err
	}
	if err := validateActionPayload(
		"inverse", a.Inverse, a.Kind.persistent(),
	); err != nil {
		return err
	}
	if err := validateActionPayload("original", a.Original, false); err != nil {
		return err
	}
	return validateActionPayload("verify", a.Verify, false)
}

func (product Product) known() bool {
	return product == ProductOpenGauss || product == ProductGaussDB
}

func (kind ActionKind) valid() bool {
	switch kind {
	case ActionSQLMutation,
		ActionSessionSet,
		ActionSessionTransaction,
		ActionGUCFileChange,
		ActionNetworkQDisc,
		ActionNetworkFirewall,
		ActionProcessState,
		ActionNodeRole,
		ActionCloudFaultJob,
		ActionDataBaseline:
		return true
	default:
		return false
	}
}

func (kind ActionKind) requiresNode() bool {
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

func (kind ActionKind) persistent() bool {
	switch kind {
	case ActionSessionSet, ActionSessionTransaction:
		return false
	default:
		return true
	}
}

func validateActionPayload(name string, payload json.RawMessage, required bool) error {
	if len(payload) == 0 {
		if required {
			return fmt.Errorf("action %s payload is required", name)
		}
		return nil
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return fmt.Errorf("action %s payload is invalid JSON: %w", name, err)
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return fmt.Errorf("action %s payload must be a JSON object", name)
	}
	if key := secretActionPayloadKey(object); key != "" {
		return fmt.Errorf("action %s payload contains forbidden secret-bearing key %q", name, key)
	}
	if actionPayloadContainsCredentialMaterial(object) {
		return fmt.Errorf("action %s payload contains forbidden credential material", name)
	}
	return nil
}

func secretActionPayloadKey(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if obviousSecretKey(key) {
				return key
			}
			if nested := secretActionPayloadKey(child); nested != "" {
				return nested
			}
		}
	case []any:
		for _, child := range typed {
			if nested := secretActionPayloadKey(child); nested != "" {
				return nested
			}
		}
	}
	return ""
}

func obviousSecretKey(key string) bool {
	segments := actionPayloadKeySegments(key)
	if len(segments) == 0 {
		return false
	}
	if last := segments[len(segments)-1]; last == "ref" || last == "id" {
		return false
	}
	switch segments[len(segments)-1] {
	case "password", "passwd", "pwd", "secret", "token",
		"authorization", "credential", "credentials":
		return true
	}
	for index := 0; index+1 < len(segments); index++ {
		pair := segments[index] + "_" + segments[index+1]
		if pair == "private_key" || pair == "api_key" ||
			pair == "client_secret" {
			return true
		}
	}
	if len(segments) == 1 {
		switch segments[0] {
		case "apikey", "dbpassword", "oauthtoken", "accesstoken",
			"refreshtoken", "authtoken", "privatekey", "clientsecret":
			return true
		}
	}
	return false
}

func actionPayloadKeySegments(key string) []string {
	var normalized strings.Builder
	runes := []rune(strings.TrimSpace(key))
	for index, current := range runes {
		if unicode.IsUpper(current) && index > 0 {
			previous := runes[index-1]
			var next rune
			if index+1 < len(runes) {
				next = runes[index+1]
			}
			if unicode.IsLower(previous) || unicode.IsDigit(previous) ||
				unicode.IsUpper(previous) && unicode.IsLower(next) {
				normalized.WriteByte('_')
			}
		}
		if unicode.IsLetter(current) || unicode.IsDigit(current) {
			normalized.WriteRune(unicode.ToLower(current))
		} else if normalized.Len() == 0 ||
			normalized.String()[normalized.Len()-1] != '_' {
			normalized.WriteByte('_')
		}
	}
	return strings.FieldsFunc(normalized.String(), func(value rune) bool {
		return value == '_'
	})
}

var credentialMaterialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:password|passwd)\b\s*=\s*[^\s;,]+`),
	regexp.MustCompile(`(?i)\b(?:password|passwd)\b\s+to\s+[^\s;,]+`),
	regexp.MustCompile(`(?is)\b(?:alter|create)\s+(?:role|user)\b.*\b(?:password|passwd)\b\s+(?:'|"|\$|[^\s;,]+)`),
	regexp.MustCompile(`(?i)(?:authorization\s*:\s*)?bearer\s+[^\s,;]+`),
	regexp.MustCompile(`(?i)(?:authorization\s*:\s*)?basic\s+[A-Za-z0-9+/=]{8,}`),
	regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/@:]+:[^@\s/]+@`),
	regexp.MustCompile(`(?i)(?:[?&;])(?:x-(?:amz|goog)-signature|signature|sig|access_token|auth(?:orization)?|api[_-]?key|token)=[^&#\s]+`),
}

func actionPayloadContainsCredentialMaterial(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			if actionPayloadContainsCredentialMaterial(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if actionPayloadContainsCredentialMaterial(child) {
				return true
			}
		}
	case string:
		return journalStringContainsCredentialMaterial(typed)
	}
	return false
}

func validateJournalStringField(name, value string) error {
	if value != "" && journalStringContainsCredentialMaterial(value) {
		return fmt.Errorf(
			"action %s contains forbidden credential material",
			name,
		)
	}
	return nil
}

func journalStringContainsCredentialMaterial(value string) bool {
	for _, pattern := range credentialMaterialPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}

const journalErrorMaxRunes = 512

func journalSafeErrorText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if journalStringContainsCredentialMaterial(value) {
		return "journal error details redacted"
	}
	value = strings.Join(strings.FieldsFunc(value, func(character rune) bool {
		return unicode.IsControl(character) || unicode.IsSpace(character)
	}), " ")
	runes := []rune(value)
	if len(runes) <= journalErrorMaxRunes {
		return value
	}
	return string(runes[:journalErrorMaxRunes-1]) + "…"
}

func SQLAction(m Mutation) Action {
	m = normalizeMutationCompatibility(m)
	action := Action{
		RunID:         m.RunID,
		ScenarioCode:  m.ScenarioCode,
		Kind:          ActionSQLMutation,
		TargetProduct: m.TargetProduct,
		Target:        m.TargetEndpoint,
		Node:          m.TargetNode,
	}
	if m.OriginalState != "" {
		action.Original = marshalActionPayload(struct {
			Value string `json:"value"`
		}{Value: m.OriginalState})
	}
	if m.ForwardAction != "" {
		action.Forward = marshalActionPayload(struct {
			SQL string `json:"sql"`
		}{SQL: m.ForwardAction})
	}
	if m.InverseAction != "" {
		if len(m.InverseSessionSQL) != 0 {
			action.Inverse = marshalActionPayload(struct {
				SessionSQL []string `json:"session_sql"`
			}{SessionSQL: append([]string(nil), m.InverseSessionSQL...)})
		} else {
			action.Inverse = marshalActionPayload(struct {
				SQL string `json:"sql"`
			}{SQL: m.InverseAction})
		}
	}
	if m.VerifyAction != "" {
		action.Verify = marshalActionPayload(struct {
			SQL      string `json:"sql"`
			Expected string `json:"expected"`
		}{SQL: m.VerifyAction, Expected: m.VerifyValue})
	}
	return action
}

func marshalActionPayload(value any) json.RawMessage {
	payload, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("marshal internal action payload: %v", err))
	}
	return payload
}

func sqlStatementFromActionPayload(payload json.RawMessage) (string, bool) {
	var decoded struct {
		SQL        string   `json:"sql"`
		SessionSQL []string `json:"session_sql"`
	}
	if json.Unmarshal(payload, &decoded) != nil {
		return "", false
	}
	statement := strings.TrimSpace(decoded.SQL)
	if statement != "" {
		return statement, true
	}
	for _, candidate := range decoded.SessionSQL {
		candidate = strings.TrimSpace(candidate)
		fields := strings.Fields(candidate)
		if len(fields) != 0 && strings.EqualFold(fields[0], "ANALYZE") {
			return candidate, true
		}
	}
	for _, candidate := range decoded.SessionSQL {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			return candidate, true
		}
	}
	return "", false
}
