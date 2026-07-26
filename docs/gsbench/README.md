# gsbench

版本：v1.1.0

Author: WangYingJie <sqlrush@gmail.com>

gsbench 是面向 openGauss/GaussDB 的轻量故障与压力场景模拟工具。它以三位场景码选择负载，先探测产品、拓扑和能力，再在有限时长内运行场景，并统一停止和恢复。当前 Foundation 版本用于安全地建立场景目录、数据集、恢复日志和少量可运行场景，不是完整故障注入平台。

## 快速开始

发布配置默认选择场景 `101`，Risk B/C 均关闭：

```bash
chmod 600 configs/gsbench.cfg
export GSBENCH_CONFIG="$PWD/configs/gsbench.cfg"
export GSBENCH_PASSWORD='数据库密码'

gsbench scenarios
gsbench doctor
gsbench init --size 5GB --dry-run
gsbench init --size 5GB
gsbench run -s 101 -d 2m --dry-run
gsbench run -s 101 -d 2m
gsbench status
gsbench restore --dry-run
gsbench restore
```

`help`、`version` 和 `scenarios` 不读取配置，也不连接数据库。其他命令按以下优先级选择配置：

```text
-c/--config
GSBENCH_CONFIG
./gsbench.cfg
./configs/gsbench.cfg
```

## 三位场景码

使用 `gsbench scenarios` 查看完整稳定目录。`run` 接受三位场景码或目录中的完整名称，可用逗号组合；不再接受一位旧编号和旧别名：

```bash
gsbench run -s 101,102 -d 5m
gsbench run -s tp_cpu,ap_cpu -d 5m
gsbench run 101,401
```

当前只注册了以下六个可运行 factory：

| 场景码 | 名称 | 当前策略 |
|---:|---|---|
| 101 | `tp_cpu` | TP 短事务 CPU 反馈负载 |
| 102 | `ap_cpu` | AP JOIN/聚合/排序 CPU 反馈负载 |
| 103 | `mixed_cpu` | TP/AP 混合 CPU 反馈负载 |
| 401 | `connection_pool` | 带 run 标签的连接状态组合 |
| 402 | `thread_pool` | 真实线程池压力；要求 doctor 检出 `thread_pool` |
| 801 | `vacuum_pressure` | VACUUM 与前台延迟退化 |

目录中的其他场景尚未注册 factory。在适用环境中选择它们会得到 `NOT_IMPLEMENTED`；不适用产品或拓扑会先得到 `NOT_APPLICABLE`。`doctor` 的 `SUPPORTED` 只表示产品、拓扑和能力前置条件满足，不代表场景 factory 已实现。

## 生命周期

推荐按以下顺序使用：

1. `gsbench scenarios`：离线查看三位场景目录、风险和适用环境。
2. `gsbench doctor`：只读探测产品、版本、拓扑、节点和能力，逐场景输出 `SUPPORTED`、`DEGRADED`、`UNSUPPORTED` 或 `NOT_APPLICABLE`；只报告 stale recovery，不执行恢复。
3. `gsbench init --dry-run`：连接目标并只读检查容量和已有高水位，展示检测到的集中式或分布式 DDL 方言，不创建 schema 或数据。
4. `gsbench init`：创建/续建专用 schema、元数据、journal 和负载数据。
5. `gsbench run --dry-run`：加载配置、探测环境、确认专用 schema，并展示场景和生命周期；它不进入 runner 的风险/恢复/工作负载 preflight，也不施加负载。
6. `gsbench run`：按 `preflight → prepare → ramp → hold → verify → stop → restore → verify_restore` 执行。新的变更型 run 会先恢复 stale run。
7. `gsbench status [--run-id RUN_ID]`：查看运行记录、phase、status，以及数据库 journal/本地 ledger 的 stale recovery 摘要。
8. `gsbench stop [--run-id RUN_ID]`：停止目标负载，并委托同一个通用恢复协调器完成恢复；不指定 run ID 时处理全部活动运行。
9. `gsbench restore [--run-id RUN_ID]`：紧急恢复入口。无 run ID 时发现全部活动/失败运行和未完成动作；有 run ID 时只选择该运行。

恢复前可以先查看计划：

```bash
gsbench restore --dry-run
gsbench restore --run-id RUN_ID --dry-run
```

restore dry-run 只发现数据库 journal 和本地 ledger 中的动作，不申请恢复锁、不改运行状态、不终止会话、不执行逆操作，也不会因缺失 ledger 而创建文件。

`cleanup` 默认等价于先停止并恢复；只有以下命令会删除专用数据：

```bash
gsbench cleanup --data
```

删除的 schema 只能通过再次 `init` 重建。

## 初始化容量

`init --size` 支持 1 GiB–2 TiB，输入单位为 `GB`/`TB`（大小写不敏感），最多两位小数：

```bash
gsbench init --size 1GB --dry-run
gsbench init --size 100GB --dry-run
gsbench init --size 1.5TB --dry-run
gsbench init --size 2TB --dry-run
```

容量优先级为：

```text
init --size > data.max_size_gb > profile 默认值
```

`quick` 默认 5 GiB，`stress` 默认 20 GiB。目标必须保留 `data.min_free_disk_percent` 指定的磁盘余量；初始化按批次提交并记录高水位，中断后可再次执行 `init` 续建，不自动缩容。

