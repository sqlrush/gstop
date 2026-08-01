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
# 内存、I/O、网络资源场景
gsbench run -s 201,301,321 -d 2m

# 两个独立锁场景；或一个锁冲突矩阵场景
gsbench run -s 501,504 -d 2m
gsbench run -s 520 -d 2m

# 执行计划变化与硬解析
gsbench run -s 601 -d 2m
gsbench run -s 605 -d 2m
gsbench run -s 621,624 -d 2m
```

601–606 会修改同一组 `plan_data` 计划状态，必须每次只选择一个并逐个串行运行；配置层会拒绝在同一次 run 中组合其中两个或更多场景。

`run` 也接受完整名称和逗号组合，例如 `gsbench run -s memory_workmem_sort,io_sequential_read -d 2m`。

## 生命周期与恢复

推荐顺序是 `scenarios`、`doctor`、`init --dry-run`、`init`、`run --dry-run`、`run`、`status`。发布配置默认 `run.validation_enabled=false`，运行按 `preflight → prepare → ramp → hold → stop → restore` 执行；改为 `true` 后增加计划/场景验证和恢复结果验证阶段。新的变更型运行会先恢复 stale run。

`gsbench restore` 是所有场景共用的恢复入口。无 `--run-id` 时，它发现全部活动/失败运行和未完成动作；指定 `--run-id RUN_ID` 时只处理该运行。每次 `gsbench run` 完成时都会无条件调用同一个恢复协调器。恢复前可先 dry-run：

```bash
gsbench restore --dry-run
gsbench restore --run-id RUN_ID --dry-run
gsbench restore
```

dry-run 仅发现数据库 journal 和本地 ledger 中的动作：不申请恢复锁、不改运行状态、不终止会话、不执行逆操作，也不会因 ledger 缺失而创建文件。看到 `RESTORE_FAILED` 或 stale run 时，先执行 dry-run，再执行 restore 和 status。

## 风险与配置

Risk A 无额外开关。Risk B 还要求 `safety.allow_admin_mutation=true` 和 `--allow-risk B`（或 C）；Risk C 还要求 `safety.allow_infrastructure_fault=true`、`--allow-risk C`、已注册的外部 provider 与恢复 ledger。发布配置将 B/C 全部保持关闭，且内置 provider 为 fail-closed 的 `none`。

配置只包含实际读取的键：`database.*`、`run.*`、`data.*`、`safety.*`、`fault_provider.*`，以及现有 CPU、连接池、线程池和 vacuum 场景的 `scenario.*` 参数。不要在配置中写密码；使用 `database.password_env=GSBENCH_PASSWORD`，或以 `database.password_config` 引用同发布目录的 gstop 配置。日志会脱敏 DSN 密码，但配置文件仍应限制权限。

`run.validation_enabled=false` 只跳过模型预估、场景结果门槛和数据布局一致性判断。容量检查、物理大小测量、未知数据库版本拒绝、真实 DDL/DML/负载错误，以及恢复锁、journal/ledger 持久化、逆操作和其他恢复安全边界仍然强制执行；关闭验证不能把这些失败降级为成功。需要模型、结果和布局的严格判定时设置：

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
