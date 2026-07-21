package gsbench

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

type CapabilityProber interface {
	Probe(ctx context.Context, name, query string) (string, error)
}

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
	Warnings          []string
}

type capabilityProbe struct {
	name  string
	query string
	set   func(*Capabilities, string)
}

func DetectCapabilities(ctx context.Context, p CapabilityProber) Capabilities {
	c := Capabilities{Centralized: true}
	probes := []capabilityProbe{
		{"version", "SELECT version()", func(c *Capabilities, v string) {
			c.Version = v
			lower := strings.ToLower(v)
			switch {
			case strings.Contains(lower, "opengauss"):
				c.Product = "openGauss"
			case strings.Contains(lower, "gaussdb"):
				c.Product = "GaussDB"
			default:
				c.Product = "Unknown"
			}
		}},
		{"distributed_nodes", "SELECT count(*) FROM pg_catalog.pgxc_node", func(c *Capabilities, v string) {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 1 {
				c.Centralized = false
			}
		}},
		{"admin", "SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname = current_user", func(c *Capabilities, v string) { c.Admin = truthy(v) }},
		{"thread_pool_enabled", "SHOW enable_thread_pool", func(c *Capabilities, v string) { c.ThreadPoolEnabled = truthy(v) }},
		{"thread_pool_view", "SELECT 1 FROM dbe_perf.global_threadpool_status LIMIT 1", func(c *Capabilities, v string) { c.ThreadPoolView = v != "" }},
		{"dynamic_memory", "SELECT 1 FROM pg_catalog.pv_total_memory_detail() LIMIT 1", func(c *Capabilities, v string) { c.DynamicMemoryView = v != "" }},
		{"database_cpu", "SELECT 1 FROM dbe_perf.os_runtime LIMIT 1", func(c *Capabilities, v string) { c.DatabaseCPU = v != "" }},
		{"statement_history", "SELECT 1 FROM dbe_perf.statement_history LIMIT 1", func(c *Capabilities, v string) { c.StatementHistory = v != "" }},
		{"vacuum_stats", "SELECT 1 FROM pg_catalog.pg_stat_all_tables LIMIT 1", func(c *Capabilities, v string) { c.VacuumStats = v != "" }},
	}
	for _, probe := range probes {
		value, err := p.Probe(ctx, probe.name, probe.query)
		if err != nil {
			c.Warnings = append(c.Warnings, fmt.Sprintf("%s: %v", probe.name, err))
			continue
		}
		probe.set(&c, value)
	}
	c.Supported = c.Centralized && (c.Product == "openGauss" || c.Product == "GaussDB")
	return c
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "on", "yes":
		return true
	default:
		return false
	}
}
