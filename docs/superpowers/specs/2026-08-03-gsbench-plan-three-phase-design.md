# gsbench 601–606 三阶段执行计划跳变设计

## 目标

把 601–606 从单命令自动完成“采样、故障、压测、恢复”的流程，重构为用户可从
两个终端手工控制的三个阶段：

```text
终端一：gsbench run <scenario> init    持续制造基础流量
终端二：gsbench run <scenario> fault   注入故障后退出
终端二：gsbench run <scenario> recover 恢复故障后退出
```

601–606 继续表示六种执行计划跳变故障类型。三阶段必须使用同一批 worker
连接和同一组 SQL；`fault` 与 `recover` 不创建负载 worker，也不停止终端一的
流量。

## 命令行接口

六个场景使用统一语法：

```text
gsbench run 601 init --worker 10 --duration 1m
gsbench run 601 fault
gsbench run 601 recover
```

`602`～`606` 只需替换场景编号。正式帮助展示用户指定的 `--worker`；为避免和
101–103、201–202 已发布的 `--workers` 参数产生不必要的不兼容，两个拼写都接受，
但不能同时提供。

参数语义如下：

- `--worker N` 只用于 601–606 的 `init` 阶段，表示长期保持的 workload
  数据库连接数，必须大于零。
- `--duration D` 是终端一基础流量进程的总运行时间。`1m` 表示运行一分钟后
  自动停止；第一次 Ctrl+C 可以提前立即停止流量并退出。
- `fault` 和 `recover` 不接受 worker 或 duration 参数。
- 原来的 `gsbench run 601`～`gsbench run 606` 不再自动注入和恢复故障，改为
  返回三阶段命令提示，防止用户误以为基础流量已经建立。
- 601–606 仍必须单独选择，不能和其他场景代码在同一次命令中组合。

推荐实际操作：

```text
# 终端一
gsbench run 601 init --worker 10 --duration 5m

# 终端二，在终端一仍运行时执行
gsbench run 601 fault
gsbench run 601 recover
```

一分钟同样合法，但 duration 必须覆盖用户希望观察的基础、故障和恢复三个时间段。
索引重建可能超过一分钟；此时终端一会按 duration 准时停止流量，终端二仍等待恢复
SQL 完成。

## 实例发现与并发规则

`fault` 和 `recover` 没有 `run-id` 参数，因此通过目标数据库、dataset schema 和
场景编号发现当前实验。控制状态保存在数据库元数据中，不能使用只在本机可见的临时
文件。

同一目标数据库和 schema 同时只允许一个 601–606 实验处于活动状态。第二个
`init` 必须在创建 worker 前失败并显示已有场景和 run ID。这样既避免控制命令选错
实例，也避免六种故障同时修改 `plan_data` 后污染性能证据。

流量存活和数据库 mutation 使用两个正交状态，避免 duration 到期后无法表达“流量已
结束但故障仍待恢复”：

```text
traffic_state:  RUNNING -> ENDED

mutation_state: BASELINE -> FAULTING -> FAULT_ACTIVE -> RECOVERING -> RECOVERED
```

控制记录至少包含 run ID、场景、schema、init 进程心跳、phase、阶段时间戳和最后错误。

- `fault` 只允许在匹配场景的 `traffic_state=RUNNING` 且
  `mutation_state=BASELINE` 实例上执行。没有活跃 `init` 时拒绝注入，不能在无基础
  流量时静默修改数据库；一个实验只执行一次 fault/recover 周期。
- 已处于 `FAULT_ACTIVE` 时再次执行 `fault` 返回 `ALREADY_FAULTED`，不重复 DDL。
- `recover` 在 `FAULT_ACTIVE` 时执行逆向动作；已经恢复时返回
  `ALREADY_RECOVERED`。
- 即使 init 已因 duration 或 Ctrl+C 退出，`recover` 仍可依据持久化 journal 修复
  已注入故障。这是唯一允许在没有活跃 init 时执行的控制动作。
- `fault` 和 `recover` 使用数据库级互斥控制，防止两个终端同时修改同一个实验。

## 基础流量

`init` 首先收敛到该场景的标准基线对象状态并记录候选 SQL 的基线执行计划，然后
创建准确的 worker 数。每个 worker 持有一个 tagged 数据库连接，在 duration 内
连续循环执行候选 SQL，不做 CPU、QPS 或延迟反馈控制，也不在请求之间主动休眠。

一个场景包含多条候选 SQL 时，worker 使用确定性的错位轮转，保证三个阶段每条 SQL
的流量比例不变。例如三个 SQL、两个 worker：

```text
worker-1: SQL-1 -> SQL-2 -> SQL-3 -> ...
worker-2: SQL-2 -> SQL-3 -> SQL-1 -> ...
```

查询必须使用 literal/simple execution 路径，避免客户端永久复用故障前的 prepared
plan；数据库在统计信息或索引发生变化后必须有机会重新规划下一次请求。

init 按数据库控制状态切分 BASELINE、FAULT 和 RECOVERED 三段指标，但状态轮询不得
阻塞 worker。每个阶段按候选 SQL 记录 operations、errors、QPS、P50/P95/P99、执行
计划签名和结果指纹。`fault`/`recover` 只改变 phase，不改变 worker 数、连接、SQL
顺序或流量速度。

## 故障和恢复 SQL

所有 mutation 在执行前写入现有持久化 journal。`fault` 只有在对应 SQL 真正执行
完成并把状态更新为 `FAULT_ACTIVE` 后才返回；“执行后马上结束”表示命令不常驻，
不表示异步提交后提前报告成功。`recover` 按 journal 逆序执行恢复 SQL，并在恢复
对象定义得到确认后返回。

