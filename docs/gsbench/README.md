# gsbench

`gsbench` 是面向 openGauss/GaussDB 的轻量故障与压力场景模拟工具。它先只读探测产品、拓扑和能力，再在有限时长内运行负载，并通过统一恢复协调器收尾。

发布使用请先看[Linux ARM64 安装手册](INSTALL.md)和[配置手册](CONFIG.md)；本轮自动化与现场待验证项见[2026-08-01 全场景测试报告](FULL_SCENARIO_REPORT_20260801.md)。

当前轻量发布目录共 90 个场景：**65 个已注册可运行，25 个已编目但刻意延后**。这不是完整故障注入平台。

## 快速开始

发布配置默认仅运行安全的 `101`，Risk B/C 均关闭：

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

`help`、`version`、`scenarios` 不读取配置，也不连接数据库。其余命令按 `-c/--config`、`GSBENCH_CONFIG`、当前目录的 `gsbench.cfg`、当前目录的 `configs/gsbench.cfg`、可执行文件同目录的 `gsbench.cfg`、可执行文件父目录下的 `configs/gsbench.cfg` 的固定优先级加载配置。显式路径不存在会直接报错，不会退回其他候选；自动化与生产运行推荐传绝对路径，例如 `gsbench doctor --config "$PWD/configs/gsbench.cfg"`。

配置加载后会固定为绝对路径。默认日志和 recovery ledger 都写入配置文件同目录的 `logs/`；显式相对 `fault_provider.ledger_path` 和 `database.password_config` 也相对配置目录解析，因此从不同工作目录启动仍使用同一组状态文件和锁。

## 已实现与延后目录

已注册的 65 个 factory：

- CPU：101–103。
- 资源：201–205、207–208、301–304、321–322、331–333、401–404、801。
- 锁：501–506、508–510、520–540（独立锁与锁冲突矩阵）。
- 执行计划/硬解析：601–606、621–625。

以下 25 个代码已在 `gsbench scenarios` 中编目，但当前不注册：`206, 209, 305, 341–343, 405, 507, 511–512, 626, 701–706, 721–728`。延后原因是它们需要尚未验证的复杂编排或额外能力：全局缓存/计划缓存、检查点与 OOM 风险控制、外部网络故障 provider、分布式 pooler/全局锁视图、可证明的 VACUUM 持锁编排、主备控制，或分布式集群/节点故障协调。特别是 507 需要证明短暂、非事务 VACUUM 在 DDL waiter 启动时仍持有锁，当前不能可靠证明，因而不应报告成功。

选择不适用的产品或拓扑，结果为 `NOT_APPLICABLE`；在适用环境选择已编目但未注册的代码，结果为 `NOT_IMPLEMENTED`。`doctor` 中的 `SUPPORTED` 只表示环境前置条件满足，不表示 factory 一定已实现。直接依赖的指标或视图不可用时，场景会进入声明的 `DEGRADED` 路径或失败，绝不会把缺失证据当作成功。

## 常用运行示例

场景参数以 `configs/gsbench.cfg` 中的实际键为准；先执行 `doctor`，再在测试环境用短时运行验证。

```bash
# work_mem 排序/Hash：固定 worker、每 worker 的 work_mem、持续时间
gsbench run 201 --workers 8 --work-mem 256MB --duration 1m
gsbench run 202 --workers 8 --work-mem 256MB --duration 1m

# I/O、网络资源场景
gsbench run -s 301,321 -d 2m

# 连接池：从运行前基线补足到 90%，达标后保持到 5m 结束
gsbench run 401 --percent 90 --duration 5m

# 线程池：用真实 thread-pool 指标补足到 90%，达标后冻结 worker 数
gsbench run 402 --percent 90 --duration 5m

# 两个独立锁场景；或一个锁冲突矩阵场景
gsbench run -s 501,504 -d 2m
gsbench run -s 520 -d 2m

# 执行计划变化：终端一持续造流
gsbench run 601 init --worker 10 --duration 1m

# 终端二：等同编号 init 进入 RUNNING 后注入并恢复
gsbench run 601 fault
gsbench run 601 recover

# 硬解析
gsbench run -s 621,624 -d 2m
```

