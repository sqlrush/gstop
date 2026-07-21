package gsbench

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func datasetConfig(profile string, maxGB int) BenchConfig {
	return BenchConfig{
		Run:  RunConfig{Profile: profile},
		Data: DataConfig{Schema: "gsbench", MaxSizeGB: maxGB, MinFreeDiskPercent: 20, ReuseExisting: true},
	}
}

type guardedDatasetExecutor struct {
	recordingDatasetExecutor
	checks int
}

func (e *guardedDatasetExecutor) CheckCapacity(context.Context) error {
	e.checks++
	return errors.New("disk safety threshold reached")
}

func TestDatasetChecksDiskCapacityBetweenBatches(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 2), Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30})
	if err != nil {
		t.Fatal(err)
	}
	exec := &guardedDatasetExecutor{recordingDatasetExecutor: recordingDatasetExecutor{completed: map[string]int64{}}}
	err = NewDatasetManager(exec).Init(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "disk safety") {
		t.Fatalf("err=%v", err)
	}
	if exec.checks != 1 {
		t.Fatalf("capacity checks=%d", exec.checks)
	}
}

func TestDatasetQuickPlanRespectsFiveGBAndDiskReserve(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 5), Capacity{TotalBytes: 30 << 30, FreeBytes: 30 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if plan.EstimatedBytes > 5<<30 {
		t.Fatalf("estimated bytes = %d", plan.EstimatedBytes)
	}
	if plan.ReservedFreeBytes < 6<<30 {
		t.Fatalf("reserved bytes = %d", plan.ReservedFreeBytes)
	}
	if plan.EstimatedBytes > plan.AvailableForData {
		t.Fatalf("plan exceeds safe capacity: %+v", plan)
	}
}

func TestDatasetStressPlanDefaultsToAtMostTwentyGB(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("stress", 20), Capacity{TotalBytes: 200 << 30, FreeBytes: 100 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if plan.EstimatedBytes != 20<<30 {
		t.Fatalf("estimated bytes = %d", plan.EstimatedBytes)
	}
}

func TestDatasetPlanFailsWhenSafeFreeSpaceIsTooSmall(t *testing.T) {
	_, err := PlanDataset(datasetConfig("quick", 5), Capacity{TotalBytes: 100 << 30, FreeBytes: 15 << 30})
	if err == nil {
		t.Fatal("expected disk reserve error")
	}
}

func TestDatasetDDLIncludesEveryScenarioTable(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 2), Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30})
	if err != nil {
		t.Fatal(err)
	}
	ddl := strings.Join(plan.DDL, "\n")
	for _, table := range []string{
		"accounts", "customers", "orders", "order_items", "fact_sales",
		"dim_product", "dim_store", "plan_data", "lock_targets", "lock_table_targets", "lock_ddl_targets", "vacuum_targets",
		"meta_runs", "meta_journal", "meta_batches",
	} {
		if !strings.Contains(ddl, "gsbench."+table) {
			t.Errorf("DDL missing %s", table)
		}
	}
}

type recordingDatasetExecutor struct {
	statements []string
	completed  map[string]int64
}

func (e *recordingDatasetExecutor) Exec(_ context.Context, query string, _ ...any) error {
	e.statements = append(e.statements, query)
	return nil
}

func (e *recordingDatasetExecutor) BatchHighWater(_ context.Context, table string) (int64, error) {
	return e.completed[table], nil
}

func TestDatasetInitSkipsCompletedBatches(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 2), Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30})
	if err != nil {
		t.Fatal(err)
	}
	first := plan.Batches[0]
	exec := &recordingDatasetExecutor{completed: map[string]int64{first.Table: first.Rows}}
	manager := NewDatasetManager(exec)
	if err := manager.Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	for _, query := range exec.statements {
		if strings.Contains(query, "INSERT INTO gsbench."+first.Table) {
			t.Fatalf("completed table was generated again: %s", query)
		}
	}
}
