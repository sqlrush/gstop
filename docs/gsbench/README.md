# gsbench

`gsbench` 是面向 openGauss/GaussDB 的轻量故障与压力场景模拟工具。v1.1.8 会在运行前只读探测产品、拓扑、数据、元数据和执行计划；不符合场景预期的项目只输出 `PRECHECK_WARN`，不自动优化，也不阻挡场景执行。

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

选择不适用的产品或拓扑、缺少指标/视图、数据或计划形态不符合预期时，v1.1.8 会记录告警并继续执行能够执行的部分；已编目但没有 factory 的代码仍为 `NOT_IMPLEMENTED`。真实 SQL、连接或故障注入失败只使当前场景失败，同一命令中的后续场景继续运行。

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

# 终端二：等同编号 init 进入 RUNNING 后注入故障，再展示恢复 SQL
gsbench run 601 fault
gsbench run 601 recover

# 硬解析
gsbench run -s 621,624 -d 2m
```

601–606 会修改同一组 `plan_data` 计划状态，一次只能运行一个场景；602–606 使用时只需将上述三条命令换成同一编号。`init` 在终端一中使用固定 worker 持续造流；`--duration` 从终端一显示 `RUNNING`、worker 开始造流时计时，不包含此前的基线准备时间，并需覆盖故障与恢复观察时间。第一次 Ctrl+C 会立即停止流量并退出 `init`，不需要再输入一次。

601 持续执行由 `(lookup_key,dist_key)` 复合唯一索引支撑的 lookup key 点查；`fault` 删除专用唯一索引，使新查询退化为全表扫描。`recover` 只展示重建索引的 DDL，用户确认后自行执行。

`fault` 必须在同编号 `init` 仍存活时执行；它同步完成一次故障注入后退出，不会停止终端一的流量。`recover` 不执行任何 DDL/DML；即使 `init` 已因 duration 到期或 Ctrl+C 退出，仍可用它查看该编号的恢复建议。

索引方法未显式限定时，openGauss/GaussDB 的 `btree` 与 Ustore 的 `ubtree` 视为等价；显式要求某一种方法时仍严格匹配。gsbench 不会因服务端把 `CREATE INDEX` 规范化为 `ubtree` 而误报索引定义错误。

六个场景的故障动作、状态和异常恢复说明见[601–606 三阶段执行计划跳变手册](PLAN_601_606_CN.md)。

`run` 也接受完整名称和逗号组合，例如 `gsbench run -s io_sequential_read,network_client_egress -d 2m`；601–606 的三阶段语法除外。101–103、201–202 是可精确预算的固定 worker 场景，不能与其他并发模型的场景放在同一次运行中；201/202 使用命令行 worker/work_mem 覆盖时一次只能选择其中一个。

201/202 会只读检查工作集、Sort/Hash 内存和落盘情况；不满足预期时记录告警并继续使用用户给定的 worker 与 `work_mem`，不自动调小数据或参数。第一次 Ctrl+C 会结束本地进程，数据库通过连接断开释放对应事务和游标。

401/402 的 `--percent N` 接受正整数，并只覆盖本次选中的 401/402；同一命令选中两者时使用同一个值。401 的分母是 `max_connections - sysadmin_reserved_connections`，例如当前 80%、目标 90% 时只新建补足差值所需的连接；目标不高于基线、超过 100% 或物理余量不足时会告警，但不再因策略上限提前拒绝执行。

`safety.max_connections`、`safety.max_workers` 等旧策略上限仅为兼容配置并输出告警，不再裁剪或拒绝场景目标。数据库真实拒绝连接、SQL 或 worker 执行失败仍使当前场景失败；后续场景不被阻塞。达到目标后 401/402 停止增加并保持本次资源到 duration 结束；正常结束、失败或 Ctrl+C 会关闭本次创建的 tagged 会话。

第一次 Ctrl+C 会停止当前进程拥有的负载并关闭 tagged 会话。后续运行只报告 stale run 与待恢复动作，不执行 stale recovery，也不阻塞新的场景；使用 `restore` 查看全部人工恢复 SQL。

GaussDB 默认可能使用 `explain_perf_mode=pretty`，而 201/202 需要 normal 格式中的 Sort 方法、Hash Batches 和 Memory Usage 才能证明算子未落盘。gsbench 会读取原始模式，并只在每次标定事务中执行 `SET LOCAL explain_perf_mode=normal`；事务 `ROLLBACK` 后自动恢复，不修改用户、数据库或其他会话的配置。结果证据中的 `original_explain_perf_mode` 和 `explain_perf_mode` 分别记录原始与实际标定模式。pretty 输出的 `Peak Memory` 不会被当作完整的 Hash 溢写证据。

## 生命周期与恢复

推荐顺序是 `scenarios`、`doctor`、`init --dry-run`、`init`、`run --dry-run`、`run`、`status`、`restore`。普通场景按 `preflight → prepare → ramp → hold → stop` 执行；不会在启动或结束时自动恢复元数据。601–606 使用 `init → fault → recover(展示)` 三阶段。

`gsbench restore` 是全局只读恢复规划入口：扫描配置的 gsbench schema、数据库 journal、本地 ledger 和记录状态，展示全部待执行 DDL/DML 或人工动作；指定 `--run-id RUN_ID` 时只展示该运行。`gsbench run 601-606 recover` 只展示对应场景的恢复 DDL/DML。两者均不会执行输出内容、不会修改 journal/ledger，也不会把动作标记为已恢复：

```bash
gsbench restore --run-id RUN_ID --dry-run
gsbench restore
gsbench run 602 recover
```

`restore --dry-run` 与 `restore` 的只读语义相同，只为兼容旧脚本保留。输出项会标记 `PENDING`、`ALREADY_RESTORED`、`UNVERIFIED` 或 `CONFLICT`；冲突不会被静默覆盖。用户执行 SQL 后可再次运行相同命令复核。

`gsbench stop` 只请求停止并取消/终止 gsbench 标记会话，不执行元数据恢复。普通 `cleanup` 也只清理会话；只有显式 `cleanup --data` 才在 schema 所有权校验后删除测试 schema。

## 风险与配置

Risk A 无额外开关。Risk B 还要求 `safety.allow_admin_mutation=true` 和 `--allow-risk B`（或 C）；Risk C 还要求 `safety.allow_infrastructure_fault=true`、`--allow-risk C`、已注册的外部 provider 与恢复 ledger。发布配置将 B/C 全部保持关闭，且内置 provider 为 fail-closed 的 `none`。

不要在配置中写密码；使用 `database.password_env=GSBENCH_PASSWORD`，或以 `database.password_config` 引用同发布目录的 gstop 配置。日志会脱敏 DSN 密码，但配置文件仍应限制权限。

`run.validation_enabled` 在 v1.1.8 中仅为兼容配置保留。产品/拓扑、容量、物理大小、数据布局、百分比目标和执行计划形态检查始终只记录告警，不自动优化、不阻挡运行。仍然硬失败的边界只有：配置/SQL 标识符等语法与注入防护、Risk B/C 显式授权、持久变更前 journal 写入，以及实际 SQL、连接、worker 或故障注入错误。

```ini
[run]
validation_enabled = true
```

请只在可承受故障的测试环境使用；告警不会保护数据库稳定性。执行 `restore` 输出的 DDL/DML 前，由用户自行审核对象、锁等待和业务影响。

## 结果

| 结果 | 含义 | run 退出码 |
|---|---|---:|
| `SUCCESS` | 场景实际执行完成且没有告警 | 0 |
| `COMPLETED_WITH_WARNINGS` | 场景执行完成，但前置/结果/恢复状态存在告警 | 0 |
| `NOT_APPLICABLE` | 旧结果值；v1.1.8 运行路径通常降为告警并继续 | 0 |
| `DEGRADED` | 运行但关键指标证据不可用，且使用声明降级路径 | 3 |
| `NOT_IMPLEMENTED` | 已编目但当前没有 factory | 1 |
| `FAILED` | 实际 SQL、连接、负载、故障注入或停止失败 | 1 |
| `RESTORE_FAILED` | 旧结果值；恢复命令现为只读规划 | 1 |
