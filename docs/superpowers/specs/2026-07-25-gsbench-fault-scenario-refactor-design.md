# gsbench 三位编号故障场景重构设计

**作者：** WangYingJie <sqlrush@gmail.com>

**日期：** 2026-07-25

**状态：** 方案已确认，开发计划已完成，待实现

## 1. 目标

本设计对 `gsbench` 的故障场景体系进行一次统一重构，目标如下：

1. 使用三位编号管理场景，第一位表示故障类别，后两位表示该类别中的具体场景。
2. 一个可独立触发、独立验证、独立恢复的故障原因对应一个编号。
3. 同时支持：
   - openGauss 单机和主备；
   - 集中式 GaussDB；
   - 分布式 GaussDB。
4. 每个场景必须说明前置条件、制造方式、核心 SQL 或系统命令、成功证据、风险等级和恢复动作。
5. 提供一个通用恢复入口：

   ```bash
   gsbench restore
   ```

   它可以停止当前故障负载、释放锁和连接、撤销数据库变更、撤销网络或节点故障注入，并验证数据库已经恢复到正常状态。
6. 场景成功不能只依据“工作线程已启动”，必须取得数据库、节点或操作系统的直接证据。
7. 不允许因为只在 openGauss 上通过，就宣称该场景支持 GaussDB；三种目标形态分别进行真实环境验收。

本设计取代旧的 1～15 编号、组合式 `lock_storm` 以及单一
`dynamic_memory` 场景。旧编号不再作为正式接口；没有名称冲突的旧文本别名可以在
一个过渡版本中保留并打印废弃提示。

## 2. 不在普通模式中承诺的能力

以下行为不作为普通 `gsbench run` 的默认能力：

- 把数据库主机故意打到操作系统 OOM Killer；
- 在没有独立恢复通道时执行网络隔离；
- 在生产实例上停止主节点、强制故障切换或破坏仲裁；
- 伪造一个并不存在的数据库内核内存泄漏；
- 接受用户提供的任意 SQL、Shell 命令或业务对象作为故障注入目标。

数据库层受控 OOM、内存持续驻留、网络延迟、节点停止和切换仍可以作为独立场景，
但必须满足本设计的能力探测、风险开关、目标白名单、恢复日志和专用测试环境要求。

## 3. 编号和类别

| 编号范围 | 类别 | 当前规划 |
|---|---|---|
| 1xx | CPU | TP、AP、混合 CPU |
| 2xx | 内存 | work_mem、shared buffers、计划缓存、会话、总内存、受控 OOM |
| 3xx | I/O 与网络 | 数据读写、WAL、临时文件、客户端网络、CN/DN 网络、故障注入 |
| 4xx | 连接池与线程池 | 连接占用、连接抖动、线程排队、分布式 pooler |
| 5xx | 锁与并发 | 业务锁、八级表锁冲突矩阵、分布式全局锁 |
| 6xx | 执行计划 | `planchange_*` 和 `hardparse_*` |
| 7xx | 主备与集群架构 | WAL 追赶、同步提交、回放、分片、CN/DN/GTM、节点故障 |
| 8xx | 维护操作 | VACUUM 等维护压力 |

场景的正式名称和编号一一对应。CLI 同时接受编号和正式名称：

```bash
gsbench run 621
gsbench run hardparse_literal_flood
gsbench run 301,304,702
```

### 3.1 完整场景目录（90 项）

适用环境缩写：`OG` 为 openGauss，`CG` 为集中式 GaussDB，`DG` 为分布式
GaussDB，`PS` 表示必须存在主备复制，`CAP` 表示必须通过对应能力探测。风险等级
沿用第 17 节定义；`A/B` 表示普通模式为 A，但启用管理型变体时提升为 B。

| 编号 | 正式名称 | 类别 | 适用环境 | 风险等级 | 制造方式摘要 | 核心数据表/对象 |
|---|---|---|---|---|---|---|
| 101 | `tp_cpu` | CPU | OG/CG/DG | A | 并发点查、更新、插入和短事务提交 | `accounts`、`orders` |
| 102 | `ap_cpu` | CPU | OG/CG/DG | A | 大表扫描、Hash Join、聚合和排序 | `fact_sales`、`dim_product` |
| 103 | `mixed_cpu` | CPU | OG/CG/DG | A | 按比例同时运行 TP 与 AP worker | 101、102 的公共对象 |
| 201 | `memory_workmem_sort` | 内存 | OG/CG/DG | A | 提高 session `work_mem` 并执行大排序 | `sort_data`、`fact_sales` |
| 202 | `memory_workmem_hash` | 内存 | OG/CG/DG | A | 大 Hash Join 和 Hash Aggregate | `fact_sales` |
| 203 | `memory_sharedbuffer_churn` | 内存 | OG/CG/DG | A | 扫描大于共享缓存的变化工作集 | `fact_sales` |
| 204 | `memory_plancache_growth` | 内存 | OG/CG/DG/CAP | A | 长连接创建大量唯一 PREPARE | `plan_data`、plan cache |
| 205 | `memory_session_context_growth` | 内存 | OG/CG/DG | A | 长事务打开大量 cursor/Portal | `fact_sales`、session context |
| 206 | `memory_global_cache_pressure` | 内存 | OG/CG/DG/CAP | A | 分批创建和访问小表、索引及目录 | `global_cache_targets`、catalog cache |
| 207 | `memory_total_pressure` | 内存 | OG/CG/DG | A | 组合排序、Hash、计划缓存和 cursor | 201、202、204、205 的对象 |
| 208 | `memory_retention` | 内存 | OG/CG/DG | A | 停止新 SQL 后保持长连接观察驻留 | plan/session memory context |
| 209 | `memory_oom_guarded` | 内存 | OG/CG/DG | B | 受保护地增加执行内存直到数据库拒绝或保护线 | `sort_data`、`fact_sales` |
| 301 | `io_sequential_read` | I/O 与网络 | OG/CG/DG | A | 变化范围的大表顺序扫描 | `fact_sales` |
| 302 | `io_random_read` | I/O 与网络 | OG/CG/DG | A | 超过有效缓存集合的随机主键点查 | `accounts` |
| 303 | `io_wal_write` | I/O 与网络 | OG/CG/DG | A | 高频更新和小事务提交生成 WAL | `accounts` |
| 304 | `io_temp_spill` | I/O 与网络 | OG/CG/DG | A | 降低 `work_mem` 强制排序/Hash 落盘 | `fact_sales` |
| 305 | `io_checkpoint_flush` | I/O 与网络 | OG/CG/DG | B | 制造脏页后执行或观察 checkpoint | `vacuum_targets`、checkpoint |
| 321 | `network_client_egress` | I/O 与网络 | OG/CG/DG | A | 向客户端返回大量 payload | `fact_sales` |
| 322 | `network_client_ingress` | I/O 与网络 | OG/CG/DG | A | 客户端 PBE 批量上传 payload | `network_ingress` |
| 331 | `network_cn_dn_stream` | I/O 与网络 | DG | A | 多 DN 聚合产生 GATHER stream | `fact_sales` |
| 332 | `network_distributed_shuffle` | I/O 与网络 | DG | A | 非共置列关联产生 REDISTRIBUTE | `fact_sales`、`dist_join_data` |
| 333 | `network_distributed_broadcast` | I/O 与网络 | DG | A | Hash 小表强制 BROADCAST | `fact_sales`、`dist_small_hash` |
| 341 | `network_latency_injection` | I/O 与网络 | OG/CG/DG/provider | C | 对精确 peer/port 注入 netem 延迟 | `tc` qdisc/filter |
| 342 | `network_packet_loss` | I/O 与网络 | OG/CG/DG/provider | C | 对精确 peer/port 注入 netem 丢包 | `tc` qdisc/filter |
| 343 | `network_partition` | I/O 与网络 | OG/CG/DG/provider | C | run 专属 nft 规则隔离精确链路 | `nft` table/chain |
| 401 | `connection_pool` | 连接池和线程池 | OG/CG/DG | A | 建立 idle、idle-in-tx 和 active 连接 | tagged sessions |
| 402 | `thread_pool` | 连接池和线程池 | OG/CG/DG/CAP | A | active session 数超过可用线程 | thread-pool views |
| 403 | `connection_churn` | 连接池和线程池 | OG/CG/DG | A | 高频连接、认证、`SELECT 1`、断开 | connection/authentication |
| 404 | `threadpool_queue` | 连接池和线程池 | OG/CG/DG/CAP | A | 有界 CPU SQL 使会话进入线程队列 | thread-pool queue |
| 405 | `pooler_cn_dn_pressure` | 连接池和线程池 | DG | A | 多 CN session 竞争 DN pooler 连接 | pooler、`fact_sales` |
| 501 | `lock_row_chain` | 锁与并发 | OG/CG/DG | A | 多事务逐行形成 blocker/waiter 链 | `lock_targets` |
| 502 | `lock_table_exclusive` | 锁与并发 | OG/CG/DG | A | AccessExclusive 阻塞 AccessShare | `lock_table_targets` |
| 503 | `lock_ddl_wait` | 锁与并发 | OG/CG/DG | A | DML 持锁，DDL 等待 | `lock_ddl_targets` |
| 504 | `lock_deadlock` | 锁与并发 | OG/CG/DG | A | 两事务反向更新形成死锁环 | `lock_targets` |
| 505 | `lock_ddl_blocks_dml` | 锁与并发 | OG/CG/DG | A | 未提交 DDL 阻塞 DML | `lock_ddl_targets` |
| 506 | `lock_select_blocks_ddl` | 锁与并发 | OG/CG/DG | A | 长事务 SELECT 阻塞 DDL | `lock_ddl_targets` |
| 507 | `lock_vacuum_blocks_ddl` | 锁与并发 | OG/CG/DG | A | 运行中 VACUUM 与 DDL 竞争 | `vacuum_targets` |
| 508 | `lock_ddl_blocks_vacuum` | 锁与并发 | OG/CG/DG | A | AccessExclusive 阻塞 VACUUM | `vacuum_targets` |
| 509 | `lock_createindex_blocks_dml` | 锁与并发 | OG/CG/DG | A | 普通 CREATE INDEX 阻塞 DML | `vacuum_targets` |
| 510 | `lock_dml_blocks_createindex` | 锁与并发 | OG/CG/DG | A | 未提交 DML 阻塞 CREATE INDEX | `vacuum_targets` |
| 511 | `lock_distributed_ddl_global` | 锁与并发 | DG | A | 多 CN DDL 竞争全局 regular lock | `ddl_global_*` |
| 512 | `lock_distributed_txn_chain` | 锁与并发 | DG | A | 跨分片事务形成全局等待链 | `dist_lock_targets` |
| 520 | `lockmode_accessshare_accessexclusive` | 锁与并发 | OG/CG/DG | A | AS holder 阻塞 AX waiter | `lock_mode_targets` |
| 521 | `lockmode_rowshare_exclusive` | 锁与并发 | OG/CG/DG | A | RS holder 阻塞 X waiter | `lock_mode_targets` |
| 522 | `lockmode_rowshare_accessexclusive` | 锁与并发 | OG/CG/DG | A | RS holder 阻塞 AX waiter | `lock_mode_targets` |
| 523 | `lockmode_rowexclusive_share` | 锁与并发 | OG/CG/DG | A | RX holder 阻塞 S waiter | `lock_mode_targets` |
| 524 | `lockmode_rowexclusive_sharerowexclusive` | 锁与并发 | OG/CG/DG | A | RX holder 阻塞 SRX waiter | `lock_mode_targets` |
| 525 | `lockmode_rowexclusive_exclusive` | 锁与并发 | OG/CG/DG | A | RX holder 阻塞 X waiter | `lock_mode_targets` |
| 526 | `lockmode_rowexclusive_accessexclusive` | 锁与并发 | OG/CG/DG | A | RX holder 阻塞 AX waiter | `lock_mode_targets` |
| 527 | `lockmode_shareupdateexclusive_self` | 锁与并发 | OG/CG/DG | A | SUE holder 阻塞 SUE waiter | `lock_mode_targets` |
| 528 | `lockmode_shareupdateexclusive_share` | 锁与并发 | OG/CG/DG | A | SUE holder 阻塞 S waiter | `lock_mode_targets` |
| 529 | `lockmode_shareupdateexclusive_sharerowexclusive` | 锁与并发 | OG/CG/DG | A | SUE holder 阻塞 SRX waiter | `lock_mode_targets` |
| 530 | `lockmode_shareupdateexclusive_exclusive` | 锁与并发 | OG/CG/DG | A | SUE holder 阻塞 X waiter | `lock_mode_targets` |
| 531 | `lockmode_shareupdateexclusive_accessexclusive` | 锁与并发 | OG/CG/DG | A | SUE holder 阻塞 AX waiter | `lock_mode_targets` |
| 532 | `lockmode_share_sharerowexclusive` | 锁与并发 | OG/CG/DG | A | S holder 阻塞 SRX waiter | `lock_mode_targets` |
| 533 | `lockmode_share_exclusive` | 锁与并发 | OG/CG/DG | A | S holder 阻塞 X waiter | `lock_mode_targets` |
| 534 | `lockmode_share_accessexclusive` | 锁与并发 | OG/CG/DG | A | S holder 阻塞 AX waiter | `lock_mode_targets` |
| 535 | `lockmode_sharerowexclusive_self` | 锁与并发 | OG/CG/DG | A | SRX holder 阻塞 SRX waiter | `lock_mode_targets` |
| 536 | `lockmode_sharerowexclusive_exclusive` | 锁与并发 | OG/CG/DG | A | SRX holder 阻塞 X waiter | `lock_mode_targets` |
| 537 | `lockmode_sharerowexclusive_accessexclusive` | 锁与并发 | OG/CG/DG | A | SRX holder 阻塞 AX waiter | `lock_mode_targets` |
| 538 | `lockmode_exclusive_self` | 锁与并发 | OG/CG/DG | A | X holder 阻塞 X waiter | `lock_mode_targets` |
| 539 | `lockmode_exclusive_accessexclusive` | 锁与并发 | OG/CG/DG | A | X holder 阻塞 AX waiter | `lock_mode_targets` |
| 540 | `lockmode_accessexclusive_self` | 锁与并发 | OG/CG/DG | A | AX holder 阻塞 AX waiter | `lock_mode_targets` |
| 601 | `planchange_stats_target` | 执行计划 | OG/CG/DG/CAP | A | 降低列统计采样目标后 ANALYZE | `plan_data.stats_target_key` |
| 602 | `planchange_index_unusable` | 执行计划 | OG/CG/DG/CAP | A | 将基准索引置为 UNUSABLE | `plan_index_unusable_idx` |
| 603 | `planchange_stats_ndistinct` | 执行计划 | OG/CG/DG/CAP | A | 修改 `n_distinct` 和统计目标 | `plan_data.stats_ndistinct_key` |
| 604 | `planchange_stats_extended` | 执行计划 | OG/CG/DG/CAP | A | 删除相关列扩展统计 | `plan_data.stats_corr_a/b` |
| 605 | `planchange_index_drop` | 执行计划 | OG/CG/DG | A | 删除基准索引使访问路径跳变 | `plan_index_drop_idx` |
| 606 | `planchange_index_shape` | 执行计划 | OG/CG/DG | A | 将优良复合索引替换为反向列序索引 | `plan_data.index_shape_*` |
| 621 | `hardparse_literal_flood` | 执行计划 | OG/CG/DG | A | 持续生成唯一整数常量 SQL 文本 | `fact_sales` |
| 622 | `hardparse_unprepared` | 执行计划 | OG/CG/DG | A | simple-query 重复发送未预编译 SQL | `fact_sales` |
| 623 | `hardparse_force_custom` | 执行计划 | OG/CG/DG/CAP | A | 强制 prepared statement 使用 custom plan | `fact_sales`、plan cache |
| 624 | `hardparse_session_churn` | 执行计划 | OG/CG/DG | A | 短连接反复 PREPARE/EXECUTE | `fact_sales` |
| 625 | `hardparse_ddl_invalidation` | 执行计划 | OG/CG/DG | A | 索引 DDL 反复使缓存计划失效 | `hardparse_targets` |
| 626 | `hardparse_gpc_bypass` | 执行计划 | OG/CG/DG/CAP | A | `no_gpc` 绕过全局计划缓存 | GPC、`fact_sales` |
| 701 | `replication_wal_pressure` | 主备与集群 | OG/CG/DG/PS | A | 主库或主 DN 高频更新提交 | `replication_targets` |
| 702 | `replication_sync_commit_block` | 主备与集群 | OG/CG/DG/PS | A | `remote_apply` 高频提交等待同步备库 | `replication_targets` |
| 703 | `replication_replay_delay` | 主备与集群 | OG/CG/DG/PS/provider | B | 设置备库回放延迟或短时暂停回放 | standby replay control |
| 704 | `replication_standby_read_conflict` | 主备与集群 | OG/CG/DG/PS | A | 备库长快照与主库 DELETE/VACUUM 冲突 | `replication_conflict_targets` |
| 705 | `replication_network_delay` | 主备与集群 | OG/CG/DG/PS/provider | C | 对复制 peer 注入精确网络延迟 | replication link、`tc` |
| 706 | `replication_network_partition` | 主备与集群 | OG/CG/DG/PS/provider | C | 隔离一个复制 peer/分片主备链路 | replication link、`nft` |
| 721 | `cluster_data_skew` | 主备与集群 | DG | A | 95% 数据写入同一 HASH key | `cluster_skew_data` |
| 722 | `cluster_cn_hotspot` | 主备与集群 | DG | A | 所有 workload 固定连接一个 CN | CN endpoint |
| 723 | `cluster_dn_hotspot` | 主备与集群 | DG | A | 选择映射到同一 DN 的分布键 | `fact_sales.dist_key` |
| 724 | `cluster_cross_shard_txn` | 主备与集群 | DG | A | 一个事务更新两个不同 shard | `dist_txn_targets` |
| 725 | `cluster_gtm_pressure` | 主备与集群 | DG/GTM | A | 高频极短跨分片事务申请全局 XID | `dist_txn_targets`、GTM |
| 726 | `cluster_partial_node_slow` | 主备与集群 | DG/provider | C | 降低一个 CN/DN peer 链路质量 | node link、`tc` |
| 727 | `cluster_node_failure` | 主备与集群 | DG/provider | C | 停止一个有健康副本的节点 | node process/provider |
| 728 | `cluster_switchover` | 主备与集群 | DG/provider | C | 在同步健康前提下计划内切换 | node roles/provider |
| 801 | `vacuum_pressure` | 维护操作 | OG/CG/DG | A/B | 制造 dead tuples 后运行 VACUUM；FULL 为 B | `vacuum_targets` |