集中式目标使用经过校验的本地 `data_directory` 或有限的 tablespace quota。分布式 dry-run 会选择带 `DISTRIBUTE BY` 的方言；当前发布构建没有按 primary DN 验证的容量和物理大小 provider wiring，因此分布式真实 `init` 会 fail closed。

## 产品和拓扑适用性

下表是代码中的 Foundation 适用性边界，不是 live database 验收结果：

| 目标 | 六个已实现场景的目录适用性 | 额外边界 |
|---|---|---|
| openGauss standalone / primary-standby | 101、102、103、401、402、801 | 402 需要真实线程池能力；初始化使用集中式方言 |
| 集中式 GaussDB | 101、102、103、401、402、801 | 402 需要真实线程池能力；初始化使用集中式方言 |
| 分布式 GaussDB | 101、102、103、401、402、801 | 402 需要真实线程池能力；真实 init 因缺少 per-primary-DN provider wiring 而拒绝，dry-run 仅展示分布式方言 |

产品、拓扑和节点由只读探测确定。示例配置保留空的 `[topology]` 作为边界说明；当前 `BenchConfig` 没有手工覆盖拓扑的字段。

尚未在真实 openGauss、集中式 GaussDB、分布式 GaussDB 组合上完成 live matrix 验收。生产使用前应在同版本测试环境执行 doctor、init dry-run、最短时长 run 和 restore 演练。

## 风险授权

风险分为 A、B、C：

- Risk A 不需要额外风险开关；当前六个已实现 factory 的目录风险均为 A。
- Risk B 同时要求 `safety.allow_admin_mutation=true` 和命令行 `--allow-risk B`（或 C）。
- Risk C 同时要求 `safety.allow_infrastructure_fault=true`、命令行 `--allow-risk C`、可用外部 fault provider 能力和恢复 ledger。

示例配置保持：

```ini
[safety]
allow_admin_mutation = false
allow_infrastructure_fault = false
restore_original_role = false
allow_instance_parameter_change = false
allow_database_restart = false

[fault_provider]
type = none
```

当前发布构建只在默认 registry 中注册 `none`。配置解析器保留 `local`、`ssh` 和 `gaussdb_api` 类型名，但没有对应编译并注册的 adapter 时会拒绝使用，不能据此声称 Risk C 可运行。

`fault_provider.ledger_path` 留空时，使用 `logs` 下按配置文件身份隔离的 JSON recovery ledger。ledger 用于数据库不可达时的控制面恢复；不要把密码、令牌或完整 DSN 写入 provider 元数据。

## 结果与退出码

runner 和 `status` 可能显示以下最终结果：

| 结果 | 含义 | run 退出码 |
|---|---|---:|
| `SUCCESS` | 负载目标和恢复验证成功 | 0 |
| `NOT_APPLICABLE` | 场景不适用于检测到的产品/拓扑 | 0 |
| `DEGRADED` | 场景运行，但关键指标证据不可用或走了声明的降级路径 | 3 |
| `NOT_IMPLEMENTED` | 目录有定义，但当前构建没有 factory | 1 |
| `FAILED` | 配置、前置检查、负载、验证或停止失败 | 1 |
| `RESTORE_FAILED` | 通用恢复或恢复验证失败；必须重试 restore | 1 |

活动记录还可能暂时显示 `running`、`stop_requested` 或 `restore_requested`。`status` 同时列出未完成数据库动作和本地 ledger 动作；看到 `restore_failed`/`RESTORE_FAILED` 或 stale run 时，应执行：

```bash
gsbench restore --dry-run
gsbench restore
gsbench status
```

## 配置边界

示例配置中的主要实际字段：

- `database.*`：地址、端口、数据库、用户、SSL、应用名、连接超时和密码来源。
- `[topology]`：当前无可配置字段，产品/拓扑/节点由探测器决定。
- `run.scenarios/duration/ramp_interval/profile/dry_run`：默认场景、总时长和运行方式。
- `data.schema/max_size_gb/min_free_disk_percent/reuse_existing/capacity_provider/physical_size_provider`：专用数据和容量保护。
- `safety.cpu_target_percent/max_connections/max_workers/query_timeout/restore_timeout/profile_cap_gb`：资源与恢复硬边界。
- `safety.restore_on_exit/allow_admin_mutation/allow_infrastructure_fault/restore_original_role/allow_instance_parameter_change/allow_database_restart/restart_command`：恢复和显式授权开关。
- `fault_provider.type/ledger_path`：控制面 provider 类型和恢复 ledger。
- `scenario.ap_cpu.*`、`scenario.mixed_cpu.*`、`scenario.connection_pool.*`、`scenario.thread_pool.*`、`scenario.vacuum_pressure.*`：六个已实现 factory 实际读取的场景参数。

配置中的 `run.duration` 必须为有限正值。`safety.max_workers` 和 `safety.max_connections` 是 gsbench 上限，不会自动扩大数据库参数。

## 密码和日志

推荐只在配置中保存环境变量名：

```ini
[database]
password_env = GSBENCH_PASSWORD
```

也可以用 `database.password_config` 引用同发布目录的 gstop 配置并读取 `main.db_password`。该文件应限制为 `chmod 600`。屏幕和日志会对 DSN 密码做脱敏，但仍不应把密钥写入普通配置或命令行。

变更型命令的日志默认写到 `logs/gsbench_<run_id>.log`。doctor、status、restore dry-run 和 stop dry-run 是只读路径，不创建命令日志；恢复 ledger 也不会仅因 dry-run discovery 而创建。
