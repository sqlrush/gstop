# gsbench 501–503 可配置会话数与等待链深度设计

## 目标

重构 501 `lock_row_chain`、502 `lock_table_exclusive` 和 503
`lock_ddl_wait`，让用户显式控制锁压力使用的 workload 会话总数；501 另外允许控制
人工构造的行锁等待链最大深度。该功能不改变 504 及其后的锁场景。

命令行示例：

```text
gsbench run 501 --sessions 10 --chain-depth 3 --duration 1m
gsbench run 502 --sessions 10 --duration 1m
gsbench run 503 --sessions 10 --duration 1m
```

## 参数语义

- `--sessions N` 表示每个所选锁场景的 workload 会话总数，包含一个 holder 和
  `N-1` 个 waiter；gsbench 的控制、元数据和观测连接不计入该值。
- `--chain-depth D` 只配置 501 的人工等待链最大深度，默认 1，允许 1～5。
- 三个场景的 `sessions` 默认均为 2，因此默认拓扑都是一个 holder 加一个
  waiter；501 的默认实际深度为 1。
- `sessions` 最小为 2，并受 `safety.max_connections` 硬上限约束。由于 Runner
  会并发执行所选场景，校验使用所有选中场景的 workload 会话累计值（包括同时
  选择的固定 worker 场景），而不是只检查单个锁场景。该上限只做资源边界检查，
  不动态改变用户请求的会话数。
- 501 必须满足 `sessions >= chain_depth + 1`，否则无法构造用户指定的深度，
  命令在连接数据库前失败并说明约束。

对应配置默认值：

```ini
[scenario.lock_row_chain]
sessions = 2
chain_depth = 1

[scenario.lock_table_exclusive]
sessions = 2

[scenario.lock_ddl_wait]
sessions = 2
```

CLI 显式参数覆盖配置。`--sessions` 只允许用于全部属于 501～503 的场景选择；
一次选择多个 501～503 时，同一个 CLI 值分别作用于每个场景。`--chain-depth`
要求选择中包含 501，并且只作用于 501；502、503 始终不人工构造多层链。若未
显式选择场景，则在配置文件确定最终场景后再次执行同样的兼容性检查。

## 501 行锁链拓扑

501 使用一个根 holder，并把 `sessions-1` 个 waiter 顺序填入多条分支；每条
分支的长度不超过 `chain_depth`。最后一条分支可以短于指定深度。例如：

```text
sessions=10, chain_depth=3

holder
├─ waiter-1-1 ← waiter-1-2 ← waiter-1-3
├─ waiter-2-1 ← waiter-2-2 ← waiter-2-3
└─ waiter-3-1 ← waiter-3-2 ← waiter-3-3
```

箭头右侧会话等待左侧会话。每个分支节点先在独立事务中更新并持有自己的唯一
`lock_targets` 行，再异步更新上游节点持有的行：第一层等待根 holder，第二层
等待第一层，依次类推。全部会话使用包含分支号和层号的唯一 tagged session
名称。构造结果应有 `sessions-1` 条受控等待边，观测到的最大层数应等于
`chain_depth`。

分支和行号分配必须是确定性的纯逻辑，以便在打开连接之前完成边界检查和单元
测试。最大深度为 5；可用行数不足以为每个会话分配唯一行时必须明确失败，不能
复用行而改变拓扑。

## 502 表级排他锁

一个 holder 事务执行：

```sql
LOCK TABLE <schema>.lock_table_targets IN ACCESS EXCLUSIVE MODE;
```

随后创建 `sessions-1` 个 tagged waiter，会话分别执行：

```sql
SELECT count(*) FROM <schema>.lock_table_targets;
```

这些 AccessShare 请求彼此兼容，但都被 holder 的 AccessExclusive 锁阻塞。场景
就绪条件是全部 `sessions-1` 个 waiter 已进入等待。

## 503 DML 阻塞 DDL

