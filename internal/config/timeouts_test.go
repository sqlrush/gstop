package config

import (
	"testing"
	"time"
)

func TestCollectTimeoutUsesThirtySecondDefault(t *testing.T) {
	if got := CollectTimeout(FromMap(map[string]any{})); got != 30*time.Second {
		t.Fatalf("CollectTimeout() = %v, want 30s", got)
	}
}

func TestCollectTimeoutHonorsFractionalConfiguredSeconds(t *testing.T) {
	cfg := FromMap(map[string]any{"main": map[string]any{"collect_timeout": 0.125}})
	if got := CollectTimeout(cfg); got != 125*time.Millisecond {
		t.Fatalf("CollectTimeout() = %v, want 125ms", got)
	}
}

func TestCollectTimeoutRejectsNonPositiveValues(t *testing.T) {
	for _, value := range []any{int64(0), int64(-1), float64(-0.5)} {
		cfg := FromMap(map[string]any{"main": map[string]any{"collect_timeout": value}})
		if got := CollectTimeout(cfg); got != DefaultCollectTimeout {
			t.Fatalf("CollectTimeout(%v) = %v, want %v", value, got, DefaultCollectTimeout)
		}
	}
}
