# gsbench 201/202 可控 work_mem 压力设计

## 目标

重构 201 `memory_workmem_sort` 和 202 `memory_workmem_hash`，让用户只提供固定 worker 数、`work_mem` 和压力时长。gsbench 自动标定有界工作集，在不落盘的前提下让每个 worker 的主内存算子接近 `work_mem`，并保证第一次 Ctrl+C 由操作系统立即终止进程。

命令行：

```text
gsbench run 201 --workers 8 --work-mem 256MB --duration 1m
gsbench run 202 --workers 8 --work-mem 256MB --duration 1m
```

配置兼容默认值：201、202 均为 `workers=1`、`work_mem=256MB`。用户不输入行数；工作集范围由标定结果决定。

## CLI 与配置

- `--workers` 保持支持 101、102，并新增支持单独运行 201 或 202。
- `--work-mem` 只允许用于单独运行 201 或 202，接受正整数 `kB`、`MB`、`GB`，最小 64kB，内部统一为 kB 后再生成 SQL，禁止原始输入直接拼接。
- 配置键为 `scenario.memory_workmem_sort.workers/work_mem` 和 `scenario.memory_workmem_hash.workers/work_mem`。
- worker 必须不超过 `safety.max_workers`，也必须不超过 `safety.max_connections` 留给场景的连接预算。

## 数据和 SQL

两种场景都使用现有 `<schema>.sort_data`，以 `dist_key BETWEEN 1 AND K` 选择有界工作集，从现有 `(dist_key,id)` 主键获益，避免无前导 `id` 索引造成整表扫描。

201 只保留一个主导 Sort。标定 SQL 使用 `row_number() OVER (ORDER BY payload,sort_key DESC,id)`，外层消费 `max(rn)`，保证优化器不能删除完整排序。正式压力使用相同排序键的服务端游标。

202 只保留一个主导 Hash Join，不使用 `GROUP BY` 产生第二个 HashAggregate。右侧宽行子查询包含 512 字节 `payload`，通过 LEFT Hash Join 作为构建侧；会话关闭 NestLoop 和 MergeJoin、开启 HashJoin。正式压力游标第一次 FETCH 前必须完整建立内侧 Hash 表。

## 自动标定

每个场景启动时只用一个 tagged 会话执行标定，不给每个 worker 重复一轮。目标区间固定为 `work_mem` 的 90%～97%。

- 201 解析 `Sort Method` 和 `Memory`；只接受内存 quicksort，任何 `Disk`/external merge 都视为工作集过大。
- 202 解析 Hash 的 `Memory Usage` 和 `Batches`；只接受 `Batches=1`，大于1视为分批/落盘。
- 内存低于90%时扩大 `K`；超过97%或落盘时缩小 `K`；采用有界倍增加二分，最多16次。
- 找不到满足区间的工作集时，Prepare 明确失败并报告最大内存、首次落盘边界和 `work_mem`，不得把I/O压力伪报为内存压力。
- 标定属于负载定标，不受 `run.validation_enabled` 控制。

## Worker 生命周期和时长

Prepare 先标定并建立全部 worker 连接。Ramp 同时释放 worker：

1. `BEGIN`
2. `SET LOCAL work_mem=<canonical-kB>`、`SET LOCAL query_dop=1`
3. 202 额外设置 Hash Join GUC
4. `DECLARE ... CURSOR FOR <bounded SQL>`
5. `FETCH 1`，完成 Sort 或内侧 Hash 构建
6. 向场景报告 ready，保持事务、游标和连接

全部 worker ready 后才开始计算 `--duration`。时间到后 Stop 取消 worker，逐连接执行 `CLOSE`、`ROLLBACK` 并关闭 tagged 会话。任何 worker 在 ready 前失败会让 Ramp 立即失败。

## Ctrl+C

命令入口继续保留操作系统默认 SIGINT/SIGTERM 行为，不安装需要第二次信号的恢复处理器。第一次 Ctrl+C 直接终止 gsbench；TCP连接关闭后数据库自动释放游标、事务和执行内存。库内 context 取消路径仍执行有界 Stop，供测试和非进程调用使用。

## 证据和测试

即使模型校验关闭，也输出 workers、work_mem_kB、标定范围、标定算子内存、是否落盘和实际持续时间。测试覆盖 CLI/配置、SQL形状、计划解析、标定决策、worker ready/持有/清理、context 取消和现有单次 SIGINT 进程测试。
