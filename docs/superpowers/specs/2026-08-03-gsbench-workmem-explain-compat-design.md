# gsbench 201/202 EXPLAIN 格式兼容设计

## 问题与目标

GaussDB 的 `explain_perf_mode` 默认是 `pretty`。201/202 的内存标定解析器只接受
`normal` 格式中的 `Sort Method ... Memory/Disk` 和
`Buckets ... Batches ... Memory Usage`，因此 pretty 计划会被误报为没有 Sort/Hash
算子，负载在 Prepare 阶段结束。

本次修改让 201/202 在 openGauss、GaussDB 集中式和分布式环境中稳定取得 normal
格式的标定证据，同时保留严格的内存与溢写判定，不用不完整的 pretty 信息伪造成功。

## 会话与数据流

标定连接建立后、开始第一次探测前读取一次 `SHOW explain_perf_mode`，保存数据库原始
模式。每次探测继续使用独立事务，并在执行 `EXPLAIN (ANALYZE, BUFFERS)` 前执行：

```sql
SET LOCAL work_mem='<canonical-kB>';
SET LOCAL query_dop=1;
SET LOCAL explain_perf_mode=normal;
```

Hash 场景继续设置现有的 join GUC。`SET LOCAL explain_perf_mode` 只加入标定事务，
不加入正式压力 worker；探测完成后的 `ROLLBACK` 自动恢复原始模式，不改变用户、
数据库或其他会话的配置。

## 解析与错误处理

现有 normal 解析规则保持不变：Sort 必须报告 quicksort/Memory 或明确的 Disk，Hash
必须报告 Batches 和 Memory Usage。pretty 的 Peak Memory 不足以证明 Hash 未分批，
因此不增加宽松的 pretty 成功解析路径。

如果 SET 成功后仍收到 pretty 计划，错误必须包含：原始模式、请求模式 `normal`、
检测到的输出模式、算子类型，以及经过控制字符转义且有长度上限的计划片段。普通的
“确实没有目标算子”错误也附带相同上下文，避免把格式不兼容误诊为数据或 work_mem
问题。错误和证据不得包含连接密码。

成功的 `work_mem_kb` evidence details 记录原始和有效 explain 模式，使现场结果能够
证明兼容路径已经生效。

## 兼容边界

当前支持目录仅包含 openGauss、GaussDB 集中式和分布式；这些产品均支持
`explain_perf_mode`。读取或设置该参数失败时直接返回明确的 Prepare 错误，不继续使用
不可验证的算子证据。未来若增加 PostgreSQL 等产品，应先在 capability 层声明支持，
而不是在失败事务里吞掉 unknown-GUC 错误。

## 测试与验收

- 受控 SQL 驱动证明先读取原始模式，再在同一标定事务中强制 normal，最后 ROLLBACK。
- normal Sort/Hash 计划仍按现有规则成功解析。
- 模拟 GaussDB pretty 计划时，失败信息明确指出 pretty/normal 不兼容并包含截断计划。
- 成功 evidence 同时记录原始模式和有效模式。
- 运行 `go test ./internal/gsbench` 和 `go build ./cmd/gsbench`。

本次不修改全局/用户数据库参数，不放宽溢写判定，也不改变 worker 数、压测时长、
Ctrl+C 或恢复语义。
