# gstop (Go)

当前版本：**v1.5.0**

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
- **健康大盘**（`h`）：实例全部 SQL 单次平均耗时 TOP3、启动后执行次数增量 TOP3、活跃 SQL
  动态内存 TOP3、启动后计划跳变、跨用户库 ANALYZE 历史与失效索引、启动后等待事件 TOP5
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
| 运行时执行计划 | `gs_get_explain(pid)`（运行线程实时计划） | 无此函数 | og 用 `EXPLAIN <当前SQL>` 估算（参数化语句优雅降级）+ 历史计划(statement_history，og 上含真实 actual time) |

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
执行计划恶化、锁堆积和 vacuum 性能问题。所有敏感变更先写恢复日志；支持
`doctor/init/run/status/stop/cleanup` 生命周期与管理员/普通用户双路径。

完整配置和安全说明见 [docs/gsbench/README.md](docs/gsbench/README.md)。

常用原生命令：

```bash
export GSBENCH_CONFIG=/absolute/path/to/configs/gsbench.cfg
export GSBENCH_PASSWORD='数据库密码'

./gsbench doctor
./gsbench init --profile quick
./gsbench run -s 1,2,8 -d 5m
```

`-s` 等价于 `--scenario`，`-d` 等价于 `--duration`；场景支持 `1–9` 编号、别名和完整名称混用。`-c/--config` 仍保留兼容。

## 构建

```bash
# 本地构建
go build ./...

# gsbench
go build -o gsbench ./cmd/gsbench
./gsbench version

# 交叉编译目标平台产物（静态二进制 + 配置 + 脚本打包）
./scripts/build.sh v1.5.0
#  -> dist/gstop_v1.5.0_linux_arm64_YYYYMMDD.tar.gz
#  -> dist/gstop_v1.5.0_linux_amd64_YYYYMMDD.tar.gz
```

## 安装与运行

```bash
# 解包后
./install.sh          # 检查 pidstat/iostat/nproc/uptime/lsblk，装到 ~/.local/bin
gstop                 # 前台 TUI
gstop -u dbadmin -p 12345 -i 5 -l 1   # 指定用户/端口/刷新/落盘间隔
gstop -d              # 守护模式

# 守护管理
./gstop-manage.sh start|stop|status|check|register|unregister
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
立即取消采集并退出进程。SQL/等待/计划跳变
跟随 `main.interval`，动态内存跟随 `main.mem_interval`，跨库慢项跟随
`main.health_slow_interval`（默认 300 秒）；动态内存查询同时沿用 CPU 阈值和最小刷新间隔保护。
每个采集模块共享 `main.collect_timeout`（默认 5 秒）预算；超时模块本轮显示空白但不推进计算基线，
下一轮成功后继续按上次成功快照计算差值或平均值。

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
