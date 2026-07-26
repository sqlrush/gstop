package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

type physicalDatasetExecutor struct {
	atomicDatasetExecutor
	sizes          []DatasetSizeSample
	sizeCalls      int
	layoutCalls    int
	layoutError    error
	capacityChecks int
}

func (e *physicalDatasetExecutor) DatasetSize(
	context.Context,
	string,
) (DatasetSizeSample, error) {
	index := e.sizeCalls
	e.sizeCalls++
	if len(e.sizes) == 0 {
		return DatasetSizeSample{}, errors.New("no size sample")
	}
	if index >= len(e.sizes) {
		index = len(e.sizes) - 1
	}
	return e.sizes[index], nil
}

func (e *physicalDatasetExecutor) ValidateDatasetLayout(
	context.Context,
	DatasetPlan,
) error {
	e.layoutCalls++
	return e.layoutError
}

func (e *physicalDatasetExecutor) CheckCapacity(context.Context) error {
	e.capacityChecks++
	return nil
}

func physicalTestPlan() DatasetPlan {
	return DatasetPlan{
		Schema:         "gsbench",
		EstimatedBytes: 1_000_000,
		Batches: []TableBatch{{
			Table:             "fact_sales",
			WeightPercent:     20,
			EstimatedRowBytes: 100,
			Rows:              100,
			BatchSize:         10,
			InsertSQL:         `INSERT INTO "gsbench".fact_sales SELECT g FROM generate_series($1,$2) g`,
		}},
	}
}

func TestDatasetInitStopsOptionalGrowthAtNinetyFivePercentActualSize(t *testing.T) {
	plan := physicalTestPlan()
	exec := &physicalDatasetExecutor{
		atomicDatasetExecutor: atomicDatasetExecutor{
			recordingDatasetExecutor: recordingDatasetExecutor{
				completed:    map[string]int64{},
				schemaExists: true,
			},
		},
		sizes: []DatasetSizeSample{{TotalBytes: 950_000, Source: "catalog"}},
	}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(exec.applied) != 0 {
		t.Fatalf("optional batches applied at 95%%: %v", exec.applied)
	}
	if exec.layoutCalls != 1 {
		t.Fatalf("layout validations=%d", exec.layoutCalls)
	}
}

func TestDatasetInitCalibratesAtMostThreeRoundsBelowNinetyPercent(t *testing.T) {
	plan := physicalTestPlan()
	exec := &physicalDatasetExecutor{
		atomicDatasetExecutor: atomicDatasetExecutor{
			recordingDatasetExecutor: recordingDatasetExecutor{
				completed:    map[string]int64{"fact_sales": 100},
				schemaExists: true,
			},
		},
		sizes: []DatasetSizeSample{{TotalBytes: 800_000, Source: "catalog"}},
	}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(exec.applied) != 3 {
		t.Fatalf("calibration batches=%d want=3 (%v)", len(exec.applied), exec.applied)
	}
}

func TestDatasetInitFailsWhenPhysicalLayoutValidationFails(t *testing.T) {
	plan := physicalTestPlan()
	exec := &physicalDatasetExecutor{
		atomicDatasetExecutor: atomicDatasetExecutor{
			recordingDatasetExecutor: recordingDatasetExecutor{
				completed:    map[string]int64{"fact_sales": 100},
				schemaExists: true,
			},
		},
		sizes:       []DatasetSizeSample{{TotalBytes: 960_000, Source: "catalog"}},
		layoutError: errors.New("fact_sales DN row imbalance exceeds 10%"),
	}
	err := NewDatasetManager(exec).Init(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "row imbalance") {
		t.Fatalf("err=%v", err)
	}
}

func TestDatasetInitFailsLoudlyWhenCommittedBatchMeasuresAboveHardTarget(t *testing.T) {
	plan := physicalTestPlan()
	exec := &physicalDatasetExecutor{
		atomicDatasetExecutor: atomicDatasetExecutor{
			recordingDatasetExecutor: recordingDatasetExecutor{
				completed:    map[string]int64{},
				schemaExists: true,
			},
		},
		sizes: []DatasetSizeSample{
			{TotalBytes: 800_000, Source: "catalog"},
			{TotalBytes: 1_050_000, Source: "catalog"},
		},
	}
	err := NewDatasetManager(exec).Init(context.Background(), plan)
	if err == nil ||
		!strings.Contains(err.Error(), "measured dataset overshoot") ||
		!strings.Contains(err.Error(), "1050000") ||
		!strings.Contains(err.Error(), "1000000") {
		t.Fatalf("err=%v", err)
	}
	if exec.layoutCalls != 0 {
		t.Fatalf("oversized dataset was finalized")
	}
}

