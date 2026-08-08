# gsbench v1.1.8 配置手册

发布包的基准配置是 [`configs/gsbench.cfg`](../../configs/gsbench.cfg)。建议复制后修改并保持 `0600` 权限；不要新增程序未读取的“占位配置”。安装和命令流程见[安装手册](INSTALL.md)。

## 配置加载与路径规则

配置优先级固定为：

```text
-c/--config
  > GSBENCH_CONFIG
  > ./gsbench.cfg
  > ./configs/gsbench.cfg
  > 可执行文件目录/gsbench.cfg
  > 可执行文件父目录/configs/gsbench.cfg
```

显式指定的文件不存在时立即失败，不会继续搜索。加载后配置路径会固定为绝对路径；以下相对路径都以配置文件目录为基准，而不是以启动命令的当前目录为基准：

- `database.password_config`；
- `fault_provider.ledger_path`；
- 默认 `logs/` 和 recovery ledger。

因此，同一配置从不同目录启动时仍会使用同一组日志、恢复状态和锁。自动化运行仍推荐显式传入绝对路径。

## `[database]` 数据库连接

```ini
[database]
host = 127.0.0.1
port = 5433
database = postgres
user = gaussdb
password_env = GSBENCH_PASSWORD
# password_config = gstop.cfg
sslmode = disable
application_name = gsbench
connect_timeout = 5s
```

- `host`、`port`、`database`、`user`：目标数据库连接信息；端口必须为 1–65535。
- `password_env`：密码环境变量名，默认 `GSBENCH_PASSWORD`。
- `password_config`：可选。设置后从该配置的 `main.db_password` 读取密码；相对路径以当前 `gsbench.cfg` 的目录解析。
- `sslmode`、`application_name`、`connect_timeout`：连接参数；超时必须大于 0。

推荐密码方式：

```sh
export GSBENCH_PASSWORD='数据库密码'
chmod 0600 configs/gsbench.cfg
```

如果使用 gstop 配置，必须同样限制文件权限：

```ini
[main]
db_password = 数据库密码
```

## `[run]` 运行行为

```ini
[run]
scenarios = 101
duration = 10m
ramp_interval = 2s
profile = quick
dry_run = false
validation_enabled = false
```

- `scenarios`：默认场景编号或规范名称，逗号分隔；命令行 `-s` 会覆盖它。
- `duration`：101–103、201–202 以及 601–606 `init` 的持续加压时长，从所有固定 worker 就绪后开始计时；命令行 `-d`/`--duration` 会覆盖它。其他场景仍按各自的 ramp/hold 生命周期执行。
- `ramp_interval`：控制器调节间隔，必须大于 0。
- `profile`：只允许 `quick` 或 `stress`。
- `dry_run`：只展示/检查动作，不执行工作负载修改；命令行 `--dry-run` 会覆盖它。
- `validation_enabled`：兼容旧配置保留；v1.1.8 的适用性、容量、数据、元数据和计划检查始终为告警模式。

v1.1.8 的检查策略：

- 运行前只读扫描模型、容量、物理大小、产品/拓扑、数据布局和执行计划；
- 不符合预期时输出 `PRECHECK_WARN`，不自动优化，不阻挡场景；
- 结果门槛或验证不满足时以 `COMPLETED_WITH_WARNINGS` 结束；
- 一个场景的真实执行错误不阻塞同一命令中的后续场景。

以下边界仍然硬失败：

- 真实 DDL、DML、查询和负载错误；
- schema/SQL 标识符、参数语法和注入防护；
- Risk B/C 配置与命令行双重授权；
- 持久变更前 journal/ledger 记录失败。

`validation_enabled=true` 不会恢复旧版强制门禁或自动优化行为。

601–606 共用计划基线，每次只能选择一个并串行运行。它们不再接受原来的单条 `run -s 601 -d 2m`；使用两个终端的三阶段命令，例如：

```sh
# 终端一，RUNNING 后开始计算 duration
gsbench run 601 init --worker 10 --duration 2m --config /absolute/path/gsbench.cfg

# 终端二
gsbench run 601 fault --config /absolute/path/gsbench.cfg
gsbench run 601 recover --config /absolute/path/gsbench.cfg
```

602–606 使用相同语法并替换为同一个编号。`fault` 必须在对应 `init` 存活且显示 `RUNNING` 后启动；`recover` 在 `init` 退出后仍可执行，但只展示该编号的恢复 DDL/DML，不执行恢复。

601 使用 `(lookup_key,dist_key)` 复合唯一索引按 lookup key 点查。缺少索引或定义不符只告警，gsbench 不再自动转换；`fault` 删除索引，`recover` 展示重建 DDL。未显式要求索引方法时，`btree` 与 Ustore 的 `ubtree` 等价；显式方法仍严格匹配。

