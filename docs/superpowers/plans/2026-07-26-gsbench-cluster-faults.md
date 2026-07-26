# gsbench Replication and Cluster Faults Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement primary/standby, distributed-cluster, and opt-in infrastructure fault scenarios with node-aware evidence, allowlisted providers, out-of-band recovery, and universal restore.

**Architecture:** Add a normalized replication observer and distributed placement/topology service for SQL-driven scenarios, then implement local `tc`/`nft` and node-control providers as typed actions. Every infrastructure action is preflighted, mirrored to the local recovery ledger, journaled before mutation, and restored before database mutations. Managed GaussDB actions use a provider interface rather than guessed host commands.

**Tech Stack:** Go 1.26.5, `database/sql`, `os/exec` without shell expansion, JSON action payloads, Linux `tc`/`nft`, openGauss `gs_guc`/`gs_ctl`, GaussDB provider APIs, Go `testing`.

## Global Constraints

- Implement exactly 341–343, 701–706, and 721–728.
- 701–704 are topology/capability dependent; 705–706 and 726–728 are risk C.
- 703 is risk B and must restore replay delay or resume replay before verification.
- Never partition all distributed shards at once.
- Never run infrastructure faults without an independent recovery channel and a successful inverse-action dry-run.
- Never execute configured text through `sh -c`; every executable and argument is allowlisted and passed separately.
- Managed GaussDB uses provider actions and exact component IDs; do not invent `cm_ctl` commands.
- Healthy topology after switchover is more important than forcing the original primary role.
- The current workspace has no top-level `.git`; use test checkpoints and do not initialize a repository.

---

### Task 1: Normalized Replication and Topology Observers

**Files:**
- Create: `internal/gsbench/replication_observer.go`
- Create: `internal/gsbench/replication_observer_test.go`
- Create: `internal/gsbench/cluster_topology.go`
- Create: `internal/gsbench/cluster_topology_test.go`
- Create: `internal/gsbench/register_cluster.go`

**Interfaces:**
- Produces: `type ReplicationSample struct`
- Produces: `type ReplicationObserver interface`
- Produces: `type ClusterTopology interface`
- Produces: `func ClusterScenarioFactories() map[ScenarioCode]ScenarioFactory`

- [ ] **Step 1: Write normalization tests**

```go
func TestReplicationSampleNormalizesLocalAndDistributedRows(t *testing.T) {
	want := ReplicationSample{
		Node:"dn_6001", Shard:"shard_1", LocalRole:RoleDNPrimary,
		PeerRole:RoleDNStandby, State:"Streaming",
		SentLSN:"0/500", FlushLSN:"0/400", ReplayLSN:"0/300",
	}
	local := map[string]string{
		"peer_role":"DN Standby", "state":"Streaming",
		"sent_location":"0/500", "flush_location":"0/400",
		"replay_location":"0/300",
	}
	distributed := map[string]string{
		"node_name":"dn_6001", "shard_name":"shard_1",
		"local_role":"DN Primary", "peer_role":"DN Standby",
		"state":"Streaming", "sent_location":"0/500",
		"flush_location":"0/400", "replay_location":"0/300",
	}
	gotLocal, err := NormalizeReplicationRow(SourceLocalWALSender, "dn_6001", "shard_1", local)
	if err != nil {
		t.Fatal(err)
	}
	gotDistributed, err := NormalizeReplicationRow(SourceDistributedWALSender, "", "", distributed)
	if err != nil {
		t.Fatal(err)
	}
	if gotLocal != want || gotDistributed != want {
		t.Fatalf("local=%+v distributed=%+v want=%+v", gotLocal, gotDistributed, want)
	}
}

func TestLagBytesRejectsInvalidLSN(t *testing.T) {
	if _, err := LSNDistance("bad", "0/100"); err == nil {
		t.Fatal("invalid LSN accepted")
	}
}
```

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/gsbench -run 'ReplicationSample|LSNDistance|ClusterTopology' -count=1
```

- [ ] **Step 3: Implement observer selection**

Use local WAL sender/receiver functions for openGauss/centralized and
`pgxc_stat_get_wal_senders()` for distributed GaussDB. Preserve node, shard,
roles, state, sent/write/flush/replay locations, lag bytes, and sample time.

- [ ] **Step 4: Implement topology/placement service**

Expose:

```go
type Placement struct { Key int64; Node, Shard string }

