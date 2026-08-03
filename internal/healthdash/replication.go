package healthdash

import (
	"math"
	"strconv"
	"strings"

	"gstop/internal/dbconn"
)

func parseReplicationHealth(rows []dbconn.Row) (ReplicationHealth, bool) {
	if rows == nil {
		return ReplicationHealth{}, false
	}
	health := ReplicationHealth{}
	for _, row := range rows {
		direction := strings.ToUpper(strings.TrimSpace(row.Str(0)))
		if direction == "LOCAL" {
			health.LocalRole = row.Str(1)
			continue
		}
		if direction != "SENDER" && direction != "RECEIVER" {
			continue
		}
		channel := ReplicationChannel{
			Direction: direction, LocalRole: row.Str(1), PeerRole: row.Str(2),
			PeerState: row.Str(3), State: row.Str(4), Channel: row.Str(5),
			SenderSent: row.Str(6), ReceiverReceived: row.Str(7),
			ReceiverWrite: row.Str(8), ReceiverFlush: row.Str(9), ReceiverReplay: row.Str(10),
			SyncState: row.Str(12),
		}
		channel.SyncPercent = replicationPercent(row.Str(11))
		channel.SyncPriority, _ = row.Int(13)
		channel.LagBytes = lagBytes(channel.SenderSent, channel.ReceiverReplay)
		health.Channels = append(health.Channels, channel)
		if health.LocalRole == "" {
			health.LocalRole = channel.LocalRole
		}
	}
	return health, true
}

func replicationPercent(value string) float64 {
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "%"))
	percent, _ := strconv.ParseFloat(value, 64)
	return percent
}

func parseLSN(value string) (uint64, bool) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "(")
	value = strings.TrimSuffix(value, ")")
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return 0, false
	}
	high, errHigh := strconv.ParseUint(parts[0], 16, 32)
	low, errLow := strconv.ParseUint(parts[1], 16, 32)
	if errHigh != nil || errLow != nil {
		return 0, false
	}
	return high<<32 | low, true
}

func lagBytes(source, target string) int64 {
	sourceLSN, sourceOK := parseLSN(source)
	targetLSN, targetOK := parseLSN(target)
	if !sourceOK || !targetOK || sourceLSN < targetLSN {
		return 0
	}
	delta := sourceLSN - targetLSN
	if delta > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(delta)
}