## 4. 产品、拓扑和能力模型

### 4.1 环境识别

`doctor` 不只根据 `version()` 判断产品，还必须建立完整环境模型：

```text
Product:
  openGauss | GaussDB

Topology:
  standalone | primary_standby | centralized_gaussdb | distributed_gaussdb

Node roles:
  CN | DN_PRIMARY | DN_STANDBY | GTM | CM | ETCD

Capabilities:
  statement_history
  hard_parse_counters
  global_plan_cache
  global_lock_views
  thread_pool
  pooler_views
  memory_node_views
  wal_sender_views
  standby_control
  external_fault_provider
```

分布式 GaussDB 由 CN 生成并下发执行计划，DN 存储和处理数据，GTM 管理全局事务。
因此不能把集中式 SQL 机械复用到分布式环境。分布式策略必须知道 CN、DN、分片、
主备角色和节点名称。

### 4.2 场景策略选择

同一个场景编号保持相同故障语义，但允许选择不同实现：

```text
Scenario definition
    ├── openGauss strategy
    ├── centralized GaussDB strategy
    └── distributed GaussDB strategy
            ├── CN trigger/observer
            ├── DN trigger/observer
            └── GTM/replication observer
```

每个场景在注册表中声明：

```go
type ScenarioDefinition struct {
    Code          int
    Name          string
    Category      string
    Risk          RiskLevel
    AppliesTo     []EnvironmentClass
    Requires      []Capability
    Strategy      StrategyFactory
    Evidence      EvidenceContract
    RestorePolicy RestorePolicy
}
```

结果状态为：

- `SUCCESS`：目标故障已由直接证据证明；
- `DEGRADED`：执行了明确标注的替代负载，但缺少关键证据；
- `FAILED`：触发、验证或恢复失败；
- `NOT_APPLICABLE`：场景在该拓扑上没有语义，例如在单机 openGauss 上运行
  `cluster_gtm_pressure`；
- `RESTORE_FAILED`：负载已停止，但至少一个逆向动作或恢复验证失败。

产品能力缺失不能静默转为成功。

## 5. 测试数据 DDL、容量换算和建数 SQL

### 5.1 占位符和生成边界

本节给出 `gsbench init` 必须实现的完整逻辑 DDL 和建数 SQL。占位符含义如下：

- `<S>`：经过标识符校验和正确引用的 benchmark schema；
- `<RUN>`：仅含安全字符的 run ID；
- `<NODE>`：能力探测得到的节点名；
- `<IFACE>`、`<PEER_IP>`、`<PEER_PORT>`：外部故障提供器确认过的精确目标；
- `<DATA_DIR>`：自管环境中经过白名单检查的实例数据目录；
- `<CUSTOMER_ROWS>`、`<ORDER_ROWS>`：容量算法得到并绑定进内部 SQL 模板的目标行数；
- `<ROWS_PER_OBJECT>`：206 根据对象数和该场景容量保护线计算的单对象行数；
- `<N>`：程序生成且经过上下界检查的整数序号；
- `$1/$2`：当前插入批次的起止高水位，均为程序绑定的 `bigint` 参数。

SQL 由程序内部模板生成，配置文件不能提供任意 SQL。所有表都属于 `<S>`，不会访问
业务表。DDL 中的对象名来自固定目录，不接受用户输入的表名、列名或分布键。

### 5.2 用户输入容量的解析和安全校验

目标容量优先级固定为：

```text
init --size
  > data.max_size_gb
  > profile 默认值（quick=5GiB，stress=20GiB）
```

`--size` 接受 `100GB`、`1.5TB`、`2TB`；`data.max_size_gb` 接受 1～2048。
换算统一使用二进制单位：

```text
1 GiB = 1024^3 bytes
1 TiB = 1024^4 bytes
```

令用户请求值为 `T`。开始建表前必须同时满足：

```text
0 < T <= 2 TiB
safe_available = free_bytes - total_bytes * min_free_disk_percent / 100
T <= safe_available
```

如果请求值大于安全容量，`init` 直接失败并打印请求值、剩余空间和保留空间，不能
静默缩小用户输入。建数过程中每个批次前重新检查磁盘；空间跌破保留线时停止并保留
已写入的高水位，以便扩容后续建。

### 5.3 表预算、目标行数和批次数

目标容量 `T` 表示整个 benchmark 数据集的容量上限，不只是 heap。算法预留：

```text
79%  可按容量扩展的基础表 heap 预算
 6%  固定小表、锁表和元数据预算
15%  索引、行头、FSM/VM、TOAST 和估算误差
```

对每张可扩展表 `i`：

```text
target_rows_i =
  max(min_rows_i, floor(T * weight_i / 100 / estimated_row_bytes_i))

batch_rows_i =
  clamp(floor(64 MiB / estimated_row_bytes_i), 10,000, 250,000)

batch_count_i =
  ceil(max(0, target_rows_i - high_water_i) / batch_rows_i)
```

权重只用于初始计划，不代表实际物理占用。表完成后读取 heap 和 index 的实际大小；
数据集总占用达到 `0.95 * T` 后，不再启动新的可选扩展批次。达到 `T` 或磁盘安全线
时立即停止。低于 `0.90 * T` 且仍有安全空间时，按权重差额继续补批，最多校准三轮，
防止压缩率或行头估算差异造成无限循环。

| 表 | 权重 | 估算行宽 | 最低行数 | 主要场景 |
|---|---:|---:|---:|---|
| `customers` | 2% | 320 B | 10,000 | 101、102 |
| `accounts` | 6% | 352 B | 100,000 | 101、302、303 |
| `orders` | 6% | 160 B | 100,000 | 101 |
| `order_items` | 7% | 128 B | 200,000 | 101 |
| `fact_sales` | 20% | 192 B | 500,000 | 102、201～205、301、321、331～333、621～624 |
| `sort_data` | 8% | 640 B | 100,000 | 201、209 |
| `plan_data` | 12% | 640 B | 1,000,000 | 204、601～606 |
| `hardparse_targets` | 3% | 320 B | 100,000 | 625 |
| `vacuum_targets` | 5% | 1,152 B | 100,000 | 305、507～510、801 |
| `replication_targets` | 2% | 320 B | 100,000 | 701～703 |
| `replication_conflict_targets` | 1% | 320 B | 100,000 | 704 |
| `cluster_skew_data` | 2% | 320 B | 100,000 | 721 |
| `dist_join_data` | 2% | 192 B | 100,000 | 332 |
| `dist_small_hash` | 1% | 128 B | 10,000 | 333 |
| `dist_txn_targets` | 2% | 128 B | 100,000 | 724、725 |

`cluster_skew_data` 的 2% 是运行 721 时的瞬时容量预留；公共 `init` 只创建空表，
721 的 Prepare 再按本表目标行数分批装载并在恢复时清空。`network_ingress` 同样
使用瞬时预留，不在公共 `init` 中预装 payload。

以下对象使用固定基线，不按 `T` 无限放大：

| 表 | 固定目标行数 |
|---|---:|
| `dim_product` | 100,000 |
| `dim_store` | 10,000 |
| `global_cache_targets` | 10,000 |
| `lock_targets` | 10,000 |
| `lock_table_targets`、`lock_ddl_targets`、`lock_mode_targets` | 各 1,000 |
| `ddl_global_1`～`ddl_global_4` | 各 10,000 |
| `dist_lock_targets` | 100,000 |
| `network_ingress` | 初始 0，由 322 按 run 写入并恢复删除 |
| `meta_*` | 仅写运行和恢复元数据 |

示例：用户执行 `gsbench init --size 100GB`，`fact_sales` 的初始目标为：

```text
floor(100 * 1024^3 * 20% / 192) = 111,848,106 rows
```

批次约为 `64MiB / 192B = 349,525` 行，经上限裁剪后使用 250,000 行，共 448 批。
`init --dry-run` 必须打印输入容量、每张表的权重、估算行宽、目标行数、批大小、批数、
当前高水位和预计新增字节。

### 5.4 集中式和分布式 DDL 方言

为避免维护两份会漂移的列定义，代码以本节的逻辑 DDL 为唯一来源，并展开两个宏：

| 逻辑宏 | openGauss/集中式 GaussDB | 分布式 GaussDB |
|---|---|---|
| `@HASH(column_list)` | 删除宏，不追加子句 | `DISTRIBUTE BY HASH (column_list)` |
| `@REPLICATION` | 删除宏，不追加子句 | `DISTRIBUTE BY REPLICATION` |

因此，去掉宏后的 SQL 就是 openGauss/集中式 DDL；替换宏后的 SQL 是分布式
GaussDB DDL。`init --dry-run` 输出的必须是展开后的可执行 SQL，不能把宏发送给
数据库。

GaussDB 分布式 HASH 表的主键全部包含分布键，避免因为主键/唯一键不包含分布键而
无意创建 GSI。只有 `dim_product`、`dim_store` 和只读的 `meta_dataset` 使用
REPLICATION；高频写入的元数据仍按稳定键 HASH 分布。

