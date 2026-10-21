# pg-tools

English | [中文](README.md)

A recursive delete CLI for PostgreSQL: deletes referencing rows first, then the target row, all inside a single transaction.

```bash
pg-tools -dsn "postgres://user:pass@localhost:5432/mydb?sslmode=disable" public.orders id 42
```

## Features

- Starts from a `table + column + value` and clears every foreign key reference that blocks the delete.
- Two strategies:
  - `plan` (default): reads all foreign keys at startup, topologically sorts them and deletes children before parents.
  - `detail`: reproduces the original TS script by parsing the key value out of the PostgreSQL error detail and recursing.
- Supports `schema.table` names, composite foreign keys (`plan`) and self-referencing tables.
- Parameterized SQL; identifiers are always quoted.
- `-dry-run` executes the whole chain and rolls it back.
- Prints a per-table row count when done.

## Build

```bash
go build -o pg-tools .
# Override the version (defaults to 0.1.0)
go build -ldflags "-X main.version=0.1.0" -o pg-tools .
```

- Go 1.27.3 (see `go.mod`, `mise.toml`)
- Dependency: `github.com/jackc/pgx/v5`

## Usage

```
pg-tools [flags] <table> <column> <value>
```

| Flag | Default | Description |
|---|---|---|
| `-dsn` | `$DATABASE_URL` | Connection string, URL or keyword/value form |
| `-strategy` | `plan` | `plan` or `detail` |
| `-dry-run` | `false` | Execute the whole chain, then roll back |
| `-max-statements` | `1000` | Hard cap on DELETE statements, guards against FK cycles |
| `-version` | - | Print the version and exit |

Using the environment variable:

```bash
export DATABASE_URL='postgres://user:pass@localhost:5432/mydb?sslmode=disable'
./pg-tools public.users id 1
```

```powershell
$env:DATABASE_URL = "postgres://user:pass@localhost:5432/mydb?sslmode=disable"
.\pg-tools.exe public.users id 1
```

Examples:

```bash
# Rehearse: print the plan, then roll back
pg-tools -dsn "$DATABASE_URL" -dry-run public.orders id 42

# Reproduce the TS error-detail strategy
pg-tools -dsn "$DATABASE_URL" -strategy detail public.orders id 42

# Schema-qualified table
pg-tools -dsn "$DATABASE_URL" app.orders id 42
```

Progress and every DELETE go to stderr; the final summary goes to stdout. Exit codes: `0` success, `1` runtime failure (rolled back), `2` usage error.

`plan` mode prints the deletion order at startup:

```
delete order: public.order_items, public.orders, public.users
```

## Strategy comparison

| | `plan` | `detail` |
|---|---|---|
| FK information | Reads `pg_constraint` once at startup | Same, used only for column mapping |
| Deletion order | Topological sort at startup | Recursion triggered by errors |
| Row selection | Parent predicate subqueries | Concrete values parsed from `DETAIL` |
| Composite foreign keys | ✅ | ❌ explicit error |
| Key value containing `)` | ✅ | ❌ truncated by the regex |
| Self-referencing table | Fallback recursion | Native recursion |
| Multi-table cycles | Error before any delete | Rolled back after the budget runs out |
| CASCADE children | Deleted explicitly, counted in the summary | Left to PostgreSQL, not counted |

## Permissions and safety

- Requires `DELETE` on the target and child tables, plus read access to `pg_constraint` (granted by default).
- All values are bound parameters; table and column names go through `pgx.Identifier`, never user input.
- Any failure rolls back the whole transaction, so data is never left half-deleted.
- On production, run with `-dry-run` first to check the scope.

## Tests

The integration tests need a real PostgreSQL; they are skipped when `PGTOOLS_TEST_DSN` is unset.

```bash
export PGTOOLS_TEST_DSN='postgres://user:pass@localhost:5432/testdb?sslmode=disable'
go test -v ./...
```

```powershell
$env:PGTOOLS_TEST_DSN = "postgres://user:pass@localhost:5432/testdb?sslmode=disable"
go test -v ./...
```

Each test creates a temporary schema `pgtools_test_<nanos>` and drops it with `DROP SCHEMA ... CASCADE` when done; existing data is untouched.

## Known limitations

- `plan`: mutually referencing tables (cycles) cannot be ordered and fail immediately; self-referencing trees use fallback recursion, so the statement count grows with tree depth.
- `detail`: single-column foreign keys only; depends on the human-readable `DETAIL` format; values containing `)` are truncated; cycles are only caught by `-max-statements`.
- `DEFERRABLE INITIALLY DEFERRED` foreign keys are checked at commit, so no recursion is triggered; the run fails at commit and rolls back.
- Rows kept by `ON DELETE SET NULL/DEFAULT` survive and are not counted in the summary.
- A `.` in a table or column name is treated as the schema separator; identifiers that contain a literal dot are not supported.

## Layout

```
main.go          CLI, connection, transaction, summary output
cascade.go       FK snapshot, topological plan, plan/detail strategies
cascade_test.go  Integration tests (require PGTOOLS_TEST_DSN)
go.mod           Go module and dependencies
mise.toml        Pinned Go version
```

## License

[MIT](LICENSE) © 2026 ZyqGithub1