## `[data]` 数据集

```ini
[data]
schema = gsbench
max_size_gb = 5
min_free_disk_percent = 20
reuse_existing = true
capacity_provider = auto
data_directory_host_root =
physical_size_provider = auto
```

- `schema`：测试 schema；只允许安全 SQL 标识符。不得指向业务 schema。
- `max_size_gb`：未传 `init --size` 时的目标大小；必须为正数。旧版策略上限不再阻挡运行。
- `min_free_disk_percent`：兼容旧配置保留，只用于输出磁盘余量告警。
- `reuse_existing`：是否续用已有测试 schema。专用一次性验收建议设为 `false`；若 schema 已存在，初始化会拒绝并要求先安全处理。
- `capacity_provider`：`auto`、`local_data_directory` 或 `tablespace_quota`。
- `data_directory_host_root`：仅用于本地数据库位于 chroot 时拼接宿主机前缀；非空值必须是绝对路径。
- `physical_size_provider`：`auto` 或 `catalog`。

`init --size` 接受带单位的正数，例如 `20GB`、`100GB`、`1.5TB`。`safety.profile_cap_gb`、磁盘余量和 provider 能力只产生告警，不再裁剪目标或拒绝执行。

## `[safety]` 风险授权与兼容配置

```ini
[safety]
max_connections = 800
max_workers = 640
query_timeout = 30s
restore_timeout = 10m
profile_cap_gb = 256
restore_on_exit = true
restore_original_role = false
allow_admin_mutation = false
allow_infrastructure_fault = false
allow_instance_parameter_change = false
allow_database_restart = false
restart_command =
```

- `max_connections`、`max_workers`、`profile_cap_gb`：旧版策略限制，v1.1.8 仅兼容读取并输出弃用告警，不裁剪目标。
- `query_timeout`：真实查询超时，必须大于 0。
- `restore_timeout`：兼容旧恢复实现保留；只读恢复规划不会用它执行 DDL/DML。
- `restore_on_exit`、`restore_original_role`：弃用配置；任何取值都不会触发自动恢复或角色恢复。
- `allow_admin_mutation`：Risk B 配置授权；仍须命令行显式传 `--allow-risk B` 或 `C`。
- `allow_infrastructure_fault`：Risk C 配置授权；仍须 `--allow-risk C` 和已注册外部 provider。
- `allow_instance_parameter_change`、`allow_database_restart`、`restart_command`：实例参数/重启的附加边界；启用重启时 `restart_command` 不能为空。

发布样例默认关闭 Risk B/C。先用 `gsbench scenarios` 查看场景风险与适用范围，不应为了绕过前置失败而放宽风险开关。

## `[fault_provider]` 恢复台账

```ini
[fault_provider]
type = none
# ledger_path =
```

当前轻量发布只内置 fail-closed 的 `none`。外部故障 provider 必须经过验证并显式注册，不能仅通过把 `type` 改成 `local`、`ssh` 或 `gaussdb_api` 来假定可用。

`ledger_path` 留空时，会在配置目录的 `logs/` 中创建按配置文件身份隔离的恢复 JSON；显式相对路径仍以配置目录解析。不要把 ledger 指向共享业务文件、符号链接或非 JSON 路径。

## 场景专用参数

发布样例包含当前实现会读取的专用参数：

| 配置段 | 参数 | 样例值 | 含义 |
|---|---|---:|---|
| `scenario.tp_cpu` | `workers` | 1 | 101 固定 TP worker 数 |
| `scenario.ap_cpu` | `workers` | 1 | 102 固定 AP worker 数 |
|  | `scan_rows` | 1000000 | 每个 AP 查询最多扫描行数 |
| `scenario.mixed_cpu` | `tp_workers` | 1 | 103 固定 TP worker 数 |
|  | `ap_workers` | 1 | 103 固定 AP worker 数 |
|  | `scan_rows` | 1000000 | AP 部分每个查询最多扫描行数 |
| `scenario.memory_workmem_sort` | `workers` | 1 | 201 固定排序 worker 数 |
|  | `work_mem` | 256MB | 每个 201 worker 的 `work_mem`；支持整数 kB/MB/GB，最小 64kB |
| `scenario.memory_workmem_hash` | `workers` | 1 | 202 固定 Hash worker 数 |
|  | `work_mem` | 256MB | 每个 202 worker 的 `work_mem`；支持整数 kB/MB/GB，最小 64kB |
| `scenario.lock_row_chain` | `sessions` | 2 | 501 workload 会话总数，包含 1 个 holder 和其余 waiter |
|  | `chain_depth` | 1 | 501 每条人工等待链的最大深度，范围 1–5 |
| `scenario.lock_table_exclusive` | `sessions` | 2 | 502 workload 会话总数，包含 1 个 holder 和其余 waiter |
| `scenario.lock_ddl_wait` | `sessions` | 2 | 503 workload 会话总数，包含 1 个 holder 和其余 waiter |
| `scenario.memory_total_pressure` | `workers` | 4 | 有界内存压力 worker 数，不代表主机内存百分比 |
| `scenario.connection_pool` | `target_percent` | 95 | 401 连接池目标百分比；CLI `--percent` 仅在选中 401 时覆盖 |
|  | `idle_percent` | 60 | 目标连接中的 idle 比例 |
|  | `idle_in_transaction_percent` | 20 | 目标连接中的 idle in transaction 比例 |
| `scenario.thread_pool` | `target_percent` | 95 | 402 线程池目标百分比；CLI `--percent` 仅在选中 402 时覆盖 |
| `scenario.vacuum_pressure` | `mode` | `vacuum` | vacuum 模式 |
|  | `allow_vacuum_full` | false | 是否允许高风险 `VACUUM FULL` |
|  | `minimum_slowdown` | 1.5 | 严格验证时的最小变慢倍数 |