type ClusterTopology interface {
	Refresh(context.Context) (Environment, error)
	Placement(context.Context, string, []int64) ([]Placement, error)
	Healthy(context.Context) (bool, []string, error)
}
```

Use catalog/system functions selected by capability; do not infer node placement
from hash arithmetic in client code.

- [ ] **Step 5: Run observer tests**

```bash
gofmt -w internal/gsbench/replication_observer.go internal/gsbench/replication_observer_test.go internal/gsbench/cluster_topology.go internal/gsbench/cluster_topology_test.go
go test ./internal/gsbench -run 'Replication|LSN|ClusterTopology|Placement' -count=1
```

### Task 2: SQL-Driven Replication Scenarios 701, 702, and 704

**Files:**
- Create: `internal/gsbench/scenario_replication.go`
- Create: `internal/gsbench/scenario_replication_test.go`

**Interfaces:**
- Produces: factories for 701, 702, and 704.

- [ ] **Step 1: Write registration and success-contract tests**

Assert 701 requires a standby and measurable WAL generation/lag, 702 requires
remote-apply commit wait evidence, and 704 requires a standby conflict/cancel.

- [ ] **Step 2: Run failing tests**

```bash
go test ./internal/gsbench -run 'ReplicationScenario|SyncCommit|StandbyConflict' -count=1
```

- [ ] **Step 3: Implement 701**

Run fixed `replication_targets` updates with commit batch 1. Distributed strategy
spreads keys across shards. Stop writes and wait for lag to converge during
VerifyRestore.

- [ ] **Step 4: Implement 702**

Set `synchronous_commit='remote_apply'` on each workload session, run frequent
updates/commits, and require commit p95 plus sync waiters and replay lag. Reset the
session GUC before close.

- [ ] **Step 5: Implement 704**

Open a read-only standby transaction, hold its snapshot, delete current-run rows
and vacuum on primary, then require conflict/cancel or replay conflict evidence.
Restore rolls back standby and invokes baseline repair.

- [ ] **Step 6: Run replication tests**

```bash
gofmt -w internal/gsbench/scenario_replication.go internal/gsbench/scenario_replication_test.go
go test ./internal/gsbench -run 'ReplicationScenario|WALPressure|SyncCommit|StandbyConflict' -count=1
```

### Task 3: Distributed SQL Cluster Scenarios 721–725

**Files:**
- Create: `internal/gsbench/scenario_cluster_sql.go`
- Create: `internal/gsbench/scenario_cluster_sql_test.go`

**Interfaces:**
- Produces: factories for 721–725.

- [ ] **Step 1: Write factory and topology tests**

```go
func TestClusterSQLFactoriesAreDistributedOnly(t *testing.T) {
	for _, code := range []ScenarioCode{721,722,723,724,725} {
		def := DefaultScenarioCatalog().MustCode(code)
		if !reflect.DeepEqual(def.AppliesTo, []EnvironmentClass{EnvironmentDistributedGaussDB}) {
			t.Fatalf("%d applies=%v", code, def.AppliesTo)
		}
	}
}
```

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/gsbench -run 'ClusterSQL|DistributedOnly' -count=1
```

- [ ] **Step 3: Implement 721**

Insert the exact 95-percent single-key distribution into `cluster_skew_data`.
Success requires node tuple max/min ratio; restore truncates only that benchmark
table.

- [ ] **Step 4: Implement 722–723**

722 pins all worker DSNs to one discovered CN without changing cluster routing.
723 obtains keys mapped to one DN and drives only those keys. Both require
target-versus-other node evidence.

- [ ] **Step 5: Implement 724–725**

724 uses two placements in different shards per transaction. 725 uses the same
bounded transactions at high frequency and requires GTM/global-XID evidence;
without it the result is degraded.

- [ ] **Step 6: Run distributed SQL tests**

```bash
gofmt -w internal/gsbench/scenario_cluster_sql.go internal/gsbench/scenario_cluster_sql_test.go
go test ./internal/gsbench -run 'ClusterSQL|Skew|CNHotspot|DNHotspot|CrossShard|GTM' -count=1
```