```sql
CREATE TABLE <S>.meta_dataset (
    key varchar(128) PRIMARY KEY,
    value text NOT NULL,
    updated_at timestamp NOT NULL DEFAULT current_timestamp
) @REPLICATION;

CREATE TABLE <S>.meta_runs (
    run_id varchar(96) NOT NULL,
    scenarios text NOT NULL,
    phase varchar(32) NOT NULL,
    status varchar(32) NOT NULL,
    owner_name varchar(128) NOT NULL,
    started_at timestamp NOT NULL,
    updated_at timestamp NOT NULL,
    detail text,
    PRIMARY KEY (run_id)
) @HASH(run_id);

CREATE TABLE <S>.meta_journal (
    run_id varchar(96) NOT NULL,
    action_id bigint NOT NULL,
    scenario_code integer NOT NULL,
    action_kind varchar(64) NOT NULL,
    target_node varchar(128),
    target_endpoint varchar(256),
    original_state text,
    forward_action text NOT NULL,
    inverse_action text NOT NULL,
    verify_action text,
    state varchar(32) NOT NULL,
    error_text text,
    created_at timestamp NOT NULL DEFAULT current_timestamp,
    updated_at timestamp NOT NULL DEFAULT current_timestamp,
    PRIMARY KEY (run_id, action_id)
) @HASH(run_id);

CREATE TABLE <S>.meta_batches (
    table_name varchar(128) NOT NULL,
    high_water bigint NOT NULL,
    target_rows bigint NOT NULL,
    estimated_row_bytes bigint NOT NULL,
    dataset_version varchar(32) NOT NULL,
    updated_at timestamp NOT NULL DEFAULT current_timestamp,
    PRIMARY KEY (table_name)
) @HASH(table_name);

CREATE TABLE <S>.meta_plan_cache (
    signature varchar(64) NOT NULL,
    scenario_code integer NOT NULL,
    sql_text text NOT NULL,
    plan_text text NOT NULL,
    updated_at timestamp NOT NULL DEFAULT current_timestamp,
    PRIMARY KEY (signature, scenario_code)
) @HASH(signature);

CREATE TABLE <S>.customers (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    region_id integer NOT NULL,
    name varchar(96),
    payload varchar(256),
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.accounts (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    customer_id bigint NOT NULL,
    balance numeric(18,2) NOT NULL,
    payload varchar(256),
    updated_at timestamp NOT NULL,
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.orders (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    customer_id bigint NOT NULL,
    status integer NOT NULL,
    amount numeric(18,2) NOT NULL,
    created_at timestamp NOT NULL,
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.order_items (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    order_id bigint NOT NULL,
    product_id integer NOT NULL,
    quantity integer NOT NULL,
    amount numeric(18,2) NOT NULL,
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.dim_product (
    id integer PRIMARY KEY,
    category_id integer NOT NULL,
    name varchar(96),
    payload varchar(256)
) @REPLICATION;

CREATE TABLE <S>.dim_store (
    id integer PRIMARY KEY,
    region_id integer NOT NULL,
    name varchar(96)
) @REPLICATION;

CREATE TABLE <S>.fact_sales (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    sale_date date NOT NULL,
    customer_id bigint NOT NULL,
    product_id integer NOT NULL,
    store_id integer NOT NULL,
    amount numeric(18,2) NOT NULL,
    quantity integer NOT NULL,
    payload varchar(256),
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.sort_data (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    group_id integer NOT NULL,
    sort_key bigint NOT NULL,
    payload varchar(512),
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.network_ingress (
    run_id varchar(96) NOT NULL,
    dist_key bigint NOT NULL,
    seq bigint NOT NULL,
    payload varchar(1024) NOT NULL,
    created_at timestamp NOT NULL DEFAULT current_timestamp,
    PRIMARY KEY (dist_key, run_id, seq)
) @HASH(dist_key);

CREATE TABLE <S>.global_cache_targets (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    value varchar(64),
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.plan_data (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    lookup_key bigint NOT NULL,
    skew_key integer NOT NULL,
    stats_target_key integer NOT NULL,
    stats_ndistinct_key bigint NOT NULL,
    stats_corr_a integer NOT NULL,
    stats_corr_b integer NOT NULL,
    index_unusable_key bigint NOT NULL,
    index_drop_key bigint NOT NULL,
    index_shape_lead integer NOT NULL,
    index_shape_tail bigint NOT NULL,
    payload varchar(512),
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.hardparse_targets (
    lookup_key bigint NOT NULL,
    id bigint NOT NULL,
    value bigint NOT NULL,
    payload varchar(256),
    PRIMARY KEY (lookup_key, id)
) @HASH(lookup_key);

CREATE TABLE <S>.lock_targets (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    value bigint NOT NULL,
    payload varchar(256),
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.lock_table_targets (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    value bigint NOT NULL,
    payload varchar(256),
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.lock_ddl_targets (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    value bigint NOT NULL,
    payload varchar(256),
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.lock_mode_targets (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    value bigint NOT NULL,
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.ddl_global_1 (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    value bigint NOT NULL,
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.ddl_global_2 (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    value bigint NOT NULL,
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.ddl_global_3 (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    value bigint NOT NULL,
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.ddl_global_4 (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    value bigint NOT NULL,
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.dist_lock_targets (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    value bigint NOT NULL,
    payload varchar(256),
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.replication_targets (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    version bigint NOT NULL,
    payload varchar(256),
    updated_at timestamp NOT NULL,
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.replication_conflict_targets (
    dist_key bigint NOT NULL,
    run_id varchar(96) NOT NULL,
    id bigint NOT NULL,
    payload varchar(256),
    created_at timestamp NOT NULL,
    PRIMARY KEY (dist_key, run_id, id)
) @HASH(dist_key);

CREATE TABLE <S>.cluster_skew_data (
    skew_key bigint NOT NULL,
    id bigint NOT NULL,
    payload varchar(256),
    PRIMARY KEY (skew_key, id)
) @HASH(skew_key);

CREATE TABLE <S>.dist_join_data (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    join_key bigint NOT NULL,
    value bigint NOT NULL,
    payload varchar(128),
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.dist_small_hash (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    product_id integer NOT NULL,
    category_id integer NOT NULL,
    payload varchar(64),
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.dist_txn_targets (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    value bigint NOT NULL,
    payload varchar(64),
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE TABLE <S>.vacuum_targets (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    group_id integer NOT NULL,
    version bigint NOT NULL,
    payload varchar(1024),
    updated_at timestamp NOT NULL,
    PRIMARY KEY (dist_key, id)
) @HASH(dist_key);

CREATE INDEX accounts_customer_idx
ON <S>.accounts (customer_id, dist_key, id);

CREATE INDEX orders_customer_idx
ON <S>.orders (customer_id, dist_key, id);

CREATE INDEX order_items_order_idx
ON <S>.order_items (order_id, dist_key, id);

CREATE INDEX fact_sales_product_idx
ON <S>.fact_sales (product_id, dist_key, id);

CREATE INDEX fact_sales_customer_idx
ON <S>.fact_sales (customer_id, dist_key, id);

CREATE INDEX sort_data_sort_idx
ON <S>.sort_data (sort_key, dist_key, id);

CREATE INDEX plan_data_lookup_idx
ON <S>.plan_data (lookup_key, dist_key, id);

CREATE INDEX plan_stats_target_idx
ON <S>.plan_data (stats_target_key, dist_key, id);

CREATE INDEX plan_stats_ndistinct_idx
ON <S>.plan_data (stats_ndistinct_key, dist_key, id);

CREATE INDEX plan_stats_corr_idx
ON <S>.plan_data (stats_corr_a, stats_corr_b, dist_key, id);

CREATE INDEX plan_index_unusable_idx
ON <S>.plan_data (index_unusable_key, dist_key, id);

CREATE INDEX plan_index_drop_idx
ON <S>.plan_data (index_drop_key, dist_key, id);

CREATE INDEX plan_index_shape_good_idx
ON <S>.plan_data (index_shape_lead, index_shape_tail, dist_key, id);

CREATE INDEX hardparse_targets_lookup_idx
ON <S>.hardparse_targets (lookup_key, id);

CREATE INDEX replication_conflict_run_idx
ON <S>.replication_conflict_targets (run_id, dist_key, id);

CREATE INDEX vacuum_targets_group_idx
ON <S>.vacuum_targets (group_id, dist_key, id);
```

分片键选择规则：

| 表组 | 分片字段 | 原因 |
|---|---|---|
| `customers`、`accounts`、`orders` | `dist_key=customer_id` | 客户维度 TP 操作可定位并尽量共置 |
| `order_items` | `dist_key=order_id` | 订单明细按订单定位 |
| `fact_sales`、`sort_data`、`plan_data` | `dist_key=mod(id, distribution_cardinality)` | 高基数且均匀，适合扫描、计划和 DN 热点选键 |
| `hardparse_targets` | `lookup_key` | prepared 查询可单 DN 定位 |
| 普通锁、复制、事务和维护表 | `dist_key` | 客户端可以明确选择同 DN 或不同 DN |
| `cluster_skew_data` | `skew_key` | 721 故意让 95% 行使用相同值 |
| `dist_join_data` | `dist_key`，关联列为 `join_key` | 332 故意制造非共置 JOIN 和 REDISTRIBUTE |
| `dist_small_hash` | `dist_key`，关联列为 `product_id` | 333 保持为 HASH 小表以制造 BROADCAST |
| `dim_product`、`dim_store` | REPLICATION | 只读小维表，避免普通星型查询产生额外 stream |
| `meta_runs`、`meta_journal` | `run_id` | 同一 run 的状态和动作稳定路由 |

除 721 的专用倾斜表外，普通 HASH 表的 `dist_key` 必须具有足够基数。不能对
`region_id`、`status`、`category_id` 等低基数字段分片。

### 5.5 按容量生成测试数据的 SQL

每张表都使用同一个增量协议：

1. 从 `<S>.meta_batches` 读取 `high_water`；
2. 计算本轮 `start=high_water+1` 和
   `end=min(start+batch_rows-1,target_rows)`；
3. 绑定 `$1=start`、`$2=end` 执行固定 INSERT；
4. 提交成功后更新高水位；
5. 批次失败时不推进高水位，重试不会重复已提交范围。

可扩展表和固定基线表的 SQL 如下。这里的 `mod(g,1048576)+1` 提供约一百万个普通
分布键；分布式能力适配器也可以根据 bucket/shard 数提高该基数，但不能降低到少于
`1024 * shard_count`。

```sql
INSERT INTO <S>.customers
    (dist_key,id,region_id,name,payload)
SELECT g, g, mod(g,100), 'customer-' || g, repeat('c',128)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.accounts
    (dist_key,id,customer_id,balance,payload,updated_at)
SELECT mod(g,<CUSTOMER_ROWS>)+1, g, mod(g,<CUSTOMER_ROWS>)+1,
       1000, repeat('a',128), current_timestamp
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.orders
    (dist_key,id,customer_id,status,amount,created_at)
SELECT mod(g,<CUSTOMER_ROWS>)+1, g, mod(g,<CUSTOMER_ROWS>)+1,
       mod(g,5), mod(g,10000),
       current_date - (mod(g,365)::integer)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.order_items
    (dist_key,id,order_id,product_id,quantity,amount)
SELECT mod(g,<ORDER_ROWS>)+1, g, mod(g,<ORDER_ROWS>)+1,
       mod(g,100000)+1, mod(g,10)+1, mod(g,5000)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.dim_product
    (id,category_id,name,payload)
SELECT g, mod(g,1000), 'product-' || g, repeat('p',128)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.dim_store
    (id,region_id,name)
SELECT g, mod(g,100), 'store-' || g
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.fact_sales
    (dist_key,id,sale_date,customer_id,product_id,store_id,
     amount,quantity,payload)
SELECT mod(g,1048576)+1, g,
       current_date - (mod(g,730)::integer),
       mod(g,<CUSTOMER_ROWS>)+1,
       mod(g,100000)+1,
       mod(g,10000)+1,
       mod(g,100000)/100.0,
       mod(g,20)+1,
       repeat('f',96)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.sort_data
    (dist_key,id,group_id,sort_key,payload)
SELECT mod(g,1048576)+1, g, mod(g,4096), mod(g*7919,2147483647),
       repeat(chr(65+mod(g,26)::integer),512)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.global_cache_targets
    (dist_key,id,value)
SELECT g, g, 'cache-' || g
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.plan_data
    (dist_key,id,lookup_key,skew_key,stats_target_key,
     stats_ndistinct_key,stats_corr_a,stats_corr_b,
     index_unusable_key,index_drop_key,index_shape_lead,
     index_shape_tail,payload)
SELECT mod(g,1048576)+1,
       g,
       g,
       CASE WHEN mod(g,100)<95 THEN 1 ELSE mod(g,1000) END,
       CASE WHEN mod(g,100)<80 THEN mod(g,4)+1
            ELSE mod(g,1000)+100 END,
       mod(g,1000000)+1,
       mod(g,1000),
       mod(g,1000),
       g,
       g,
       mod(g,1000),
       g,
       repeat('s',400)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.hardparse_targets
    (lookup_key,id,value,payload)
SELECT mod(g,1000000)+1, g, mod(g,10000), repeat('h',192)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.lock_targets
    (dist_key,id,value,payload)
SELECT g, g, 0, repeat('l',128)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.lock_table_targets
    (dist_key,id,value,payload)
SELECT g, g, 0, repeat('t',128)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.lock_ddl_targets
    (dist_key,id,value,payload)
SELECT g, g, 0, repeat('d',128)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.lock_mode_targets
    (dist_key,id,value)
SELECT g, g, 0
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.ddl_global_1 (dist_key,id,value)
SELECT g,g,0 FROM generate_series($1,$2) AS g;
INSERT INTO <S>.ddl_global_2 (dist_key,id,value)
SELECT g,g,0 FROM generate_series($1,$2) AS g;
INSERT INTO <S>.ddl_global_3 (dist_key,id,value)
SELECT g,g,0 FROM generate_series($1,$2) AS g;
INSERT INTO <S>.ddl_global_4 (dist_key,id,value)
SELECT g,g,0 FROM generate_series($1,$2) AS g;

INSERT INTO <S>.dist_lock_targets
    (dist_key,id,value,payload)
SELECT mod(g,1048576)+1, g, 0, repeat('x',128)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.replication_targets
    (dist_key,id,version,payload,updated_at)
SELECT mod(g,1048576)+1, g, 0, repeat('r',192), current_timestamp
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.replication_conflict_targets
    (dist_key,run_id,id,payload,created_at)
SELECT mod(g,1048576)+1, 'baseline', g, repeat('c',192),
       current_timestamp
FROM generate_series($1,$2) AS g;

-- 704 Prepare：从 baseline 克隆当前 run 的受控子集。
INSERT INTO <S>.replication_conflict_targets
    (dist_key,run_id,id,payload,created_at)
SELECT dist_key, '<RUN>', id, payload, current_timestamp
FROM <S>.replication_conflict_targets
WHERE run_id='baseline'
  AND id BETWEEN $1 AND $2;

-- 721 Prepare：该表在公共 init 后保持为空，本语句按 2% 容量预留装载。
INSERT INTO <S>.cluster_skew_data
    (skew_key,id,payload)
SELECT CASE WHEN mod(g,100)<95 THEN 1 ELSE g END,
       g, repeat('k',192)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.dist_join_data
    (dist_key,id,join_key,value,payload)
SELECT mod(g*17,1048576)+1, g, mod(g,<CUSTOMER_ROWS>)+1,
       mod(g,10000), repeat('j',96)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.dist_small_hash
    (dist_key,id,product_id,category_id,payload)
SELECT mod(g*31,1048576)+1, g, mod(g,100000)+1,
       mod(g,1000), repeat('b',48)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.dist_txn_targets
    (dist_key,id,value,payload)
SELECT mod(g,1048576)+1, g, 0, repeat('z',48)
FROM generate_series($1,$2) AS g;

INSERT INTO <S>.vacuum_targets
    (dist_key,id,group_id,version,payload,updated_at)
SELECT mod(g,1048576)+1, g, mod(g,1000), 0,
       repeat('v',900), current_timestamp
FROM generate_series($1,$2) AS g;
```