一个 holder 事务更新 `lock_ddl_targets` 并持有 RowExclusive 锁。随后创建
`sessions-1` 个 tagged waiter，每个会话执行自己的：

```sql
ALTER TABLE <schema>.lock_ddl_targets
  ADD COLUMN ddl_<run-token>_<waiter-index> integer;
```

唯一列名避免多个 waiter 使用同一 DDL 标识。全部 DDL 请求 AccessExclusive 锁
并保持等待。这里不承诺人工链深度：由于 AccessExclusive 请求彼此不兼容，数据库
锁队列可能自然显示前序 waiter，但参数语义仍是一个 holder 加 `sessions-1` 个
并发 DDL waiter。

## 生命周期、失败和退出

1. Prepare 创建 holder，开始事务并取得根锁。
2. Ramp 创建全部 waiter，按场景规则启动阻塞语句，并在短轮询窗口内确认请求的
   waiter 都已进入等待。
3. Hold 保持拓扑直到 `--duration` 到期。
4. Stop 先取消最深层/最后创建的 waiter，逆序回滚并关闭 waiter，再回滚和关闭
   holder，避免释放上游锁后让阻塞 SQL 意外继续执行。

任意连接、事务或 SQL 初始化失败时，Ramp 返回带场景、分支/层级或 waiter 编号的
错误，并对已经创建的资源执行同样的逆序清理。请求的等待拓扑未形成属于 workload
创建失败，不是可关闭的模型阈值验证；CPU、内存或等待时长均不参与成功判定。

第一次 Ctrl+C 继续由操作系统直接终止 gsbench 进程。进程 socket 关闭后数据库
回滚未提交事务并释放锁，不引入需要第二次 Ctrl+C 的恢复流程。正常 duration、
context 取消和 `gsbench stop` 仍走有界 Stop。

## 证据和验证开关

501 记录请求/实际 workload 会话数、waiter 数、等待边数、请求/实际最大深度和
每条分支长度。502、503 记录请求/实际 workload 会话数和等待 waiter 数。证据中的
目标 waiter 数均为 `sessions-1`，不再沿用固定目标 1 或固定两条边。502、503 按
唯一 tagged waiter 会话计数，不按观测到的锁边条数计数，避免同一 DDL waiter
同时受 holder 和锁队列前序请求影响时被重复计算。

`run.validation_enabled=false` 时仍输出上述运行证据，但不执行最终模型通过/失败
判定。连接创建、SQL 执行和锁拓扑就绪失败始终作为执行错误处理，因为此时实际
workload 未达到用户请求。

当前通用 plan preflight 对 501～503 返回 `unknown scenario`。实现需要为可
`EXPLAIN` 的锁场景 DML/查询提供只读 workload statement，跳过不可 `EXPLAIN`
的 DDL 形状，使验证开关打开时不在 Prepare 前误报；实际多会话锁拓扑仍由
LockEngine 建立。

## 测试范围

- CLI 和配置默认值、覆盖优先级、跨场景兼容性、会话最小值/安全上限、深度
  1～5，以及 `sessions < chain_depth+1` 的拒绝路径。
- 501 的确定性分支分配，包括整除和尾部分支，例如 10/3 得到 3、3、3，8/3
  得到 3、3、1。
- 501 精确打开 `sessions` 个 workload 会话并形成 `sessions-1` 条预期边；502、
  503 精确打开一个 holder 和 `sessions-1` 个 waiter。
- 503 每个 waiter 的 DDL 列名唯一。
- 部分创建失败、context 取消和正常 Stop 均逆序释放事务与 tagged connection。
- validation 开关打开时 501～503 不再因 workload catalog 缺项报
  `unknown scenario`；关闭时仍保留 runtime evidence。

## 非目标

- 不为 502、503 增加可配置的人工链深度。
- 不修改 504 及其后的锁/死锁场景。
- 不根据数据库负载动态增减会话，也不增加 CPU 或等待时长反馈控制。