### Task 4: Allowlisted Local and SSH Network Providers

**Files:**
- Create: `internal/gsbench/provider_local_network.go`
- Create: `internal/gsbench/provider_local_network_test.go`
- Create: `internal/gsbench/provider_ssh.go`
- Create: `internal/gsbench/provider_ssh_test.go`
- Create: `internal/gsbench/remote_agent_protocol.go`
- Create: `internal/gsbench/remote_agent_protocol_test.go`
- Create: `cmd/gsbench-agent/main.go`
- Create: `internal/gsbench/command_runner.go`
- Create: `internal/gsbench/command_runner_test.go`

**Interfaces:**
- Produces: `type CommandRunner interface`
- Produces: `type LocalNetworkProvider struct`
- Produces: `type SSHTransport interface`
- Produces: `type RemoteAgentRequest struct`
- Produces: restricted `gsbench-agent` remote action handling.
- Consumes: typed action journal and local recovery ledger.

- [ ] **Step 1: Write exact argv tests**

```go
func TestNetemActionBuildsFilteredArgvWithoutShell(t *testing.T) {
	cmds, err := BuildNetemCommands(NetemSpec{
		Interface:"eth1", PeerIP:"10.0.0.12", PeerPort:5433,
		Delay:"100ms", Jitter:"20ms",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range cmds {
		if cmd.Path != "tc" || slices.Contains(cmd.Args, "sh") {
			t.Fatalf("command=%+v", cmd)
		}
	}
}
```

Add rejection tests for unsafe interfaces, non-IP peers, invalid ports, existing
non-owned root qdisc, missing `tc`/`nft`, and absent out-of-band recovery.

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/gsbench -run 'Netem|Nft|CommandRunner' -count=1
```

- [ ] **Step 3: Implement command runner**

Use `exec.CommandContext(path, args...)`; capture bounded output; never invoke a
shell. Allow only exact executable names selected by provider code.

- [ ] **Step 4: Implement filtered netem**

Build the design's `prio`, child `netem`, and `u32` peer IP/port filter commands.
Snapshot `tc -json qdisc show`; refuse to replace unrelated state. Inverse removes
only the owned handle and VerifyRestored re-reads qdisc state.

- [ ] **Step 5: Implement run-specific nft**

Normalize run ID into an allowlisted table name, create only that table/chains and
exact peer rules, and inverse with `nft delete table inet <owned-name>`.

- [ ] **Step 6: Write the SSH protocol tests**

Define an injected transport:

```go
type SSHTransport interface {
	RunAgent(context.Context, Node, RemoteAgentRequest) (RemoteAgentResponse, error)
}

