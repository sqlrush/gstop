# gstop 当前活跃 SQL 耗时 TOP5 设计

## 目标

在健康检查大盘顶部增加 `CURRENT ACTIVE SQL ELAPSED TOP5`，让索引删除后仍在执行的慢 SQL 立即可见，不再受“gstop 启动以来累计平均耗时”中大量历史快速调用的稀释。

## 展示语义

- 每个 `SQL_ID` 只显示一行。
- 按该 SQL 当前所有活跃会话中的最长已运行时间降序排列，最多显示 5 行。
- 相同最长时间时按 `SQL_ID` 升序，保证刷新结果稳定。
- 列为：`SQL_ID`、`MAX_ELAPSED`、`SESSIONS`、`SQL`。
- 榜单放在健康大盘第 1 区；现有区块顺延编号。
- 行继续支持 `s` 选择和 `p` 打开 SQL 详情。详情使用最长运行会话的 PID、session ID、query start、已运行时间和采集时间，确保显示值与详情目标一致。
- 选择行为沿用当前大盘的“按本轮渲染结果选择”规则，不新增快捷键或选择状态。

## 数据流

复用 `RefreshFast` 已执行的 `pg_stat_activity` 活跃 SQL 查询，不新增数据库查询：

1. `parseActive` 生成本轮 `[]ActiveSQL`。
2. 新的纯函数按 `SQL_ID` 聚合活跃会话。
3. 聚合结果写入 `Snapshot.ActiveElapsedSQL`。
4. `View` 在累计平均榜单之前渲染新榜单。

不能从 `AverageSQL` 反向筛选，因为该列表已经截断为 TOP5，可能丢失真正运行最久的 SQL。

## 聚合规则

每个 SQL_ID 聚合以下字段：

- `ActiveSessions`：当前活跃逻辑会话数；openGauss 同一 `sessionid` 的并行线程只计一次，空/零 `sessionid` 时按 PID 计数，两个身份都不可用时忽略该行。
- `RepresentativeElapsedUS`：最长运行会话的当前耗时，也是排序和展示值。
- `RepresentativePID`、`RepresentativeSessionID`、`RepresentativeQueryStart`：同一个最长运行会话的身份；打开详情时用它们精确确认仍是同一次 SQL 执行。
- `Query`、`Databases`、`Users`：沿用当前 SQL 指标的聚合与去重规则。
- `CapturedAt`：本轮快速采集时间。

不计算历史调用次数或累计平均值，避免故障前数据影响当前榜单。
当前耗时按时间间隔总秒数计算，不使用会在 24 小时后回绕的小时/分钟字段。

## 失败与刷新行为

- 活跃会话查询成功但没有活跃 SQL：显示“当前无活跃 SQL”。
- 活跃会话查询失败：清空该榜单，避免把上一轮会话误标为“当前”；大盘已有 `FAST ERROR` 同时说明采集失败。
- statement 统计查询失败但 active 查询成功：新榜单仍正常发布，因为两者数据源独立。
- SQL_ID 为 0 的内部或不可识别会话继续忽略，与现有活跃 SQL 口径一致。
- 本次不改变现有活跃会话 SQL 的过滤条件，避免影响累计平均和内存榜单的既有口径。

## 兼容性

- 不新增配置项、命令行参数或数据库权限要求。
- 不改变现有累计平均、执行次数、内存、计划跳变等榜单的计算规则。
- `Snapshot.Clone` 必须深复制新字段，保持并发发布的不可变约束。
- gstop 版本从 `v1.6.2` 增加到 `v1.6.3`，并在验证通过后原子替换本地 `gstop.real`，保留 `v1.6.2` 备份。

## 验收

自动化测试覆盖：

1. 相同 SQL_ID 多会话聚合，取最长会话并统计会话数。
2. 不同 SQL_ID 排序、稳定 tie-break 和 TOP5 截断。
3. collector 在 active 成功、statement 失败时仍发布新榜单。
4. active 查询失败时清空上一轮数据。
5. Snapshot 深复制和 View 区块、列值、选择详情身份。

真实 og5 验收使用 601：基线流量期间执行 `fault`，确认 `SQL_ID=3877360001` 出现在当前活跃榜单，`SESSIONS` 与 worker 数一致或与采集瞬间活跃数一致，进入详情后显示当前 `Seq Scan` 和索引建议，随后执行 `recover` 并清理测试状态。
