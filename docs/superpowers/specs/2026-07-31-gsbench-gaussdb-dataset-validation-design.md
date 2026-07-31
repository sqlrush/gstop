# gsbench GaussDB 数据集校验兼容设计

## 背景

GaussDB ORA/A 兼容模式会把 DDL 中的 `DATE` 登记为
`TIMESTAMP(0) WITHOUT TIME ZONE`。gsbench 当前按类型字符串严格比较，导致全新
创建的 `fact_sales.sale_date` 被误判为不兼容。旧版数据集的 `accounts` 表则没有
新版需要的 `dist_key`，初始化会在创建 `accounts_customer_idx` 时才返回数据库
缺列错误。

## 设计

保留数据集结构合同，不自动删除或重建用户 schema：

1. 列结构比较仅增加一个定向等价规则：期望类型为 `date` 时，允许数据库实际
   类型为 `timestamp(0) without time zone`。其他类型、列顺序和可空性仍严格比较。
2. 初始化顺序调整为“建表 → 补列/迁移 → 验证表 → 创建索引 → 验证索引”。
   旧版 `accounts` 因缺少 `dist_key` 会提前返回 `validate dataset table accounts`
   类错误，而不是晚到索引创建阶段才失败；可迁移旧表则先完成迁移再接受校验。
3. 新建表仍在全部对象创建后执行完整校验，覆盖列、主键、分布键和索引合同。

## 测试与验收

- 回归测试证明 `date` 与 GaussDB ORA/A 返回的
  `timestamp(0) without time zone` 可通过列合同校验。
- 回归测试证明其他 `date`/`timestamp` 差异仍会失败。
- 回归测试证明旧版 `accounts` 表在创建索引之前就被验证并停止初始化。
- `internal/gsbench` 测试通过，Linux ARM64 静态二进制完成构建并生成 SHA-256。

## 非目标

- 不删除 schema，不自动迁移旧业务数据。
- 不关闭主键、分布键或索引校验。
- 不把所有日期/时间类型无条件归一化为同一种类型。
