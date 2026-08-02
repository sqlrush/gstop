# gsbench 全场景测试报告（2026-08-01）

## 结论

当前代码的自动化单元测试、静态检查、race 检查和验证脚本自测已通过；验证脚本与场景目录交叉确认了 **65 个已实现场景**。本次执行环境是 macOS，既没有可连接的目标 openGauss/GaussDB，也没有 Linux `/proc` 指标源，因此没有执行真实数据库负载测试。

以下项目统一标记为 **`LIVE_NOT_RUN / 现场待验证`**，不能记为 PASS：

- `init --size 20GB` 生成 20 GiB 测试数据及 19–21 GiB 物理大小确认；
- 65 个场景的真实 SQL、锁、执行计划、连接池和线程池验证；
- 101–103 的固定 TP/AP worker 数、持续流量与到时停止行为；
- 401、402、404 的容量上限、实际负载和不可达 ceiling 证据；
- gstop 覆盖全场景的同步采样；
- 最终 restore、stale recovery 检查和专用 schema 清理。

本报告没有引用或复用历史测试数据，也没有把缺少现场环境的项目推断为成功。

## 自动化验证结果

| 验证项 | 状态 | 范围 |
|---|---|---|
| Go 单元/回归测试 | PASS | gsbench 及仓库 Go 测试 |
| Go 静态检查 | PASS | `go vet ./...` |
| Go race 检查 | PASS | gsbench 并发路径 |
| Shell 静态检查 | PASS | 验证脚本语法与 shellcheck |
| 验证脚本自测 | PASS | 参数、安全边界、场景列表和失败关闭行为 |
| 场景清单一致性 | PASS | 脚本白名单为 65 个唯一编号，均能在 `gsbench scenarios` 中找到 |
| 真实 GaussDB/Linux 联调 | LIVE_NOT_RUN | 当前环境不具备数据库和 Linux `/proc` |

自动化测试只能证明代码路径、边界条件和脚本行为满足预期，不能替代数据库现场的负载达标证据。

## 初始化、同步采样与清理

| 阶段 | 状态 | 现场验收条件 |
|---|---|---|
| `doctor` | LIVE_NOT_RUN | 目标数据库连通，产品/拓扑/权限/容量探测成功 |
| `init --size 20GB` | LIVE_NOT_RUN | 日志包含 `target_bytes=21474836480`，最终物理大小在 19–21 GiB |
| gstop 同步采样 | LIVE_NOT_RUN | Linux 主机可读取 `/proc`，gstop 从第一个场景前持续运行至最后一个场景结束 |
| 每场景内置 restore | LIVE_NOT_RUN | EVIDENCE 中恢复状态成功，失败时保留现场并显式重试 |
| final restore/stale 检查 | LIVE_NOT_RUN | `stale recovery runs=0 database_runs=0 local_actions=0` |
| `cleanup --data` | LIVE_NOT_RUN | 仅对 `gsbench_e2e_20260801` 执行；配置、环境确认和恢复状态三重校验通过 |

## 负载目标与 ceiling 证据要求

默认值来自当前 `configs/gsbench.cfg`。现场若修改配置，应同时在报告中记录实际配置值，不能仍沿用下表默认值。

