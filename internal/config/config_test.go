package config_test

import (
	"strings"
	"testing"

	"crossvenue/internal/config"
)

// The live-execution guard is a safety property: setting
// ENABLE_LIVE_EXECUTION=true must make startup refuse to run, not trade.
func TestLoadRejectsLiveExecutionEnv(t *testing.T) {
	t.Setenv("ENABLE_LIVE_EXECUTION", "true")
	if _, err := config.Load(""); err == nil || !strings.Contains(err.Error(), "live execution") {
		t.Fatalf("ENABLE_LIVE_EXECUTION=true must refuse startup, got %v", err)
	}
}

func TestLoadDefaultsAreSimulationOnly(t *testing.T) {
	t.Setenv("ENABLE_LIVE_EXECUTION", "")
	c, err := config.Load("")
	if err != nil {
		t.Fatalf("default load: %v", err)
	}
	if c.Mode != config.ModeSynthetic {
		t.Fatalf("default mode = %q, want synthetic", c.Mode)
	}
	if c.Execution.Mode != "simulation" {
		t.Fatalf("execution mode = %q, want simulation", c.Execution.Mode)
	}
}
