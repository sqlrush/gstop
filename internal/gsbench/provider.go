package gsbench

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

type FaultProvider interface {
	Name() string
	Preflight(context.Context, Environment, Action) error
	Apply(context.Context, Action) error
	Restore(context.Context, Action) error
	VerifyRestored(context.Context, Action) error
}

type NoopFaultProvider struct {
	ConfiguredProvider string
}

type FaultProviderFactory func(FaultProviderConfig) (FaultProvider, error)

type FaultProviderRegistry struct {
	mu        sync.RWMutex
	factories map[string]FaultProviderFactory
}

var defaultFaultProviderRegistry = NewFaultProviderRegistry()

// DefaultFaultProviderRegistry is the process-wide registry used by scenario
// execution and recovery. Adapters register once on this explicit shared
// instance so restore never constructs a disconnected registry.
func DefaultFaultProviderRegistry() *FaultProviderRegistry {
	return defaultFaultProviderRegistry
}

func NewFaultProviderRegistry() *FaultProviderRegistry {
	return &FaultProviderRegistry{
		factories: map[string]FaultProviderFactory{
			"none": func(config FaultProviderConfig) (FaultProvider, error) {
				return NoopFaultProvider{
					ConfiguredProvider: defaultFaultProviderType(config.Type),
				}, nil
			},
		},
	}
}

func NewFaultProvider(
	registry *FaultProviderRegistry,
	config FaultProviderConfig,
) (FaultProvider, error) {
	if registry == nil {
		return nil, fmt.Errorf("fault provider registry is required")
	}
	return registry.Build(config)
}

func (r *FaultProviderRegistry) Register(
	providerType string,
	factory FaultProviderFactory,
) error {
	providerType = defaultFaultProviderType(providerType)
	if !supportedFaultProviderType(providerType) {
		return fmt.Errorf("fault provider type %q is unsupported", providerType)
	}
	if factory == nil {
		return fmt.Errorf("fault provider %q factory is nil", providerType)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factories == nil {
		r.factories = make(map[string]FaultProviderFactory)
	}
	if _, exists := r.factories[providerType]; exists {
		return fmt.Errorf(
			"fault provider %q factory is already registered",
			providerType,
		)
	}
	r.factories[providerType] = factory
	return nil
}

func (r *FaultProviderRegistry) Build(
	config FaultProviderConfig,
) (FaultProvider, error) {
	providerType := defaultFaultProviderType(config.Type)
	if !supportedFaultProviderType(providerType) {
		return nil, fmt.Errorf(
			"fault provider type %q is unsupported",
			providerType,
		)
	}
	r.mu.RLock()
	factory := r.factories[providerType]
	r.mu.RUnlock()
	if factory == nil {
		return nil, fmt.Errorf(
			"fault provider %q adapter is not registered in this build",
			providerType,
		)
	}
	config.Type = providerType
	provider, err := factory(config)
	if err != nil {
		detail := journalSafeErrorText(err.Error())
		if detail == "" {
			detail = "unknown initialization error"
		}
		return nil, fmt.Errorf(
			"initialize fault provider %q: %s",
			providerType,
			detail,
		)
	}
	if faultProviderIsNil(provider) {
		return nil, fmt.Errorf(
			"fault provider %q factory returned no provider",
			providerType,
		)
	}
	return provider, nil
}

func faultProviderIsNil(provider FaultProvider) bool {
	if provider == nil {
		return true
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan,
		reflect.Func,
		reflect.Interface,
		reflect.Map,
		reflect.Pointer,
		reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (p NoopFaultProvider) Name() string {
	return "none"
}

func (p NoopFaultProvider) Preflight(
	_ context.Context,
	_ Environment,
	action Action,
) error {
	return p.unavailable(action)
}

func (p NoopFaultProvider) Apply(_ context.Context, action Action) error {
	return p.unavailable(action)
}

func (p NoopFaultProvider) Restore(_ context.Context, action Action) error {
	return p.unavailable(action)
}

func (p NoopFaultProvider) VerifyRestored(
	_ context.Context,
	action Action,
) error {
	return p.unavailable(action)
}

func (p NoopFaultProvider) unavailable(action Action) error {
	configured := defaultFaultProviderType(p.ConfiguredProvider)
	if !supportedFaultProviderType(configured) {
		configured = "none"
	}
	return fmt.Errorf(
		"fault provider %q is unavailable for action kind %q",
		configured,
		action.Kind,
	)
}

func defaultFaultProviderType(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "none"
	}
	return value
}

func supportedFaultProviderType(value string) bool {
	return stringInSet(value, "none", "local", "ssh", "gaussdb_api")
}
