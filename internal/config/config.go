// Package config loads and exposes gstop's INI configuration (gstop.cfg).
//
// It reproduces the behaviour of the original Python common/config.py:
// nested sections addressed with dotted keys (e.g. [emergency.plan_change]),
// per-value type inference, command-line overrides, and a post-processing
// step that keeps the screen refresh no slower than the persistence interval.
//
// A Config is immutable after Load. Overrides (used at start-up to force the
// terminate safety switches off) are applied with With, which returns a new
// Config and never mutates the receiver.
package config

import (
	"fmt"
	"strings"
)

// Config holds a parsed configuration tree. The zero value is not usable;
// obtain a Config from Load.
type Config struct {
	data map[string]any
}

// Args carries the command-line overrides that take precedence over the file.
// A nil pointer field means "not supplied on the command line".
type Args struct {
	Interval    *int
	LogInterval *int
	User        *string
	Port        *int
	Database    *string
	Daemon      *bool
}

// argMapping maps an Args field to its dotted configuration path, mirroring
// the Python MAPPING table.
func (a Args) overrides() map[string]any {
	out := map[string]any{}
	if a.Interval != nil {
		out["main.interval"] = int64(*a.Interval)
	}
	if a.LogInterval != nil {
		out["main.log_interval"] = int64(*a.LogInterval)
	}
	if a.User != nil {
		out["main.user"] = *a.User
	}
	if a.Port != nil {
		out["main.port"] = int64(*a.Port)
	}
	if a.Database != nil {
		out["main.database"] = *a.Database
	}
	if a.Daemon != nil {
		out["main.daemon"] = *a.Daemon
	}
	return out
}

// Load reads the INI file at path, applies command-line overrides, and runs
// post-processing. A missing file yields an empty configuration (matching the
// Python behaviour of logging an error and returning {}), so callers relying on
// defaults still function; a malformed file is a hard error.
func Load(path string, args Args) (*Config, error) {
	tree, err := loadFile(path)
	if err != nil {
		return nil, err
	}

	for dotted, value := range args.overrides() {
		tree = setPath(tree, dotted, value)
	}

	tree = postProcess(tree)
	return &Config{data: tree}, nil
}

// FromMap builds a Config from an already-parsed tree. It copies the input so
// the caller cannot later mutate the Config's backing data. Primarily for tests.
func FromMap(tree map[string]any) *Config {
	return &Config{data: cloneTree(tree)}
}

// Get returns the value at the dotted key, or nil if any path segment is
// missing. The returned value is one of: int64, float64, bool, string, or a
// nested map[string]any.
func (c *Config) Get(key string) any {
	if c == nil {
		return nil
	}
	var current any = c.data
	for _, segment := range strings.Split(key, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		next, present := m[segment]
		if !present {
			return nil
		}
		current = next
	}
	return current
}

// With returns a new Config with the dotted key set to value. The receiver is
// left unchanged. Used at start-up to force safety switches; the path may be
// arbitrarily deep, unlike the two-level Python Config.set.
func (c *Config) With(key string, value any) *Config {
	return &Config{data: setPath(cloneTree(c.data), key, value)}
}

// postProcess clamps main.interval to be no larger than a positive
// main.log_interval, matching Python Config._post_process.
func postProcess(tree map[string]any) map[string]any {
	interval, hasInterval := intFrom(lookup(tree, "main.interval"))
	logInterval, hasLog := intFrom(lookup(tree, "main.log_interval"))
	if hasInterval && hasLog && logInterval > 0 && logInterval < interval {
		return setPath(tree, "main.interval", logInterval)
	}
	return tree
}

// lookup walks a dotted path over a raw tree, returning nil if absent.
func lookup(tree map[string]any, key string) any {
	var current any = tree
	for _, segment := range strings.Split(key, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[segment]
	}
	return current
}

// setPath returns a shallow-cloned tree with the dotted key set to value,
// creating intermediate maps as needed. It never mutates nested maps of the
// input beyond the path being written (copy-on-write along the path).
func setPath(tree map[string]any, key string, value any) map[string]any {
	segments := strings.Split(key, ".")
	root := shallowClone(tree)
	current := root
	for _, segment := range segments[:len(segments)-1] {
		child, ok := current[segment].(map[string]any)
		if !ok {
			child = map[string]any{}
		} else {
			child = shallowClone(child)
		}
		current[segment] = child
		current = child
	}
	current[segments[len(segments)-1]] = value
	return root
}

func shallowClone(tree map[string]any) map[string]any {
	out := make(map[string]any, len(tree)+1)
	for k, v := range tree {
		out[k] = v
	}
	return out
}

func cloneTree(tree map[string]any) map[string]any {
	out := make(map[string]any, len(tree))
	for k, v := range tree {
		if nested, ok := v.(map[string]any); ok {
			out[k] = cloneTree(nested)
		} else {
			out[k] = v
		}
	}
	return out
}

// String renders the tree for debugging. Not a stable format.
func (c *Config) String() string {
	return fmt.Sprintf("Config%v", c.data)
}
