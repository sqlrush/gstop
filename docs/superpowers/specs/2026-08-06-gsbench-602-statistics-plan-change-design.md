# gsbench v1.1.7 场景 602 统计信息计划跳变设计

## 目标

在 gsbench v1.1.7 中完全替换原 602“索引置为不可用”故障。新 602 与
601 使用完全相同的高并发唯一键点查流量，但不删除或禁用索引，而是通过伪造
`lookup_key` 的列统计信息，使执行计划从 `plan_data_lookup_idx` 唯一索引扫描
退化为全表扫描。恢复时撤销统计信息覆盖并重新采样，使全部点查恢复使用该索引。

`fault` 和 `recover` 都以数据库当前的实际执行计划为强制成功条件。统计信息 SQL
执行成功但执行计划没有达到目标时，命令必须返回失败，不能记录为成功。

## CLI 和兼容性

三阶段命令保持不变：

```bash
gsbench run 602 init --worker 10 --duration 5m
gsbench run 602 fault
gsbench run 602 recover
```

602 的规范场景名改为 `planchange_stats_lookup`。场景解析继续接受旧名称
`planchange_index_unusable` 作为兼容别名，避免旧配置和脚本立即失效；帮助、日志和
新写入的状态使用新名称。

升级前已经由 v1.1.6 注入且尚未恢复的旧 602 不迁移 journal 内容。恢复协调器按
journal 中保存的原逆操作执行 `ALTER INDEX ... REBUILD`，完成旧故障恢复。存在旧
活动故障时，不允许直接注入新版 602 故障。

## 流量和基线

602 复用 601 的三个候选 SQL，不复制另一套可能漂移的字面量定义：

```sql
SELECT id,payload FROM <schema>.plan_data WHERE lookup_key=1;
SELECT id,payload FROM <schema>.plan_data WHERE lookup_key=500000;
SELECT id,payload FROM <schema>.plan_data WHERE lookup_key=1000000;
```

基线索引仍为：

```sql
CREATE UNIQUE INDEX plan_data_lookup_idx
ON <schema>.plan_data (lookup_key,dist_key);
```

`init` 放流前除执行现有计划基线修复外，还必须保证 `lookup_key` 没有人工
`n_distinct` 覆盖并已执行 `ANALYZE`。三条候选 SQL 必须全部包含
`plan_data_lookup_idx`，任何一条为全表扫描都不能启动 602 流量。

602 使用 literal/simple query 路径，保持 worker、连接数、SQL 顺序和请求速率与
601 一致。统计信息变化后，每次请求都能使用数据库的当前计划，而不会永久复用
故障前的客户端 prepared plan。

## 故障注入

故障通过现有持久化 journal 分成可独立恢复的 mutation：

```sql
ALTER TABLE <schema>.plan_data
  ALTER COLUMN lookup_key SET (n_distinct=1);
ANALYZE <schema>.plan_data(lookup_key);
```

`n_distinct=1` 把实际接近行数规模的唯一值基数伪装成单一值。查询只限定复合唯一
索引的首列 `lookup_key`，优化器不能从 `(lookup_key,dist_key)` 的复合唯一性推导
首列本身唯一，因此错误基数会把等值谓词估算成大结果集并选择全表扫描。

执行故障 SQL 后，`fault` 必须对全部三个候选 SQL 执行 `EXPLAIN`。只有三条计划
都包含 `Seq Scan`，且都不包含 `plan_data_lookup_idx`，才把故障阶段标记为活动并
输出成功。

如果任一计划未跳变，`fault` 在同一维护超时范围内立即调用 journal 恢复路径，
撤销 `n_distinct` 覆盖并重新 `ANALYZE`。自动恢复也通过基线后置条件后，命令返回
非零并说明未能制造计划跳变；自动恢复失败时同时保留待恢复 journal，明确要求
执行 `602 recover`。任何情况下都不能把未验证的故障标记为成功。

## 恢复

journal 的逻辑逆操作为：

```sql
ALTER TABLE <schema>.plan_data
  ALTER COLUMN lookup_key RESET (n_distinct);
ANALYZE <schema>.plan_data(lookup_key);
```

恢复计划必须保证 `RESET` 在恢复用 `ANALYZE` 之前执行。通用计划基线修复增加
`lookup_key` 检查和相同的幂等修复步骤，作为 journal 逆操作失败、中断或物理状态
已经提前恢复时的兜底。基线修复成功后，恢复协调器按现有动作对账机制消除已经
物理恢复的待处理 journal 动作，不重复执行有风险的逆操作。

`recover` 只有同时满足以下后置条件才输出成功并把运行状态写为已恢复：

1. `pg_attribute.attoptions` 中不存在 `lookup_key` 的 `n_distinct` 覆盖；
2. 三个候选 SQL 的计划都包含 `plan_data_lookup_idx`；
3. 三个候选 SQL 的计划都不包含 `Seq Scan`；
4. 该故障运行没有待恢复的数据库 journal 动作。

恢复 SQL、基线修复或任一后置条件失败时，`recover` 返回非零，故障运行继续保持
可恢复状态，不能误标成功。重复执行 `recover` 必须安全收敛到上述同一基线。

## 能力和边界

新版 602 依赖现有 `plan_ndistinct` 能力探测，不再依赖
`plan_index_unusable`。旧能力探测暂时保留，以便恢复旧 journal 和兼容现有环境
报告，但不作为新 602 的运行门槛。

不直接更新 `pg_statistic`，不使用 `enable_indexscan=off` 等会话级优化器开关，也
不删除、禁用或重建 `plan_data_lookup_idx`。这些手段分别存在系统目录安全风险、
无法影响其他 worker 会话，或不符合“仅修改统计信息”的故障定义。

数据库锁等待、权限不足、`ANALYZE` 失败和维护超时仍按真实错误处理。代码通过
强制后置校验保证不会误报成功；特定 GaussDB/openGauss 版本是否能稳定形成目标
计划，最终仍需用发布目标环境的真实 602 三阶段验收确认。

## 测试和验收

实现按 TDD 逐项增加失败测试：

- 602 与 601 的候选 SQL 完全相同，且期望基线索引均为
  `plan_data_lookup_idx`；
- 602 mutation 只设置/重置 `lookup_key` 的 `n_distinct` 并执行列级
  `ANALYZE`，恢复顺序为先重置、后分析；
- 新场景名、旧名称别名和能力映射正确，旧 602 journal 仍按保存的逆操作恢复；
- fault 仅在三个计划全部变为 `Seq Scan` 后成功，验证失败会自动恢复且不标记活动；
- recover 仅在统计覆盖清除、三个计划全部恢复索引扫描且 journal 清空后成功；
- 基线修复能幂等清理中断遗留的 `lookup_key n_distinct` 覆盖；
- 更新 602 中文说明、CLI 帮助和 v1.1.7 版本信息。

聚焦测试通过后，只执行最小必要验证：

```bash
go test ./internal/gsbench -count=1
go vet ./internal/gsbench
go build ./cmd/gsbench
```

发布前在目标 GaussDB/openGauss 环境以至少 100 万行 `plan_data` 完成真实验收：

1. `602 init` 三条 SQL 均使用 `plan_data_lookup_idx`；
2. `602 fault` 成功后三条 SQL 均为 `Seq Scan`；
3. `602 recover` 成功后三条 SQL 均重新使用 `plan_data_lookup_idx`；
4. journal 待恢复数为零，并验证重复 `recover` 安全。
