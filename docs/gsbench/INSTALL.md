# gsbench v1.1.1 Linux ARM64 安装与操作手册

本文适用于 `gsbench v1.1.1` Linux ARM64 发布包。完整功能、场景范围和结果含义见[使用说明](README.md)，配置项见[配置手册](CONFIG.md)。

## 1. 安装

先确认目标主机是 64 位 ARM：

```sh
uname -m
```

输出应为 `aarch64` 或 `arm64`。将发布包复制到测试主机后执行：

```sh
tar -xzf gsbench-v1.1.1-linux-arm64-20260801.tar.gz
cd gsbench-v1.1.1-linux-arm64-20260801
sha256sum -c SHA256SUMS
chmod 0755 bin/gsbench
chmod 0600 configs/gsbench.cfg

file bin/gsbench
./bin/gsbench version
./bin/gsbench scenarios
```

`file` 应显示 ARM aarch64/ARM64 Linux 可执行文件，`version` 应显示 `v1.1.1`。如果发布包未附带 `SHA256SUMS`，应先向提供方取得校验值，不能跳过来源校验后直接在数据库主机运行。

## 2. 配置数据库连接

以发布包内的 [`configs/gsbench.cfg`](../../configs/gsbench.cfg) 为模板，至少确认：

```ini
[database]
host = 127.0.0.1
port = 5433
database = postgres
user = gaussdb
password_env = GSBENCH_PASSWORD

[data]
schema = gsbench_test

[run]
scenarios = 101
validation_enabled = false
```

不要把数据库密码直接写入 `gsbench.cfg`。推荐通过环境变量提供：

```sh
export GSBENCH_PASSWORD='数据库密码'
export GSBENCH_CONFIG="$PWD/configs/gsbench.cfg"
```

也可以设置 `database.password_config=gstop.cfg`，从同目录 gstop 配置的 `main.db_password` 读取密码。相对路径以 `gsbench.cfg` 所在目录为基准。

推荐始终传绝对配置路径，避免运维脚本依赖当前目录：

```sh
GSBENCH_CFG="$PWD/configs/gsbench.cfg"
./bin/gsbench doctor --config "$GSBENCH_CFG"
```

未显式指定配置时，固定按以下顺序查找：

1. `-c/--config`；
2. `GSBENCH_CONFIG`；
3. 当前目录的 `gsbench.cfg`；
4. 当前目录的 `configs/gsbench.cfg`；
5. 可执行文件同目录的 `gsbench.cfg`；
6. 可执行文件父目录下的 `configs/gsbench.cfg`。

显式路径不存在会直接失败，不会退回其他候选。`help`、`version`、`scenarios` 不读取配置，也不连接数据库。默认日志和 recovery ledger 位于配置文件同目录的 `logs/`。

## 3. 首次运行

先只读探测，再 dry-run，确认目标数据库、schema、容量和动作符合预期：

```sh
./bin/gsbench doctor --config "$GSBENCH_CFG"
./bin/gsbench init --config "$GSBENCH_CFG" --size 5GB --dry-run
./bin/gsbench init --config "$GSBENCH_CFG" --size 5GB
./bin/gsbench run --config "$GSBENCH_CFG" -s 101 -d 2m --dry-run
./bin/gsbench run --config "$GSBENCH_CFG" -s 101 -d 2m
./bin/gsbench status --config "$GSBENCH_CFG"
```

`-d` 是包含 ramp 与 hold 的总时长。601–606 会修改同一组执行计划状态，每次只能选择一个并逐个串行运行；配置层会拒绝将其中两个或更多放进同一次 run。

## 4. 恢复与清理

每次 `run` 返回前都会调用统一恢复协调器。看到 `RESTORE_FAILED`、stale run，或运行被异常中断时，按以下顺序处理：

```sh
./bin/gsbench restore --config "$GSBENCH_CFG" --dry-run
./bin/gsbench restore --config "$GSBENCH_CFG"
./bin/gsbench status --config "$GSBENCH_CFG"
```

只恢复一个运行时增加 `--run-id RUN_ID`。

普通 cleanup 不删除测试数据；只有显式增加 `--data` 才删除数据：

```sh
./bin/gsbench cleanup --config "$GSBENCH_CFG"
./bin/gsbench cleanup --config "$GSBENCH_CFG" --data
```

`cleanup --data` 会执行 `DROP SCHEMA ... CASCADE`，不可恢复。执行前必须再次核对配置文件、数据库和 `data.schema`，确认没有 stale recovery、遗留测试会话，且该 schema 只包含可丢弃的测试数据。

## 5. `validation_enabled` 的边界

发布配置默认：

```ini
[run]
validation_enabled = false
```

这只关闭模型预估、场景结果门槛和数据布局一致性校验，不会关闭容量与物理大小检查、未知数据库版本拒绝、真实 DDL/DML/负载错误，也不会关闭恢复锁、journal/ledger 持久化、逆操作和恢复安全边界。关闭验证不能把真实失败变成成功。

需要严格验收负载目标时，将其设为 `true`；此时未取得严格证据的运行不能算目标通过。

## 6. 20 GiB 现场验证

20 GiB 初始化和全场景验收必须使用专用测试 schema，不应复用客户业务 schema。建议复制配置为独立文件，并至少设置：

```ini
[run]
validation_enabled = true
dry_run = false

[data]
schema = gsbench_e2e_20260801
max_size_gb = 20
reuse_existing = false

[safety]
profile_cap_gb = 256
```

先执行：

```sh
export GSBENCH_E2E_SCHEMA=gsbench_e2e_20260801
GSBENCH_E2E_CFG=/absolute/path/gsbench-e2e.cfg

./bin/gsbench doctor --config "$GSBENCH_E2E_CFG"
./bin/gsbench init --config "$GSBENCH_E2E_CFG" --size 20GB --dry-run
./bin/gsbench init --config "$GSBENCH_E2E_CFG" --size 20GB
```

应以 gsbench 输出的物理大小证据确认结果，而不能只按估算行数判断。完整的 65 场景串行运行、gstop 同步采样、19–21 GiB 判定和安全清理流程见[全场景串行验证手册](FULL_SCENARIO_TEST.md)。

本手册描述的是目标 Linux/GaussDB 环境中的执行方法；当前 macOS 构建环境未完成 GaussDB live 连接、20 GiB 初始化或全场景负载验收。编译和单元测试通过不能替代现场 live 验证。