func TestDatasetCalibrationFailsLoudlyWhenMeasuredAboveHardTarget(t *testing.T) {
	plan := physicalTestPlan()
	exec := &physicalDatasetExecutor{
		atomicDatasetExecutor: atomicDatasetExecutor{
			recordingDatasetExecutor: recordingDatasetExecutor{
				completed:    map[string]int64{"fact_sales": 100},
				schemaExists: true,
			},
		},
		sizes: []DatasetSizeSample{
			{TotalBytes: 800_000, Source: "catalog"},
			{TotalBytes: 1_100_000, Source: "catalog"},
		},
	}
	err := NewDatasetManager(exec).Init(context.Background(), plan)
	if err == nil ||
		!strings.Contains(err.Error(), "calibration") ||
		!strings.Contains(err.Error(), "measured dataset overshoot") {
		t.Fatalf("err=%v", err)
	}
}

func TestDatasetBatchUsesConservativeProspectivePhysicalBound(t *testing.T) {
	plan := physicalTestPlan()
	plan.Batches[0].Rows = 2_000
	plan.Batches[0].BatchSize = 2_000
	exec := &physicalDatasetExecutor{
		atomicDatasetExecutor: atomicDatasetExecutor{
			recordingDatasetExecutor: recordingDatasetExecutor{
				completed:    map[string]int64{},
				schemaExists: true,
			},
		},
		sizes: []DatasetSizeSample{
			{TotalBytes: 900_000, Source: "catalog"},
			{TotalBytes: 950_000, Source: "catalog"},
		},
	}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(exec.applied) != 1 ||
		!strings.Contains(exec.applied[0], ":1-500:") {
		t.Fatalf("prospectively bounded batches=%v", exec.applied)
	}
}

func TestConsecutiveShortenedDatasetBatchesRemainGapFree(t *testing.T) {
	plan := physicalTestPlan()
	plan.Batches[0].Rows = 4_000
	plan.Batches[0].BatchSize = 2_000
	exec := &physicalDatasetExecutor{
		atomicDatasetExecutor: atomicDatasetExecutor{
			recordingDatasetExecutor: recordingDatasetExecutor{
				completed:    map[string]int64{},
				schemaExists: true,
			},
		},
		sizes: []DatasetSizeSample{
			{TotalBytes: 900_000, Source: "catalog"},
			{TotalBytes: 920_000, Source: "catalog"},
			{TotalBytes: 950_000, Source: "catalog"},
		},
	}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"gsbench:fact_sales:1-500:4",
		"gsbench:fact_sales:501-900:4",
	}
	if !reflect.DeepEqual(exec.applied, want) {
		t.Fatalf("shortened batches=%v want=%v", exec.applied, want)
	}
}

func TestNextDatasetBatchStartIsCommittedEndPlusOneAndOverflowSafe(t *testing.T) {
	next, ok := nextDatasetBatchStart(500, 4_000)
	if !ok || next != 501 {
		t.Fatalf("next=%d ok=%v", next, ok)
	}
	for _, tc := range []struct {
		end  int64
		rows int64
	}{
		{end: 4_000, rows: 4_000},
		{end: math.MaxInt64, rows: math.MaxInt64},
	} {
		if next, ok := nextDatasetBatchStart(tc.end, tc.rows); ok {
			t.Fatalf("end=%d rows=%d returned next=%d", tc.end, tc.rows, next)
		}
	}
}

func TestDatasetCalibrationUsesConservativeProspectivePhysicalBound(t *testing.T) {
	plan := physicalTestPlan()
	plan.Batches[0].BatchSize = 2_000
	exec := &physicalDatasetExecutor{
		atomicDatasetExecutor: atomicDatasetExecutor{
			recordingDatasetExecutor: recordingDatasetExecutor{
				completed:    map[string]int64{"fact_sales": 100},
				schemaExists: true,
			},
		},
		sizes: []DatasetSizeSample{
			{TotalBytes: 800_000, Source: "catalog"},
			{TotalBytes: 950_000, Source: "catalog"},
		},
	}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(exec.applied) != 1 ||
		!strings.Contains(exec.applied[0], ":101-1100:") {
		t.Fatalf("prospectively bounded calibration=%v", exec.applied)
	}
}

