package config

// Typed accessors return the configured value or the supplied default when the
// key is missing or of an unexpected type. This mirrors how the Python code
// treats absent keys as None and falls back to hard-coded defaults at each call
// site, but centralises the fallback so call sites stay readable.

// GetString returns the string at key, or def if absent or not a string.
func (c *Config) GetString(key, def string) string {
	if s, ok := c.Get(key).(string); ok {
		return s
	}
	return def
}

// GetInt returns the integer at key, or def if absent or non-numeric.
// Float values that are configured where an int is expected are truncated.
func (c *Config) GetInt(key string, def int) int {
	if n, ok := intFrom(c.Get(key)); ok {
		return n
	}
	return def
}

// GetFloat returns the float at key, or def if absent or non-numeric.
// Integer values are widened to float64.
func (c *Config) GetFloat(key string, def float64) float64 {
	switch v := c.Get(key).(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	}
	return def
}

// GetBool returns the bool at key, or def if absent or not a bool.
func (c *Config) GetBool(key string, def bool) bool {
	if b, ok := c.Get(key).(bool); ok {
		return b
	}
	return def
}

// Has reports whether a value exists at the dotted key.
func (c *Config) Has(key string) bool {
	return c.Get(key) != nil
}

// intFrom coerces a parsed config value to int when it is an integer-like type.
func intFrom(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	case int:
		return n, true
	case float64:
		return int(n), true
	}
	return 0, false
}
