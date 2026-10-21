# pg-tools

[English](README.en.md) | 中文

PostgreSQL 递归删除 CLI：按外键关系先删除引用行，再删除目标行，整条链在一个事务里完成。

```bash
pg-tools -dsn "postgres://user:pass@localhost:5432/mydb?sslmode=disable" public.orders id 42
```

## 功能

- 从一个 `表 + 列 + 值` 出发，自动处理所有阻塞删除的外键引用。
- 两种策略：
  - `plan`（默认）：启动时读取全部外键并拓扑排序，按"子表在前"的顺序执行。
  - `detail`：复刻原 TS 脚本，撞外键错误后从 PostgreSQL 错误详情里解析键值，递归删除。
- 支持 `schema.table` 形式的表名、复合外键（`plan`）、自引用表。
- SQL 全部参数化，标识符统一加引号。
- `-dry-run` 跑完整条链后回滚，用于演练。
- 结束时按表汇总删除行数。

## 构建

```bash
go build -o pg-tools .
```

- Go 1.27.3（见 `go.mod`、`mise.toml`）
- 依赖：`github.com/jackc/pgx/v5`

## 用法

```
pg-tools [flags] <table> <column> <value>
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-dsn` | `$DATABASE_URL` | 连接串，URL 或 keyword/value 格式均可 |
| `-strategy` | `plan` | `plan` 或 `detail` |
| `-dry-run` | `false` | 执行完整条链后回滚 |
| `-max-statements` | `1000` | DELETE 语句硬上限，防止外键环路无限递归 |

环境变量方式：

```bash
export DATABASE_URL='postgres://user:pass@localhost:5432/mydb?sslmode=disable'
./pg-tools public.users id 1
```

```powershell
$env:DATABASE_URL = "postgres://user:pass@localhost:5432/mydb?sslmode=disable"
.\pg-tools.exe public.users id 1
```

示例：

```bash
# 演练：打印删除计划，最后回滚
pg-tools -dsn "$DATABASE_URL" -dry-run public.orders id 42

# 复刻 TS 的报错解析策略
pg-tools -dsn "$DATABASE_URL" -strategy detail public.orders id 42

# 指定 schema
pg-tools -dsn "$DATABASE_URL" app.orders id 42
```

进度和每一条 DELETE 打印到 stderr，最终汇总打印到 stdout。退出码：`0` 成功，`1` 运行失败（已回滚），`2` 参数错误。

`plan` 模式启动时会打印删除顺序：

```
delete order: public.order_items, public.orders, public.users
```

## 两种策略对比

| | `plan` | `detail` |
|---|---|---|
| 外键信息来源 | 启动时一次读取 `pg_constraint` | 同左，仅用于列映射 |
| 删除顺序 | 启动时拓扑排序 | 撞错后逐层递归 |
| 行选择方式 | 父谓词子查询 | 从错误 `DETAIL` 解析具体值 |
| 复合外键 | ✅ | ❌ 明确报错 |
| 键值含 `)` | ✅ | ❌ 正则截断 |
| 自引用表 | 回退递归 | 原生递归 |
| 多表环 | 执行任何删除前报错 | 预算耗尽后回滚 |
| CASCADE 子表 | 计划内显式删除，计入汇总 | 交给 PostgreSQL，不计入汇总 |

## 权限与安全

- 需要对目标表及其子表的 `DELETE` 权限，以及读取 `pg_constraint`（默认可用）。
- 所有值走绑定参数；表名、列名经 `pgx.Identifier` 引用，不拼接用户输入。
- 任何一步失败都会整体回滚，不会留下删了一半的数据。
- 生产库建议先加 `-dry-run` 确认删除范围。

## 测试

集成测试需要真实 PostgreSQL；未设置 `PGTOOLS_TEST_DSN` 时自动跳过。

```bash
export PGTOOLS_TEST_DSN='postgres://user:pass@localhost:5432/testdb?sslmode=disable'
go test -v ./...
```

```powershell
$env:PGTOOLS_TEST_DSN = "postgres://user:pass@localhost:5432/testdb?sslmode=disable"
go test -v ./...
```

测试会创建临时 schema `pgtools_test_<纳秒>`，结束时 `DROP SCHEMA ... CASCADE`，不影响现有数据。

## 已知限制

- `plan`：多表互相引用（环）无法排序，直接失败；自引用树走回退递归，语句数随树深增长。
- `detail`：只支持单列外键；依赖 `DETAIL` 的人类可读格式；键值含 `)` 会被截断；环路只能靠 `-max-statements` 兜底。
- `DEFERRABLE INITIALLY DEFERRED` 外键在 commit 时才校验，工具不会触发递归，最终以事务失败退出。
- `ON DELETE SET NULL/DEFAULT` 的子行会保留，且不计入汇总。
- 表名/列名中的 `.` 会被当作 schema 分隔符，名字本身含点的标识符不支持。

## 目录结构

```
main.go          CLI、连接、事务、汇总输出
cascade.go       外键快照、拓扑计划、plan/detail 两种删除策略
cascade_test.go  集成测试（需要 PGTOOLS_TEST_DSN）
go.mod           Go 模块与依赖
mise.toml        固定 Go 版本
```

## 许可证

[MIT](LICENSE) © 2026 ZyqGithub1