601–606 会修改同一组 `plan_data` 计划状态，一次只能运行一个场景；602–606 使用时只需将上述三条命令换成同一编号。`init` 在终端一中使用固定 worker 持续造流；`--duration` 从终端一显示 `RUNNING`、worker 开始造流时计时，不包含此前的基线准备时间，并需覆盖故障与恢复观察时间。第一次 Ctrl+C 会立即停止流量并退出 `init`，不需要再输入一次。

601 从 v1.1.4 起持续执行由 `(lookup_key,dist_key)` 复合唯一索引支撑的 lookup key 点查；`fault` 删除专用唯一索引，使新查询退化为全表扫描，`recover` 重建该索引。已有大数据集首次转换索引以及后续恢复可能耗时。

`fault` 必须在同编号 `init` 仍存活时执行；它同步完成一次故障注入后退出，不会停止终端一的流量。`recover` 同步、幂等地恢复同编号场景后退出；即使 `init` 已因 duration 到期或 Ctrl+C 退出，仍可单独执行 `recover`。

六个场景的故障动作、状态和异常恢复说明见[601–606 三阶段执行计划跳变手册](PLAN_601_606_CN.md)。

`run` 也接受完整名称和逗号组合，例如 `gsbench run -s io_sequential_read,network_client_egress -d 2m`；601–606 的三阶段语法除外。101–103、201–202 是可精确预算的固定 worker 场景，不能与其他并发模型的场景放在同一次运行中；201/202 使用命令行 worker/work_mem 覆盖时一次只能选择其中一个。

201/202 会先用 `sort_data` 自动标定有界工作集，只接受实际 Sort/Hash 内存达到输入 `work_mem` 的 90%–97% 且不落盘的结果。随后每个 worker 建立一个服务端游标并完成首次 `FETCH`；所有 worker 就绪后才开始计算 `--duration`。第一次 Ctrl+C 会立即结束本地进程，数据库通过连接断开释放对应事务和游标；之后可执行 `gsbench restore --run-id RUN_ID` 收敛运行状态。

401/402 的 `--percent N` 接受 1–100，并只覆盖本次选中的 401/402；同一命令选中两者时使用同一个值，其他场景忽略该参数。目标必须严格大于运行前基线：401 的分母是 `max_connections - sysadmin_reserved_connections`，例如当前 80%、目标 90% 时只新建补足差值所需的连接；402 的占用率是 `global_threadpool_status` 中 `(actual-idle)/actual`，active-backend fallback 不会被当作百分比成功证据。

`safety.max_connections` 和 `safety.max_workers` 仍是其他容量感知场景的硬上限，但 401/402 明确忽略这两个人工上限：两者按数据库物理会话余量尝试达到请求百分比。数据库真实拒绝连接、真实指标缺失或 `--duration` 到期前仍未达标都会使对应场景失败，不会自动降低目标，即使 `run.validation_enabled=false` 也不会降级。同一次运行中的其他场景继续自己的生命周期，最终命令聚合为非零退出码。达到目标后，401 不再补连接，402 连续 3 次确认后冻结 worker 数；已建立的本次连接/worker 保持到 duration 结束，丢失则失败。正常结束和场景失败都会自动关闭本次 tagged 会话；402 只关闭客户端会话和本地 worker，数据库自有 idle worker 的回收由数据库管理。

第一次 Ctrl+C 仍由操作系统立即终止进程；客户端 socket 断开后数据库会结束对应会话并回滚未提交事务。下次运行仍会执行 stale recovery。只有旧 run 身份可完整证明为非空的 401/402-only、database/local recovery action 均为零且恢复锁已正常释放时，旧恢复失败才记录原始错误为 WARN 并允许新 run 使用新 ID 继续；发现失败、未知场景、任何 action（包括 402 自动启用线程池的实例参数 mutation）或其他场景仍会阻断。

GaussDB 默认可能使用 `explain_perf_mode=pretty`，而 201/202 需要 normal 格式中的 Sort 方法、Hash Batches 和 Memory Usage 才能证明算子未落盘。gsbench 会读取原始模式，并只在每次标定事务中执行 `SET LOCAL explain_perf_mode=normal`；事务 `ROLLBACK` 后自动恢复，不修改用户、数据库或其他会话的配置。结果证据中的 `original_explain_perf_mode` 和 `explain_perf_mode` 分别记录原始与实际标定模式。pretty 输出的 `Peak Memory` 不会被当作完整的 Hash 溢写证据。

