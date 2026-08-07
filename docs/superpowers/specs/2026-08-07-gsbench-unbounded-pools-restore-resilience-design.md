# gsbench 401/402 无人工容量上限与恢复锁韧性设计

日期：2026-08-07  
目标版本：gsbench v1.1.7  
状态：方案 1 已批准，待规格确认后实施

## 1. 背景

v1.1.7 为 401/402 增加了百分比目标。当前实现同时使用数据库真实容量与
`safety.max_connections`、`safety.max_workers` 计算可达上限。本地 401 测试中，数据库可用连接
容量为 997，90% 目标需要 898 个总连接、约 871 个新增连接，但配置
`safety.max_connections=800` 使场景在注入前以 82.9% 人工 ceiling 失败。

随后执行 60% 测试时，数据库在约 463 个新增连接后返回
`memory is temporarily unavailable`。401 正确把真实资源耗尽判为失败并关闭了压测 socket，但统一恢复
协调器在获取数据库 advisory lock 时又报告：

```text
pq: got 1 parameters but the statement requires 3
close database restore lock session: sql: connection is already closed
```

锁 SQL 源码只有一个 `$1`。错误来自 openGauss Go 驱动的 prepared-statement 参数状态路径；错误处理随后
丢弃物理连接又重复关闭同一 `sql.Conn`，把 `sql.ErrConnDone` 追加成第二个恢复错误。该组合导致本来没有
recovery action 的 401 被标记为 `RESTORE_FAILED`。

本工具用于授权测试环境的故障模拟。用户已批准 401/402 不再受 gsbench 人工容量上限保护，但要求真实失败
必须准确报告、已创建资源必须清理、恢复路径必须可重试，并对 v1.1.7 新功能完成极端测试后才交付。

## 2. 范围

### 2.1 本次变更

1. 401 忽略 `safety.max_connections`，仅按数据库实际可用连接容量、运行前基线和 `--percent` 计算新增连接数。
2. 402 忽略 `safety.max_connections` 与 `safety.max_workers`，仅按数据库实际剩余会话容量和真实线程池指标升压。
3. 保留 1–100 百分比范围、目标必须严格高于基线、真实指标强制校验、duration、资源丢失检测和失败退出。
4. 修复数据库 session advisory lock 的参数化语句兼容问题、异常连接复用问题和重复关闭错误。
5. 更新配置手册、使用说明和发布说明，明确两个配置上限不再约束 401/402。
6. 对 401、402、602、action-free stale recovery 和恢复锁执行单元、回归及本地数据库极端验证。
7. 验证后重新构建并部署本地 macOS ARM64 v1.1.7，重新生成 Linux ARM64 发布包及 SHA-256。

### 2.2 明确保留

- `safety.max_connections`、`safety.max_workers` 配置项继续存在、继续要求为正数，并继续约束其他场景。
- 其他场景的固定 worker、锁会话、连接 churn、线程池 queue 和计划基础流量预算保持不变。
- `query_timeout`、`restore_timeout`、`profile_cap_gb`、`min_free_disk_percent` 和 `restore_on_exit=true` 保持不变。
- Risk B/C、实例参数修改、数据库重启、外部故障 provider 和恢复台账授权保持不变。
- 数据库自身 `max_connections`、reserved connections、内存、线程、文件描述符和网络限制无法也不会被绕过。
- 一个场景真实失败时，同一次运行中的其他场景继续各自生命周期，最终命令以聚合非零退出码结束。

### 2.3 非目标

- 不取消所有场景的安全边界。
- 不把数据库资源耗尽伪装为压测成功。
- 不在失败后自动降低用户输入的目标百分比。
- 不在达到目标后继续追加连接或 worker。
- 不修改数据库 `max_connections`，也不为 401 自动重启数据库。
- 不改变 gsbench 版本号，修订后的交付仍为 v1.1.7。

## 3. 401 行为

准备阶段读取：

```text
usable = SHOW max_connections - SHOW sysadmin_reserved_connections
baseline = SELECT count(*) FROM pg_stat_activity
desired = ceil(usable * percent / 100)
needed = desired - baseline
physical_headroom = usable - baseline
```

要求 `percent > baseline / usable * 100`。目标合法时 `needed` 必然不大于
`physical_headroom`，401 将 `needed` 作为本次准确新增连接数，不再与
`safety.max_connections` 取最小值，也不再产生人工 `ceiling` 预检失败。

连接仍逐个建立并使用本次 run/scenario/worker tag。任一连接建立、事务创建或主动 SQL 执行失败时：

1. 记录首个真实错误和已成功建立数量；
2. 场景失败，不继续追赶目标；
3. Stop 取消主动 SQL、回滚 idle-in-transaction、关闭所有本次 tagged connection；
4. 统一恢复协调器运行；401 本身没有 database/local recovery action；
5. 后续独立场景不因该 action-free 失败永久堵塞。

