package healthdash

import (
	"testing"

	"gstop/internal/dbconn"
)

func TestParseLSNAndLag(t *testing.T) {
	if got := lagBytes("0/200", "0/100"); got != 256 {
		t.Fatalf("lag=%d, want 256", got)
	}
	if got := lagBytes("bad", "0/100"); got != 0 {
		t.Fatalf("bad lag=%d, want 0", got)
	}
	if got := lagBytes("0/100", "0/200"); got != 0 {
		t.Fatalf("backwards lag=%d, want 0", got)
	}
}

func TestParseReplicationHealthStandaloneAndChannels(t *testing.T) {
	standalone, ok := parseReplicationHealth([]dbconn.Row{
		{"LOCAL", "Primary", "", "", "", "", "", "", "", "", "", float64(0), "", int64(0)},
	})
	if !ok || standalone.LocalRole != "Primary" || len(standalone.Channels) != 0 {
		t.Fatalf("standalone=%+v ok=%v", standalone, ok)
	}

	got, ok := parseReplicationHealth([]dbconn.Row{
		{"LOCAL", "Primary", "", "", "", "", "", "", "", "", "", float64(0), "", int64(0)},
		{"SENDER", "Primary", "Standby", "Normal", "Streaming", "10.0.0.2:5432", "0/200", "0/180", "0/170", "0/160", "0/100", float64(98.5), "Sync", int64(1)},
		{"RECEIVER", "Standby", "Primary", "Normal", "Streaming", "10.0.0.1:5432", "0/200", "0/200", "0/1f0", "0/1e0", "0/1d0", float64(97), "Async", int64(2)},
	})
	if !ok || got.LocalRole != "Primary" || len(got.Channels) != 2 {
		t.Fatalf("replication=%+v ok=%v", got, ok)
	}
	if got.Channels[0].Direction != "SENDER" || got.Channels[0].LagBytes != 256 || got.Channels[1].Direction != "RECEIVER" {
		t.Fatalf("channels=%+v", got.Channels)
	}
}

func TestParseReplicationHealthRejectsFailedSample(t *testing.T) {
	if got, ok := parseReplicationHealth(nil); ok || got.LocalRole != "" || len(got.Channels) != 0 {
		t.Fatalf("failed result=%+v ok=%v", got, ok)
	}
}
