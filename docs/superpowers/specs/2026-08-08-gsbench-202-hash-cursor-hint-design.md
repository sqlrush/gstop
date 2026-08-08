# gsbench 202 游标 Hash Join Hint 修复设计

## 问题与目标

场景 202 的标定 SQL 通过 `EXPLAIN ANALYZE` 全量消费结果，现场得到
`Hash Left Join` 和约 48MB Hash 内存；正式 worker 则使用
`DECLARE CURSOR` + `FETCH 1` 持有执行器。openGauss 为游标选择首行优先计划，5 个
worker 全部退化为 `Nested Loop Left Join`，每会话仅约 1.04MB，导致压测没有形成
预期的 Hash 内存压力。

目标是在不改变 worker 数、`work_mem`、标定范围、持续时间和清理语义的前提下，让
202 的标定 SQL 与正式游标 SQL 都明确绑定同一个 Hash Join 形态。

## 方案选择

采用查询级 openGauss Plan Hint：

```sql
/*+ leading((p h)) hashjoin(p h) set(enable_index_nestloop off) */
```

- `leading((p h))` 固定 `p` 为外侧、`h` 为内侧构建侧。
- `hashjoin(p h)` 指定两侧使用 Hash Join。
- `set(enable_index_nestloop off)` 只在该 SQL 的优化阶段关闭 openGauss 独立的
  index-nestloop 路径。现场已验证：只使用 `hashjoin` 时游标仍走 Nested Loop；加入
  该查询级参数 Hint 后，游标会话保留约 47.7MB `HashBatchContext`。

不采用仅设置 `cursor_tuple_fraction=1` 的方案，因为它只是改变成本偏好，不是绑定
Join 方法；也不采用新增会话级 `SET LOCAL enable_index_nestloop=off` 作为唯一方案，
因为影响范围大于单条目标 SQL，且不能直接表达用户要求的 Hash 绑定。

## 代码边界

在 `scenario_workmem.go` 中定义一个 202 专用 Hint 常量，并把它紧跟在 Hash 标定 SQL
和 Hash 游标 SQL 的 `SELECT` 后。201 Sort SQL 保持原样，现有三条 Hash 会话 GUC
继续保留作为兼容保护。

不新增配置项，不修改数据集、恢复逻辑或全局/用户/数据库参数。

## 测试与验收

- 单元测试先证明当前 202 标定 SQL 和游标 SQL 缺少 Hint，再由最小实现使其通过。
- 测试同时断言 Hint 紧跟 `SELECT`、包含固定 join 顺序、`hashjoin(p h)` 和查询级
  `enable_index_nestloop off`；201 SQL 不应包含该 Hint。
- 运行 `go test ./internal/gsbench` 和 `go test ./...`。
- 构建候选二进制后，在 og5 用短时场景 202 验证正式 worker 的计划含
  `Hash Left Join`，且 `GS_SESSION_MEMORY_CONTEXT` 出现显著 `HashBatchContext`。