达到目标后不补建。外部连接退出不会促使 401 继续增加；本次 tagged connection 丢失仍判失败。duration 结束后
关闭本次全部连接。

## 4. 402 行为

402 仍强制使用 `global_threadpool_status` 的真实数据：

```text
utilization = (actual_workers - idle_workers) / actual_workers * 100
```

运行前目标必须高于真实基线。最大可尝试客户端会话数只由数据库物理会话余量决定：

```text
physical_session_headroom =
    max_connections - reserved_connections - existing_connections
```

402 不再与 `safety.max_workers` 或 `safety.max_connections` 取最小值。Controller 可以在物理余量内逐步增加
客户端 worker，连续 3 个真实样本达到目标后冻结 worker 数。达到目标后不补建，冻结后的 worker 丢失判失败。

线程池未启用或真实视图不可用时仍不能用 active-backend fallback 冒充成功。自动启用线程池仍需要
`allow_instance_parameter_change=true`、`allow_database_restart=true`、非空 `restart_command`、管理员能力和单独
运行 402；这些授权不属于本次取消的容量上限。

数据库拒绝新会话、内存不足、物理会话余量耗尽或 duration 前未取得连续 3 个达标样本时，402 明确失败并关闭
本次客户端 worker。数据库自身 idle thread-pool worker 的销毁仍由数据库管理。

## 5. Advisory lock 修复

### 5.1 独立锁会话

数据库 run/plan/restore session lock 使用独立的单连接 `sql.DB` 与其专属 `sql.Conn`，DSN、数据库和认证信息与
主 Database 相同，但使用明确的 control-lock application name。锁会话不从经历压力采样和错误的主连接池复用
物理连接。

会话对象同时持有 pool 和 conn：

- 获取成功后由同一个物理 session 持有全部 session advisory lock；
- restore ownership 查询和逆操作继续通过这个受保护 session 执行；
- 正常释放时按获取逆序 unlock，再关闭 conn 和专属 pool；
- 锁状态不确定时把物理连接标记为 bad，随后关闭 wrapper 和 pool，不允许返回任何共享池；
- Close/Discard 使用 `sync.Once`，并把 `sql.ErrConnDone`、`driver.ErrBadConn` 视为已完成关闭，不重复报错。

### 5.2 无绑定参数的锁查询

session advisory lock 的 try/unlock 查询改为 simple-query 路径，不传 driver bind 参数：

```sql
SELECT pg_try_advisory_lock(hashtext(<安全 SQL 字面量>))
SELECT pg_advisory_unlock(hashtext(<安全 SQL 字面量>))
```

函数名由代码常量选择；锁键使用当前 openGauss connector 提供的 `QuoteLiteral` 生成 SQL 字面量，禁止调用方传入
SQL 片段。这样不会经过触发本次 `paramTyps` 数量错乱的 prepared-statement 分支，同时保持不同进程、不同版本
使用数据库 `hashtext` 得到相同锁键。

事务级 journal allocation lock 不在本次故障路径内，继续使用原参数化事务 SQL，避免扩大无关变更。

### 5.3 重试与错误分类

以下锁会话错误按数据库暂时不可用处理：

- `driver.ErrBadConn`、`sql.ErrConnDone`；
- SQLSTATE `08` 连接异常；
- SQLSTATE `53` 资源不足（包括 out-of-memory、too-many-connections）；
- SQLSTATE `57P01`、`57P02`、`57P03`；
- openGauss 未提供 SQLSTATE 时的已知 `memory is temporarily unavailable` 兼容文本。

出现上述错误时释放已经确定持有的部分锁、丢弃锁会话，等待数据库重新可连接后使用全新会话重试一次既有的
plan-first restore 流程。`pg_try_advisory_lock=false` 是锁竞争，不按连接故障处理；SQL 语法、权限、所有权和
未知错误立即失败，不隐藏程序缺陷。

## 6. stale recovery

既有 fail-closed 规则不变。只有同时满足以下条件才允许恢复失败降为 WARN 并启动新 run：

- 所有旧 run 身份完整、场景列表非空且只含 401/402；
- database journal 和 local ledger 都没有 action；
- 恢复锁已正常释放；
- 新 run 回调尚未执行。

本次修复的目标是让数据库恢复后真正取得/释放锁，而不是依赖 stale bypass 掩盖锁实现错误。发现失败、未知场景、
任何 action、锁释放不确定或其他场景仍阻断。

## 7. 测试设计

### 7.1 TDD 与单元测试

生产代码修改前先增加失败测试并确认失败原因正确。

401：

- safety cap 为 1、真实 headroom 足够时，90% 预算仍等于基线到目标的完整差值；
- 100% 使用全部物理 headroom；
- 目标等于/低于基线仍拒绝；
- 打开第 N 个连接失败时停止增加，Stop 关闭前 N-1 个连接；
- duration、tagged connection 丢失和外部基线变化保持既定语义。