| 场景 | 默认目标 | 运行要求 | 必须保存的达标/ceiling 证据 |
|---|---:|---|---|
| 101 `tp_cpu` | 固定 TP workers | 所有 worker 就绪后持续满速执行 60 秒 | 配置 workers、tagged TP session 峰值、duration、operations、errors；到时 session 归零且 operations 不再增长 |
| 102 `ap_cpu` | 固定 AP workers | 所有 worker 就绪后持续满速执行 60 秒，每次最多扫描 1,000,000 行 | 配置 workers、tagged AP session 峰值、scan_rows、duration、operations、errors；到时停止注入 |
| 103 `mixed_cpu` | 固定 TP/AP workers | 两组 worker 共享启动屏障并持续满速执行 60 秒 | `tp_workers`、`ap_workers` 与两类 tagged session 分别一致；记录总 workers、duration、operations、errors并确认到时归零 |
| 401 `connection_pool` | 可用连接容量的 95% | 以 `max_connections - reserved` 为分母，扣除已有连接后补足；同时受 `safety.max_connections` 限制 | `usable_connection_capacity`、`reserved_connections`、`existing_connections`、`workload_connection_target`、实际连接百分比、`reachable_max`、`connection_capacity_ceiling_percent`、operations、errors |
| 402 `thread_pool` | 线程池利用率 95% | 仅真实线程池视图可用于达标判断；默认受 max workers、max connections、reserved 和已有连接共同限制 | `thread_pool_actual_workers`、`thread_pool_idle_workers`、`thread_pool_pending_sessions`、实际利用率、`reachable_max`、`topology_session_ceiling_percent`、operations、errors；fallback 或指标不可用不能记为达标 |
| 404 `threadpool_queue` | 产生真实 pending，不是百分比目标 | 建立至少 `actual_workers + 1` 个会话并观测 `pending > 0` | actual/idle workers、峰值 `thread_pool_pending_sessions`、所需会话数、实际建立会话数、`thread_queue_session_ceiling`、operations、errors；会话 ceiling 不足必须明确报不可达 |

101–103 的现场验收不再检查 CPU 目标或闭环收敛。验证应在 Hold 窗口持续对照 EVIDENCE、tagged session 和 gstop：实际 TP/AP worker 数必须等于输入参数，时间到后会话归零且不再注入请求。CPU 只作为旁路观测数据。

## 65 个已实现场景

清单来源于 `scripts/validate-gsbench-scenarios.sh` 的执行白名单，并用当前 `go run ./cmd/gsbench scenarios` 补充名称和适用产品。未进入脚本白名单的目录条目不在本轮范围内。

### 1xx：CPU（3）

| 编号 | 场景名 | 适用范围 | 当前状态 | 现场重点证据 |
|---:|---|---|---|---|
| 101 | `tp_cpu` | openGauss / 集中式 GaussDB / 分布式 GaussDB | LIVE_NOT_RUN | 输入 TP workers 与 tagged session 一致、持续流量、到时停止、operations/errors |
| 102 | `ap_cpu` | openGauss / 集中式 GaussDB / 分布式 GaussDB | LIVE_NOT_RUN | 输入 AP workers 与 tagged session 一致、scan_rows、到时停止、operations/errors |
| 103 | `mixed_cpu` | openGauss / 集中式 GaussDB / 分布式 GaussDB | LIVE_NOT_RUN | TP/AP 输入分别与 tagged session 一致、同步起停、operations/errors |

### 2xx：内存（7）

| 编号 | 场景名 | 适用范围 | 当前状态 | 现场重点证据 |
|---:|---|---|---|---|
| 201 | `memory_workmem_sort` | 全部支持产品 | LIVE_NOT_RUN | work_mem sort 行为、内存/临时文件指标、operations/errors |
| 202 | `memory_workmem_hash` | 全部支持产品 | LIVE_NOT_RUN | hash 内存行为、spill/内存指标、operations/errors |
| 203 | `memory_sharedbuffer_churn` | 全部支持产品 | LIVE_NOT_RUN | shared buffer churn 指标、operations/errors |
| 204 | `memory_plancache_growth` | 全部支持产品 | LIVE_NOT_RUN | plan cache 增长指标、operations/errors |
| 205 | `memory_session_context_growth` | 全部支持产品 | LIVE_NOT_RUN | session context 增长指标、operations/errors |
| 207 | `memory_total_pressure` | 全部支持产品 | LIVE_NOT_RUN | 组合内存压力的真实 worker/内存指标；不得伪造主机内存百分比 |
| 208 | `memory_retention` | 全部支持产品 | LIVE_NOT_RUN | 会话保留前后内存证据、operations/errors |

