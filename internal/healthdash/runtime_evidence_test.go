package healthdash

import (
	"math"
	"testing"
	"time"
)

func TestSummarizeCPUEvenMedianAndRatio(t *testing.T) {
	older := time.Date(2026, 7, 23, 11, 0, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	got := SummarizeCPU([]CPUObservation{
		{At: newer, CPUUS: 1000, DBTimeUS: 4000},
		{At: older, CPUUS: 3000, DBTimeUS: 4000},
	})

	if !got.Available || got.NewestAt != newer || got.NewestMS != 1 ||
		got.MedianMS != 2 || got.MaxMS != 3 {
		t.Fatalf("summary=%+v", got)
	}
	if !got.CPUToDBAvailable || math.Abs(got.CPUToDBRatio-.5) > .000001 {
		t.Fatalf("ratio summary=%+v", got)
	}
}

func TestSummarizeCPUHandlesOddMedianAndMissingDBTime(t *testing.T) {
	got := SummarizeCPU([]CPUObservation{
		{CPUUS: 9000},
		{CPUUS: 1000},
		{CPUUS: 4000},
	})
	if got.MedianMS != 4 || got.MaxMS != 9 {
		t.Fatalf("summary=%+v", got)
	}
	if got.CPUToDBAvailable {
		t.Fatalf("ratio should be unavailable: %+v", got)
	}
	if empty := SummarizeCPU(nil); empty.Available {
		t.Fatalf("empty=%+v", empty)
	}
}

func TestSummarizeASHExcludesIdleAndInfersOnCPU(t *testing.T) {
	got := SummarizeASH([]ASHSample{
		{State: "active", Event: "none", WaitStatus: "none"},
		{State: "ACTIVE", Event: "", WaitStatus: ""},
		{State: "active", Event: "WALFlushWait", WaitStatus: "wait io"},
		{State: "active", Event: "", WaitStatus: "wait cmd"},
		{State: "idle in transaction", Event: "none", WaitStatus: "none"},
	})

	if !got.Available || got.ActiveSamples != 4 || got.OnCPUSamples != 2 ||
		math.Abs(got.OnCPUShare-.5) > .000001 {
		t.Fatalf("summary=%+v", got)
	}
	if len(got.Waits) != 2 ||
		got.Waits[0].Event != "WALFlushWait" || got.Waits[0].Samples != 1 ||
		got.Waits[1].Event != "wait cmd" || got.Waits[1].Samples != 1 {
		t.Fatalf("waits=%+v", got.Waits)
	}
}

func TestSummarizeASHMarksNoActiveSamplesUnavailable(t *testing.T) {
	got := SummarizeASH([]ASHSample{{State: "idle in transaction"}})
	if got.Available || got.ActiveSamples != 0 {
		t.Fatalf("summary=%+v", got)
	}
}