402：

- safety max workers/connections 为 1 时，402 最大 worker 仍等于真实物理 session headroom；
- 100%、零 headroom、基线等于/高于目标、真实视图缺失和 worker 丢失；
- 连续 3 个样本达标后冻结；实际资源错误停止增加并清理。

恢复锁：

- try/unlock SQL 不含 `$1`，driver 收到零参数；恶意引号和反斜杠锁键仍被安全编码；
- 每次锁获取使用独立单连接 pool，不复用主 pool；
- 部分获取、锁竞争、资源不足、连接关闭、重连和逆序释放；
- Discard 后再次 Close 不产生 `sql.ErrConnDone`；物理连接与 pool 都只关闭一次；
- 参数数量异常不能再由 session lock 查询路径产生；未知错误不被错误重试。

stale recovery 与 602：

- action-free 401/402 恢复失败允许继续；有 action/未知身份/锁释放失败仍阻断；
- 602 fault 只有三条计划均为 Seq Scan 才成功；recover 只有覆盖清除、三条均恢复唯一索引且 journal 清空才成功；
- 重复 fault/recover、中途失败和旧版 journal 兼容恢复。

### 7.2 回归测试

- `go test ./internal/gsbench -count=1`；
- `go test ./cmd/gsbench` 中不依赖 sandbox listen 的进程/中断测试；
- 其余 Go package 全量测试；
- `go build ./cmd/gsbench`；
- gstop 内存大盘 5 秒刷新既有测试，确认本次 gsbench 修改没有影响 gstop。

已知 sandbox 禁止测试进程执行 `net.Listen("127.0.0.1:0")` 时，单独记录该环境阻塞，不把它误报为代码失败。

### 7.3 本地 og5 极端验证

测试使用专用 `gsbench_e2e_20260801_100g` schema 和现有本地配置，密码只从既有安全配置/环境读取，日志必须保持
`<redacted>`。所有压力场景串行运行，每轮后检查 tagged sessions、journal、ledger、status 和下一场景可启动性。

1. 基线：`doctor`、`status`、`restore --dry-run`，记录数据库容量、线程池能力和遗留动作。
2. 401 人工 cap 绕过：把测试配置 cap 设为 1，选择一个高于基线且本机可达到的短时目标，证明实际新增数大于 1。
3. 401 极限：依次测试基线边界、60%、90%、100%。数据库因真实内存/连接资源拒绝属于预期压力结果，但必须得到
   主错误、清理全部本次连接、恢复协调器成功且后续低目标场景可运行；不能再出现参数数量或重复关闭恢复错误。
4. 402 人工 cap 绕过：cap 设为 1，在真实线程池可用时证明 worker 可超过 1 并连续 3 次达标。若本地实例没有真实
   线程池能力，必须由 doctor 明确证明能力缺失，单元/驱动集成测试仍须覆盖完整算法；不得用 fallback 冒充 live 成功。
5. 402 极限：测试基线边界、90%、100%、worker 丢失和资源拒绝后的清理与下一场景启动。
6. 602：启动固定基础流量，验证基线唯一索引；执行 fault 并验证三条 Seq Scan；执行 recover 并验证统计覆盖清除、
   三条唯一索引扫描和 journal 清空；再测试重复 recover 和 fault/recover 中断重试。
7. stale recovery：构造 action-free 401/402 失败与有 action 的失败，分别验证继续和阻断边界。
8. 收尾：`restore --dry-run` 无 action、`status` 无活动故障、无本次 tagged session；运行一个短时安全场景证明后续测试
   不被堵塞。

“极端测试通过”不要求物理上不可达的 60%/90%/100% 返回 SUCCESS；它要求成功目标准确达标，不可达目标准确失败，
并且每一种结果都完成连接清理、恢复收敛和后续可运行性证明。

## 8. 交付验收

只有同时满足以下条件才重新交付：

- 401/402 不再读取人工 cap 作为自身容量 ceiling，其他场景仍受原 cap 约束；
- 真实数据库资源耗尽不会误报成功，也不会遗留本次客户端连接；
- 恢复锁不再走参数化 session-lock 查询，不再报告 `got 1 parameters ... requires 3`；
- 异常锁连接关闭不再追加 `sql: connection is already closed`；
- 602 的 fault/recover 强制计划证据和 journal 收敛全部通过；
- action-free stale recovery 与 fail-closed 边界全部通过；
- 自动测试和可执行的本地 og5 极端测试完成，任何环境能力缺失单独列出且不冒充成功；
- 本地 macOS ARM64 二进制显示 v1.1.7、内嵌当前干净提交；
- Linux ARM64 静态包的版本说明、BUILD_INFO、包内及归档 SHA-256 与同一提交一致。