### 3xx：I/O 与网络（9）

| 编号 | 场景名 | 适用范围 | 当前状态 | 现场重点证据 |
|---:|---|---|---|---|
| 301 | `io_sequential_read` | 全部支持产品 | LIVE_NOT_RUN | 顺序读 operations/errors 与数据库/gstop I/O 对照 |
| 302 | `io_random_read` | 全部支持产品 | LIVE_NOT_RUN | 随机读 operations/errors 与数据库/gstop I/O 对照 |
| 303 | `io_wal_write` | 全部支持产品 | LIVE_NOT_RUN | WAL 写入量、operations/errors 与 gstop 对照 |
| 304 | `io_temp_spill` | 全部支持产品 | LIVE_NOT_RUN | 临时文件/spill 指标、operations/errors |
| 321 | `network_client_egress` | 全部支持产品 | LIVE_NOT_RUN | 客户端下行字节/吞吐、operations/errors |
| 322 | `network_client_ingress` | 全部支持产品 | LIVE_NOT_RUN | 客户端上行字节/吞吐、operations/errors |
| 331 | `network_cn_dn_stream` | 仅分布式 GaussDB | LIVE_NOT_RUN | CN/DN stream 证据；其他拓扑只能记 NOT_APPLICABLE |
| 332 | `network_distributed_shuffle` | 仅分布式 GaussDB | LIVE_NOT_RUN | 分布式 shuffle 证据；其他拓扑只能记 NOT_APPLICABLE |
| 333 | `network_distributed_broadcast` | 仅分布式 GaussDB | LIVE_NOT_RUN | 分布式 broadcast 证据；其他拓扑只能记 NOT_APPLICABLE |

### 4xx：连接池与线程池（4）

| 编号 | 场景名 | 适用范围 | 当前状态 | 现场重点证据 |
|---:|---|---|---|---|
| 401 | `connection_pool` | 全部支持产品 | LIVE_NOT_RUN | 95% 目标、已有/保留/可用连接、实际连接、容量 ceiling |
| 402 | `thread_pool` | 全部支持产品 | LIVE_NOT_RUN | 95% 目标、actual/idle/pending workers、拓扑会话 ceiling |
| 403 | `connection_churn` | 全部支持产品 | LIVE_NOT_RUN | created/closed/failures/create latency、operations/errors |
| 404 | `threadpool_queue` | 全部支持产品 | LIVE_NOT_RUN | 建立 `actual_workers+1` 会话、pending 峰值、session ceiling |

### 5xx：锁与锁模式（30）

