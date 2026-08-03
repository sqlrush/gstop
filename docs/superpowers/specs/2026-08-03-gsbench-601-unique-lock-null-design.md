# gsbench 601 唯一索引跳变与 501 NULL 扫描修复设计

## 目标

在同一个 `v1.1.4` 版本中完成两项修改：

1. 将 601 重构为高并发唯一键点查。基线使用专用复合唯一索引，
   `fault` 删除该索引使后续查询退化为全表扫描，`recover` 精确重建索引。
2. 修复 501 在 GaussDB ORA 兼容模式下读取锁证据时，将数据库 `NULL`
   直接扫描到 Go `string` 而失败的问题。

CLI 继续使用现有三阶段命令，不新增参数：

```bash
gsbench run 601 init --worker 10 --duration 5m
gsbench run 601 fault
gsbench run 601 recover
```

## 601 数据和索引设计

`plan_data.lookup_key` 由数据生成器写入行号，和 `dist_key` 一起可以作为稳定的
点查键。601 使用以下专用索引：

```sql
CREATE UNIQUE INDEX plan_data_lookup_idx
ON <schema>.plan_data (dist_key, lookup_key);
```

索引包含分布键 `dist_key`，避免分布式 GaussDB 对唯一索引的限制；不删除或修改
`plan_data` 的主键约束。现有同名普通索引由基线修复检测定义差异并重建为上述
唯一索引。第一次在已有大数据集上运行新版 601 时，这次索引转换可能耗时。

601 定义三个保证落在最小一百万行数据集内的字面量点查 SQL。每个 SQL 同时
限定 `dist_key` 和 `lookup_key`，并读取非索引列，使基线计划明确使用
`plan_data_lookup_idx` 的 `Index Scan`。`--worker N` 保持现有实现：N 个会话持续
轮换这三个候选 SQL，直到 duration 到期或进程被 Ctrl+C 终止。

## 601 三阶段状态变化

`init` 在放流前通过现有基线修复确保唯一索引定义正确，并用
`ExpectedBaselineToken=plan_data_lookup_idx` 检查基线计划（仅在运行时验证开启时
执行严格验证）。

`fault` 通过现有 journal 机制执行：

```sql
DROP INDEX <schema>.plan_data_lookup_idx;
```

活动 worker 不更换 SQL；DDL 完成后的新执行应失去可用点查索引并退化为
`Seq Scan`。`recover` 使用 canonical index definition 重建完全相同的
`CREATE UNIQUE INDEX`。journal 继续保证重复恢复和崩溃恢复的幂等性。

旧的 601 `stats_target_key SET STATISTICS 1` 故障不再由 601 使用；相关数据列和
索引暂时保留，避免本次修改扩大为数据格式迁移。

## 501 NULL-safe 锁证据

`observeLockEvidence` 查询返回的以下文本列全部视为可空：节点、对象、锁类型、
holder 模式、waiter 模式、blocker application name、waiter application name。
扫描边界统一使用 `database/sql.NullString`，成功扫描后再把 `.String` 复制到
`LockEvidence`。

这不依赖 SQL 中 `COALESCE(value, '')` 的行为，因此兼容将空字符串视为 `NULL`
的 GaussDB ORA 模式，也覆盖 `transactionid` 锁天然没有 relation name 的情况。
布尔值和等待秒数保持当前非空扫描方式；501 row-chain 对象名的既有回填逻辑
保持不变。集中式和分布式锁查询共用同一个 NULL-safe 扫描路径。

## 测试和验收

按 TDD 分别先增加失败测试，再修改生产代码：

- 601 canonical DDL 必须包含 `CREATE UNIQUE INDEX plan_data_lookup_idx` 和
  `(dist_key,lookup_key)`；三个基线 SQL 必须为复合键等值点查并期望该索引。
- 601 fault 必须删除专用唯一索引，inverse/recover 必须精确重建唯一索引；605
  保持原有行为。
- 锁证据驱动测试返回真实数据库 `NULL`，501 必须成功读取并保留 row-chain
  对象回填；集中式与分布式环境都覆盖。
- 聚焦测试通过后运行 `go test ./internal/gsbench -count=1`，再执行最小构建检查。
- 版本提升为 `v1.1.4`；构建 macOS 本地二进制和 Linux ARM64 发布包，Linux
  二进制在 `og5` 中完成 `version`/帮助烟测，不在 100GB 主 schema 上自动执行
  DROP/CREATE 大索引。

## 发布范围

更新 601–606 中文说明中 601 的 SQL、故障和恢复语义，注明第一次索引转换以及
DROP/CREATE 大索引可能等待锁或耗时。发布包沿用现有配置和安装文档布局，生成
SHA-256 校验信息。源代码、文档和版本提交到当前 `main`，验证后推送远端。
