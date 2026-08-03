package healthdash

import (
	"testing"

	"gstop/internal/dbconn"
)

func TestParseClusterNodesRequiresCoordinatorTopology(t *testing.T) {
	rows := []dbconn.Row{
		{"cn_5001", "C", "10.0.0.1", int64(5001), "10.0.0.2", int64(5001), true, true, true, true, true, int64(1)},
		{"dn_6001", "D", "10.0.0.1", int64(6001), "10.0.0.2", int64(6001), true, true, false, false, false, int64(2)},
		{"dn_6001_standby", "S", "10.0.0.2", int64(6001), "10.0.0.1", int64(6001), false, false, false, false, true, int64(3)},
	}

	nodes, available := parseClusterNodes(rows)
	if !available || len(nodes) != 3 {
		t.Fatalf("nodes=%+v available=%v", nodes, available)
	}
	if !nodes[0].ActiveKnown || !nodes[0].Active || nodes[1].ActiveKnown || nodes[2].ActiveKnown {
		t.Fatalf("node activity capability parsed incorrectly: %+v", nodes)
	}

	if nodes, available := parseClusterNodes([]dbconn.Row{}); available || len(nodes) != 0 {
		t.Fatalf("standalone nodes=%+v available=%v", nodes, available)
	}
	if nodes, available := parseClusterNodes(rows[1:]); available || len(nodes) != 0 {
		t.Fatalf("DN-side catalog must be rejected: nodes=%+v available=%v", nodes, available)
	}
	if nodes, available := parseClusterNodes(nil); available || len(nodes) != 0 {
		t.Fatalf("failed catalog nodes=%+v available=%v", nodes, available)
	}
}

func TestParseCMStatusRecognizesKnownSectionsAndIgnoresUnknown(t *testing.T) {
	fixture := `[ CMServer State ]
node node_ip instance state
--------------------------------------------------
1 cm1 10.0.0.1 1 Primary
2 cm2 10.0.0.2 2 Standby

[ Cluster State ]
cluster_state : Normal
redistributing : No

[ Coordinator State ]
node node_ip instance state
1 cn1 10.0.0.1 5001 Normal

[ Datanode State ]
node node_ip instance state | node node_ip instance state
1 dn1 10.0.0.1 6001 P Primary Normal | 2 dn2 10.0.0.2 6002 S Standby Normal

[ CM Agent State ]
1 cm1 10.0.0.1 1001 Normal

[ Unknown Future State ]
1 future 9999 Broken`

	got := parseCMStatus(fixture)
	if len(got) != 7 {
		t.Fatalf("components=%d, want 7: %+v", len(got), got)
	}
	wantKinds := []string{"CM SERVER", "CM SERVER", "CLUSTER", "COORDINATOR", "DATANODE", "DATANODE", "CM AGENT"}
	for i, want := range wantKinds {
		if got[i].Kind != want {
			t.Fatalf("component[%d]=%+v, want kind %q", i, got[i], want)
		}
	}
	if got[2].State != "Normal" || got[4].Role != "P Primary" || got[4].State != "Normal" {
		t.Fatalf("parsed states=%+v", got)
	}
}
