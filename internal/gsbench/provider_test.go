package gsbench

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNoopFaultProviderRejectsEveryOperation(t *testing.T) {
	provider := NoopFaultProvider{ConfiguredProvider: "local"}
	var _ FaultProvider = provider
	action := validLedgerAction("run-1", "firewall-rule-1")
	operations := []struct {
		name string
		run  func() error
	}{
		{
			name: "preflight",
			run: func() error {
				return provider.Preflight(context.Background(), Environment{}, action)
			},
		},
		{name: "apply", run: func() error {
			return provider.Apply(context.Background(), action)
		}},
		{name: "restore", run: func() error {
			return provider.Restore(context.Background(), action)
		}},
		{name: "verify restored", run: func() error {
			return provider.VerifyRestored(context.Background(), action)
		}},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.run()
			if err == nil {
				t.Fatal("operation unexpectedly succeeded")
			}
			if !strings.Contains(err.Error(), "local") ||
				!strings.Contains(err.Error(), string(ActionNetworkFirewall)) {
				t.Fatalf("error = %q, want configured provider and action kind", err)
			}
		})
	}
}

func TestNoopFaultProviderRejectsEveryInfrastructureKind(t *testing.T) {
	provider := NoopFaultProvider{ConfiguredProvider: "ssh"}
	for _, kind := range []ActionKind{
		ActionGUCFileChange,
		ActionNetworkQDisc,
		ActionNetworkFirewall,
		ActionProcessState,
		ActionNodeRole,
		ActionCloudFaultJob,
	} {
		t.Run(string(kind), func(t *testing.T) {
			action := validLedgerAction("run-1", "target-1")
			action.Kind = kind
			err := provider.Apply(context.Background(), action)
			if err == nil || !strings.Contains(err.Error(), "ssh") ||
				!strings.Contains(err.Error(), string(kind)) {
				t.Fatalf("Apply() error = %v", err)
			}
		})
	}
}

type namedFaultProvider struct {
	name string
}

func (p namedFaultProvider) Name() string {
	return p.name
}

func (p namedFaultProvider) Preflight(
	context.Context,
	Environment,
	Action,
) error {
	return nil
}

func (p namedFaultProvider) Apply(context.Context, Action) error {
	return nil
}

func (p namedFaultProvider) Restore(context.Context, Action) error {
	return nil
}

func (p namedFaultProvider) VerifyRestored(context.Context, Action) error {
	return nil
}

func TestFaultProviderRegistryDispatchesOnlyRegisteredAdapter(t *testing.T) {
	registry := NewFaultProviderRegistry()
	if err := registry.Register(
		"local",
		func(config FaultProviderConfig) (FaultProvider, error) {
			if config.LedgerPath != "/safe/logs/recovery.json" {
				return nil, errors.New("factory received wrong config")
			}
			return namedFaultProvider{name: "local-test"}, nil
		},
	); err != nil {
		t.Fatal(err)
	}

	provider, err := NewFaultProvider(registry, FaultProviderConfig{
		Type:       "local",
		LedgerPath: "/safe/logs/recovery.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "local-test" {
		t.Fatalf("provider name = %q", provider.Name())
	}
}

func TestDefaultFaultProviderRegistryIsShared(t *testing.T) {
	first := DefaultFaultProviderRegistry()
	second := DefaultFaultProviderRegistry()
	if first == nil || first != second {
		t.Fatalf("default registries are not shared: %p %p", first, second)
	}
}

func TestFaultProviderRegistryFailsClosedForAbsentAdapters(t *testing.T) {
	for _, providerType := range []string{"local", "ssh", "gaussdb_api"} {
		t.Run(providerType, func(t *testing.T) {
			provider, err := NewFaultProvider(
				NewFaultProviderRegistry(),
				FaultProviderConfig{
					Type:       providerType,
					LedgerPath: "/safe/logs/recovery.json",
				},
			)
			if err == nil {
				t.Fatalf("provider = %T, want unavailable error", provider)
			}
			if !strings.Contains(err.Error(), providerType) ||
				!strings.Contains(err.Error(), "not registered") {
				t.Fatalf("error = %q", err)
			}
		})
	}
}

func TestFaultProviderRegistryBuildsRejectingNoneProvider(t *testing.T) {
	provider, err := NewFaultProvider(
		NewFaultProviderRegistry(),
		FaultProviderConfig{
			Type:       "none",
			LedgerPath: "/safe/logs/recovery.json",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "none" {
		t.Fatalf("provider name = %q", provider.Name())
	}
	if err := provider.Apply(
		context.Background(),
		validLedgerAction("run-1", "target-1"),
	); err == nil || !strings.Contains(err.Error(), "none") {
		t.Fatalf("Apply() error = %v", err)
	}
}

type pointerFaultProvider struct{}

func (*pointerFaultProvider) Name() string {
	return "typed-nil"
}

func (*pointerFaultProvider) Preflight(
	context.Context,
	Environment,
	Action,
) error {
	return nil
}

func (*pointerFaultProvider) Apply(context.Context, Action) error {
	return nil
}

func (*pointerFaultProvider) Restore(context.Context, Action) error {
	return nil
}

func (*pointerFaultProvider) VerifyRestored(context.Context, Action) error {
	return nil
}

func TestFaultProviderRegistryRejectsTypedNilProvider(t *testing.T) {
	registry := NewFaultProviderRegistry()
	if err := registry.Register(
		"local",
		func(FaultProviderConfig) (FaultProvider, error) {
			var provider *pointerFaultProvider
			return provider, nil
		},
	); err != nil {
		t.Fatal(err)
	}
	provider, err := NewFaultProvider(
		registry,
		FaultProviderConfig{Type: "local"},
	)
	if err == nil || !strings.Contains(err.Error(), "no provider") {
		t.Fatalf("provider=%T error=%v", provider, err)
	}
}

func TestFaultProviderRegistryRejectsUnknownDuplicateAndNilFactories(t *testing.T) {
	registry := NewFaultProviderRegistry()
	if err := registry.Register("imaginary", func(
		FaultProviderConfig,
	) (FaultProvider, error) {
		return namedFaultProvider{name: "imaginary"}, nil
	}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unknown Register() error = %v", err)
	}
	if err := registry.Register("ssh", nil); err == nil ||
		!strings.Contains(err.Error(), "nil") {
		t.Fatalf("nil Register() error = %v", err)
	}
	factory := func(FaultProviderConfig) (FaultProvider, error) {
		return namedFaultProvider{name: "ssh"}, nil
	}
	if err := registry.Register("ssh", factory); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("ssh", factory); err == nil ||
		!strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate Register() error = %v", err)
	}
	if _, err := registry.Build(FaultProviderConfig{Type: "imaginary"}); err == nil ||
		!strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unknown Build() error = %v", err)
	}
}
