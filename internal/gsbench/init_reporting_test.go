package gsbench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type failingHighWaterExecutor struct {
	recordingDatasetExecutor
}

func (e *failingHighWaterExecutor) BatchHighWater(
	context.Context,
	string,
) (int64, error) {
	return 0, errors.New("metadata unavailable")
}

func TestLoadDatasetHighWaterReadsResumableStateAndSurfacesFailures(t *testing.T) {
	plan := physicalTestPlan()
	exec := &recordingDatasetExecutor{
		completed:    map[string]int64{"fact_sales": 73},
		schemaExists: true,
	}
	if err := LoadDatasetHighWater(context.Background(), exec, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.HighWater["fact_sales"] != 73 {
		t.Fatalf("high-water=%v", plan.HighWater)
	}
	failing := &failingHighWaterExecutor{recordingDatasetExecutor{
		completed:    map[string]int64{},
		schemaExists: true,
	}}
	if err := LoadDatasetHighWater(
		context.Background(), failing, &plan,
	); err == nil || !strings.Contains(err.Error(), "metadata unavailable") {
		t.Fatalf("err=%v", err)
	}
}

func TestLogDatasetPlanIncludesRequestedSizeAndPerTableTargets(t *testing.T) {
	var output bytes.Buffer
	log, err := NewRunLog(&output, "", Version)
	if err != nil {
		t.Fatal(err)
	}
	cfg := datasetConfig("quick", 5)
	cfg.Run.DryRun = true
	cfg.Data.RequestedSize = "100GB"
	cfg.Data.TargetBytes = 100 << 30
	cfg.Data.CapacityProvider = "tablespace_quota"
	cfg.Data.PhysicalSizeProvider = "catalog"
	plan, err := PlanDataset(cfg, Capacity{
		TotalBytes: 1 << 40, FreeBytes: 1 << 40,
	}, testDatasetEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	var fact TableBatch
	for _, batch := range plan.Batches {
		if batch.Table == "fact_sales" {
			fact = batch
			break
		}
	}
	plan.HighWater["fact_sales"] = fact.Rows - fact.BatchSize/2

	logDatasetPlan(log, cfg, plan, "database")

	text := output.String()
	if !strings.Contains(text, "requested_size=100GB") ||
		!strings.Contains(text, "target_bytes=107374182400") ||
		!strings.Contains(text, "product=openGauss") ||
		!strings.Contains(text, "topology=standalone") ||
		!strings.Contains(text, "capacity_provider=tablespace_quota") ||
		!strings.Contains(text, "physical_size_provider=catalog") ||
		!strings.Contains(text, "table=fact_sales") ||
		!strings.Contains(text, "weight_percent=20") ||
		!strings.Contains(text, "estimated_row_bytes=192") ||
		!strings.Contains(text, "target_rows=") ||
		!strings.Contains(text, "batch_size=") ||
		!strings.Contains(text, "batch_count=1 current_high_water=") ||
		!strings.Contains(text, "remaining_rows="+
			fmt.Sprint(fact.BatchSize/2)) ||
		!strings.Contains(text, "estimated_new_bytes=") {
		t.Fatalf("dataset plan log incomplete:\n%s", text)
	}
	if strings.Contains(text, "current_high_water=unknown") {
		t.Fatalf("dry-run did not use resumable high-water:\n%s", text)
	}
}
