# pgmem

`pgmem` is a process-local PostgreSQL emulator for fast Go tests. It ports the
database-and-schema testing model popularized by
[`oguimbal/pg-mem`](https://github.com/oguimbal/pg-mem) to an idiomatic Go API,
with no PostgreSQL server. SQL is parsed into PostgreSQL's own AST through
[`pg_query_go`](https://github.com/pganalyze/pg_query_go), then translated for
the embedded execution engine; syntax is never classified with regular
expressions.

```go
database := pgmem.MustNew()
defer database.Close()

public := database.Public()
if err := public.None(`
  CREATE TABLE users (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL
  )
`); err != nil {
  panic(err)
}

if err := public.None(`INSERT INTO users (name) VALUES ($1)`, "Ada"); err != nil {
  panic(err)
}

user, err := public.One(`SELECT id, name FROM users`)
```

The same database can back code written against `database/sql`:

```go
pool := database.Open()
defer pool.Close()

ctx := context.Background()
var name string
err = pool.QueryRowContext(ctx,
  `SELECT name FROM users WHERE id = $1`,
  1,
).Scan(&name)
```

`DB.Open` uses the public schema. Use `schema.Open` when unqualified statements
should target a named schema. Close every returned pool before closing the
owning `DB`.

Backups are reusable restore points for the public schema and every named
schema:

```go
backup, err := database.Backup()
if err != nil {
  panic(err)
}

// Run a test that changes rows, then reset its data for the next case.
if err := backup.Restore(); err != nil {
  panic(err)
}
```

Data-only changes can be restored repeatedly. Restore returns
`ErrSchemaChanged` if DDL or the named-schema topology changed after the
backup, without modifying the current data. Context cancellation is honored
while validating the restore; once its commit point starts, the already
validated public and named-schema swap runs to completion without cancellation.
Exceptional engine failures during that internal multi-schema swap are not a
transactional rollback boundary.

Schema-local interceptors can supply ad-hoc results before parsing or
execution:

```go
subscription := public.InterceptQueries(func(
  ctx context.Context,
  statement string,
  arguments []any,
) (pgmem.Result, bool, error) {
  if statement != "current user" {
    return pgmem.Result{}, false, nil // pass through
  }
  return pgmem.Result{
    Rows: []pgmem.Row{{"name": "Ada"}},
  }, true, nil
})
defer subscription.Unsubscribe()
```

Interceptors run in registration order and the first handled result wins.
They currently apply to `Schema` methods only, not calls made through
`DB.Open`, `Schema.Open`, or `DB.SQLDB`.

Scalar functions can be registered after the database is already in use. A
function registered on `DB` belongs to `public`; register on a `Schema` for a
schema-local function:

```go
err := database.RegisterFunction(pgmem.Function{
  Name:  "greet",
  Arity: 1,
  Implementation: func(arguments ...any) (any, error) {
    return "hello " + arguments[0].(string), nil
  },
})
if err != nil {
  panic(err)
}

row, err := public.One(`SELECT greet('Ada') AS greeting`)
```

Fixed arities may be overloaded; a variadic definition uses `Arity` as its
minimum. Registrations and replacements are isolated per database and schema.
Callback errors and panics become query errors instead of escaping through the
embedded engine. Callbacks run synchronously inside the database's sole
executor connection: they must be prompt and must not query, back up, restore,
or close their owning database.

## Compatibility target

The v0.1 contract covers the database/schema API, isolated schemas, typed
errors, materialized query results, PostgreSQL AST/type translation, backups,
query interception, schema-local scalar functions, raw table fixtures, and the
translating `database/sql` adapter. See [COMPATIBILITY.md](./COMPATIBILITY.md)
for the exact tested surface and known gaps.

Like upstream pg-mem, this is a best-effort unit-test emulator rather than a
production PostgreSQL replacement. Always validate migrations and integration
behavior against the PostgreSQL versions your application supports.

## Development

The module requires Go 1.26 and a C compiler because `pg_query_go` embeds the
PostgreSQL parser through cgo. Behavior tests use Ginkgo v2 and Gomega;
expectation-based interface mocks use gomock.

```bash
go test -race ./...
go generate ./...
```

## License and origin

Licensed under the [MIT License](./LICENSE). This is an independent Go
implementation inspired by pg-mem's documented behavior; see
[UPSTREAM.md](./UPSTREAM.md) for the compatibility and attribution boundary.