| 编号 | 场景名 | 适用范围 | 当前状态 | 现场重点证据 |
|---:|---|---|---|---|
| 501 | `lock_row_chain` | 全部支持产品 | LIVE_NOT_RUN | 行锁等待链边数、holder/waiter、恢复结果 |
| 502 | `lock_table_exclusive` | 全部支持产品 | LIVE_NOT_RUN | 表级排他锁等待、holder/waiter、恢复结果 |
| 503 | `lock_ddl_wait` | 全部支持产品 | LIVE_NOT_RUN | DDL 等待可观测性、operations/errors、恢复结果 |
| 504 | `lock_deadlock` | 全部支持产品 | LIVE_NOT_RUN | 真实死锁环和数据库检测结果、恢复结果 |
| 505 | `lock_ddl_blocks_dml` | 全部支持产品 | LIVE_NOT_RUN | DDL 阻塞 DML 的锁证据、恢复结果 |
| 506 | `lock_select_blocks_ddl` | 全部支持产品 | LIVE_NOT_RUN | SELECT 阻塞 DDL 的锁证据、恢复结果 |
| 508 | `lock_ddl_blocks_vacuum` | 全部支持产品 | LIVE_NOT_RUN | DDL 阻塞 VACUUM 的锁证据、恢复结果 |
| 509 | `lock_createindex_blocks_dml` | 全部支持产品 | LIVE_NOT_RUN | CREATE INDEX 阻塞 DML 的锁证据、恢复结果 |
| 510 | `lock_dml_blocks_createindex` | 全部支持产品 | LIVE_NOT_RUN | DML 阻塞 CREATE INDEX 的锁证据、恢复结果 |
| 520 | `lockmode_accessshare_accessexclusive` | 全部支持产品 | LIVE_NOT_RUN | AccessShare / AccessExclusive 冲突证据 |
| 521 | `lockmode_rowshare_exclusive` | 全部支持产品 | LIVE_NOT_RUN | RowShare / Exclusive 冲突证据 |
| 522 | `lockmode_rowshare_accessexclusive` | 全部支持产品 | LIVE_NOT_RUN | RowShare / AccessExclusive 冲突证据 |
| 523 | `lockmode_rowexclusive_share` | 全部支持产品 | LIVE_NOT_RUN | RowExclusive / Share 冲突证据 |
| 524 | `lockmode_rowexclusive_sharerowexclusive` | 全部支持产品 | LIVE_NOT_RUN | RowExclusive / ShareRowExclusive 冲突证据 |
| 525 | `lockmode_rowexclusive_exclusive` | 全部支持产品 | LIVE_NOT_RUN | RowExclusive / Exclusive 冲突证据 |
| 526 | `lockmode_rowexclusive_accessexclusive` | 全部支持产品 | LIVE_NOT_RUN | RowExclusive / AccessExclusive 冲突证据 |
| 527 | `lockmode_shareupdateexclusive_self` | 全部支持产品 | LIVE_NOT_RUN | ShareUpdateExclusive 自冲突证据 |
| 528 | `lockmode_shareupdateexclusive_share` | 全部支持产品 | LIVE_NOT_RUN | ShareUpdateExclusive / Share 冲突证据 |
| 529 | `lockmode_shareupdateexclusive_sharerowexclusive` | 全部支持产品 | LIVE_NOT_RUN | ShareUpdateExclusive / ShareRowExclusive 冲突证据 |
| 530 | `lockmode_shareupdateexclusive_exclusive` | 全部支持产品 | LIVE_NOT_RUN | ShareUpdateExclusive / Exclusive 冲突证据 |
| 531 | `lockmode_shareupdateexclusive_accessexclusive` | 全部支持产品 | LIVE_NOT_RUN | ShareUpdateExclusive / AccessExclusive 冲突证据 |
| 532 | `lockmode_share_sharerowexclusive` | 全部支持产品 | LIVE_NOT_RUN | Share / ShareRowExclusive 冲突证据 |
| 533 | `lockmode_share_exclusive` | 全部支持产品 | LIVE_NOT_RUN | Share / Exclusive 冲突证据 |
| 534 | `lockmode_share_accessexclusive` | 全部支持产品 | LIVE_NOT_RUN | Share / AccessExclusive 冲突证据 |
| 535 | `lockmode_sharerowexclusive_self` | 全部支持产品 | LIVE_NOT_RUN | ShareRowExclusive 自冲突证据 |
| 536 | `lockmode_sharerowexclusive_exclusive` | 全部支持产品 | LIVE_NOT_RUN | ShareRowExclusive / Exclusive 冲突证据 |
| 537 | `lockmode_sharerowexclusive_accessexclusive` | 全部支持产品 | LIVE_NOT_RUN | ShareRowExclusive / AccessExclusive 冲突证据 |
| 538 | `lockmode_exclusive_self` | 全部支持产品 | LIVE_NOT_RUN | Exclusive 自冲突证据 |
| 539 | `lockmode_exclusive_accessexclusive` | 全部支持产品 | LIVE_NOT_RUN | Exclusive / AccessExclusive 冲突证据 |
| 540 | `lockmode_accessexclusive_self` | 全部支持产品 | LIVE_NOT_RUN | AccessExclusive 自冲突证据 |