### 601 低精度统计信息

流量使用 `stats_target_key=1/2/3/4` 的四条聚合查询。

```sql
-- fault
ALTER TABLE <schema>.plan_data
  ALTER COLUMN stats_target_key SET STATISTICS 1;
ANALYZE <schema>.plan_data(stats_target_key);

-- recover
ALTER TABLE <schema>.plan_data
  ALTER COLUMN stats_target_key SET STATISTICS -1;
ANALYZE <schema>.plan_data(stats_target_key);
```

### 602 索引不可用

流量使用 `index_unusable_key` 的三个固定范围聚合查询。

```sql
-- fault
ALTER INDEX <schema>.plan_index_unusable_idx UNUSABLE;

-- recover
ALTER INDEX <schema>.plan_index_unusable_idx REBUILD;
```

### 603 伪造 n_distinct

流量使用 `stats_ndistinct_key=424242/777777` 的两条聚合查询。

```sql
-- fault
ALTER TABLE <schema>.plan_data
  ALTER COLUMN stats_ndistinct_key SET STATISTICS 1;
ALTER TABLE <schema>.plan_data
  ALTER COLUMN stats_ndistinct_key SET (n_distinct=1);
ANALYZE <schema>.plan_data(stats_ndistinct_key);

-- recover
ALTER TABLE <schema>.plan_data
  ALTER COLUMN stats_ndistinct_key RESET (n_distinct);
ALTER TABLE <schema>.plan_data
  ALTER COLUMN stats_ndistinct_key SET STATISTICS -1;
ANALYZE <schema>.plan_data(stats_ndistinct_key);
```

### 604 删除扩展统计信息

流量使用 `stats_corr_a`、`stats_corr_b` 的三个固定相关范围聚合查询。

```sql
-- fault
ALTER TABLE <schema>.plan_data
  DELETE STATISTICS ((stats_corr_a,stats_corr_b));
ANALYZE <schema>.plan_data(stats_corr_a,stats_corr_b);

-- recover（同一数据库 session）
ALTER TABLE <schema>.plan_data
  ADD STATISTICS ((stats_corr_a,stats_corr_b));
SET default_statistics_target=-2;
ANALYZE <schema>.plan_data ((stats_corr_a,stats_corr_b));
RESET default_statistics_target;
```

### 605 删除索引

流量使用 `index_drop_key` 的三个固定范围聚合查询。

```sql
-- fault
DROP INDEX <schema>.plan_index_drop_idx;

-- recover
CREATE INDEX plan_index_drop_idx ON <schema>.plan_data
  (index_drop_key,dist_key,id);
```

### 606 错误索引列顺序

流量使用 `index_shape_lead=42` 和两个固定 `index_shape_tail` 范围的聚合查询。

```sql
-- fault
DROP INDEX <schema>.plan_index_shape_good_idx;
CREATE INDEX plan_index_shape_bad_idx ON <schema>.plan_data
  (index_shape_tail,index_shape_lead,dist_key,id);

-- recover（按 journal 逆序执行）
DROP INDEX IF EXISTS <schema>.plan_index_shape_bad_idx;
CREATE INDEX plan_index_shape_good_idx ON <schema>.plan_data
  (index_shape_lead,index_shape_tail,dist_key,id);
```

## 失败、退出和验证语义

连接失败、worker 数不足、控制状态冲突、fault/recover SQL 失败和对象恢复定义不正确
属于执行失败。计划未跳变、性能下降不明显或恢复性能尚未回到基线只输出 WARNING，
不因模型阈值强制结束；这与默认关闭 runtime validation 的现有策略一致。

第一次 Ctrl+C 立即停止 init 的 worker 并退出，不需要第二次 Ctrl+C。DDL 和统计信息
变更不会随客户端断开自动回滚；如果 Ctrl+C 时仍为 `FAULT_ACTIVE`，退出前必须在
终端明确打印场景、run ID 和 `gsbench run <scenario> recover` 命令。恢复 journal
保持可发现，后续 recover 不依赖 init 进程存活。

`fault` 或 `recover` 等待数据库锁、ANALYZE、REBUILD/CREATE INDEX 完成时可以被
Ctrl+C 取消；已经成功执行的每一步仍保留在 journal 中，recover 可继续执行剩余
逆向动作。

## 最小验证和交付

自动测试覆盖：

- 三阶段 CLI 语法、worker 参数别名、非法参数组合和旧命令提示；
- duration 到期和单次 Ctrl+C 停止持续流量；
- 跨进程活动实例发现、唯一活动实例约束和 phase 状态转换；
- fault/recover 幂等、无 init 时禁止 fault、init 退出后仍可 recover；
- 六个场景的 forward/inverse SQL 与 journal 顺序；
- worker 在 fault/recover 前后不被重建，SQL 配比保持不变；
- validation 关闭时计划/性能阈值只告警，真实 SQL/恢复错误仍失败。

完成后将版本从 `v1.1.2` 提升到 `v1.1.3`，运行 gsbench 包的最小必要 Go 测试，
构建 Linux ARM64 二进制，并在 `release` 下生成带三阶段简明使用说明的新压缩包。

## 非目标

- 不允许并发运行多个 601–606 实验。
- 不为计划场景增加 CPU、QPS 或延迟目标反馈控制。
- 不让 fault/recover 自己制造基础流量。
- 不把 fault/recover 改成异步后台任务或常驻守护进程。
