# gsbench

Author: WangYingJie <sqlrush@gmail.com>

gsbench 是面向 openGauss 和集中式 GaussDB 的独立压测与故障场景生成器。它自动创建专用基准 schema 和数据，通过配置组合负载，并使用数据库证据判断场景是否真实达到目标。

## 命令

先设置一次配置文件环境变量：

```bash
export GSBENCH_CONFIG=/absolute/path/to/configs/gsbench.cfg
```

然后直接使用原生命令：

```bash
gsbench doctor
gsbench init [--profile quick|stress] [--dry-run]
gsbench run [-s|--scenario LIST] [-d|--duration DURATION] [--dry-run]
gsbench status [--run-id RUN_ID]
gsbench stop [--run-id RUN_ID]
gsbench cleanup [--run-id RUN_ID] [--data]
gsbench version
```

配置路径优先级为 `-c/--config`、`GSBENCH_CONFIG`、`./gsbench.cfg`、`./configs/gsbench.cfg`。旧命令保持兼容：

```bash
gsbench doctor -c configs/gsbench.cfg
```

不指定 run-id 时，stop 会停止所有状态为 running 或 stop_requested 的 gsbench 运行。cleanup 默认只停止负载并恢复变更；只有 cleanup --data 才删除专用 gsbench schema。

## 作者信息

屏幕输出、新后台日志、version 输出、示例配置和本文档开头固定显示：

    gsbench <version>
    Author: WangYingJie <sqlrush@gmail.com>

## 密码

配置只保存环境变量名：

    [database]
    password_env = GSBENCH_PASSWORD

运行前设置该变量。密码和完整 DSN 不会写入屏幕或日志。

## 初始化数据

quick 默认生成不超过 5 GB，stress 默认不超过 20 GB。可用 data.max_size_gb 进一步限制。生成器创建：

- accounts、customers、orders、order_items：TP 基准数据。
- fact_sales、dim_product、dim_store：AP 基准数据。
- plan_data 和专用索引：计划跳变。
- lock_targets：阻塞链和扇出等待。
- vacuum_targets：更新膨胀和 vacuum。
- meta_runs、meta_journal、meta_batches：运行状态、恢复日志和批次断点。

造数以提交批次执行。中断后再次 init 会从最后高水位继续。每批开始前检查磁盘；可用空间达到保留线时立即终止。默认至少保留文件系统总容量的 20%。

## 九个场景

- tp_cpu：短事务点查、更新、插入，闭环提升并发至 CPU 目标。
- ap_cpu：事实表扫描、JOIN、聚合和排序，闭环提升并发。
- mixed_cpu：TP/AP 两组 worker，默认 80:20。
- connection_pool：idle、idle in transaction、active 连接组合，逼近 max_connections 目标。
- thread_pool：填满已启用的数据库线程池；线程池不存在时只给 DEGRADED，不伪报成功。
- dynamic_memory：并发 Hash Join、Sort、Hash Aggregate，依据动态内存视图闭环升压。
- plan_regression：index_unusable、stats_skew 或 hard_parse，要求计划变化、结果一致且实测变慢。
- lock_storm：多级阻塞链和扇出等待；死锁默认关闭。
- vacuum_pressure：普通 VACUUM、VACUUM ANALYZE 或显式启用的 VACUUM FULL，并测量前台延迟退化。

场景可以使用完整名称、别名或编号：

| 编号 | 完整名称 | 别名 |
|---:|---|---|
| 1 | `tp_cpu` | `tp` |
| 2 | `ap_cpu` | `ap` |
| 3 | `mixed_cpu` | `mixed` |
| 4 | `connection_pool` | `connections` |
| 5 | `thread_pool` | `threads` |
| 6 | `dynamic_memory` | `memory` |
| 7 | `plan_regression` | `plan` |
| 8 | `lock_storm` | `locks` |
| 9 | `vacuum_pressure` | `vacuum` |

三种写法可以混用，位置参数也支持编号：

```bash
gsbench run -s 1,ap,lock_storm -d 5m
gsbench run 1,2,8
```

## 结果

- SUCCESS：真实数据库指标达到并持续满足目标。
- DEGRADED：负载已运行，但真实指标无权限读取或目标能力不存在。
- FAILED：准备、升压、验证、停止或恢复失败，或未达到目标。

run 在 SUCCESS 时退出 0，DEGRADED 时退出 3，FAILED 时退出 1。

## 权限策略

doctor 先探测产品版本、拓扑、管理员权限、线程池、动态内存、OS_RUNTIME、statement_history 和 vacuum 统计视图。权限不足只影响对应增强路径；普通路径使用基准用户自有对象和会话级设置。

线程池若根本未启用，普通用户无法创造一个不存在的真实池，此时工具明确输出 DEGRADED。重启相关参数只有 safety.allow_database_restart=true 且配置了 restart_command 才允许使用；默认禁止。

## 恢复与停止

所有计划、统计和 vacuum 基准数据变更都先写 meta_journal，再执行前向操作。恢复按 ID 逆序执行并验证结果。进程被 SIGKILL 后，下一次 doctor、run、stop 或 cleanup 会处理未恢复条目。

每个连接使用：

    application_name=gsbench/<run_id>/<scenario>/<worker_id>

stop 只终止完全匹配该 run 边界的会话，不会匹配 run ID 前缀相似的其他运行。

## 重要安全项

- 所有 run 必须有有限 duration。
- CPU 默认目标 95%，动态内存默认 90%。
- max_workers、max_connections 和 query_timeout 始终生效。
- VACUUM FULL、死锁、实例参数修改和数据库重启分别显式控制。
- 连接池场景预先保留 control pool，默认目标 95% 而非锁死全部连接。
- cleanup --data 删除专用 schema，数据只能通过重新 init 恢复。

## 日志

后台日志默认写入当前目录的 logs/gsbench_<run_id>.log。日志包含作者、能力探测、阶段、并发调整、错误分类、验证证据和每次恢复结果。