322 的网络入流量数据不能在 `init` 中预生成，而由客户端通过 PBE 发送实际 payload：

```sql
INSERT INTO <S>.network_ingress
    (run_id,dist_key,seq,payload)
VALUES ($1,$2,$3,$4);
```

其中 `$2=mod(seq,1048576)+1`，`$4` 为客户端实际传输的 payload。恢复只删除当前 run：

```sql
DELETE FROM <S>.network_ingress WHERE run_id=$1;
```

206 的全局缓存场景需要动态对象，但对象定义仍是固定模板。`<N>` 只能是程序生成的
有界整数：

```sql
CREATE TABLE <S>.gcache_<RUN>_<N> (
    dist_key bigint NOT NULL,
    id bigint NOT NULL,
    value varchar(64),
    PRIMARY KEY (dist_key,id)
) @HASH(dist_key);

CREATE INDEX gcache_<RUN>_<N>_value_idx
ON <S>.gcache_<RUN>_<N> (value,dist_key,id);

INSERT INTO <S>.gcache_<RUN>_<N>
SELECT g,g,'value-' || g
FROM generate_series(1,<ROWS_PER_OBJECT>) AS g;
```

每个 CREATE 前先 journal 对应 DROP；恢复只删除当前 run 的对象。

场景级临时数据同样计入 `T`：206 的动态对象最多使用 `1% * T`，322 的未清理
payload 最多使用 `1% * T`，721 最多使用预留的 `2% * T`。三者还必须受
`profileCapGB` 和实时磁盘安全线共同约束，不能因为公共 `init` 已完成就绕过容量
保护。

### 5.6 实际容量和分布校验

集中式/openGauss 在每轮校准后读取：

```sql
SELECT c.relname,
       pg_relation_size(c.oid) AS heap_bytes,
       pg_indexes_size(c.oid) AS index_bytes
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname=$1
  AND c.relkind='r';
```

分布式 GaussDB 由能力适配器在各主 DN 执行同等查询并按逻辑表求和；不能只把当前
CN 的本地大小当作整个集群大小。若版本只提供实例级存储 API，则 provider 返回每个
DN 的对象大小并保留 `node_name`。无法取得全局实际大小时，`--dry-run` 可以继续，
真实 `init` 只允许使用保守估算并明确输出 `size_source=estimate`。

普通 HASH 表建数后执行分布检查：

```sql
SELECT xc_node_id, count(*) AS rows
FROM <S>.fact_sales
GROUP BY xc_node_id
ORDER BY xc_node_id;
```

除 `cluster_skew_data` 外，最大/最小 DN 行数差超过 10% 时，`init` 返回失败并提示
分布键异常。721 的专用表反向要求 95% 热 key 已形成明显 DN 倾斜。分布键还必须通过
目录或产品函数验证，例如：

```sql
SELECT getdistributekey('<S>.fact_sales');
```

预期为 `dist_key`；`dim_product`、`dim_store` 必须验证为 REPLICATION。

### 5.7 schema 和对象创建兼容规则

schema 创建继续采用：

1. 查询 `pg_catalog.pg_namespace`：

   ```sql
   SELECT 1
   FROM pg_catalog.pg_namespace
   WHERE nspname=$1;
   ```

2. 不存在时执行普通 `CREATE SCHEMA <S>`；
3. 并发创建遇到重复 schema 后重新查询。

不使用 `CREATE SCHEMA IF NOT EXISTS`。每张表和索引同样先查询系统目录；只有不存在
时才执行本节展开后的普通 `CREATE TABLE` 或 `CREATE INDEX`。已存在对象必须校验
列定义、分布策略和 dataset version，不能用 `IF NOT EXISTS` 掩盖结构不一致。

## 6. 场景生命周期和动作日志

每个场景使用统一生命周期：

```text
preflight -> prepare -> trigger -> ramp -> hold -> verify -> stop -> restore -> verify_restore
```

所有可持续状态必须在执行前写入 journal。现有只保存 SQL 的 journal 扩展为类型化
动作日志：

```text
SQL_MUTATION
SESSION_SET
SESSION_TRANSACTION
GUC_FILE_CHANGE
NETWORK_QDISC
NETWORK_FIREWALL
PROCESS_STATE
NODE_ROLE
CLOUD_FAULT_JOB
DATA_BASELINE
```

每条记录至少包含：

```text
run_id
scenario_code
action_kind
target_product
target_node
target_endpoint
original_state
forward_action
inverse_action
verify_action
state
created_at
updated_at
last_error
```

数据库中的 `<S>.meta_journal` 是权威记录；本地同时保存
`logs/gsbench_<RUN>_recovery.json`。网络隔离导致数据库暂时不可达时，恢复进程仍可
根据本地副本撤销外部动作。两份记录都不能保存密码、私钥或完整认证令牌。

## 7. 通用恢复命令

### 7.1 命令接口

恢复所有活动、停止中和存在未完成 journal 的场景：

```bash
gsbench restore
```

精确恢复一个 run：

```bash
gsbench restore --run-id <RUN_ID>
```

只检查恢复计划，不执行：

```bash
gsbench restore --dry-run
```

`gsbench stop --run-id <RUN_ID>` 停止负载后必须委托相同的恢复引擎，不能只杀工作
线程而留下参数、索引、网络规则或暂停的备库。

### 7.2 无参数恢复的选择规则

`gsbench restore` 默认发现并恢复：

1. `<S>.meta_runs` 中状态为 `running`、`stop_requested`、
   `restore_requested` 或 `restore_failed` 的 run；
2. `<S>.meta_journal` 中存在 `planned`、`applied`、`restoring`、
   `restore_failed` 动作的 run；
3. 本地恢复文件中存在未关闭的网络、进程、节点或云故障任务的 run。

多条 run 按开始时间倒序恢复，同一个 run 内按 journal ID 倒序执行逆向动作。

### 7.3 恢复顺序

统一恢复必须执行以下步骤：

1. 获得恢复互斥锁，防止两个恢复进程同时操作同一 run。
2. 将 run 标记为 `restore_requested`。
3. 关闭负载控制器，禁止创建新连接和新事务。
4. 使用精确的
   `application_name=gsbench/<RUN>/<SCENARIO>/<WORKER>` 标签取消并终止会话。
5. 等待事务回滚，确认 benchmark 锁已经释放。
6. 先撤销会阻断控制面的外部动作：
   - 删除防火墙隔离规则；
   - 删除 netem 延迟或丢包规则；
   - 恢复被暂停或停止的节点；
   - 结束云故障注入任务。
7. 按 journal 倒序恢复 GUC、索引、统计信息、DDL 和受控数据基线。
8. 重新探测拓扑；对已经发生切换的集群，以“所有节点健康、复制恢复”为第一目标，
   不强制在不安全条件下切回原主。
9. 执行场景级 `verify_restore`。
10. 全部通过后标记 `restored`；任一失败则保留 journal 并返回非零退出码。

### 7.4 快速恢复的最低验证

通用恢复成功至少要求：

- 不存在该 run 的活动会话；
- 不存在该 run 制造的等待锁；
- session-local 参数随连接关闭而消失；
- journal 中所有持久化动作均为 `restored`；
- benchmark 索引、列统计和扩展统计回到基线；
- 外部 provider 报告不存在该 run 的 netem、防火墙或节点故障任务；
- 主备复制恢复连接，WAL 差距继续收敛；
- 分布式 GaussDB 的 CN、DN、GTM 拓扑达到健康状态。

如果数据库仍不可连接，但网络和节点逆向动作已经完成，命令继续重试数据库验证到
`restore_timeout`，而不是立即宣称恢复成功。

## 8. 1xx CPU 场景

### 8.1 101 `tp_cpu`

**制造方法：** 多个 worker 持续执行短事务，混合索引点查、更新、插入和提交；反馈
控制器逐级增加并发，直到数据库 CPU 达到目标区间。

核心 SQL：

```sql
BEGIN;
SELECT balance
FROM <S>.accounts
WHERE id = $1;

UPDATE <S>.accounts
SET balance = balance + $2,
    updated_at = current_timestamp
WHERE id = $1;
COMMIT;
```

分布式 GaussDB 使用 `dist_key` 定位单个 DN，并按配置比例加入跨分片事务。成功证据
包括数据库 CPU、TPS、活跃 TP 会话和延迟；只有 worker 数量不构成成功。

**恢复：** 停止新事务，取消正在执行的 SQL，等待或回滚未提交事务。已提交的
benchmark 余额变化不需要反向回放。

### 8.2 102 `ap_cpu`

**制造方法：** 并发执行大表扫描、Hash Join、聚合和排序：

```sql
SELECT f.store_id,
       p.category_id,
       sum(f.amount),
       count(*)
FROM <S>.fact_sales f
JOIN <S>.dim_product p ON p.id = f.product_id
WHERE f.id BETWEEN $1 AND $2
GROUP BY f.store_id, p.category_id
ORDER BY sum(f.amount) DESC;
```

分布式策略使用均匀分布数据，首先验证计划覆盖多个 DN，避免意外退化为单 DN 查询。

**恢复：** 取消查询并关闭 tagged 会话，不产生持久化逆向动作。

### 8.3 103 `mixed_cpu`

**制造方法：** 同时运行 101 和 102 的 worker 组，默认 TP/AP 比例为 80/20；一个总
CPU 控制器调节并发，同时保持两组的配置比例。

**恢复：** 同时停止两组 worker，取消 SQL，等待 TP 事务结束或回滚。

## 9. 2xx 内存场景

内存场景必须同时报告进程总内存、动态执行内存、共享内存、会话内存以及适用时的
CN/DN 节点。集中式/openGauss 优先使用 `pv_total_memory_detail()`、
`gs_session_memory_detail` 等可用视图；分布式 GaussDB 优先使用
`DBE_PERF.GLOBAL_MEMORY_NODE_DETAIL`/`MEMORY_NODE_DETAIL` 和全局会话内存视图。

### 9.1 201 `memory_workmem_sort`

**制造方法：** 每个会话设置独立的 `work_mem`，对大结果集排序：

```sql
SET work_mem = '256MB';

SELECT id, customer_id, amount, payload
FROM <S>.fact_sales
WHERE id BETWEEN $1 AND $2
ORDER BY payload, amount DESC, id;
```

并发从 1 逐步增加。成功要求观察到执行内存增长；如果目标是内存排序，还要求计划或
运行证据表明排序未完全落盘。

**恢复：**

```sql
RESET work_mem;
```

随后关闭所有 tagged 会话。由于参数为 session-local，不修改实例参数。

### 9.2 202 `memory_workmem_hash`

**制造方法：** 运行大 Hash Join 和 Hash Aggregate：

```sql
SET work_mem = '256MB';

SELECT f1.product_id, sum(f1.amount), count(*)
FROM <S>.fact_sales f1
JOIN <S>.fact_sales f2
  ON f1.customer_id = f2.customer_id
WHERE mod(f1.id, 16) = 0
GROUP BY f1.product_id
ORDER BY sum(f1.amount) DESC;
```

成功证据为 Hash 算子、动态内存增长和多次连续目标采样。

**恢复：** 取消查询、`RESET work_mem` 并关闭会话。

### 9.3 203 `memory_sharedbuffer_churn`

**制造方法：** 选择明显大于当前 shared buffer 容量的 benchmark 数据范围，多个
worker 使用变化的范围和随机页反复扫描：

```sql
SELECT sum(amount), count(payload)
FROM <S>.fact_sales
WHERE id BETWEEN $1 AND $2;
```

控制器不断改变 `$1/$2`，使工作集不能稳定驻留。该场景不修改 `shared_buffers`。
成功证据为 buffer 命中率下降、物理读增加、buffer 映射或 I/O 等待增加。

**恢复：** 停止扫描。缓存自然回稳；不执行重启、清缓存或系统级 drop cache。

### 9.4 204 `memory_plancache_growth`

**制造方法：** 长连接中创建大量名称和 SQL 文本均唯一的预编译语句：

```sql
PREPARE gsbench_pc_<RUN>_<N>(bigint) AS
SELECT sum(amount)
FROM <S>.fact_sales
WHERE customer_id = $1
  AND id >= <N>;
```

openGauss/集中式读取 session plan cache；启用 GPC 时读取全局计划缓存；分布式
GaussDB 按节点读取 `GLOBAL_PLANCACHE_STATUS`。成功证据是
`CachedPlanSource`、计划条目数或 GPC 占用持续增长。

**恢复：**

```sql
DEALLOCATE ALL;
```

关闭会话后再次验证计划缓存占用下降。不会调用影响其他业务会话的全局清缓存动作。

### 9.5 205 `memory_session_context_growth`

**制造方法：** 在少量长连接中打开大量游标，使 Portal 和查询上下文持续存在：

```sql
BEGIN;
DECLARE gsbench_cur_<N> NO SCROLL CURSOR FOR
SELECT id, payload
FROM <S>.fact_sales
WHERE mod(id, 97) = <N>;
FETCH 1 FROM gsbench_cur_<N>;
```

每个会话按上限创建多个 cursor，但不提交事务。成功证据是目标 session 的内存上下文
增长，而不是进程总内存的偶然波动。

**恢复：**

```sql
CLOSE ALL;
ROLLBACK;
```

随后关闭会话并验证 session memory 记录消失。

### 9.6 206 `memory_global_cache_pressure`

**制造方法：** 在 benchmark schema 内分批创建并访问大量小表和索引，使 catalog/
global syscache 条目增长：

```sql
CREATE TABLE <S>.gcache_<RUN>_<N> (
    id bigint PRIMARY KEY,
    value varchar(64)
);

CREATE INDEX gcache_<RUN>_<N>_value_idx
ON <S>.gcache_<RUN>_<N> (value);

SELECT c.oid, a.attname
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid
WHERE c.oid = '<S>.gcache_<RUN>_<N>'::regclass;
```

每个 CREATE 在执行前记录对应 DROP。成功证据是全局系统缓存或元数据相关内存增长。

**恢复：**

```sql
DROP TABLE <S>.gcache_<RUN>_<N> CASCADE;
```

按创建顺序的逆序执行，只删除带当前 run 前缀且属于 `<S>` 的对象。

### 9.7 207 `memory_total_pressure`

**制造方法：** 同时组合 201、202、204 和 205 的内部 workload，但共享一个总内存
反馈控制器。并发只增加到以下最小限制：

```text
configured target percent
profileCapGB
max_process_memory safety margin
max workers
first allocation error
```