101–103 不再设置或追踪 CPU 百分比目标。它们按 sysbench 模型固定创建 worker，每个 worker 完成一笔请求后立即执行下一笔，到达 `duration` 后不再开始新请求。CPU 仅由 gstop 观测，不参与控制或成功判定。其他带百分比参数的场景仍受实际数据库参数、拓扑和容量限制。

命令行示例：

```sh
gsbench run 101 --workers 16 --duration 60s
gsbench run 102 --workers 4 --duration 60s
gsbench run 103 --tp-workers 12 --ap-workers 2 --duration 60s
gsbench run 401 --percent 90 --duration 5m
gsbench run 402 --percent 90 --duration 5m
gsbench run 501 --sessions 10 --chain-depth 3 --duration 1m
gsbench run 502 --sessions 10 --duration 1m
gsbench run 503 --sessions 10 --duration 1m
```

CLI worker 参数覆盖上述场景配置。103 的总并发为 `tp_workers + ap_workers`；所有 worker 先建立独立 tagged session 并等待同一个启动屏障，加压计时开始后才统一执行。

`--percent` 只允许 `run` 且最终场景必须包含 401 或 402；同一值覆盖所有选中的池场景。401 使用 `max_connections - sysadmin_reserved_connections` 为容量分母，只创建基线到目标之间的连接差值；402 优先使用 `global_threadpool_status`。目标低于基线、超过 100%、指标缺失或物理余量不足会输出告警，不再因 gsbench 策略门禁提前退出；数据库真实拒绝连接仍使当前场景失败。

达标后 401/402 停止增加并保持本次资源到 duration 结束。正常结束、失败和 Ctrl+C 会关闭本次创建的 tagged 会话；数据库自身 idle thread-pool worker 的销毁不由 gsbench 控制。

新 run 只报告 stale run、database journal 和 local ledger 的待恢复动作，不执行 stale recovery，也不阻塞后续场景。使用 `gsbench restore` 展示全部人工恢复 SQL。

`gsbench restore` 与 `gsbench run 601-606 recover` 均为只读恢复规划：只发现、校验和展示 DDL/DML/人工动作，不申请恢复锁、不执行逆操作、不修改 journal/ledger。

201/202 的命令行覆盖一次只允许选择一个场景，例如 `gsbench run 201 --workers 8 --work-mem 256MB --duration 1m`。不同并发模型可以组合；旧版累计 worker/连接预算只输出告警，不再使用 `safety.max_connections` 拒绝运行。

`--sessions` 只允许所选场景全部属于 501–503，并对每个所选场景分别生效。`--chain-depth` 要求选择中包含 501，只影响 501；502、503 不人工构造多层等待链。501 还要求 `sessions >= chain_depth + 1`。gsbench 自身的控制、元数据和观测连接不计入 `sessions`。

## 20 GiB 专用配置要点

现场全场景验收至少满足：

```ini
[run]
validation_enabled = true
dry_run = false

[data]
schema = gsbench_e2e_20260801
max_size_gb = 20
reuse_existing = false

[safety]
profile_cap_gb = 256
```

并设置同名环境确认：

```sh
export GSBENCH_E2E_SCHEMA=gsbench_e2e_20260801
```

验证必须在能连接 openGauss/GaussDB 的 Linux 主机完成，并使用专用 schema。`cleanup --data` 会不可恢复地删除整个 schema；失败或中断时先用 `restore` 查看人工恢复 SQL，确认自行执行完成后再决定是否清理数据。

当前 macOS 构建环境未完成 GaussDB live、20 GiB 初始化或真实负载验收；本配置手册不能作为这些现场测试已完成的证明。
