package gsbench

import (
	"context"
	"os"
	"testing"
)

func TestIntegrationDoctor(t *testing.T) {
	configPath := os.Getenv("GSBENCH_INTEGRATION_CONFIG")
	if configPath == "" {
		t.Skip("set GSBENCH_INTEGRATION_CONFIG to run live openGauss integration")
	}
	cfg, err := LoadConfig(configPath, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	db, err := OpenDatabase(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	capabilities := DetectCapabilities(context.Background(), db)
	if !capabilities.Supported {
		t.Fatalf("unsupported live target: %+v", capabilities)
	}
}