核心 SQL 沿用各子 workload。成功要求总内存或动态内存达到目标区间并持续多次采样。

**恢复：** 停止所有子 workload，先关闭 cursor/事务，再 `DEALLOCATE ALL`、
`RESET work_mem` 并关闭连接。

### 9.8 208 `memory_retention`

**制造方法：** 先用 204 和 205 让指定会话内存稳定增长，然后停止执行新 SQL，但保持
连接存活一个观察窗口。若内存没有随查询结束立即下降，即制造出“持续驻留”的泄漏
症状。该名称刻意不用 `memory_leak`，因为外部负载不能证明数据库内核存在真实泄漏。

**恢复：** `CLOSE ALL`、`DEALLOCATE ALL`、`ROLLBACK`、关闭会话；恢复验证要求目标
session 消失且节点内存开始回落。内存分配器未立即归还操作系统页面时，只能依据
数据库 memory context 的释放判断，不能错误宣称 RSS 必须瞬间回到原值。

### 9.9 209 `memory_oom_guarded`

**制造方法：** 在专用测试实例中，以较高 `work_mem` 和受控并发运行 201/202，逐步
接近数据库内存保护线：

```sql
SET work_mem = '512MB';
-- 执行 201/202 的排序或 Hash SQL
```

成功条件是数据库主动拒绝新的内存分配、返回可识别的内存不足错误，或者达到配置的
OOM 保护阈值。以下任一条件出现时立即停止 ramp：

- 数据库连接健康检查失败；
- 主机可用内存低于保留线；
- swap 或 OOM 风险超过限制；
- 达到 `profileCapGB`；
- 首个数据库内存分配错误。

该场景风险等级为 B，默认禁用。它不以触发操作系统 OOM Killer 为成功条件。

**恢复：** 立即取消重查询，关闭 workload 会话，重试控制连接；不重启数据库。若
数据库已经不可连接，外部 provider 只执行健康检查和告警，不擅自重启生产实例。

## 10. 3xx I/O 与网络场景

### 10.1 301 `io_sequential_read`

**制造方法：** 使用不断变化的大范围扫描绕开稳定缓存工作集：

```sql
SELECT sum(amount), avg(quantity), count(payload)
FROM <S>.fact_sales
WHERE id BETWEEN $1 AND $2;
```

成功证据为 `blks_read`、`data_io_time`、读吞吐或 I/O 等待增长。

**恢复：** 取消扫描并关闭会话。

### 10.2 302 `io_random_read`

**制造方法：** 多 worker 在主键索引上随机点查，目标集合大于有效缓存：

```sql
SELECT balance, payload
FROM <S>.accounts
WHERE id = $1;
```

成功证据为随机读 IOPS、buffer miss 和读延迟增长。

**恢复：** 停止点查，无持久动作。

### 10.3 303 `io_wal_write`

**制造方法：** 高频小事务反复更新不同记录并提交：

```sql
BEGIN;
UPDATE <S>.accounts
SET balance = balance + 0.01,
    updated_at = current_timestamp
WHERE id = $1;
COMMIT;
```

成功证据为 WAL 位置增长、WAL/数据写吞吐、提交等待和 TPS。

**恢复：** 停止新事务，等待或回滚未提交事务；已提交的 benchmark 数据保留。

### 10.4 304 `io_temp_spill`

**制造方法：** 将 session `work_mem` 降到足以稳定落盘的值，再执行排序和 Hash：

```sql
SET work_mem = '64kB';

SELECT customer_id, sum(amount)
FROM <S>.fact_sales
GROUP BY customer_id
ORDER BY sum(amount) DESC;
```

成功要求观测到临时文件/临时字节增长或计划中的 disk spill 证据。

**恢复：** 取消 SQL、`RESET work_mem`、关闭连接；临时文件由数据库清理。

### 10.5 305 `io_checkpoint_flush`

**制造方法：** 先批量更新 benchmark 数据制造脏页，再由具备权限的控制连接执行：

```sql
UPDATE <S>.vacuum_targets
SET version = version + 1,
    updated_at = current_timestamp
WHERE id BETWEEN $1 AND $2;

CHECKPOINT;
```

场景风险等级为 B。没有 `CHECKPOINT` 权限时可以观察自然 checkpoint，但结果只能为
`DEGRADED`，不能宣称执行了强制 checkpoint。

**恢复：** 停止更新并等待 checkpoint 结束；该动作不修改长期参数。

### 10.6 321 `network_client_egress`

**制造方法：** 从数据库向客户端返回大量不可高度压缩的数据，客户端完整读取结果：

```sql
SELECT id, customer_id, product_id, store_id, amount, payload
FROM <S>.fact_sales
WHERE id BETWEEN $1 AND $2;
```

成功证据为客户端实际接收字节、数据库发送吞吐和网络等待。只执行
`SELECT count(*)` 不算网络出流量场景。

**恢复：** 取消查询并关闭客户端连接。

### 10.7 322 `network_client_ingress`

**制造方法：** 客户端使用 PBE 批量发送带 payload 的 INSERT；不能用服务器内部
`INSERT ... SELECT` 代替网络入流量：

```sql
INSERT INTO <S>.network_ingress (
    run_id, seq, payload
) VALUES ($1, $2, $3);
```

数据按批提交。成功证据为客户端发送字节、数据库接收吞吐和提交速率。

**恢复：**

```sql
DELETE FROM <S>.network_ingress WHERE run_id = '<RUN>';
```

删除动作按批执行并受超时限制。

### 10.8 331 `network_cn_dn_stream`

**适用范围：** 仅分布式 GaussDB。

**制造方法：** 在 CN 上执行覆盖全部 DN 的聚合，并确保执行计划存在
`Streaming(type: GATHER)`：

```sql
SELECT store_id, sum(amount), count(*)
FROM <S>.fact_sales
GROUP BY store_id;
```

成功证据来自实际计划、`net_stream_send_info` 和节点级网络吞吐。

**恢复：** 取消 CN 查询，等待 DN 子查询退出。

### 10.9 332 `network_distributed_shuffle`

**适用范围：** 仅分布式 GaussDB。

**制造方法：** 两张表使用不同分布键，按非共置列关联，并用 GaussDB 支持的 stream
hint 保证生成 REDISTRIBUTE：

```sql
SELECT /*+ redistribute(f d) */
       f.store_id, count(*), sum(f.amount)
FROM <S>.fact_sales f
JOIN <S>.dist_join_data d
  ON f.customer_id = d.join_key
GROUP BY f.store_id;
```

运行前必须用 `EXPLAIN` 验证存在 `Streaming(type: REDISTRIBUTE)`；hint 未生效时场景
不得继续。成功证据为 redistribute 字节、时间和 DN 网络吞吐。

**恢复：** 取消查询，无持久化动作。

### 10.10 333 `network_distributed_broadcast`

**适用范围：** 仅分布式 GaussDB。

**制造方法：** 对受控小表强制 BROADCAST：

```sql
SELECT /*+ broadcast(d) */
       f.product_id, sum(f.amount)
FROM <S>.fact_sales f
JOIN <S>.dist_small_hash d
  ON f.product_id = d.product_id
GROUP BY f.product_id;
```

`dist_small_hash` 必须是 HASH 表而不是 REPLICATION 表。运行前验证计划包含
`Streaming(type: BROADCAST)`。

**恢复：** 取消查询，无持久化动作。

### 10.11 341 `network_latency_injection`

**风险等级：** C，仅允许自管专用测试环境或已批准的云故障 provider。

**制造方法：** 自管 Linux 使用精确 peer 过滤的 `tc netem`。示例命令：

```bash
tc qdisc add dev <IFACE> root handle 1: prio
tc qdisc add dev <IFACE> parent 1:3 handle 30: \
  netem delay 100ms 20ms distribution normal
tc filter add dev <IFACE> protocol ip parent 1:0 prio 3 u32 \
  match ip dst <PEER_IP>/32 \
  match ip dport <PEER_PORT> 0xffff \
  flowid 1:3
```

执行前如果接口已有非默认 root qdisc，provider 必须拒绝，不能覆盖用户规则。目标地址
和端口由 provider 的流量过滤器限制，不能对整台主机所有流量盲目加延迟。

恢复命令：

```bash
tc qdisc del dev <IFACE> root handle 1:
```

只有确认该 root qdisc 属于当前 run 时才能删除。

### 10.12 342 `network_packet_loss`

**风险等级：** C。

制造命令：

```bash
tc qdisc add dev <IFACE> root handle 1: prio
tc qdisc add dev <IFACE> parent 1:3 handle 30: \
  netem loss 5% 25%
tc filter add dev <IFACE> protocol ip parent 1:0 prio 3 u32 \
  match ip dst <PEER_IP>/32 \
  match ip dport <PEER_PORT> 0xffff \
  flowid 1:3
```

成功证据为 provider 规则存在、数据库网络重试/等待和可控的 SQL 延迟或错误。

恢复命令：

```bash
tc qdisc del dev <IFACE> root handle 1:
```

### 10.13 343 `network_partition`

**风险等级：** C。必须具有不经过被隔离链路的带外恢复通道。

**制造方法：** 自管 Linux 使用 run 专属 nftables 表，示例：

```bash
nft add table inet gsbench_<RUN>
nft 'add chain inet gsbench_<RUN> output { type filter hook output priority -10; policy accept; }'
nft 'add chain inet gsbench_<RUN> input  { type filter hook input  priority -10; policy accept; }'
nft add rule inet gsbench_<RUN> output ip daddr <PEER_IP> tcp dport <PEER_PORT> drop
nft add rule inet gsbench_<RUN> input  ip saddr <PEER_IP> tcp sport <PEER_PORT> drop
```

恢复命令：

```bash
nft delete table inet gsbench_<RUN>
```

托管 GaussDB 不执行主机命令，而由已配置的云故障 provider 创建和关闭故障任务。
provider 返回的 job ID 必须写入本地和数据库 journal。

## 11. 4xx 连接池和线程池场景

### 11.1 401 `connection_pool`

**制造方法：** 预先保留一个控制连接，再逐步建立三类 tagged 连接：

- idle；
- idle in transaction；
- active。

idle in transaction 示例：

```sql
BEGIN;
SELECT balance FROM <S>.accounts WHERE id = $1;
-- 保持事务，由客户端控制持续时间
```

active 连接执行有界查询：

```sql
SELECT count(*)
FROM <S>.fact_sales
WHERE mod(id, 97) = $1;
```

目标是达到 `max_connections` 的配置比例，而不是无条件把所有新连接全部拒绝。成功
证据为 tagged 会话数、连接上限、连接成功/拒绝数和控制连接仍可用。

**恢复：** 取消 active SQL，回滚 idle-in-transaction，关闭所有 workload 连接。

### 11.2 402 `thread_pool`

**制造方法：** `doctor` 先确认线程池已启用和工作线程数。场景建立多于可用工作线程
的 active session，并执行 bounded CPU SQL：

```sql
SELECT sum(f.amount)
FROM <S>.fact_sales f
JOIN <S>.dim_product p ON p.id = f.product_id
WHERE mod(f.id, 101) = $1;
```

成功证据为线程池 busy worker、等待 session 和排队时间。没有实际线程池时只能返回
`NOT_APPLICABLE` 或明确的 `DEGRADED`，不能把普通 backend 并发当作线程池故障。

**恢复：** 取消查询并关闭连接。默认不修改需要重启的线程池参数。

### 11.3 403 `connection_churn`

**制造方法：** worker 循环执行“建立连接—认证—`SELECT 1`—关闭连接”，且不使用
客户端连接池：

```sql
SELECT 1;
```

成功证据为每秒新建连接数、认证/建连延迟、连接错误和数据库会话创建开销。

**恢复：** 停止建立新连接并关闭尚未结束的连接。

### 11.4 404 `threadpool_queue`

**制造方法：** 在已启用线程池的环境中，建立明显多于线程数的 active session，
执行时间可控的 CPU 查询，使后续 session 进入线程池等待队列。

核心 SQL沿用 402。成功要求直接观察到等待队列或 busy/total worker 达到阈值，并且
存在 tagged 排队会话。

**恢复：** 取消 active SQL，排空等待队列，关闭连接。

### 11.5 405 `pooler_cn_dn_pressure`

**适用范围：** 分布式 GaussDB。

**制造方法：** 在一个或多个 CN 上建立大量并发 session，每个 session 执行覆盖多个
DN 的查询，使 CN 频繁向 pooler 请求 DN 连接：

```sql
SELECT f.store_id, p.category_id, sum(f.amount)
FROM <S>.fact_sales f
JOIN <S>.dim_product p ON p.id = f.product_id
GROUP BY f.store_id, p.category_id;
```

成功证据至少包含 `wait pooler get conn`、`get conn` 或对应版本的 pooler 指标；
只有 CN 客户端连接数升高不算成功。

**恢复：** 取消 CN 查询，关闭 tagged CN 会话，确认 DN 子会话和 pooler 等待消失。

## 12. 5xx 锁与并发场景

锁场景使用至少两个独立连接。所有事务由客户端保持，不使用无限 `pg_sleep()`。
客户端 context、query timeout 和总 duration 构成三层超时。恢复时优先取消 waiter，
再回滚 blocker。

openGauss 和集中式 GaussDB 使用 `pg_locks`、`pg_stat_activity` 和线程等待视图；
分布式 GaussDB 使用 `DBE_PERF.GLOBAL_LOCKS`、
`GLOBAL_THREAD_WAIT_STATUS` 等全局证据，并保留 `node_name`。

### 12.1 501 `lock_row_chain`

**制造方法：** 建立事务锁链。第一个事务锁住 row 1；第二个事务先锁 row 2，再等待
row 1；第三个事务先锁 row 3，再等待 row 2，以此类推：

```sql
-- blocker 1
BEGIN;
UPDATE <S>.lock_targets SET value = value + 1 WHERE id = 1;

-- blocker/waiter N
BEGIN;
UPDATE <S>.lock_targets SET value = value + 1 WHERE id = <N>;
UPDATE <S>.lock_targets SET value = value + 1 WHERE id = <N-1>;
```

可在链上增加 fan-out waiter：

```sql
BEGIN;
UPDATE <S>.lock_targets SET value = value + 1 WHERE id = <BLOCKED_ROW>;
```

成功证据为实际 blocker/waiter 图、链深、等待时间和锁对象。

**恢复：** 从叶子 waiter 向根 blocker 取消并 `ROLLBACK`，确认 tagged lock 为零。

### 12.2 502 `lock_table_exclusive`

**制造方法：**

