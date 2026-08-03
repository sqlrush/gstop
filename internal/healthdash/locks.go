package healthdash

import (
	"sort"
	"strconv"
	"strings"

	"gstop/internal/dbconn"
)

func parseLockHealth(rows []dbconn.Row) (LockHealth, bool) {
	if rows == nil {
		return LockHealth{}, false
	}

	unique := make(map[string]LockChain, len(rows))
	for _, row := range rows {
		waiterPID, waiterOK := row.Int(0)
		blockerPID, blockerOK := row.Int(2)
		if !waiterOK || !blockerOK {
			continue
		}
		chain := LockChain{
			WaiterPID: waiterPID, WaiterSession: row.Str(1),
			BlockerPID: blockerPID, BlockerSession: row.Str(3),
			LockType: row.Str(4), Mode: row.Str(5), LockTag: row.Str(6),
			Query: row.Str(8),
		}
		chain.SQLID, _ = row.Int(7)
		chain.ElapsedUS, _ = row.Float(9)
		key := lockIdentity(chain)
		if old, exists := unique[key]; !exists || chain.ElapsedUS > old.ElapsedUS {
			unique[key] = chain
		}
	}

	all := make([]LockChain, 0, len(unique))
	waiters := make(map[string]struct{})
	blockers := make(map[string]struct{})
	longest := float64(0)
	for _, chain := range unique {
		all = append(all, chain)
		waiters[sessionIdentity(chain.WaiterSession, chain.WaiterPID)] = struct{}{}
		blockers[sessionIdentity(chain.BlockerSession, chain.BlockerPID)] = struct{}{}
		if chain.ElapsedUS > longest {
			longest = chain.ElapsedUS
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].ElapsedUS != all[j].ElapsedUS {
			return all[i].ElapsedUS > all[j].ElapsedUS
		}
		return all[i].WaiterPID < all[j].WaiterPID
	})
	if len(all) > 5 {
		all = all[:5]
	}
	return LockHealth{Waiters: len(waiters), Blockers: len(blockers), LongestWaitUS: longest, Chains: all}, true
}

func lockIdentity(chain LockChain) string {
	return sessionIdentity(chain.WaiterSession, chain.WaiterPID) + "\x00" +
		sessionIdentity(chain.BlockerSession, chain.BlockerPID) + "\x00" + chain.LockTag
}

func sessionIdentity(session string, pid int64) string {
	if strings.TrimSpace(session) != "" {
		return session
	}
	return strconv.FormatInt(pid, 10)
}
