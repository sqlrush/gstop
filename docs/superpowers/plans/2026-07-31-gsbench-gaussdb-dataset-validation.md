# gsbench GaussDB 数据集校验兼容 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 GaussDB ORA/A 模式的 `DATE` 等价类型误报，并在创建索引前发现旧版缺列数据表。

**Architecture:** 保留现有结构合同，在列比较函数中加入仅针对 expected `date` 的定向等价规则；初始化按建表、迁移、表校验、建索引、索引校验的顺序执行。既不删除 schema，也不放过无关类型差异。

**Tech Stack:** Go 1.26、标准库 `testing`、现有 `internal/gsbench` 数据集目录接口。

## Global Constraints

- 不自动删除或重建用户 schema。
- 只允许 `date` 对 `timestamp(0) without time zone` 的定向等价。
- 其他列、主键、分布键和索引合同保持严格校验。
- 最终产物必须是 `linux/arm64`、`CGO_ENABLED=0` 的静态二进制。

---

### Task 1: GaussDB DATE 等价类型

**Files:**
- Modify: `internal/gsbench/sqlstore_test.go`
- Modify: `internal/gsbench/sqlstore.go`

**Interfaces:**
- Consumes: `equalDatasetColumns(actual, expected []datasetColumnShape) bool`
- Produces: `datasetColumnTypesEquivalent(actual, expected string) bool`

- [ ] 在 `sqlstore_test.go` 增加测试：actual `timestamp(0)withouttimezone`、expected `date` 应相等；actual `timestamp`、expected `date` 仍不相等。
- [ ] 运行 `go test ./internal/gsbench -run 'TestDatasetColumnShapeComparison' -count=1`，确认等价测试因缺少规则失败。
- [ ] 在 `equalDatasetColumns` 中分别比较名称、可空性和 `datasetColumnTypesEquivalent`；等价函数只接受完全相同类型或上述定向 DATE 映射。
- [ ] 重跑同一测试，确认通过。

### Task 2: 索引前验证现有表

**Files:**
- Modify: `internal/gsbench/dataset_test.go`
- Modify: `internal/gsbench/dataset.go`

**Interfaces:**
- Consumes: `DatasetObjectCatalog.ValidateDatasetObject`
- Produces: `ensureDatasetObjects` 的表阶段、表验证阶段、索引阶段顺序。

- [ ] 扩展测试执行器以便按对象返回验证错误，新增测试模拟 `accounts` 验证失败，并断言没有执行 `CREATE INDEX accounts_customer_idx`。
- [ ] 运行该测试，确认当前实现先尝试创建索引而失败测试。
- [ ] 重构初始化顺序：先处理全部表，完成补列和迁移后验证表，再处理并验证全部索引。
- [ ] 重跑相关初始化测试，确认旧表在索引前被拦截，迁移仍先于验证。

### Task 3: 回归、构建与发布

**Files:**
- Create: `release/gsbench-v1.1.0-linux-arm64-dataset-validation-fix-20260731/`
- Create: `release/gsbench-v1.1.0-linux-arm64-dataset-validation-fix-20260731.tar.gz`
- Create: `release/gsbench-v1.1.0-linux-arm64-dataset-validation-fix-20260731.tar.gz.sha256`

**Interfaces:**
- Consumes: 修复后的 `cmd/gsbench` 与发布文档。
- Produces: Linux ARM64 静态替换包及 SHA-256。

- [ ] 运行 `go test ./internal/gsbench -count=1`。
- [ ] 用 `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' ./cmd/gsbench` 构建。
- [ ] 用 `file` 和 `go version -m` 验证 ARM64、静态链接和源码提交信息。
- [ ] 复制安装、配置和替换文档，补充本次两个错误的处理说明。
- [ ] 生成文件级 `SHA256SUMS`、压缩包及压缩包 SHA-256，并逐项校验。