```sql
-- session A
BEGIN;
LOCK TABLE <S>.lock_table_targets IN ACCESS EXCLUSIVE MODE;

-- session B
BEGIN;
SELECT count(*) FROM <S>.lock_table_targets;
```

session B 的 AccessShare 与 session A 的 AccessExclusive 冲突。

**恢复：** 取消 B，`ROLLBACK` B，再 `ROLLBACK` A。

### 12.3 503 `lock_ddl_wait`

该名称明确表示 **DML 持锁，DDL 等待**。

```sql
-- session A
BEGIN;
UPDATE <S>.lock_ddl_targets
SET value = value + 1
WHERE id = 1;

-- session B
ALTER TABLE <S>.lock_ddl_targets
ADD COLUMN ddl_<RUN> integer;
```

DDL 只有在全局锁证据确认处于等待状态后才算制造成功。

**恢复：** 先取消 B；若 DDL 已意外完成，则 journal 执行：

```sql
ALTER TABLE <S>.lock_ddl_targets
DROP COLUMN ddl_<RUN>;
```

最后 `ROLLBACK` A。

### 12.4 504 `lock_deadlock`

**制造方法：**

```sql
-- session A
BEGIN;
UPDATE <S>.lock_targets SET value = value + 1 WHERE id = 100;

-- session B
BEGIN;
UPDATE <S>.lock_targets SET value = value + 1 WHERE id = 101;

-- session A: 等待 B
UPDATE <S>.lock_targets SET value = value + 1 WHERE id = 101;

-- session B: 形成环
UPDATE <S>.lock_targets SET value = value + 1 WHERE id = 100;
```

成功条件是数据库直接返回 deadlock 错误/对应 SQLSTATE，并且等待图形成环；不能用
“存在 lock waiter”代替死锁证据。GaussDB 文档中的 `gs_fault_inject` 声明为不支持
调用，因此本场景不依赖该函数。

**恢复：** 数据库会中止一个 victim；客户端对两个事务都执行 `ROLLBACK` 并关闭
连接。

### 12.5 505 `lock_ddl_blocks_dml`

该场景与 503 方向相反：

**制造方法：**

```sql
-- session A
BEGIN;
ALTER TABLE <S>.lock_ddl_targets
ADD COLUMN ddl_<RUN> integer;
-- 不提交，使 DDL 的 AccessExclusive 保持

-- session B
UPDATE <S>.lock_ddl_targets
SET value = value + 1
WHERE id = 1;
```

**恢复：** 取消 B，回滚 B，再回滚 A。A 回滚后未提交的列自动消失；仍通过目录查询
验证列不存在。

### 12.6 506 `lock_select_blocks_ddl`

**制造方法：**

```sql
-- session A
BEGIN;
SELECT count(*) FROM <S>.lock_ddl_targets;

-- session B
ALTER TABLE <S>.lock_ddl_targets
ADD COLUMN ddl_<RUN> integer;
```

SELECT 的 AccessShare 保持到事务结束，阻塞需要 AccessExclusive 的 DDL。

**恢复：** 取消 B，回滚 B，再回滚 A；若列已创建则执行 journaled DROP。

### 12.7 507 `lock_vacuum_blocks_ddl`

**制造方法：** 先在大体量 `vacuum_targets` 中制造 dead tuples，启动普通 VACUUM；
确认 VACUUM 正在运行后立即发起 DDL：

```sql
VACUUM <S>.vacuum_targets;

ALTER TABLE <S>.vacuum_targets
ADD COLUMN ddl_<RUN> integer;
```

如果 VACUUM 在 DDL 发起前已经完成，则本轮不算成功，按上限重新制造更大的 vacuum
工作量。

**恢复：** 取消 DDL，等待或取消 VACUUM；若列已创建则删除。

### 12.8 508 `lock_ddl_blocks_vacuum`

**制造方法：**

```sql
-- session A
BEGIN;
LOCK TABLE <S>.vacuum_targets IN ACCESS EXCLUSIVE MODE;

-- session B
VACUUM <S>.vacuum_targets;
```

成功证据为 VACUUM 等待 relation lock。

**恢复：** 取消 VACUUM，再回滚 A。

### 12.9 509 `lock_createindex_blocks_dml`

**制造方法：** 对足够大的 benchmark 表执行普通 CREATE INDEX，并在其持有 Share 锁
期间发起 DML：

```sql
CREATE INDEX lock_ci_<RUN>_idx
ON <S>.vacuum_targets (version, id);

UPDATE <S>.vacuum_targets
SET version = version + 1
WHERE id = $1;
```

运行前不使用并发建索引语法。成功要求 DML 的 RowExclusive 等待 CREATE INDEX。

**恢复：** 取消 DML；等待或取消 CREATE INDEX。若索引已创建：

```sql
DROP INDEX <S>.lock_ci_<RUN>_idx;
```

### 12.10 510 `lock_dml_blocks_createindex`

**制造方法：**

```sql
-- session A
BEGIN;
UPDATE <S>.vacuum_targets
SET version = version + 1
WHERE id = 1;

-- session B
CREATE INDEX lock_ci_<RUN>_idx
ON <S>.vacuum_targets (version, id);
```

成功要求 CREATE INDEX 明确等待 session A。

**恢复：** 取消 B，回滚 A；若索引已创建则删除。

### 12.11 511 `lock_distributed_ddl_global`

**适用范围：** 分布式 GaussDB。

**制造方法：** 多个 CN 会话以受控速率对不同 benchmark 表执行 DDL，使其竞争全局
regular lock table：

```sql
CREATE INDEX ddl_global_<RUN>_<N>_idx
ON <S>.ddl_global_<N> (value);
```

成功证据为 `LockMgrLock`、全局 DDL blocker/waiter 和涉及节点。未观察到全局锁竞争
时即使 DDL 较慢也不能成功。

**恢复：**

```sql
DROP INDEX <S>.ddl_global_<RUN>_<N>_idx;
```

只删除 journal 中确认创建成功的索引。

### 12.12 512 `lock_distributed_txn_chain`

**适用范围：** 分布式 GaussDB。

**制造方法：** 每个事务依次更新位于不同分片的两行，后续事务反向依赖前一事务：

```sql
BEGIN;
UPDATE <S>.dist_lock_targets
SET value = value + 1
WHERE dist_key = $1 AND id = $2;

UPDATE <S>.dist_lock_targets
SET value = value + 1
WHERE dist_key = $3 AND id = $4;
```

数据准备阶段通过分布键映射确认两行位于不同 DN。成功证据必须合并 CN 和 DN 的全局
等待关系。

**恢复：** 从等待链叶子开始取消，回滚全部跨分片事务，并确认各 DN 不再存在 tagged
事务。

### 12.13 520～540 八级表锁冲突矩阵

八级表锁缩写：

```text
AS  = AccessShare
RS  = RowShare
RX  = RowExclusive
SUE = ShareUpdateExclusive
S   = Share
SRX = ShareRowExclusive
X   = Exclusive
AX  = AccessExclusive
```

所有矩阵场景使用同一个经过测试的两会话引擎：

```sql
-- holder
BEGIN;
LOCK TABLE <S>.lock_mode_targets IN <HOLDER_MODE> MODE;

-- waiter
BEGIN;
LOCK TABLE <S>.lock_mode_targets IN <WAITER_MODE> MODE;
```

| 编号 | 正式名称 | HOLDER | WAITER |
|---|---|---|---|
| 520 | `lockmode_accessshare_accessexclusive` | AS | AX |
| 521 | `lockmode_rowshare_exclusive` | RS | X |
| 522 | `lockmode_rowshare_accessexclusive` | RS | AX |
| 523 | `lockmode_rowexclusive_share` | RX | S |
| 524 | `lockmode_rowexclusive_sharerowexclusive` | RX | SRX |
| 525 | `lockmode_rowexclusive_exclusive` | RX | X |
| 526 | `lockmode_rowexclusive_accessexclusive` | RX | AX |
| 527 | `lockmode_shareupdateexclusive_self` | SUE | SUE |
| 528 | `lockmode_shareupdateexclusive_share` | SUE | S |
| 529 | `lockmode_shareupdateexclusive_sharerowexclusive` | SUE | SRX |
| 530 | `lockmode_shareupdateexclusive_exclusive` | SUE | X |
| 531 | `lockmode_shareupdateexclusive_accessexclusive` | SUE | AX |
| 532 | `lockmode_share_sharerowexclusive` | S | SRX |
| 533 | `lockmode_share_exclusive` | S | X |
| 534 | `lockmode_share_accessexclusive` | S | AX |
| 535 | `lockmode_sharerowexclusive_self` | SRX | SRX |
| 536 | `lockmode_sharerowexclusive_exclusive` | SRX | X |
| 537 | `lockmode_sharerowexclusive_accessexclusive` | SRX | AX |
| 538 | `lockmode_exclusive_self` | X | X |
| 539 | `lockmode_exclusive_accessexclusive` | X | AX |
| 540 | `lockmode_accessexclusive_self` | AX | AX |

实现时使用完整模式名称拼接固定 SQL，不接受配置中的自由文本模式。每个场景成功要求
waiter 的 `granted=false`、mode 与对象完全匹配。

**统一恢复：** 取消 waiter 并 `ROLLBACK`，然后 `ROLLBACK` holder。21 个场景均不
留下持久化对象变更。

不冲突的锁组合不占故障编号，但必须进入自动化对照测试；测试要求 waiter 可以立即
获得锁，防止冲突矩阵或产品适配被写反。

## 13. 6xx 执行计划场景

### 13.1 计划跳变的共同验证

601～606 的正式名称统一使用 `planchange` 前缀。每个场景使用固定 SQL，先取得
baseline，再执行一个独立原因的变更，最后重新取得计划和实际耗时。

成功至少要求：

1. 计划文本或稳定 plan identity 发生改变；
2. 目标访问路径/估算变化与场景原因一致；
3. 实际中位耗时达到 `minimum_slowdown`；
4. 结果集 fingerprint 与 baseline 相同；
5. 恢复后原计划能力和结构基线重新成立。

分布式 GaussDB 还要保存 CN 计划中的 Stream、DN 列表和 Remote SQL。

### 13.2 601 `planchange_stats_target`

基准 SQL：

```sql
SELECT count(*), sum(id)
FROM <S>.plan_data
WHERE stats_target_key = 1;
```

制造 SQL：

```sql
ALTER TABLE <S>.plan_data
ALTER COLUMN stats_target_key SET STATISTICS 1;

ANALYZE <S>.plan_data (stats_target_key);
```

执行前读取并记录原始 `attstattarget`。恢复 SQL：

```sql
ALTER TABLE <S>.plan_data
ALTER COLUMN stats_target_key SET STATISTICS <ORIGINAL_TARGET>;

ANALYZE <S>.plan_data (stats_target_key);
```

### 13.3 602 `planchange_index_unusable`

该场景合并旧编号 7 和 11。

基准 SQL：

```sql
SELECT count(*), sum(id)
FROM <S>.plan_data
WHERE index_unusable_key BETWEEN 100000 AND 150000;
```

制造 SQL：

```sql
ALTER INDEX <S>.plan_index_unusable_idx UNUSABLE;
```

恢复 SQL：

```sql
ALTER INDEX <S>.plan_index_unusable_idx REBUILD;
```

恢复验证要求 `indisusable`、`indisready`、`indisvalid` 符合该产品的正常索引状态，
并且 baseline SQL 重新使用目标索引。该语法必须通过能力探测；不能直接更新
`pg_index` 系统目录。

### 13.4 603 `planchange_stats_ndistinct`

基准 SQL：

```sql
SELECT count(*), sum(id)
FROM <S>.plan_data
WHERE stats_ndistinct_key = 424242;
```

制造 SQL：

```sql
ALTER TABLE <S>.plan_data
ALTER COLUMN stats_ndistinct_key SET STATISTICS 1;

ALTER TABLE <S>.plan_data
ALTER COLUMN stats_ndistinct_key SET (n_distinct = 1);

ANALYZE <S>.plan_data (stats_ndistinct_key);
```

恢复 SQL：

```sql
ALTER TABLE <S>.plan_data
ALTER COLUMN stats_ndistinct_key RESET (n_distinct);

ALTER TABLE <S>.plan_data
ALTER COLUMN stats_ndistinct_key SET STATISTICS <ORIGINAL_TARGET>;

ANALYZE <S>.plan_data (stats_ndistinct_key);
```

### 13.5 604 `planchange_stats_extended`

基准 SQL：

```sql
SELECT count(*), sum(id)
FROM <S>.plan_data
WHERE stats_corr_a BETWEEN 100 AND 419
  AND stats_corr_b BETWEEN 100 AND 419;
```

制造 SQL：

```sql
ALTER TABLE <S>.plan_data
DELETE STATISTICS ((stats_corr_a, stats_corr_b));

ANALYZE <S>.plan_data (stats_corr_a, stats_corr_b);
```

恢复 SQL：

```sql
ALTER TABLE <S>.plan_data
ADD STATISTICS ((stats_corr_a, stats_corr_b));

ANALYZE <S>.plan_data ((stats_corr_a, stats_corr_b));
```

目录名称和字段在 openGauss/GaussDB 版本间存在差异时，由能力适配器选择验证 SQL；
触发语义不变。

### 13.6 605 `planchange_index_drop`

基准 SQL：

```sql
SELECT count(*), sum(id)
FROM <S>.plan_data
WHERE index_drop_key BETWEEN 100000 AND 150000;
```

制造 SQL：

```sql
DROP INDEX <S>.plan_index_drop_idx;
```

恢复 SQL：

```sql
CREATE INDEX plan_index_drop_idx
ON <S>.plan_data (index_drop_key);
```

创建索引的完整定义在 mutation 前从受信基准定义取得并写 journal，不从错误信息或
用户输入重建。

### 13.7 606 `planchange_index_shape`

基准 SQL：

```sql
SELECT count(*), sum(id)
FROM <S>.plan_data
WHERE index_shape_lead = 42
  AND index_shape_tail BETWEEN 100000 AND 500000;
```

制造 SQL：

```sql
DROP INDEX <S>.plan_index_shape_good_idx;

CREATE INDEX plan_index_shape_bad_idx
ON <S>.plan_data (index_shape_tail, index_shape_lead);
```

恢复 SQL：

```sql
DROP INDEX <S>.plan_index_shape_bad_idx;

CREATE INDEX plan_index_shape_good_idx
ON <S>.plan_data (index_shape_lead, index_shape_tail);
```

### 13.8 硬解析场景的共同证据

621～626 使用 `hardparse` 前缀。主要证据为：

- `n_hard_parse`；
- `n_soft_parse`；
- `parse_time`；
- `plan_time`；
- tagged SQL 的调用数；
- 分布式环境中的 `node_name`、CN/DN 和 GPC 状态。

