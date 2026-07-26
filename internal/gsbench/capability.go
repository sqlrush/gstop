package gsbench

import (
	"context"
	"fmt"
	"strings"
)

type CapabilityProber interface {
	Probe(ctx context.Context, name, query string) (string, error)
}

// Capabilities is retained for existing scenario implementations while their
// callers migrate to Environment. It is a projection, never a second probe.
type Capabilities struct {
	Product           string
	Version           string
	Supported         bool
	Centralized       bool
	Admin             bool
	ThreadPoolEnabled bool
	ThreadPoolView    bool
	DynamicMemoryView bool
	DatabaseCPU       bool
	StatementHistory  bool
	VacuumStats       bool
	PlanIndexUnusable bool
	PlanColumnStats   bool
	PlanNDistinct     bool
	PlanExtendedStats bool
	Warnings          []string
}

const (
	capabilityThreadPoolEnabled        Capability = "thread_pool_enabled"
	capabilityThreadPoolView           Capability = "thread_pool_view"
	capabilityDynamicMemory            Capability = "dynamic_memory_view"
	capabilityDatabaseCPU              Capability = "database_cpu"
	capabilityVacuumStats              Capability = "vacuum_stats"
	capabilityPlanIndexUnusable        Capability = "plan_index_unusable"
	capabilityPlanColumnStats          Capability = "plan_column_stats"
	capabilityPlanNDistinct            Capability = "plan_ndistinct"
	capabilityPlanExtendedStats        Capability = "plan_extended_stats"
	capabilityStatementHistoryRelation Capability = "statement_history_relation"
	capabilityStatementHistoryIdentity Capability = "statement_history_identity"
	capabilityGTM                      Capability = "gtm"
	capabilityPrimaryStandby           Capability = "primary_standby"
)

func DetectCapabilities(ctx context.Context, p CapabilityProber) Capabilities {
	return capabilitiesFor(DetectEnvironment(ctx, p))
}

func capabilitiesFor(env Environment) Capabilities {
	return Capabilities{
		Product: string(env.Product), Version: env.Version, Supported: env.Supported,
		Centralized:       env.Topology != TopologyDistributed,
		Admin:             env.Capabilities[CapabilityAdmin],
		ThreadPoolEnabled: env.Capabilities[capabilityThreadPoolEnabled],
		ThreadPoolView:    env.Capabilities[capabilityThreadPoolView],
		DynamicMemoryView: env.Capabilities[capabilityDynamicMemory],
		DatabaseCPU:       env.Capabilities[capabilityDatabaseCPU],
		StatementHistory:  env.Capabilities[CapabilityStatementHistory],
		VacuumStats:       env.Capabilities[capabilityVacuumStats],
		PlanIndexUnusable: env.Capabilities[capabilityPlanIndexUnusable],
		PlanColumnStats:   env.Capabilities[capabilityPlanColumnStats],
		PlanNDistinct:     env.Capabilities[capabilityPlanNDistinct],
		PlanExtendedStats: env.Capabilities[capabilityPlanExtendedStats],
		Warnings:          append([]string(nil), env.Warnings...),
	}
}

func validatePlanCapability(name string, capabilities Capabilities) error {
	supported := map[string]bool{
		"planchange_stats_target":    capabilities.PlanColumnStats,
		"planchange_index_unusable":  capabilities.PlanIndexUnusable,
		"planchange_stats_ndistinct": capabilities.PlanNDistinct,
		"planchange_stats_extended":  capabilities.PlanExtendedStats,
		"planchange_index_drop":      true,
		"planchange_index_shape":     true,
	}
	if !supported[name] {
		return fmt.Errorf("%s is unsupported by this openGauss/GaussDB catalog", name)
	}
	return nil
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "on", "yes":
		return true
	default:
		return false
	}
}
