# gstop (Go)

当前版本：**gstop v1.6.3 / gsbench v1.1.8**

GaussDB / openGauss 实时监控与应急诊断终端工具 —— 原 Python 工具
[gausstop](https://gitee.com/sqlrush/gausstop)（`gstop_ABC_1.4` 分支，作者：吴海存）的
Go 语言忠实重构。相当于数据库版的 `top`：一屏看清数据库、实例、操作系统、等待事件、
会话，并提供一键应急处置。

适配：麒麟 v10 / aarch64（也支持 x86_64），GaussDB 503.1 / 505.1 / 505.2.1 集中式。

## 功能

- **监控大盘**（3s 刷新）：
  - 数据库：版本 / 用户 / 运行时长 / 主节点 / 动态内存 / 计划缓存 / 繁忙度 db% / 等待占比 WTR%
  - 实例：会话数(SN/AN/ASC/ASI/IDL) / MBPS / TPS / QPS / P80 / P95 / XLOG / 连接率 / 线程池
  - 操作系统：LOAD / %CPU / %MEM / IO 读写次数·吞吐·延迟·队列长度（源自 /proc、iostat、pidstat）
  - 等待事件：TOP 事件，实时(`r`)/累计(`c`) 两种模式
  - 会话：SPID/USR/PROG/PGA/SQLID/SQL/OPN/BLOCKER/E-T/STA/STE/EVENT/SParse/BLK
  - 内存大盘（`m`）：进程/动态/共享内存概要 + 会话/线程内存 TOP 榜
- **健康大盘**（`h`）：实例全部 SQL 单次平均耗时 TOP5、启动后执行次数增量 TOP5、活跃 SQL
  动态内存 TOP5、启动后计划跳变、跨用户库 ANALYZE 历史与失效索引、启动后等待事件 TOP5
  和独立 DB CPU；支持滚动、SQL 光标选择、完整 SQL/执行计划明细和慢项手动刷新
- **会话管理**（`s`）：方向键选择、翻页、按耗时/内存/事件排序、阻塞标记(H/W/H&W)+死锁检测、
  详情面板(`p`：SQL全文/执行计划/双向阻塞树)、一键查杀单个(`k`)或同 SQLID 全部(`K`)
- **一键应急**（`e`）：9 种场景自动检测+处置 —— 执行计划跳变、CPU满、IO满、内存满、
  线程池满、连接数满、慢SQL、性能抖动、巡检（8 检查项）
- **持久化**：监控数据落盘（定宽`|`分隔 + gzip 滚动）、应急快照（屏幕文本 + 会话CSV）、
  告警写本地文件（连续触发+抑制去重）
- **守护 & 自愈**：`-d` 后台模式；刷新线程 5 分钟无心跳自动退出等待外部拉起；
  配套 crontab 健康检查脚本
- **安全**：默认硬关闭一切查杀能力（`support_terminate` / `support_emergency_command`），
  需显式开启；查杀 SQL bigint 校验 + 用户名白名单校验防注入

## GaussDB / openGauss 自适应

工具**运行时探测**连接的是 GaussDB（商用）还是 openGauss（开源，含 MogDB/Vastbase 等衍生版），
对二者视图/函数差异**自动路由**，用户无需感知：

- **GaussDB 查询严格沿用 gstop_ABC_1.4 分支的原版**（生产验证过），是默认与权威形态
- **openGauss 差异点自动切换到兼容变体**——见 `internal/dbcompat`

在 openGauss Lite 5.0.3 上逐条审计了工具跑的 30 条查询，**仅 2 处需要适配**（其余全部通用）：

| 差异 | GaussDB | openGauss | 处理 |
|---|---|---|---|
| 会话查询 BLOCKER 的 CASE 类型 | `ELSE (SELECT pid ...)` | openGauss 强制 CASE 分支同型 → `pid::text` | 按类型自动选变体 |
| 运行时执行计划 | `gs_get_explain(pid)`（运行线程实时计划） | 无此函数 | og 优先历史计划；gsbench SQL 可回退到场景预检缓存，其他安全 SQL 再用普通 `EXPLAIN` 估算 |

其余（`dbe_perf.*`、`GS_INSTANCE_TIME`、`pv_*_memory`、`pg_thread_wait_status`、`pg_lock_status`
等）两边通用，直连即可。新差异只需在 `internal/dbcompat` 增加变体。

## 与原版的差异

- **数据库驱动**：用 openGauss 官方 Go 驱动
  [`gitcode.com/opengauss/openGauss-connector-go-pq`](https://gitcode.com/opengauss/openGauss-connector-go-pq)
  （lib/pq 分支，支持 openGauss SHA256 认证），与原 psycopg2+libpq 语义一致。
  **好处**：纯 Go，产物是单个静态二进制，无需 Python / rpm / libpq。
- **TUI**：用 [`tcell`](https://github.com/gdamore/tcell) 复刻 curses 的 pad/绝对定位/颜色对模型。
- 一处 bug 修复：应急基类 `analyze_session` 的表名正则原为无捕获组却读 `group(1)`（会抛异常），
  按意图改为捕获组使 ANALYZE 建议生效（见 `internal/emergency/base.go`）。

## 目录结构

```
cmd/gstop/            入口：CLI、单例限流、密码、启动 TUI/daemon
internal/
  config/             gstop.cfg INI 解析（嵌套 section、类型推断、args 合并）
  dbcompat/           GaussDB/openGauss 类型探测 + 查询变体路由（自适应兼容）
  logging/            RotatingFile 日志
  alarm/              告警去重 + 写文件
  dbconn/             openGauss 连接 + 重连节流 + 探活；query/noreturn/多用户库执行
  oscmd/              shell 命令执行（一次性 + 后台）
  health/             CPU/内存刷新节流 + 心跳/退出判定
  healthdash/         健康统计聚合 + 分级刷新 + 跨库检查 + SQL/计划详情页
  timing/             慢日志 + panic 恢复计时
  model/              Style/Surface、DumpData、SQL 终结模板、SessionRow、DBInfo
  tui/                Pad(离屏缓冲) + 颜色 + Screen(tcell) + 输入助手
  monitor/            base + db/instance/operating_system/event/session/memory
  emergency/          base + 调度 + 9 场景 + MemPersist + 快照落盘
  persist/            监控数据落盘(gzip 滚动)
  app/                主循环 + 可暂停刷新线程 + 键位状态机(s/m/h/e 子模式)
configs/              gstop.cfg + monitor/*.cfg
scripts/              build / install / run / gstop-manage
cmd/gsbench/          独立压测工具入口
internal/gsbench/     自动造数、场景引擎、闭环升压、恢复日志、状态与清理
docs/gsbench/         gsbench 操作手册
```

## gsbench 压测与故障场景生成器

仓库同时提供独立的 `gsbench`。作者：WangYingJie <sqlrush@gmail.com>。

它能自动生成基准 schema 和数据，并模拟 TP/AP/混合 CPU、连接池、线程池、动态内存、
六种独立执行计划跳变、锁堆积和 vacuum 性能问题。所有敏感变更先写恢复日志；支持
`doctor/init/run/status/stop/restore/cleanup` 生命周期与管理员/普通用户双路径。

gsbench v1.1.8 将产品/拓扑、容量、数据、元数据和执行计划检查全部改为告警：不自动优化，
不使用人工策略上限阻挡场景。真实 SQL、连接或故障注入错误只使当前场景失败，后续场景继续。
`restore` 展示所有场景恢复 DDL/DML，`run 601-606 recover` 只展示对应场景；两者都不执行。
未显式指定索引方法时，`btree` 与 Ustore 的 `ubtree` 兼容。

场景 2/3 的 AP 慢 SQL默认只输入 100 万事实表行，CPU 目标为 70%；独立 AP 最大并发
8，混合场景总并发最大 20、AP 最大 4。AP 不设单 SQL超时，run 结束时通过取消并关闭
当前 run 的 tagged connections 强制终止。

完整配置和安全说明见 [docs/gsbench/README.md](docs/gsbench/README.md)。

常用原生命令：

```bash
export GSBENCH_CONFIG=/absolute/path/to/configs/gsbench.cfg
export GSBENCH_PASSWORD='数据库密码'

./gsbench doctor
./gsbench init --profile quick
./gsbench init --size 100GB
./gsbench run -s 101,102 -d 5m
./gsbench run 401 --percent 90 -d 2m
./gsbench run 601 recover
./gsbench restore
```

`-s` 等价于 `--scenario`，`-d` 等价于 `--duration`；场景使用三位编号、别名或完整名称。计划跳变慢 SQL 使用固定字面量且数据由 `init` 生成。`-c/--config` 保留兼容。

`init --size` 可直接指定 1 GB–2 TiB 的初始化目标（例如 `100GB`、`1.5TB`、`2TB`），
优先级高于 `data.max_size_gb` 和 profile 默认值。实际压测的所有可规划业务 SQL 均以完整
字面量发送，不携带绑定实参；数据库累计视图把常量归一化为 `?` 只是展示行为。

## 构建

```bash
# 本地构建
go build ./...

# gsbench
go build -o gsbench ./cmd/gsbench
./gsbench version

# 交叉编译目标平台产物（静态二进制 + 配置 + 脚本打包）
./scripts/build.sh v1.6.3
#  -> dist/gstop_v1.6.3_linux_arm64_YYYYMMDD.tar.gz
#  -> dist/gstop_v1.6.3_linux_amd64_YYYYMMDD.tar.gz
```

## 安装与运行

```bash
# 解包后
./scripts/install.sh  # 先校验 v1.6.3 包布局和 SHA-256，再装到 ~/.local/bin
gstop                 # 前台 TUI
gstop -u dbadmin -p 12345 -i 5 -l 1   # 指定用户/端口/刷新/落盘间隔
gstop -d              # 守护模式

# 守护管理
./scripts/gstop-manage.sh start|stop|status|check|register|unregister
```

### 命令行参数

| 参数 | 含义 | 默认 |
|---|---|---|
| `-i` | 屏幕刷新间隔(秒) | 3 |
| `-l` | 数据落盘间隔(秒)，0=关闭 | 0 |
| `-u` | 数据库用户 | rdsAdmin |
| `-p` | 数据库端口 | 8000 |
| `-d` | 守护模式 | false |
| `-c` | 指定 gstop.cfg 路径 | 自动查找 |

### 热键

`r` 实时事件 / `c` 累计事件 / `s` 会话选择 / `m` 内存大盘 / `h` 健康大盘 / `e` 应急面板 / `q` 退出。
子模式内：方向键移动、`n`/`N` 翻页、`t`/`m`/`e` 排序、`k`/`K` 查杀、`p` 详情。

健康大盘内：方向键滚动，`s` 开关 SQL 光标选择，`p` 查看选中 SQL 的全文和执行计划，
`r` 立即刷新跨库统计信息/失效索引，`Esc` 从明细返回健康大盘、再返回主监控；`q` 在任何页面
立即取消明细查询和采集并退出进程。SQL/等待/计划跳变跟随 `main.interval`；`m` 内存大盘
跟随 `main.mem_interval`（默认 5 秒），每轮刷新全部内存面板；健康大盘的动态内存候选采集
也跟随 `main.mem_interval`，但仍受 CPU 阈值和 `main.dynamic_mem_interval` 最短间隔保护；
跨库慢项跟随 `main.health_slow_interval`（默认 300 秒）。

### CURRENT ACTIVE SQL ELAPSED TOP5

`MAX_ELAPSED` 是该 SQL ID 当前活跃会话中运行时间最长的一条；`SESSIONS` 是该 SQL ID 当前的并发活跃会话数。

### AVG ELAPSED SINCE GSTOP TOP5 SQL

`CALLS` 是本次 gstop 进程建立首个成功基线后完成的执行次数，`ACTIVE` 是当前正在运行的会话数。
`AVG_ELAPSED` 将启动后已完成 SQL 的数据库时间增量与活跃 SQL 的完整已运行时间相加，再除以
`CALLS + ACTIVE`。启动前已完成、且启动后未再执行的旧 SQL 不进入排名；启动前已经开始且当前仍在
运行的 SQL，按 `query_start` 至采集时刻的完整运行时间计入。

`main.collect_timeout = 30` 是单个采集模块一次操作的最长预算，不是大盘等待 30 秒才展示。
常驻（resident）模块彼此独立刷新：快速模块完成后立即发布；慢模块刷新期间继续显示该模块
上一次成功值并标记 `[R]`。`[T]` 表示最近一次尝试超过 30 秒，`[E]` 表示最近一次尝试失败，
`[L]` 表示该模块尚无任何成功样本。某模块超时或失败不会清空其成功快照、阻塞其他模块或推进
该模块的差值/平均值基线。

SQL 健康详情的操作顺序是 `h` → `s` → 方向键选择 Top SQL → `p`。Top SQL 行会携带采集时的
PID 和 session ID；详情页先同时校验二者，避免 PID 被复用后误查其他会话。GaussDB 只有在该
身份校验通过时才调用已验证的 `gs_get_explain`。若原会话已结束，gstop 仅查询最近 60 分钟的
`statement_history`，并独立尝试按同一 SQL ID 重新定位其他活动会话。openGauss 或历史查询较慢
时，会并行匹配 gsbench 的场景预检计划缓存，其他安全 SQL 可先显示普通 `EXPLAIN` 估算。
计划按“采集时会话实时计划 > 历史真实计划 > 重新定位会话实时计划 > gsbench 场景预检计划 >
EXPLAIN 估算计划”的质量顺序升级；低质量的迟到结果不会覆盖高质量结果。
CPU 历史、ASH 推断、索引和统计信息目录诊断并行渐进更新，任何慢项都不会隐藏已经可用的计划。
详情标题同时显示 SQL_ID、DATABASE、SCHEMA 和 USER；多值去重排序，Schema 在计划目录诊断
完成后异步补齐。已经取得真实计划或 gsbench 预检计划时，不再显示普通 EXPLAIN 的绑定变量失败。

普通 `EXPLAIN` 只接受可安全估算的单条非绑定可规划语句（包括
`SELECT`/`WITH`/`INSERT`/`UPDATE`/`DELETE`/`MERGE`，只生成计划而不执行）；绑定变量、
多语句 SQL 和 utility 语句会被明确拒绝。
该限制只针对“临时执行普通 EXPLAIN”的最低优先级来源，不影响实时计划、历史真实计划或
gsbench 场景预检缓存计划。
gstop 绝不使用 `EXPLAIN ANALYZE`，绝不执行用户 SQL，也不会执行索引 DDL 或 `ANALYZE`。
详情区出现 `loading` 表示该证据仍在加载，`timeout` 表示该独立阶段超过预算，`error` 通常表示
查询失败、权限不足或版本目录不兼容；这些提示不影响已展示的其他证据。`Esc` 会立即取消当前详情
并返回，迟到结果不会改写已离开的页面；`q` 是最高优先级退出键。

## 测试

```bash
go test ./internal/...            # 单元测试
go test -race ./internal/...      # 竞态检测（需 CGO）
go vet ./...
```

## 连接说明

GaussDB 免密（peer/unix socket）连接：工具不显式指定 host，由部署用户环境
（`gauss_env_file` 的 `PGHOST` 等）决定，与原版一致。也可在 `gstop.cfg` 的
`[main]` 段配置 `host` / `sslmode` / `connect_timeout` 覆盖。
