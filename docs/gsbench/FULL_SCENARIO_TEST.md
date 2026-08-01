# gsbench 全场景串行验证

本手册用于在可连接目标数据库的 Linux 主机上验证 65 个已实现场景。脚本会初始化 20 GiB 专用数据集、启动一次 gstop、逐场景串行运行、保存证据并在全部安全检查通过后清理专用 schema。

## 安全边界

- 只允许 schema `gsbench_e2e_20260801`，并要求环境变量 `GSBENCH_E2E_SCHEMA` 精确确认同名 schema。
- live 配置必须设置 `reuse_existing=false`、`dry_run=false`、`validation_enabled=true` 和 `profile_cap_gb>=20`。
- `cleanup --data` 会执行不可恢复的 `DROP SCHEMA ... CASCADE`。脚本只在正常收尾路径执行；中断或失败时保留数据和全部证据。
- 每次只向 `gsbench run` 传一个场景编号。一次传多个编号会并发运行，不能用于负载目标回归。
- `NOT_APPLICABLE` 由 EVIDENCE JSON 的根 `outcome` 判断，不能由退出码或 status 推断。
- `run` 返回前已经执行内置恢复。脚本仅在内置恢复失败或证据缺失时尝试显式 restore，避免覆盖原场景结果。

建议新建一份独立配置，不要直接修改或复用客户业务 schema。配置中的数据库连接、密码来源和 gstop 配置必须提前验证。

## 前置条件

- Linux 主机能访问目标 openGauss/GaussDB。
- `gsbench`、`gstop`、`jq`、POSIX `sh` 可用。
- 数据库和表空间至少能安全容纳 20 GiB 数据及索引。
- 运行账号有创建/删除专用 schema、创建对象、执行全部所选 Risk A 场景的权限。
- 已配置 `GSBENCH_PASSWORD` 或配置引用的密码文件。

先验证脚本本身：

```sh
sh -n scripts/validate-gsbench-scenarios.sh
shellcheck -x -s sh scripts/validate-gsbench-scenarios.sh
sh tests/scripts/validate-gsbench-scenarios_test.sh
scripts/validate-gsbench-scenarios.sh --list
scripts/validate-gsbench-scenarios.sh --validate-config /absolute/path/gsbench-e2e.cfg
```

`--list` 必须输出 65 个唯一编号。

## 执行

```sh
export GSBENCH_PASSWORD='数据库密码'
export GSBENCH_E2E_SCHEMA=gsbench_e2e_20260801

scripts/validate-gsbench-scenarios.sh \
  --gsbench /absolute/path/gsbench \
  --gstop /absolute/path/gstop \
  --config /absolute/path/gsbench-e2e.cfg \
  --gstop-config /absolute/path/gstop.cfg \
  --artifacts /absolute/path/gsbench-e2e-artifacts
```

执行顺序固定为：

1. `doctor`；
2. `init --size 20GB`，确认物理大小在 19–21 GiB；
3. 启动一次 gstop；
4. 串行运行 65 个场景；
5. final restore 和 stale recovery 检查；
6. 再次校验配置指纹、schema 和环境确认；
7. `cleanup --data`。

101–103、401、402、404 各运行 60 秒；601–606、801 各运行 20 秒；其余场景各运行 10 秒。

## 产物与判定

产物目录权限默认为当前用户独占，包含：

- `results.tsv`：65 行场景报告，列为 `code name target observed ceiling operations errors outcome applicability evidence_path`；
- `runs.tsv`：run ID、退出码、时长、恢复状态和 status 路径；
- `scenarios/<code>.log` 与 `<code>.json`：原始输出和单场景 EVIDENCE；
- `gstop.log`：覆盖整个场景循环的同步采样；
- `doctor.log`、`init.log`、`status/`、`restore/`、`cleanup.log`。

判定规则：

- `SUCCESS`：适用场景达成自身验证规则且恢复成功；
- `NOT_APPLICABLE`：产品或拓扑不适用，不计失败，但必须保留证据；
- `UNVERIFIED`：模型验证关闭或没有严格判定，不能算作负载目标通过；live harness 因此要求 validation 开启；
- `DEGRADED`、`FAILED`、`RESTORE_FAILED`：均不能算通过；
- `operations>0` 且 `errors=0` 是工作负载基本条件，但不能替代 CPU、连接池、线程池等目标指标；
- `ceiling` 低于目标时应记录环境上限，不得把“跑到最大 worker”写成达标。

若脚本中断或失败，它不会清理 schema。先查看对应日志，执行：

```sh
gsbench restore --config /absolute/path/gsbench-e2e.cfg --dry-run
gsbench restore --config /absolute/path/gsbench-e2e.cfg
gsbench status --config /absolute/path/gsbench-e2e.cfg
```

只有确认无 stale recovery、无遗留测试会话，并再次核对专用 schema 后，才手工执行 `cleanup --data`。