### 6xx：执行计划与硬解析（11）

| 编号 | 场景名 | 适用范围 | 当前状态 | 现场重点证据 |
|---:|---|---|---|---|
| 601 | `planchange_stats_target` | 全部支持产品 | LIVE_NOT_RUN | 前/后 plan signature、统计目标变更、恢复后基线 |
| 602 | `planchange_index_unusable` | 全部支持产品 | LIVE_NOT_RUN | 前/后 plan signature、索引不可用状态、恢复后索引形状 |
| 603 | `planchange_stats_ndistinct` | 全部支持产品 | LIVE_NOT_RUN | 前/后 plan signature、ndistinct 变更、恢复后基线 |
| 604 | `planchange_stats_extended` | 全部支持产品 | LIVE_NOT_RUN | 前/后 plan signature、同会话扩展统计变更、恢复后基线 |
| 605 | `planchange_index_drop` | 全部支持产品 | LIVE_NOT_RUN | 前/后 plan signature、索引删除、恢复后完整索引定义 |
| 606 | `planchange_index_shape` | 全部支持产品 | LIVE_NOT_RUN | 前/后 plan signature、索引列形状、恢复后完整索引定义 |
| 621 | `hardparse_literal_flood` | 全部支持产品 | LIVE_NOT_RUN | 硬解析/计划缓存指标、operations/errors |
| 622 | `hardparse_unprepared` | 全部支持产品 | LIVE_NOT_RUN | 非预备语句解析指标、operations/errors |
| 623 | `hardparse_force_custom` | 全部支持产品 | LIVE_NOT_RUN | custom plan 行为和解析指标、operations/errors |
| 624 | `hardparse_session_churn` | 全部支持产品 | LIVE_NOT_RUN | 会话 churn 与解析指标、operations/errors |
| 625 | `hardparse_ddl_invalidation` | 全部支持产品 | LIVE_NOT_RUN | DDL 失效前后计划/解析指标、恢复结果 |

**601–606 必须串行执行。** 每次 `gsbench run` 只传一个计划场景编号；不能把多个编号放进同一次运行。验证脚本对全部 65 个场景均采用单编号串行运行，并为计划场景保留变更前、变更后和恢复后的证据。

### 8xx：VACUUM（1）

| 编号 | 场景名 | 适用范围 | 当前状态 | 现场重点证据 |
|---:|---|---|---|---|
| 801 | `vacuum_pressure` | 全部支持产品 | LIVE_NOT_RUN | VACUUM 模式、慢化比例、operations/errors、对象恢复状态 |

## 现场执行方法

完整前置条件、安全边界、专用配置、执行命令、证据格式和失败恢复步骤见 [FULL_SCENARIO_TEST.md](./FULL_SCENARIO_TEST.md)。标准执行入口为：

```sh
export GSBENCH_PASSWORD='数据库密码'
export GSBENCH_E2E_SCHEMA=gsbench_e2e_20260801

scripts/validate-gsbench-scenarios.sh \
  --gsbench /absolute/path/gsbench \
  --gstop /absolute/path/gstop \
  --config /absolute/path/gsbench-e2e.cfg \
  --gstop-config /absolute/path/gstop.cfg \
  --artifacts /absolute/path/gsbench-e2e-artifacts
```

现场执行后，应使用 `results.tsv` 汇总 65 个场景，用 `runs.tsv` 核对退出码、时长和恢复状态，并保留每个场景的 EVIDENCE JSON、gstop 日志、初始化日志、status、restore 和 cleanup 日志。只有 `SUCCESS` 且目标指标、operations、errors、ceiling 和恢复证据完整时，才能记为通过；`NOT_APPLICABLE`、`UNVERIFIED`、`DEGRADED`、`FAILED`、`RESTORE_FAILED` 均不能误记为负载达标。