type RemoteAgentRequest struct {
	RunID string
	Kind  ActionKind
	Spec  json.RawMessage
}
```

Tests must reject unregistered nodes, arbitrary executable/path fields, unknown
action kinds, plaintext credentials, request/run mismatches, and oversized
payloads. The accepted protocol contains semantic netem, nft, process, and node
actions only; it cannot carry shell text or general-purpose argv.

- [ ] **Step 7: Implement the restricted remote agent**

`provider_ssh.go` resolves only nodes from detected inventory and obtains
credentials by configured environment-variable or system credential-provider
reference. `cmd/gsbench-agent` decodes one bounded JSON request from stdin,
validates the same provider allowlists, applies it through the local provider,
and returns bounded JSON. Its SSH transport invokes only the configured absolute
`gsbench-agent` path; no request field can change that path. Forward and inverse
requests are both mirrored to the local recovery ledger before transmission.

- [ ] **Step 8: Run provider tests in fake runners**

```bash
gofmt -w internal/gsbench/provider_local_network.go internal/gsbench/provider_local_network_test.go internal/gsbench/provider_ssh.go internal/gsbench/provider_ssh_test.go internal/gsbench/remote_agent_protocol.go internal/gsbench/remote_agent_protocol_test.go internal/gsbench/command_runner.go internal/gsbench/command_runner_test.go cmd/gsbench-agent/main.go
go test ./internal/gsbench -run 'Netem|Nft|CommandRunner|LocalNetworkProvider|SSHProvider|RemoteAgent' -count=1
```

Expected: PASS without modifying the host network.

### Task 5: Network Fault Scenarios 341–343 and 705–706

**Files:**
- Create: `internal/gsbench/scenario_network_fault.go`
- Create: `internal/gsbench/scenario_network_fault_test.go`
- Modify: `internal/gsbench/register_cluster.go`

**Interfaces:**
- Produces: factories for 341–343, 705–706.

- [ ] **Step 1: Write risk and restore-order tests**

Assert all five are risk C, cannot prepare with `NoopFaultProvider`, write both
journals before provider Apply, and restore firewall/qdisc before database checks.

- [ ] **Step 2: Run failing tests**

```bash
go test ./internal/gsbench -run 'NetworkFault|ReplicationNetwork' -count=1
```

- [ ] **Step 3: Implement 341–343**

Build typed delay/loss/firewall actions from validated config. Success requires
provider state plus SQL/network impact; provider state alone is insufficient.

- [ ] **Step 4: Implement 705–706**

Resolve one exact replication peer/port from topology. Distributed mode selects one
shard only. Success requires lag/commit/reconnect evidence and restore waits for
replication convergence.

- [ ] **Step 5: Run network fault tests**

```bash
gofmt -w internal/gsbench/scenario_network_fault.go internal/gsbench/scenario_network_fault_test.go internal/gsbench/register_cluster.go
go test ./internal/gsbench -run 'NetworkFault|ReplicationNetwork|RestoreOrder' -count=1
```

### Task 6: Standby Delay Provider and Scenario 703

**Files:**
- Create: `internal/gsbench/provider_opengauss_admin.go`
- Create: `internal/gsbench/provider_opengauss_admin_test.go`
- Create: `internal/gsbench/provider_gaussdb_replay.go`
- Create: `internal/gsbench/provider_gaussdb_replay_test.go`
- Create: `internal/gsbench/scenario_replay_delay.go`
- Create: `internal/gsbench/scenario_replay_delay_test.go`

**Interfaces:**
- Produces: allowlisted `gs_guc`/`gs_ctl reload` actions, an approved managed
  GaussDB replay-delay client adapter, and factory 703.

- [ ] **Step 1: Write argv/original-value tests**

Assert provider reads and journals the original `recovery_min_apply_delay`, builds:

```text
gs_guc set -D <validated-dir> -c recovery_min_apply_delay=5000
gs_ctl reload -D <validated-dir>
```

as separate argv arrays, and inverse uses the exact original value.

- [ ] **Step 2: Run failing tests**

```bash
go test ./internal/gsbench -run 'ReplayDelay|OpenGaussAdmin' -count=1
```

- [ ] **Step 3: Implement provider validation**

Require an exact configured standby node, validated data directory under an
allowlisted root, executable discovery, standby role proof, and recovery channel.

- [ ] **Step 4: Implement the managed GaussDB equivalent**

Define and inject:

```go
type GaussDBReplayAdminClient interface {
	SetReplayDelay(context.Context, string, time.Duration) (jobID, original string, err error)
	RestoreReplayDelay(context.Context, string, string) (jobID string, err error)
	Job(context.Context, string) (ProviderJob, error)
}
```

Require an exact discovered standby component ID. Journal the original setting
and both provider job IDs, poll each job with a bounded timeout, never log
credentials, and verify the setting and replay state after each operation.

- [ ] **Step 5: Implement 703**

Apply delay, run 701's writer, require replay lag/delay, restore original GUC and
reload or call the managed provider, then wait for replay convergence. A managed
GaussDB environment without the approved equivalent returns `NOT_APPLICABLE`
with the missing capability/provider reason.

- [ ] **Step 6: Run tests**

```bash
gofmt -w internal/gsbench/provider_opengauss_admin.go internal/gsbench/provider_opengauss_admin_test.go internal/gsbench/provider_gaussdb_replay.go internal/gsbench/provider_gaussdb_replay_test.go internal/gsbench/scenario_replay_delay.go internal/gsbench/scenario_replay_delay_test.go
go test ./internal/gsbench -run 'ReplayDelay|OpenGaussAdmin|GaussDBReplay' -count=1
```

### Task 7: Node Slow, Failure, and Switchover 726–728

**Files:**
- Create: `internal/gsbench/provider_node.go`
- Create: `internal/gsbench/provider_node_test.go`
- Create: `internal/gsbench/provider_gaussdb.go`
- Create: `internal/gsbench/provider_gaussdb_test.go`
- Create: `internal/gsbench/scenario_cluster_admin.go`
- Create: `internal/gsbench/scenario_cluster_admin_test.go`

**Interfaces:**
- Produces: `type NodeProvider interface`
- Produces: factories for 726–728.

- [ ] **Step 1: Define and test semantic node actions**

```go
type NodeProvider interface {
	FaultProvider
	StopNode(context.Context, Node) (Action, error)
	StartNode(context.Context, Node) (Action, error)
	Switchover(context.Context, Node, Node) (Action, error)
}
```

Test that raw managed-provider commands are impossible and exact component IDs are
required.

- [ ] **Step 2: Implement the managed GaussDB provider**

Define an injected client:

```go
type GaussDBAdminClient interface {
	StopNode(context.Context, string) (string, error)
	StartNode(context.Context, string) (string, error)
	Switchover(context.Context, string, string) (string, error)
	Job(context.Context, string) (ProviderJob, error)
}
```

Accept only component IDs returned by environment discovery, store returned job
IDs in `ActionCloudFaultJob`, poll bounded job status, and never log credentials.

- [ ] **Step 3: Implement 726**

Use the network provider against one CN/DN peer path; require selected-node slow
evidence and restore qdisc before topology verification.

- [ ] **Step 4: Implement 727**

Self-managed provider builds `gs_ctl stop/start` argv only for an approved node
with a healthy replica. Managed provider uses semantic API calls. Restore starts
the node and waits for it to rejoin and catch up.

- [ ] **Step 5: Implement 728**

Capture original roles/LSNs, perform planned switchover only when synchronized,
and verify service continuity. Restore ensures healthy replication; switch back
only when `restore_original_role=true` and preconditions still hold.

- [ ] **Step 6: Run cluster admin tests**

```bash
gofmt -w internal/gsbench/provider_node.go internal/gsbench/provider_node_test.go internal/gsbench/provider_gaussdb.go internal/gsbench/provider_gaussdb_test.go internal/gsbench/scenario_cluster_admin.go internal/gsbench/scenario_cluster_admin_test.go
go test ./internal/gsbench -run 'NodeProvider|GaussDBAdmin|PartialNode|NodeFailure|Switchover' -count=1
```

### Task 8: Cluster Documentation, Namespace Tests, and Gate

**Files:**
- Modify: `configs/gsbench.cfg`
- Modify: `docs/gsbench/README.md`
- Create: `internal/gsbench/provider_linux_integration_test.go`
- Modify: `scripts/gsbench-direct_test.sh`

**Interfaces:**
- Produces: completed cluster factory map and safe provider acceptance.

- [ ] **Step 1: Assert exact registration**

Require:

```go
[]ScenarioCode{
	341,342,343,
	701,702,703,704,705,706,
	721,722,723,724,725,726,727,728,
}
```

- [ ] **Step 2: Add Linux namespace integration tests**

Under an explicit build tag, create a temporary network namespace/veth pair, apply
341/342/343 through the real provider, call universal restore, and assert no
gsbench qdisc/nft table remains. Skip unless running in the dedicated privileged
test container.

- [ ] **Step 3: Update configuration/docs**

Document exact risk switches, provider types, out-of-band requirement, managed
GaussDB provider behavior, and restore semantics for role changes.

- [ ] **Step 4: Run non-destructive verification**

```bash
gofmt -w internal/gsbench
go test ./internal/gsbench -run 'Replication|Cluster|Netem|Nft|Provider|Restore' -count=1
go test -race ./internal/gsbench -count=1
go test ./... -count=1
go vet ./...
```

- [ ] **Step 5: Record the checkpoint**

Record the exact command output in the execution report. Git-backed checkout:

```bash
git add internal/gsbench configs/gsbench.cfg docs/gsbench/README.md scripts/gsbench-direct_test.sh
git commit -m "feat(gsbench): add replication cluster and infrastructure faults"
```
