package config

import "time"

const DefaultCollectTimeout = 30 * time.Second

func CollectTimeout(cfg *Config) time.Duration {
	if cfg == nil {
		return DefaultCollectTimeout
	}
	seconds := cfg.GetFloat("main.collect_timeout", DefaultCollectTimeout.Seconds())
	if seconds <= 0 {
		return DefaultCollectTimeout
	}
	return time.Duration(seconds * float64(time.Second))
}