GaussDB 分布式 `STATEMENT_HISTORY` 可能要求在 `postgres` 数据库查询且只展示当前
节点，因此 observer 可以使用与 workload 分离的只读连接。未探测到硬解析计数列时，
CPU 或耗时升高只能产生 `DEGRADED`，不能产生 `SUCCESS`。

### 13.9 621 `hardparse_literal_flood`

**原因：** SQL 常量不断变化，数据库看到大量不同 SQL 文本。

制造 SQL 模板：

```sql
SELECT /* gsbench:<RUN>:621:<N> */
       sum(amount)
FROM <S>.fact_sales
WHERE product_id = <LITERAL_N>
  AND id >= <UNIQUE_LITERAL_N>;
```

所有 literal 由程序生成的有界整数构成，不接受用户字符串。该场景使用非 PBE 路径。

**恢复：** 停止发送 SQL，关闭会话；无持久化变更。

### 13.10 622 `hardparse_unprepared`

**原因：** 相同结构的 SQL 每次通过 simple query 路径发送，不使用 PREPARE。

**制造方法：**

```sql
SELECT sum(amount)
FROM <S>.fact_sales
WHERE customer_id = 424242;
```

worker 重复发送同一文本。成功要求硬解析调用随执行次数增长，并与使用 PREPARE 的
内部对照组形成明显差异。

**恢复：** 停止 SQL，关闭连接。

### 13.11 623 `hardparse_force_custom`

**原因：** Prepared Statement 强制每次生成 Custom Plan。

**制造方法：**

```sql
SET plan_cache_mode = 'force_custom_plan';

PREPARE gsbench_hp_<RUN>(bigint) AS
SELECT /*+ use_cplan */
       sum(amount)
FROM <S>.fact_sales
WHERE customer_id = $1;

EXECUTE gsbench_hp_<RUN>(424242);
```

`use_cplan` 只在能力探测确认支持时加入；否则只使用 `plan_cache_mode`。成功要求
`n_hard_parse` 和 plan time 随 EXECUTE 增长。

恢复 SQL：

```sql
DEALLOCATE ALL;
RESET plan_cache_mode;
```

### 13.12 624 `hardparse_session_churn`

**原因：** 计划只存在于短生命周期会话中，频繁重新建连导致无法复用。

**制造方法：** 每个循环执行：

```sql
PREPARE gsbench_hp(bigint) AS
SELECT sum(amount)
FROM <S>.fact_sales
WHERE customer_id = $1;

EXECUTE gsbench_hp(424242);
```

然后立即关闭连接并重新连接。成功证据为硬解析、每秒新建连接和 parse time 同时
增长。

**恢复：** 停止新建连接并关闭现存连接。

### 13.13 625 `hardparse_ddl_invalidation`

**原因：** benchmark 对象 DDL 使已缓存计划失效。

**制造方法：** 先在多个长连接中 PREPARE：

```sql
PREPARE gsbench_hp_inv(bigint) AS
SELECT sum(amount)
FROM <S>.hardparse_targets
WHERE lookup_key = $1;
```

控制连接交替执行受 journal 保护的索引 DDL：

```sql
CREATE INDEX hardparse_inv_<RUN>_idx
ON <S>.hardparse_targets (lookup_key, id);

DROP INDEX <S>.hardparse_inv_<RUN>_idx;
```

每次 DDL 后重新 EXECUTE prepared statement。成功证据为 invalidation 后硬解析和
plan time 增长。

**恢复：** 停止 DDL 循环，按 journal 删除仍存在的临时索引，`DEALLOCATE ALL`，
验证基准索引完整。

### 13.14 626 `hardparse_gpc_bypass`

**前置条件：** `enable_global_plancache=on`，PBE 和 GPC 状态视图可用。

制造 SQL：

```sql
PREPARE gsbench_hp_gpc(bigint) AS
SELECT /*+ no_gpc */
       sum(amount)
FROM <S>.fact_sales
WHERE customer_id = $1;

EXECUTE gsbench_hp_gpc(424242);
```

多 session 重复 PREPARE/EXECUTE。成功要求目标 SQL 不进入
`GLOBAL_PLANCACHE_STATUS`，同时硬解析或会话级计划构建明显高于无 `no_gpc` 的内部
对照组。

**恢复：**

```sql
DEALLOCATE ALL;
```

hint 不修改全局 GPC 参数。

## 14. 7xx 主备和集群架构场景

### 14.1 主备证据适配

openGauss 和集中式 GaussDB 优先使用 `pg_stat_get_wal_senders()`、
wal receiver 和复制状态函数。分布式 GaussDB 使用
`pgxc_stat_get_wal_senders()` 汇总各主 DN 与备 DN 的发送、接收、flush 和 replay
位置。

每条复制证据至少包含：

```text
node/shard
local_role
peer_role
sync_state
sent_lsn
write_lsn
flush_lsn
replay_lsn
lag_bytes
sample_time
```

### 14.2 701 `replication_wal_pressure`

**制造方法：** 主库/主 DN 上并发执行高频更新和提交：

```sql
BEGIN;
UPDATE <S>.replication_targets
SET version = version + 1,
    payload = $2
WHERE id = $1;
COMMIT;
```

分布式环境按分片均匀选择 dist key。成功证据为 WAL 生成率、备库 lag 和 replay
追赶速率；没有备库时返回 `NOT_APPLICABLE`。

**恢复：** 停止写入，等待 lag 在 `restore_timeout` 内持续收敛到恢复阈值。

### 14.3 702 `replication_sync_commit_block`

该场景对应“主库提交太频繁，备库来不及同步，导致主库提交堵塞”。

workload session：

```sql
SET synchronous_commit = 'remote_apply';

BEGIN;
UPDATE <S>.replication_targets
SET version = version + 1
WHERE id = $1;
COMMIT;
```

大量 worker 高频提交。只有在 synchronous standby 存在时运行。成功证据为：

- commit latency 明显上升；
- 同步复制/remote apply 等待；
- sent、flush、replay 位置差；
- tagged 提交会话处于对应等待。

**恢复：**

```sql
RESET synchronous_commit;
```

停止写入并等待复制追平。该参数为 session-local，不修改全局配置。

### 14.4 703 `replication_replay_delay`

**风险等级：** B，需要可管理的 standby。

**制造方法：** 自管 openGauss 策略在备库记录原值后执行：

```bash
gs_guc set -D <STANDBY_DATA_DIR> \
  -c "recovery_min_apply_delay=5000"
gs_ctl reload -D <STANDBY_DATA_DIR>
```

然后在主库持续运行 701 的写入 SQL。也可以在明确支持且有独立 standby 连接时使用
短时 replay pause/resume 策略，但暂停时间必须有硬上限。

恢复命令：

```bash
gs_guc set -D <STANDBY_DATA_DIR> \
  -c "recovery_min_apply_delay=<ORIGINAL_VALUE>"
gs_ctl reload -D <STANDBY_DATA_DIR>
```

如果使用 replay pause，恢复动作必须首先调用对应 replay resume 函数。托管 GaussDB
通过 approved provider 完成同等动作；没有管理接口时为 `NOT_APPLICABLE`，不能在
CN 上伪造备库延迟。

### 14.5 704 `replication_standby_read_conflict`

**制造方法：**

1. 在 standby 开启长只读事务并扫描 `replication_conflict_targets`；
2. primary 删除该表的一批记录并执行 VACUUM；
3. 观察 standby replay 与旧 snapshot 的冲突。

standby SQL：

```sql
BEGIN TRANSACTION READ ONLY;
SELECT count(*), sum(id)
FROM <S>.replication_conflict_targets;
-- 保持 snapshot
```

primary SQL：

```sql
DELETE FROM <S>.replication_conflict_targets
WHERE run_id = '<RUN>';

VACUUM <S>.replication_conflict_targets;
```

成功证据为 standby conflict/cancel、replay 等待和对应 tagged query。

**恢复：** 取消 standby 查询并 `ROLLBACK`；停止 primary 变更，等待 replay 追平。
测试数据由下一次 `init`/baseline repair 补齐。

### 14.6 705 `replication_network_delay`

**风险等级：** C。

**制造方法：** 只对 primary/standby replication 的精确 IP/port 注入延迟，命令
沿用 341：

`gsbench` 内部 provider 执行 341 所列的 `prio + netem + u32 filter` 三条 `tc`
命令，将 `<PEER_IP>:<PEER_PORT>` 设置为复制 peer，并将 netem 参数改为
`delay 200ms 50ms distribution normal`。

成功证据必须显示复制 lag 和 commit/replay 延迟，而不只是 tc 规则存在。

**恢复：**

执行 341 所列的 `tc qdisc del dev <IFACE> root handle 1:`。

随后验证复制重新连接并追赶。

### 14.7 706 `replication_network_partition`

**风险等级：** C。

**制造方法：** 使用 343 的 run 专属 nftables 规则，但只匹配复制 peer 和复制端口。分布式
GaussDB 按一个明确 shard 的主备副本执行，禁止一次隔离全部分片。

恢复命令：

```bash
nft delete table inet gsbench_<RUN>
```

恢复验证要求复制状态重新变为正常且 lag 收敛。

### 14.8 721 `cluster_data_skew`

**适用范围：** 分布式 GaussDB。

表定义使用第 5.4 节的统一 DDL：主键为 `(skew_key,id)`，分布策略为
`DISTRIBUTE BY HASH (skew_key)`。

制造 95% 单 key 数据：

```sql
INSERT INTO <S>.cluster_skew_data (skew_key,id,payload)
SELECT CASE WHEN mod(g, 100) < 95 THEN 1 ELSE g END,
       g,
       repeat('s', 128)
FROM generate_series($1, $2) AS g;
```

制造查询：

```sql
SELECT skew_key, count(*), sum(length(payload))
FROM <S>.cluster_skew_data
GROUP BY skew_key;
```

成功证据为各 DN tuple 数、执行时间或网络量的最大/最小比达到阈值。恢复 SQL：

```sql
TRUNCATE TABLE <S>.cluster_skew_data;
```

### 14.9 722 `cluster_cn_hotspot`

**适用范围：** 分布式 GaussDB。

**制造方法：** 连接拓扑中选择一个 CN endpoint，将全部 workload 连接固定到该 CN，
执行 101/102 的公共 SQL；其他 CN 不承载 gsbench workload。

该场景没有特殊 SQL，关键触发命令是连接路由：

```text
target_cn=<CN_NAME>
all worker DSNs -> <CN_NAME>
```

成功证据为目标 CN 的 tagged session、CPU/队列显著高于其他 CN。

**恢复：** 停止连接并关闭目标 CN 上的 tagged session；不修改集群路由配置。

### 14.10 723 `cluster_dn_hotspot`

**适用范围：** 分布式 GaussDB。

**制造方法：** 先通过分布键映射选择落在同一 DN 的 key，再高并发查询/更新这些 key：

```sql
UPDATE <S>.fact_sales
SET amount = amount + 0.01
WHERE dist_key = $1
  AND id = $2;
```

成功证据为目标 DN 的 tagged workload、CPU/I/O/等待明显高于其他 DN。

**恢复：** 停止 SQL，等待或回滚未提交事务。

### 14.11 724 `cluster_cross_shard_txn`

**适用范围：** 分布式 GaussDB。

**制造方法：** 每个事务更新两个已确认位于不同 shard 的 dist key：

```sql
BEGIN;
UPDATE <S>.dist_txn_targets
SET value = value + 1
WHERE dist_key = $1 AND id = $2;

UPDATE <S>.dist_txn_targets
SET value = value + 1
WHERE dist_key = $3 AND id = $4;
COMMIT;
```

成功证据为跨节点事务、提交延迟和涉及 DN 数。恢复时停止新事务并回滚未提交事务。

### 14.12 725 `cluster_gtm_pressure`

**适用范围：** 使用 GTM 的分布式 GaussDB。

**制造方法：** 高并发执行极短的跨分片事务，使全局 XID/snapshot 请求速率增加。核心
SQL沿用 724，但每次只更新极少记录并立即提交。

成功证据必须来自 GTM/全局事务等待、snapshot/XID 速率或可用的 GTM 指标。只有 TPS
高而无 GTM 证据时返回 `DEGRADED`。

**恢复：** 停止 worker，回滚未完成的全局事务，确认没有 tagged in-doubt transaction。

### 14.13 726 `cluster_partial_node_slow`

**风险等级：** C，适用于分布式 GaussDB。

**制造方法：** 优先使用 approved provider 只降低一个 CN/DN 与其 peer 间的网络质量，示例命令沿用
341，并精确过滤目标节点流量：

`gsbench` 内部 provider 执行 341 所列的 `prio + netem + u32 filter` 命令，目标设置为
指定 CN/DN 的 peer，netem 参数为 `delay 150ms 30ms`。

不使用未验证的任意进程 `kill -STOP` 或 cgroup 命令。成功证据为指定节点变慢、全局
查询被最慢节点拖延以及节点级 wait/network 指标。

**恢复：**

执行 341 所列的 `tc qdisc del dev <IFACE> root handle 1:`。

### 14.14 727 `cluster_node_failure`

**风险等级：** C，只允许专用演练集群。

**制造方法：** 自管 openGauss 节点示例：

```bash
gs_ctl stop -D <TARGET_DATA_DIR> -m fast
```

恢复示例：

```bash
gs_ctl start -D <TARGET_DATA_DIR>
```

目标默认只能是存在健康副本的 standby 或经过演练审批的节点。托管 GaussDB 使用节点
管理 API/provider；实现不能猜测或拼接未验证的 `cm_ctl` 命令。

成功证据为目标节点 Down、集群仲裁/服务连续性和故障告警。恢复要求节点重新加入、
复制追平且集群健康。

### 14.15 728 `cluster_switchover`

**风险等级：** C。

**制造方法：** 自管 openGauss 在主备同步且角色正常时执行计划内切换：

```bash
gs_ctl switchover -D <STANDBY_DATA_DIR>
```

托管 GaussDB 使用 approved switchover provider。执行前 journal 保存原主、原备、
LSN 和拓扑。

恢复不是无条件“切回原主”。`gsbench restore` 首先确保新主可写、所有副本健康和
复制追平；只有配置 `restore_original_role=true`、原节点健康且再次计划内切换安全时，
才执行反向 switchover。否则报告“健康但角色已变化”，不标记为故障遗留。

## 15. 8xx 维护操作场景

### 15.1 801 `vacuum_pressure`

先制造 dead tuples：

