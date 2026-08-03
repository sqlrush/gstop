package gsbench

import (
	"database/sql/driver"
	"testing"
	"time"
)

// This catches direct string scanning, which rejects NULL driver values from
// either standalone or distributed lock evidence queries.
func TestScanLockEvidenceRowsAcceptsNullableText(t *testing.T) {
	for _, test := range []struct {
		name string
	}{
		{name: "standalone"},
		{name: "distributed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows := openExplainRowsForTest(t, &explainRowsTestRows{
				columns: []string{
					"node", "object", "locktype", "holder_mode", "waiter_mode",
					"granted", "blocker_tag", "waiter_tag", "wait_age_seconds",
				},
				values: [][]driver.Value{{
					nil, nil, "transactionid", nil, nil, false, nil, nil, 1.25,
				}},
			})

			got, err := scanLockEvidenceRows(rows, LockDefinition{
				ExpectedKind: "row_chain",
				Object:       "lock_targets",
			})
			if err != nil {
				t.Fatal(err)
			}
			want := LockEvidence{
				LockType: "transactionid",
				Object:   "lock_targets",
				WaitAge:  1250 * time.Millisecond,
			}
			if len(got) != 1 {
				t.Fatalf("evidence count=%d want=1", len(got))
			}
			if got[0] != want {
				t.Fatalf("evidence=%+v want=%+v", got[0], want)
			}
		})
	}
}