## 生命周期与恢复

推荐顺序是 `scenarios`、`doctor`、`init --dry-run`、`init`、`run --dry-run`、`run`、`status`。发布配置默认 `run.validation_enabled=false`，普通场景按 `preflight → prepare → ramp → hold → stop → restore` 执行；601–606 改用上述 `init → fault → recover` 三阶段。改为 `true` 后增加计划/场景验证和恢复结果验证阶段。新的变更型运行会先恢复 stale run。

`gsbench restore` 是普通场景的统一恢复入口。无 `--run-id` 时，它发现全部活动/失败运行和未完成动作；指定 `--run-id RUN_ID` 时只处理该运行。普通 `gsbench run` 完成时会无条件调用同一恢复协调器；601–606 的人工故障则使用同编号 `gsbench run <code> recover`。恢复前可先 dry-run：

```bash
gsbench restore --dry-run
gsbench restore --run-id RUN_ID --dry-run
gsbench restore
```

dry-run 仅发现数据库 journal 和本地 ledger 中的动作：不申请恢复锁、不改运行状态、不终止会话、不执行逆操作，也不会因 ledger 缺失而创建文件。看到 `RESTORE_FAILED` 或 stale run 时，先执行 dry-run，再执行 restore 和 status。

运行锁和恢复锁使用独立的单连接会话，不复用正在承受压力的主控制连接池；锁键以安全 SQL 字面量发送，不使用驱动绑定参数。已知连接中断、连接耗尽或临时内存不足会在清理不确定会话后重新连接，权限、语法、锁占用和未知错误仍立即失败并保持 fail-closed。

## 风险与配置

Risk A 无额外开关。Risk B 还要求 `safety.allow_admin_mutation=true` 和 `--allow-risk B`（或 C）；Risk C 还要求 `safety.allow_infrastructure_fault=true`、`--allow-risk C`、已注册的外部 provider 与恢复 ledger。发布配置将 B/C 全部保持关闭，且内置 provider 为 fail-closed 的 `none`。

配置只包含实际读取的键：`database.*`、`run.*`、`data.*`、`safety.*`、`fault_provider.*`，以及现有 CPU、连接池、线程池和 vacuum 场景的 `scenario.*` 参数。不要在配置中写密码；使用 `database.password_env=GSBENCH_PASSWORD`，或以 `database.password_config` 引用同发布目录的 gstop 配置。日志会脱敏 DSN 密码，但配置文件仍应限制权限。

`run.validation_enabled=false` 只跳过模型预估、一般场景结果门槛、数据布局一致性和执行计划形态校验。容量检查、401/402 百分比目标、物理大小测量、未知数据库版本拒绝、真实 DDL/DML/负载错误，以及恢复锁、journal/ledger 持久化、逆操作、计划基线修复和其他恢复安全边界仍然强制执行；关闭验证不能把这些失败降级为成功。需要其他模型、结果、布局和计划形态的严格判定时设置：

```ini
[run]
validation_enabled = true
```

生产使用前，应启用验证，并在同版本测试环境完成 `doctor`、`init --dry-run`、最短运行和 restore 演练。逐场景 live 验证的配置、门禁和判定方式见[完整逐场景验证流程](FULL_SCENARIO_TEST.md)。

## 结果

| 结果 | 含义 | run 退出码 |
|---|---|---:|
| `SUCCESS` | 负载目标和恢复验证成功 | 0 |
| `NOT_APPLICABLE` | 不适用于已探测产品/拓扑 | 0 |
| `DEGRADED` | 运行但关键指标证据不可用，且使用声明降级路径 | 3 |
| `NOT_IMPLEMENTED` | 已编目但当前没有 factory | 1 |
| `FAILED` | 配置、前置检查、负载、验证或停止失败 | 1 |
| `RESTORE_FAILED` | 恢复或恢复验证失败，必须重试 restore | 1 |
