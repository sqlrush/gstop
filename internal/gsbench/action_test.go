package gsbench

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestActionValidateAcceptsEverySupportedKind(t *testing.T) {
	kinds := []ActionKind{
		ActionSQLMutation,
		ActionSessionSet,
		ActionSessionTransaction,
		ActionGUCFileChange,
		ActionNetworkQDisc,
		ActionNetworkFirewall,
		ActionProcessState,
		ActionNodeRole,
		ActionCloudFaultJob,
		ActionDataBaseline,
	}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			action := Action{
				RunID:         "run-1",
				ScenarioCode:  601,
				Kind:          kind,
				TargetProduct: ProductGaussDB,
				Target:        "gsbench.plan_data",
				Node:          "dn_1",
				Forward:       json.RawMessage(`{"operation":"apply"}`),
				Inverse:       json.RawMessage(`{"operation":"restore"}`),
			}
			if err := action.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestActionKindsUseStableJournalProtocolValues(t *testing.T) {
	got := []string{
		string(ActionSQLMutation),
		string(ActionSessionSet),
		string(ActionSessionTransaction),
		string(ActionGUCFileChange),
		string(ActionNetworkQDisc),
		string(ActionNetworkFirewall),
		string(ActionProcessState),
		string(ActionNodeRole),
		string(ActionCloudFaultJob),
		string(ActionDataBaseline),
	}
	want := []string{
		"SQL_MUTATION",
		"SESSION_SET",
		"SESSION_TRANSACTION",
		"GUC_FILE_CHANGE",
		"NETWORK_QDISC",
		"NETWORK_FIREWALL",
		"PROCESS_STATE",
		"NODE_ROLE",
		"CLOUD_FAULT_JOB",
		"DATA_BASELINE",
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("action kind %d = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestActionValidateRequiresIdentityTargetAndPersistentInverse(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Action)
		want   string
	}{
		{
			name: "run ID",
			mutate: func(action *Action) {
				action.RunID = " "
			},
			want: "run ID",
		},
		{
			name: "scenario code",
			mutate: func(action *Action) {
				action.ScenarioCode = 0
			},
			want: "scenario code",
		},
		{
			name: "target",
			mutate: func(action *Action) {
				action.Target = ""
			},
			want: "target",
		},
		{
			name: "persistent inverse",
			mutate: func(action *Action) {
				action.Inverse = nil
			},
			want: "inverse payload",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action := Action{
				RunID:         "run-1",
				ScenarioCode:  601,
				Kind:          ActionNetworkFirewall,
				TargetProduct: ProductGaussDB,
				Target:        "rule-1",
				Node:          "dn_1",
				Forward:       json.RawMessage(`{"rule":"add"}`),
				Inverse:       json.RawMessage(`{"rule":"delete"}`),
			}
			tt.mutate(&action)
			err := action.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestActionValidateRequiresKnownProductAndInfrastructureNode(t *testing.T) {
	tests := []struct {
		name   string
		action Action
		want   string
	}{
		{
			name: "unknown product",
			action: Action{
				RunID: "run-1", ScenarioCode: 601, Kind: ActionSQLMutation,
				TargetProduct: ProductUnknown, Target: "gsbench.plan_data",
				Forward: json.RawMessage(`{"sql":"SELECT 1"}`),
				Inverse: json.RawMessage(`{"sql":"SELECT 1"}`),
			},
			want: "target product",
		},
		{
			name: "infrastructure node",
			action: Action{
				RunID: "run-1", ScenarioCode: 343, Kind: ActionNetworkFirewall,
				TargetProduct: ProductGaussDB, Target: "rule-1",
				Forward: json.RawMessage(`{"rule":"add"}`),
				Inverse: json.RawMessage(`{"rule":"delete"}`),
			},
			want: "target node",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.action.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestActionValidateRejectsUnknownKind(t *testing.T) {
	action := Action{
		RunID:         "run-1",
		ScenarioCode:  601,
		Kind:          ActionKind("sql"),
		TargetProduct: ProductGaussDB,
		Target:        "gsbench.plan_data",
		Forward:       json.RawMessage(`{"sql":"SELECT 1"}`),
		Inverse:       json.RawMessage(`{"sql":"SELECT 1"}`),
	}
	err := action.Validate()
	if err == nil || !strings.Contains(err.Error(), "action kind") {
		t.Fatalf("Validate() error = %v, want invalid action kind", err)
	}
}

func TestActionValidateRejectsSecretBearingPayloadKey(t *testing.T) {
	for _, payload := range []string{
		`{"request":{"access_token":"do-not-store"}}`,
		`{"request":{"accessToken":"do-not-store"}}`,
		`{"privateKey":"do-not-store"}`,
		`{"database_password":"do-not-store"}`,
		`{"bearer_token":"do-not-store"}`,
		`{"service_credentials":"do-not-store"}`,
		`{"private_key_pem":"do-not-store"}`,
		`{"APIKey":"do-not-store"}`,
		`{"DBPassword":"do-not-store"}`,
		`{"OAuthToken":"do-not-store"}`,
	} {
		action := Action{
			RunID:         "run-1",
			ScenarioCode:  601,
			Kind:          ActionCloudFaultJob,
			TargetProduct: ProductGaussDB,
			Target:        "fault-job",
			Node:          "dn_1",
			Forward:       json.RawMessage(payload),
			Inverse:       json.RawMessage(`{"job_id":"job-1"}`),
		}
		err := action.Validate()
		if err == nil || !strings.Contains(err.Error(), "secret-bearing key") {
			t.Fatalf("Validate(%s) error = %v, want secret-key rejection", payload, err)
		}
	}
}

func TestActionValidateAllowsCredentialReferenceMetadata(t *testing.T) {
	action := Action{
		RunID: "run-1", ScenarioCode: 601, Kind: ActionSQLMutation,
		TargetProduct: ProductGaussDB, Target: "gsbench.plan_data",
		Original: json.RawMessage(`{
			"private_key_ref":"vault://keys/bench",
			"api_key_ref":"secret-manager://api/bench",
			"credential_id":"credential-record-7",
			"token_count":4
		}`),
		Forward: json.RawMessage(`{"sql":"SELECT 1"}`),
		Inverse: json.RawMessage(`{"sql":"SELECT 1"}`),
	}
	if err := action.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestActionValidateRejectsCredentialMaterialInPersistedStringFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Action)
	}{
		{
			name: "URI userinfo",
			mutate: func(action *Action) {
				action.Target = "postgres://bench:swordfish@db.example/bench"
			},
		},
		{
			name: "signed endpoint",
			mutate: func(action *Action) {
				action.Target = "https://service/fault?X-Amz-Signature=abcdef123456"
			},
		},
		{
			name: "node authorization",
			mutate: func(action *Action) {
				action.Node = "Authorization: Basic dXNlcjpwYXNz"
			},
		},
		{
			name: "caller last error",
			mutate: func(action *Action) {
				action.LastError = "provider failed password=swordfish"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action := Action{
				RunID: "run-1", ScenarioCode: 343,
				Kind: ActionNetworkFirewall, TargetProduct: ProductGaussDB,
				Target: "rule-1", Node: "dn_1",
				Forward: json.RawMessage(`{"operation":"apply"}`),
				Inverse: json.RawMessage(`{"operation":"restore"}`),
			}
			tt.mutate(&action)
			err := action.Validate()
			if err == nil || !strings.Contains(err.Error(), "credential material") {
				t.Fatalf("Validate() error = %v", err)
			}
			if strings.Contains(err.Error(), "swordfish") ||
				strings.Contains(err.Error(), "abcdef123456") ||
				strings.Contains(err.Error(), "dXNlcjpwYXNz") {
				t.Fatalf("validation error leaked credential: %v", err)
			}
		})
	}
}

func TestActionValidateAllowsOrdinaryNodeAndUnsignedEndpoint(t *testing.T) {
	action := Action{
		RunID: "run-1", ScenarioCode: 343,
		Kind: ActionNetworkFirewall, TargetProduct: ProductGaussDB,
		Target: "https://service/fault?job_id=job-7", Node: "dn_primary_1",
		Forward: json.RawMessage(`{"operation":"apply"}`),
		Inverse: json.RawMessage(`{"operation":"restore"}`),
	}
	if err := action.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestActionValidateRejectsCredentialMaterialWithoutEchoingIt(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		value  string
	}{
		{
			name:   "SQL password",
			secret: "swordfish",
			value:  `{"sql":"ALTER ROLE bench PASSWORD 'swordfish'"}`,
		},
		{
			name:   "DSN password",
			secret: "dsn-secret",
			value:  `{"value":"host=db.example password=dsn-secret"}`,
		},
		{
			name:   "bearer authorization",
			secret: "eyJhbGciOiJIUzI1NiJ9.payload.signature",
			value:  `{"header":"Bearer eyJhbGciOiJIUzI1NiJ9.payload.signature"}`,
		},
		{
			name:   "private key",
			secret: "PRIVATE KEY",
			value:  `{"value":"-----BEGIN PRIVATE KEY-----\nmaterial\n-----END PRIVATE KEY-----"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action := Action{
				RunID: "run-1", ScenarioCode: 601, Kind: ActionSQLMutation,
				TargetProduct: ProductGaussDB, Target: "gsbench.plan_data",
				Forward: json.RawMessage(tt.value),
				Inverse: json.RawMessage(`{"sql":"SELECT 1"}`),
			}
			err := action.Validate()
			if err == nil || !strings.Contains(err.Error(), "credential material") {
				t.Fatalf("Validate() error = %v", err)
			}
			if strings.Contains(err.Error(), tt.secret) {
				t.Fatalf("validation error leaked secret material: %v", err)
			}
		})
	}
}

func TestActionValidateAllowsBenignTokenCountName(t *testing.T) {
	action := Action{
		RunID: "run-1", ScenarioCode: 601, Kind: ActionSQLMutation,
		TargetProduct: ProductGaussDB, Target: "gsbench.metrics",
		Forward: json.RawMessage(`{"sql":"SELECT token_count FROM gsbench.metrics"}`),
		Inverse: json.RawMessage(`{"sql":"SELECT token_count FROM gsbench.metrics"}`),
	}
	if err := action.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestSQLActionBuildsTypedJSONPayloads(t *testing.T) {
	action := SQLAction(Mutation{
		RunID:          "run-1",
		ScenarioCode:   601,
		TargetProduct:  ProductGaussDB,
		TargetNode:     "dn_1",
		TargetEndpoint: "gsbench.plan_data",
		OriginalState:  "before",
		ForwardAction:  "ALTER TABLE forward",
		InverseAction:  "ALTER TABLE inverse",
		VerifyAction:   "SELECT state",
		VerifyValue:    "before",
	})
	if action.Kind != ActionSQLMutation ||
		action.Target != "gsbench.plan_data" ||
		action.Node != "dn_1" ||
		action.TargetProduct != ProductGaussDB {
		t.Fatalf("SQLAction() = %+v", action)
	}
	for name, payload := range map[string]json.RawMessage{
		"original": action.Original,
		"forward":  action.Forward,
		"inverse":  action.Inverse,
		"verify":   action.Verify,
	} {
		if !json.Valid(payload) {
			t.Errorf("%s payload is not JSON: %q", name, payload)
		}
	}
	if string(action.Forward) != `{"sql":"ALTER TABLE forward"}` {
		t.Fatalf("forward payload = %s", action.Forward)
	}
	if string(action.Verify) != `{"sql":"SELECT state","expected":"before"}` {
		t.Fatalf("verify payload = %s", action.Verify)
	}
}