type fakePhysicalDatabase struct {
	query string
	args  []any
	rows  *sliceJournalRows
}

func (d *fakePhysicalDatabase) Query(
	_ context.Context,
	query string,
	args ...any,
) (journalRows, error) {
	d.query = query
	d.args = append([]any(nil), args...)
	return d.rows, nil
}

func (d *fakePhysicalDatabase) Scan(context.Context, string, []any, ...any) error {
	return errors.New("unexpected scan")
}

func (d *fakePhysicalDatabase) Exec(context.Context, string, ...any) (sql.Result, error) {
	return nil, errors.New("unexpected exec")
}

func TestCentralDatasetSizeIncludesHeapAndIndexes(t *testing.T) {
	db := &fakePhysicalDatabase{rows: &sliceJournalRows{rows: [][]any{
		{"fact_sales", int64(800), int64(200)},
		{"orders", int64(400), int64(100)},
	}}}
	sample, err := readCentralDatasetSize(context.Background(), db, "Bench")
	if err != nil {
		t.Fatal(err)
	}
	if sample.TotalBytes != 1_500 || sample.Source != "database-catalog" {
		t.Fatalf("sample=%+v", sample)
	}
	if !strings.Contains(db.query, "pg_relation_size") ||
		!strings.Contains(db.query, "pg_indexes_size") ||
		!reflect.DeepEqual(db.args, []any{"Bench"}) {
		t.Fatalf("query=%s args=%v", db.query, db.args)
	}
}

type fakeDatasetPhysicalProvider struct {
	sample DatasetSizeSample
}

func (p fakeDatasetPhysicalProvider) DatasetSize(
	context.Context,
	string,
) (DatasetSizeSample, error) {
	return p.sample, nil
}

func (fakeDatasetPhysicalProvider) ValidateDatasetLayout(
	context.Context,
	DatasetPlan,
) error {
	return nil
}

func TestConfiguredDistributedPhysicalProviderIsSelectedExplicitly(t *testing.T) {
	cfg := datasetConfig("quick", 5)
	cfg.Data.PhysicalSizeProvider = "auto"
	provider := fakeDatasetPhysicalProvider{sample: DatasetSizeSample{
		TotalBytes: 1234, Source: "gaussdb_api", NodeCount: 2,
	}}
	selected, err := selectDatasetPhysicalProvider(
		cfg,
		Environment{Product: ProductGaussDB, Topology: TopologyDistributed, Nodes: []Node{
			{Name: "dn_1", Role: NodeRoleDNPrimary},
			{Name: "dn_2", Role: NodeRoleDNPrimary},
		}},
		&fakePhysicalDatabase{},
		DatasetExternalProviders{Physical: provider},
	)
	if err != nil {
		t.Fatal(err)
	}
	sample, err := selected.DatasetSize(context.Background(), "gsbench")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sample, provider.sample) {
		t.Fatalf("sample=%+v", sample)
	}
}

func TestDistributedPhysicalProviderRejectsIncompletePrimaryCoverage(t *testing.T) {
	cfg := datasetConfig("quick", 5)
	cfg.Data.PhysicalSizeProvider = "auto"
	selected, err := selectDatasetPhysicalProvider(
		cfg,
		Environment{Product: ProductGaussDB, Topology: TopologyDistributed, Nodes: []Node{
			{Name: "dn_1", Role: NodeRoleDNPrimary},
			{Name: "dn_2", Role: NodeRoleDNPrimary},
		}},
		&fakePhysicalDatabase{},
		DatasetExternalProviders{Physical: fakeDatasetPhysicalProvider{
			sample: DatasetSizeSample{
				TotalBytes: 1234, Source: "gaussdb_api", NodeCount: 1,
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = selected.DatasetSize(context.Background(), "gsbench")
	if err == nil || !strings.Contains(err.Error(), "primary DN coverage") {
		t.Fatalf("err=%v", err)
	}
}

func TestDistributedFactSalesBalanceRejectsMoreThanTenPercent(t *testing.T) {
	db := &fakePhysicalDatabase{rows: &sliceJournalRows{rows: [][]any{
		{"dn_1", int64(100)},
		{"dn_2", int64(80)},
	}}}
	err := validateDistributedFactSalesBalance(
		context.Background(), db, "gsbench",
	)
	if err == nil || !strings.Contains(err.Error(), "10%") {
		t.Fatalf("err=%v", err)
	}
}
