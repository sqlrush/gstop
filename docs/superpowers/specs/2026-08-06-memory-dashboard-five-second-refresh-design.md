# `m` 内存大盘 5 秒刷新设计

## 背景

当前 `m` 内存大盘由 `main.mem_interval` 控制刷新周期，默认值为 30 秒。其中汇总和全局动态内存面板按该周期刷新，但会话与线程动态内存面板还会经过 `Health.ShouldRefreshMemory("memory")`，因此额外受到以下保护：

- `main.dynamic_mem_interval` 默认 60 秒的最短刷新间隔；
- `main.dynamic_mem_cpu_thresh` 默认 50% 的 CPU 门槛。

这使 `m` 大盘各面板的实际刷新频率不一致。需求是只调整 `m`，使其默认每 5 秒完整刷新；`h` 健康大盘的动态内存保护保持不变。

## 目标行为

- `main.mem_interval` 的默认值由 30 秒改为 5 秒。
- `m` 每轮刷新全部四类内存数据，不再调用动态内存的 CPU/最短间隔门控。
- `m` 仍然使用 `main.mem_interval` 作为周期；用户显式配置其他正整数时继续尊重该值。
- `main.mem_interval = 0` 仍禁用 `m` 大盘。
- `main.dynamic_mem_enable = false` 仍禁用动态内存功能及 `m` 大盘入口。
- `h` 健康大盘继续受 `main.mem_interval`、`main.dynamic_mem_interval`、`main.dynamic_mem_cpu_thresh` 和单轮采集互斥保护。
- 已有配置文件中的显式 `mem_interval = 30` 不做隐式迁移；升级时需要把该配置改为 `5` 才会获得 5 秒刷新。

## 实现方案

### `m` 大盘

在 `internal/monitor/memory.go` 中：

- 将代码兜底刷新周期改为 5 秒；
- 保留当前循环的耗时补偿，避免“采集耗时 + 5 秒”造成周期漂移；
- 删除 `Refresh` 中针对会话和线程动态内存面板的 `ShouldRefreshMemory("memory")` 条件，使四类面板在每轮都刷新；
- 不修改 `Health` 的公共门控实现。

### 配置与文档

- 将发行配置 `configs/gstop.cfg` 的 `main.mem_interval` 改为 5，并同步注释中的默认值；
- 更新 README 对 `m` 与 `h` 刷新策略的描述，明确 CPU/最短间隔保护只适用于 `h` 的动态内存采集。

### `h` 健康大盘

不修改 `internal/healthdash` 和 `internal/health` 的门控逻辑。`h` 仍以独立 key `health-dashboard` 调用 `ShouldRefreshMemory`，继续执行 CPU 阈值和最短间隔判断。

`main.mem_interval` 是现有共享配置，因此发行配置改成 5 秒后，`h` 会每 5 秒检查一次动态内存是否具备采集资格；这只是进程内的轻量门控判断。默认 `main.dynamic_mem_interval = 60` 时，`h` 的实际动态内存 SQL 仍不超过每 60 秒一次，并继续在 CPU 达到门槛或上一轮未结束时跳过。本次不增加新的 `h` 刷新配置项。

## 测试与验收

增加或调整最小范围单元测试，验证：

1. `m` 未配置 `mem_interval` 时使用 5 秒兜底值；
2. `m.Refresh` 每轮都刷新会话和线程动态内存，不依赖 Health 门控结果；
3. `h` 的既有门控测试继续通过，证明其限制没有被取消；
4. 运行受影响包测试，确认编译和行为正确。

## 非目标

- 不取消 `h` 的 CPU、最短间隔或单轮采集互斥保护；
- 不改变主监控 `main.interval`；
- 不自动覆盖用户已有的显式配置；
- 不改变数据库查询内容和展示布局。