```sql
UPDATE <S>.vacuum_targets
SET version = version + 1,
    payload = payload || 'x',
    updated_at = current_timestamp
WHERE id BETWEEN $1 AND $2;

DELETE FROM <S>.vacuum_targets
WHERE id BETWEEN $3 AND $4;
```

然后按配置运行一种维护 SQL：

```sql
VACUUM <S>.vacuum_targets;
```

或：

```sql
VACUUM ANALYZE <S>.vacuum_targets;
```

`VACUUM FULL` 需要 AccessExclusive，只在显式危险开关开启时运行：

```sql
VACUUM FULL <S>.vacuum_targets;
```

成功要求同时观测维护活动和前台业务吞吐/延迟变化。只有 dead tuple 增加不等于
vacuum pressure 成功。

**恢复：** 取消前台和 VACUUM worker。普通 VACUUM 已完成时无需逆向；若场景被取消，
恢复阶段执行一次有界：

```sql
VACUUM ANALYZE <S>.vacuum_targets;
```

最后由 baseline repair 补齐被删除的 benchmark 数据。

## 16. 配置设计

配置继续以 `configs/gsbench.cfg` 为默认路径。新配置按场景正式名称分节；同一组内部
引擎可以有公共默认值。

```ini
[database]
host = 127.0.0.1
port = 5433
database = postgres
user = bench
password_env = GSBENCH_PASSWORD
sslmode = disable

[topology]
mode = auto
observer_database = postgres

[run]
scenarios = 101
duration = 10m
ramp_interval = 2s

[data]
schema = gsbench
max_size_gb = 2048
min_free_disk_percent = 20
reuse_existing = true

[safety]
max_workers = 256
max_connections = 500
query_timeout = 30s
restore_timeout = 10m
profile_cap_gb = 256
allow_admin_mutation = false
allow_infrastructure_fault = false
restore_on_exit = true
restore_original_role = false

[fault_provider]
type = none
# none | local | ssh | gaussdb_api

[scenario.memory_workmem_sort]
target_percent = 80
work_mem = 256MB

[scenario.memory_oom_guarded]
enabled = false
minimum_free_memory_gb = 8

[scenario.hardparse]
workers = 16
minimum_hard_parse_ratio = 0.80

[scenario.hardparse_force_custom]
iterations_per_worker = 1000

[scenario.lock]
wait_timeout = 30s

[scenario.replication_sync_commit_block]
workers = 64
commit_batch = 1

[scenario.network_latency_injection]
enabled = false
interface = eth1
peer = 10.0.0.12:5433
delay = 100ms
jitter = 20ms
```

远程节点、云 API 和 SSH 凭据只允许引用环境变量或系统 credential provider。配置文件
不保存明文密码或私钥。

### 16.1 配置和 CLI 优先级

```text
CLI override
  > exact scenario section
  > scenario group section
  > safety/run/data global defaults
  > built-in safe default
```

`max_size_gb=2048` 表示数据集容量上限 2 TiB。`profile_cap_gb` 是一次故障负载允许
占用或触达的资源保护线，不等于数据集大小，也不允许超过实例和主机安全探测得到的
上限。

## 17. 风险等级和运行许可

| 等级 | 定义 | 默认 |
|---|---|---|
| A | 仅使用 benchmark 对象、普通 SQL 和 session-local 参数 | 允许 |
| B | 数据库管理动作、GUC 文件、checkpoint、备库回放控制、受控 OOM | 禁止 |
| C | tc/nft、节点停止、网络隔离、切换、云故障任务 | 禁止 |

B 级要求：

```ini
[safety]
allow_admin_mutation = true
```

C 级还要求：

```ini
[safety]
allow_infrastructure_fault = true

[fault_provider]
type = local
```

并在 CLI 显式确认：

```bash
gsbench run 705 --allow-risk C
```

配置开关和 CLI 缺一不可。`--allow-risk` 只允许 A、B、C 枚举值，不能使用模糊的
`--unsafe` 一次放开全部限制。

任何 B/C 场景在 trigger 前必须通过一次恢复演练：

1. provider 能读取当前状态；
2. inverse action 能 dry-run；
3. 存在控制连接或带外恢复通道；
4. 恢复目标不依赖即将被隔离的唯一链路；
5. journal 的数据库和本地副本均已落盘。

## 18. 兼容矩阵原则

| 场景范围 | openGauss | 集中式 GaussDB | 分布式 GaussDB |
|---|---|---|---|
| 101～103 | 支持 | 支持 | 支持，使用分布式表策略 |
| 201～209 | 支持/能力探测 | 支持/能力探测 | 支持，按 CN/DN 采集 |
| 301～305 | 支持 | 支持 | 支持，按节点汇总 |
| 321～322 | 支持 | 支持 | 支持 |
| 331～333 | 不适用 | 不适用 | 支持 |
| 341～343 | provider 可用时 | provider 可用时 | provider 可用时 |
| 401～404 | 支持/线程池条件 | 支持/线程池条件 | 支持/线程池条件 |
| 405 | 不适用 | 不适用 | 支持 |
| 501～510、520～540 | 支持 | 支持 | 支持，全局证据 |
| 511～512 | 不适用 | 不适用 | 支持 |
| 601～606 | 能力探测 | 能力探测 | 能力探测和分布式计划 |
| 621～625 | 支持 | 支持 | 支持 |
| 626 | GPC 开启时 | GPC 开启时 | GPC 开启时 |
| 701～704 | 主备环境 | 主备环境 | 按 DN shard 主备 |
| 705～706 | provider 可用时 | provider 可用时 | provider 可用时 |
| 721～728 | 不适用 | 不适用 | 支持/provider 条件 |
| 801 | 支持 | 支持 | 支持 |

这里的“支持”表示代码存在对应策略，但发布验收仍必须在真实目标产品和版本执行。
能力依赖型场景在不满足前置条件时返回明确状态，不通过危险的系统目录更新或伪造视图
来绕过限制。

## 19. 成功证据和输出

每个场景输出一个稳定的 evidence envelope：

```json
{
  "run_id": "20260725-...",
  "scenario_code": 702,
  "scenario": "replication_sync_commit_block",
  "product": "GaussDB",
  "topology": "distributed",
  "strategy": "distributed_dn_remote_apply",
  "risk": "A",
  "outcome": "SUCCESS",
  "targets": [
    {"node": "dn_6001", "role": "primary", "shard": "shard_1"}
  ],
  "metrics": {
    "commit_p95_ms": 420,
    "replay_lag_bytes": 67108864,
    "sync_waiters": 32
  },
  "restore": {
    "state": "restored",
    "remaining_sessions": 0,
    "pending_actions": 0
  }
}
```

日志必须记录执行的内部 SQL 模板标识和安全处理后的命令。密码、token、私钥、完整
DSN、任意业务数据 payload 不写入日志。

## 20. 测试策略

实现采用测试先行。

### 20.1 单元测试

- 三位编号唯一、名称唯一、分类与第一位一致；
- 旧数字 1～15 不再解析；
- `planchange_*`、`hardparse_*` 正式名称稳定；
- 21 个八级表锁冲突组合与官方矩阵一致；
- 所有非冲突组合进入负面对照；
- 每个场景都有 trigger、evidence 和 restore contract；
- 每个持久动作严格遵守 write-journal-before-mutate；
- 恢复按 run 倒序、action 倒序；
- session 标签只匹配精确 run；
- B/C 场景双重授权；
- 外部 provider 只能操作白名单节点、接口、IP、端口和对象；
- 任一 trigger 失败仍进入 stop/restore；
- restore 可重复执行，第二次执行保持幂等。

### 20.2 SQL 兼容测试

为三类产品维护独立 fixture：

```text
openGauss
GaussDB centralized
GaussDB distributed
```

fixture 覆盖：

- catalog/view 存在性和列差异；
- CREATE/ALTER/ANALYZE/LOCK 语法；
- `plan_cache_mode`、GPC 和 hint；
- 内存视图；
- WAL sender/receiver；
- global locks/thread wait；
- distributed table 和 stream plan；
- 错误码与 deadlock/OOM 分类。

SQL fixture 不能代替真库验收。

### 20.3 真库验收

至少准备：

1. openGauss 单机；
2. openGauss 主备；
3. 集中式 GaussDB 主备；
4. 分布式 GaussDB，至少两个 CN、多个 DN shard 及副本。

通用场景必须在所有适用环境各运行一次。分布式专属场景必须保留带 node/shard 的
证据。B/C 场景只在隔离的演练环境验收。

每次验收结束强制执行：

```bash
gsbench restore
gsbench status
```

并证明：

- tagged session 为零；
- pending journal 为零；
- benchmark 结构基线正确；
- 无遗留 tc/nft 规则；
- 无暂停或停止的测试节点；
- 主备复制正常；
- 集群拓扑健康。

### 20.4 构建和质量门禁

```bash
go test ./...
go test -race ./...
go vet ./...
```

同时生成 Darwin/arm64、Linux/arm64 和 Linux/amd64 构建。涉及外部 provider 的测试
使用 network namespace 或隔离容器，不允许在开发主机默认网卡上执行真实隔离。

## 21. 实现分期

### 第一阶段：目录、兼容层和通用恢复

- 三位编号和 central catalog；
- product/topology/capability detector；
- journal 类型扩展；
- `gsbench restore` 通用恢复；
- 数据集分布策略；
- 所有场景的 dry-run；
- openGauss、集中式、分布式 SQL fixture。

第一阶段结束时，即使新场景尚未全部触发，恢复框架也必须先完成。

### 第二阶段：安全 SQL 场景

- 1xx；
- 201～208；
- 301～305、321～333；
- 401～405；
- 501～512、520～540；
- 601～606、621～626；
- 701、702、704、721～725；
- 801。

### 第三阶段：管理员和基础设施故障

- 209；
- 341～343；
- 703、705、706；
- 726～728；
- local/SSH/GaussDB API provider；
- 带外恢复和真实故障演练。

第三阶段不得阻塞第一、二阶段的发布，但未实现的场景必须显示
`NOT_IMPLEMENTED`，不能出现在“已支持场景”清单中。

## 22. 与现有实现的衔接

当前代码已经具备：

- `gsbench restore [--run-id]` 命令入口；
- `<S>.meta_runs`；
- `<S>.meta_journal`；
- SQL inverse action；
- tagged session 终止；
- plan baseline repair。

本次重构复用这些基础，但需要修正以下限制：

1. 当前 restore 对 `plan_data` 存在性有特殊依赖，重构后改为通用 metadata 能力检查。
2. 当前 journal 只表达 SQL，无法恢复网络、进程、节点和云故障任务。
3. 当前 `stop` 和 `restore` 的选择逻辑需要统一到同一个恢复引擎。
4. 当前场景注册表散落在 CLI、配置和 runner 中，需要改为单一 catalog。
5. 当前 `lock_storm` 需要删除并拆成独立编号。
6. 当前 `dynamic_memory` 需要拆成 201～209。
7. 当前 `caps.Supported` 拒绝分布式拓扑，需要改为产品+拓扑+场景能力判断。
8. 当前 1～15 数字 alias 和帮助文本需要替换为三位编号。

## 23. 参考依据

- [openGauss LOCK 语法和八级表锁冲突矩阵](https://docs.opengauss.org/en/docs/5.1.0/docs/SQLReference/lock.html)
- [openGauss 全局计划缓存](https://docs.opengauss.org/zh/docs/latest/characteristic_description/global_plancache.html)
- [openGauss STATEMENT_HISTORY](https://docs.opengauss.org/en/docs/2.0.1/docs/Developerguide/statement_history.html)
- [openGauss work_mem 和内存参数](https://docs.opengauss.org/en/docs/7.0.0-RC1/docs/DatabaseReference/memory.html)
- [openGauss delayed replay](https://docs.opengauss.org/en/docs/5.1.0-lite/docs/AboutopenGauss/delayed-replay.html)
- [openGauss gs_ctl](https://docs.opengauss.org/en/docs/latest/tool_and_commandreference/gs_ctl.html)
- [GaussDB 分布式架构和组件](https://support.huaweicloud.com/productdesc-gaussdb/gaussdb_01_003.html)
- [GaussDB 分布式 CREATE TABLE 和 DISTRIBUTE BY](https://support.huaweicloud.com/intl/en-us/distributed-devg-v8-gaussdb/gaussdb-12-0567.html)
- [GaussDB 分布列选择最佳实践](https://support.huaweicloud.com/eu/distributed-devg-v3-gaussdb/gaussdb-12-0657.html)
- [GaussDB 数据库对象大小函数](https://support.huaweicloud.com/intl/en-us/distributed-devg-v8-gaussdb/gaussdb-12-2660.html)
- [GaussDB 分布式 STATEMENT_HISTORY](https://support.huaweicloud.com/intl/en-us/distributed-devg-v2-gaussdb/gaussdb-12-0660.html)
- [GaussDB GLOBAL_LOCKS](https://support.huaweicloud.com/intl/en-us/distributed-devg-v8-gaussdb/gaussdb-12-1576.html)
- [GaussDB GLOBAL_THREAD_WAIT_STATUS](https://support.huaweicloud.com/intl/en-us/distributed-devg-v8-gaussdb/gaussdb-12-1476.html)
- [GaussDB WAIT_EVENT_INFO](https://support.huaweicloud.com/intl/en-us/distributed-devg-v8-gaussdb/gaussdb-12-1587.html)
- [GaussDB MEMORY_NODE_DETAIL](https://support.huaweicloud.com/intl/en-us/distributed-devg-v8-gaussdb/gaussdb-12-1578.html)
- [GaussDB pgxc_stat_get_wal_senders](https://support.huaweicloud.com/intl/en-us/distributed-devg-v8-gaussdb/gaussdb-12-2657.html)
- [GaussDB Custom Plan/Generic Plan 与 plan_cache_mode](https://support.huaweicloud.com/intl/en-us/distributed-devg-v3-gaussdb/gaussdb-12-1536.html)
- [GaussDB no_gpc hint](https://support.huaweicloud.com/intl/en-us/distributed-devg-v8-gaussdb/gaussdb-12-0288.html)
- [GaussDB Stream hints](https://support.huaweicloud.com/intl/en-us/distributed-devg-v8-gaussdb/gaussdb-12-0278.html)

## 24. 评审结论要求

进入实现计划前，需要确认本设计中的：

- 场景编号和正式名称；
- 每个场景的制造语义；
- B/C 风险场景的默认禁用策略；
- `gsbench restore` 的无参数恢复范围；
- 分布式 GaussDB 的真库验收要求。

评审提出的修改先更新本文件。正式开发从测试和实现计划开始，不直接依据聊天记录
编码。
