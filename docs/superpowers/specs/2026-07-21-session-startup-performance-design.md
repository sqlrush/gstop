# gstop 会话首屏性能优化设计

## 问题

启用 openGauss 线程池后，`pg_stat_activity` 与 `pg_thread_wait_status` 中都有大量
`sessionid=0` 行。会话查询只按 `sessionid` 连接，把约 133 个真实会话放大到约
14,413 行。查询又把无阻塞返回为空字符串，Go 代码将所有行误判为等待者，并对每行
线性扫描整个 blocker 列表和写一条 WARNING 日志，形成约 O(N²) 的处理。首轮刷新同步
等待最慢模块，因此首屏延迟约 7.3 秒。

## 方案选择

采用最小根因修复：

1. 会话查询用 `(pid/tid, sessionid)` 复合身份连接，和已验证的实例连接统计保持一致。
2. 将 SQL NULL、空字符串和纯空白 blocker 统一视为“无阻塞”。
3. 将真实 blocker PID 预建为集合，逐行 O(1) 判定；只有真实 holder/waiter 才记录日志。

不采用以下方案：

- 仅把首屏改成异步空壳：只能隐藏 7 秒耗时，后台刷新仍慢且继续产生伪告警和日志。
- 完全重写阻塞检测 SQL：改动面和数据库版本兼容风险更大，本次没有必要。

## 数据流

数据库复合 JOIN 返回一行对应一个真实会话。构建展示行时，blocker 先归一化；只有有效
数字 blocker 才进入阻塞集合。分类阶段用集合判断某 PID 是否也是 holder，生成 `W`、
`H` 或 `H&W`，无阻塞行保持空白。死锁图只接收真实数字边。

## 失败与兼容

- 复合键沿用 openGauss 5.0.3 现场验证：旧 JOIN 14,413 行，复合 JOIN 133 行。
- blocker 为空或不可解析时按无阻塞处理，不产生伪告警。
- 查询失败仍沿用现有本轮清空、下轮重试策略。
- 不修改 `collect_timeout`、刷新周期或首屏同步语义。

## 测试与验收

- 查询回归测试必须断言包含 `a.pid = t.tid AND a.sessionid = t.sessionid`。
- 空字符串、空白和 NULL blocker 不得触发阻塞分析。
- 真实等待链仍正确标记 holder、waiter 和 holder/waiter。
- 大量无阻塞行的分类不得退化为逐行扫描 blocker 列表。
- 全量测试、竞态检测和 `go vet` 通过。
- og5 现场验证会话查询行数等于 `pg_stat_activity` 行数，首轮 session 模块不再超过慢模块阈值，且不再批量写 `BLK: W`。
