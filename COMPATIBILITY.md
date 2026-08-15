# Compatibility contract

This document defines the `pgmem` v0.1 compatibility surface. The reference is
[`oguimbal/pg-mem` v3.0.14 at commit
`0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6`](https://github.com/oguimbal/pg-mem/tree/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6).
It is a behavioral target, not a claim that the Go and TypeScript APIs are
identical.

`pgmem` parses every SQL statement with PostgreSQL's parser through
`pg_query_go`, rewrites the resulting AST, and executes the supported subset in
an embedded SQLite database. Consequently, a statement may be valid PostgreSQL
but still fail or behave differently at the execution boundary. Treat anything
not listed as supported below as unsupported until it has a black-box test.

## v0.1 matrix

| Capability | v0.1 status | Go surface and verified behavior | Pinned upstream evidence | Limitations |
| --- | --- | --- | --- | --- |
| Isolated in-memory database and public schema | Supported | `New`, `MustNew`, `Public`, and idempotent `Close`; each `DB` owns isolated process-local state | [`readme.md`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/readme.md), [`src/db.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/db.ts) | No PostgreSQL server, socket, authentication, roles, or persistence |
| Basic DDL and CRUD | Supported subset | `Schema.None`, `Exec`, `Query`, `Many`, and `One` cover `CREATE TABLE`, parameterized `INSERT`, `SELECT`, `UPDATE`, and `DELETE`; rows are copied and materialized as `Row` values | [`src/tests/simple-queries.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/simple-queries.spec.ts), [`src/tests/insert.queries.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/insert.queries.spec.ts), [`src/tests/update.queries.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/update.queries.spec.ts), [`src/tests/delete.queries.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/delete.queries.spec.ts) | PostgreSQL execution semantics exist only where documented and tested here; `None`/`Exec` discard returned rows |
| PostgreSQL parameters | Supported subset | `$1`, `$2`, and later positional parameters work with single statements through every schema execution method | [`src/tests/prepare.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/prepare.spec.ts) | Arguments in multi-statement scripts are rejected with SQLSTATE `0A000`; named parameters and PostgreSQL parameter type inference are not promised |
| `database/sql` adapter | Supported subset | `DB.Connector`/`Open` target public; `Schema.Connector`/`Open` retain that schema as the default. Direct exec/query and prepare are context-aware and AST-translated, `$1` arguments scan through ordinary `database/sql` APIs, and transactions stay on one pinned internal connection through commit or rollback | Upstream's ecosystem adapters are documented in [`readme.md`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/readme.md); this is their idiomatic Go counterpart rather than a matching client API | Pools deliberately allow one logical connection; transaction isolation/read-only fidelity, multi-statement adapter calls, PostgreSQL driver-specific types, and query interception through this adapter are not promised. It is not a PostgreSQL wire driver |
| Multi-statement scripts | Supported subset | Semicolon-delimited scripts are split with the PostgreSQL parser and execute in one transaction; `Query` materializes the last statement, and a failed statement rolls back the script | [`src/tests/transactions.queries.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/transactions.queries.spec.ts), [`src/tests/invalid-syntaxes.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/invalid-syntaxes.spec.ts) | The implicit wrapper is a v0.1 Go API guarantee, not an assertion that every PostgreSQL client treats scripts identically; parameters are unsupported in scripts |
| `RETURNING` | Supported | `Query`, `Many`, and `One` materialize rows from `INSERT`, `UPDATE`, and `DELETE ... RETURNING` | [`src/tests/simple-queries.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/simple-queries.spec.ts), [`src/tests/insert.queries.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/insert.queries.spec.ts), [`src/tests/update.queries.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/update.queries.spec.ts), [`src/tests/delete.queries.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/delete.queries.spec.ts) | Use a row-returning method; `Exec` exposes affected-row metadata rather than returned rows |
| NULL logic | Supported subset | SQL three-valued filtering for `= NULL` and `!= NULL`, plus `IS NULL` and `IS NOT NULL`, is verified | [`src/tests/nulls.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/nulls.spec.ts), [`src/tests/simple-queries.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/simple-queries.spec.ts) | PostgreSQL NULL ordering defaults and JSON `null` versus SQL `NULL` are not promised |
| Filtering, ordering, and pagination | Supported subset | Basic `WHERE`, multi-expression `ORDER BY`, `LIMIT`, and `OFFSET` are verified | [`src/tests/where.queries.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/where.queries.spec.ts), [`src/tests/order-by.queries.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/order-by.queries.spec.ts), [`src/tests/limit.queries.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/limit.queries.spec.ts) | Collations, PostgreSQL `NULLS FIRST`/`NULLS LAST` defaults, planner/index behavior, and complex expressions are backend-dependent unless separately tested |
| Constraints, defaults, and statement atomicity | Supported subset | Primary key, unique, not-null, check, same-schema inline foreign key, and column default enforcement; a failed multi-row statement leaves no partial rows; explicit `NULL` is not replaced by a default | [`src/tests/constraints.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/constraints.spec.ts), [`src/tests/foreign-keys.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/foreign-keys.spec.ts), [`src/tests/insert.queries.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/insert.queries.spec.ts), [`src/tests/publicapi.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/publicapi.spec.ts) | Cross-schema and deferrable constraints, `ALTER TABLE` constraint coverage, PostgreSQL constraint names/messages, `MATCH` variants, and full cascade semantics are not promised |
| SQLSTATE-shaped errors | Supported subset | `Error` preserves statement and cause; parser errors use `42601`; common unique, not-null, foreign-key, check, undefined-table, and undefined-column failures are mapped | [`src/interfaces.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/interfaces.ts), [`src/tests/test-utils.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/test-utils.ts) | Error text comes from the embedded executor, not PostgreSQL; unmapped execution errors use `XX000`; SQLSTATE coverage is not exhaustive |
| PostgreSQL syntax validation and AST translation | Supported | Statements are parsed structurally with the PostgreSQL parser before execution; invalid syntax is rejected before the executor; public-schema qualification, a focused type set, and `ILIKE` are AST-rewritten | Upstream uses a different parser and warns about parser gaps in [`readme.md`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/readme.md); pinned invalid cases are in [`src/tests/invalid-syntaxes.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/invalid-syntaxes.spec.ts) | Valid PostgreSQL syntax can still be unsupported by the translator or SQLite executor; `ILIKE` currently follows SQLite `LIKE` behavior and is not locale-aware PostgreSQL matching |
| Common PostgreSQL type spellings | Supported subset | `serial` family, integer aliases, `bytea`, `jsonb`, `timestamptz`, and `uuid` are translated to internal storage types | [`src/tests/schema-manipulation.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/schema-manipulation.spec.ts), [`src/tests/datatypes.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/datatypes.spec.ts) | Translation is storage-oriented, not PostgreSQL type fidelity: UUID validation, JSONB operators/order, arrays, ranges, enums, domains, precision/scale, timezone semantics, and binary `bytea` formats are not promised |
| Identifier handling | Supported subset | Ordinary unquoted identifiers and quoted identifiers containing spaces or escaped quotes are accepted | [`src/tests/naming.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/naming.spec.ts) | SQLite treats identifiers case-insensitively, including quoted identifiers; PostgreSQL-distinct objects such as `example` and `"Example"` cannot coexist; reserved-word and catalog shadowing behavior can differ |
| Named schemas | Supported subset | `CreateSchema`, `Schema`, and `SchemaNames`; a named `Schema` qualifies unqualified relations, same-named tables remain isolated, and explicitly qualified cross-schema access works | [`src/db.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/db.ts), [`src/tests/schema-manipulation.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/schema-manipulation.spec.ts) | Schemas are created through the Go API; SQL `CREATE SCHEMA`, `DROP SCHEMA`, `search_path`, schema-local catalogs/functions/types, renaming, and quoted schema names are not supported in v0.1 |
| Reusable backup and restore | Supported | `DB.Backup`/`BackupContext` snapshot public plus every named schema; `Backup.Restore`/`RestoreContext` restore data repeatedly, keep existing `Schema` handles usable, and are safe with concurrent callers. Images are internally owned and copied | [`src/db.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/db.ts) | Unlike upstream's structural-sharing backup, this copies embedded database images and is not O(1). Restore rejects DDL or named-schema topology changes with `ErrSchemaChanged` before changing data; it is a restore point for the owning live `DB`, not a clone or portable dump |
| Query interception | Supported subset | `Schema.InterceptQueries` registers ordered, schema-local `QueryInterceptor` callbacks over the original SQL and copied arguments. The first callback returning handled wins; intercepted `Result` rows are copied, callback errors preserve identity, and `Subscription.Unsubscribe` is concurrent-safe and idempotent | [`src/schema/schema.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/schema/schema.ts) | Applies before parse/translation to `Schema` execution methods only. Calls through `DB.Open`, `Schema.Open`, or `DB.SQLDB`, including prepared and transactional adapter calls, are not intercepted in v0.1 |
| Custom scalar functions | Supported subset | `DB.RegisterFunction` registers in `public`; `Schema.RegisterFunction` is schema-local. `Function` supports fixed overloads and one minimum-arity variadic overload per name, runtime registration/replacement, qualified and nested calls, database isolation, and concurrent use. Callback errors, invalid return values, and panics become SQLSTATE `38000` query errors | Upstream function registration is documented in [`readme.md`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/readme.md) and implemented at the schema boundary in [`src/schema/schema.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/schema/schema.ts) | Positional scalar calls only: no PostgreSQL argument/return type declarations or coercion, named/default arguments, aggregate/window/table functions, volatility/determinism flags, `CREATE FUNCTION`, removal, or extension bundles. Names must use the API's simple identifier form and are matched case-insensitively. Callbacks are synchronous and not context-cancelable once entered; they must be prompt and must not query, back up, restore, or close their owning `DB`. Registrations are not backup data |
| Raw table fixtures and inspection | Supported subset | `Schema.Table`, `GetTable`, and `TableContext` return a checked `Table`; `Table.Insert`/`InsertContext` safely quote map keys and return the stored row after defaults/generated values; `Find`/`FindContext` combines template entries with `AND`, treats a nil value as `IS NULL`, and treats a nil or empty template as all rows | Upstream inspection and direct insertion are documented in [`readme.md`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/readme.md) and exercised in [`src/tests/publicapi.spec.ts`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/src/tests/publicapi.spec.ts) | This is a fixture helper, not an ORM: equality templates only, no projection/order/limit/raw update/delete, and no typed structs; a view can be looked up but remains subject to the executor's write rules |
| Injectable translation | Supported | `WithTranslator` supplies a context-aware `Translator`; internal-dialect output executes without PostgreSQL reparsing when no custom functions are registered, and the boundary is covered with generated Gomock expectations | No direct pg-mem analogue; custom behavior in upstream is exposed through functions/types/extensions and interception in [`readme.md`](https://github.com/oguimbal/pg-mem/blob/0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6/readme.md) | Replacing the translator can bypass the default PostgreSQL parse/rewrite guarantees; when custom functions are registered, translated statements must remain PostgreSQL-parseable for function rewriting; custom translators must be concurrency-safe |

## Exact v0.1 Go API

- Database lifecycle and discovery: `New`, `NewContext`, `MustNew`,
  `DB.Public`, `DB.CreateSchema`, `DB.Schema`, `DB.SchemaNames`, `DB.SQLDB`, and
  `DB.Close`.
- `database/sql` integration: `DB.Connector`, `DB.Open`, `Schema.Connector`, and
  `Schema.Open`.
- Backup and restore: `DB.Backup`/`BackupContext`, `Backup.Restore`/
  `RestoreContext`, and `ErrSchemaChanged`.
- SQL execution: `Schema.Name`, `None`/`NoneContext`, `Exec`/`ExecContext`,
  `Query`/`QueryContext`, `Many`/`ManyContext`, and `One`/`OneContext`.
- Interception: `Schema.InterceptQueries`, `QueryInterceptor`, `Subscription`,
  and `Subscription.Unsubscribe`.
- Scalar functions: `DB.RegisterFunction`, `Schema.RegisterFunction`,
  `Function`, and `ScalarFunction`.
- Raw fixtures: `Schema.Table`, `Schema.GetTable`, `Schema.TableContext`,
  `Table.Name`, `Table.Schema`, `Insert`/`InsertContext`, and
  `Find`/`FindContext`.
- Configuration and extension boundary: `Option`, `WithTranslator`, and the
  `Translator` interface.
- Values and diagnostics: `Row`, `Result`, `Error` (including `SQLState` and
  `Unwrap`), `ErrClosed`, `ErrNoRows`, `ErrTooManyRows`, `ErrInvalidSchema`,
  `ErrRelationNotFound`, `ErrSchemaChanged`, and `ErrUnsupported`.

`DB.SQLDB` is deliberately an unsafe internal-dialect escape hatch. Calls
through it do not pass through PostgreSQL parsing, AST rewriting, schema
qualification, or the compatibility guarantees in this document; changing
attachments or connection PRAGMAs can also violate emulator invariants.

## Execution and build boundary

- Parsing is PostgreSQL-shaped; execution is not PostgreSQL. `pg_query_go/v6`
  embeds the PostgreSQL parser and requires cgo plus a C compiler. Accepting a
  statement proves only that it is valid to that pinned parser, not that the
  translator or executor implements it.
- The executor is `modernc.org/sqlite`. SQLite owns storage, coercion,
  collation, identifier comparison, constraint timing, query planning, and any
  behavior not explicitly rewritten and tested by `pgmem`. PostgreSQL syntax
  can therefore fail at execution or produce a documented v0.1 divergence.
- The internal pool is deliberately limited to one connection so attached
  named schemas share one database state. Concurrent callers are safe to use
  through `database/sql`, but execution is serialized; this does not emulate
  PostgreSQL sessions, locks, isolation, deadlocks, or concurrent transactions.
- `DB.Open` and `Schema.Open` return a separate `*sql.DB` pool that does not own
  the emulator. Close that pool before `DB.Close`. The connector implements
  context-aware prepare, exec, query, begin, ping, validation, and session
  reset hooks; an active transaction pins the sole internal connection until
  commit or rollback.
- Materialized `Row` values contain database-driver representations such as
  `int64`, `float64`, `string`, `[]byte`, and nil. They are not a replica of
  PostgreSQL's wire encodings or of a particular PostgreSQL Go driver's scan
  choices.

## Explicitly unsupported in v0.1

The following pg-mem v3.0.14 capabilities are outside the v0.1 contract unless
a later row above says otherwise:

- equivalent type or extension registration, plus aggregate, window, and
  table-valued custom functions;
- query/schema/scan event subscriptions and index-planner diagnostics (query
  interception is supported separately above);
- Node ecosystem adapters (`pg`, pg-promise, Slonik, TypeORM, Knex, Kysely,
  MikroORM, Sequelize, and postgres.js);
- PostgreSQL system catalogs and `information_schema` compatibility;
- PostgreSQL wire protocol, users, roles, grants, authentication, server
  settings, extensions, PL/pgSQL, and production persistence;
- views/materialized views, sequences as standalone objects, enums, domains,
  arrays, ranges, geometric/network types, full JSONB semantics, and
  PostgreSQL date/time fidelity;
- planner or index fidelity, query plans, concurrency/locking fidelity,
  isolation levels, advisory locks, notifications, replication, and vacuum;
- bug-for-bug matching with pg-mem or PostgreSQL.

Some syntax in these categories may happen to execute in the embedded backend.
That is incidental and may change without compatibility notice until a
black-box test and matrix entry make it contractual.

## Fixture provenance

[`compatibility_test.go`](./compatibility_test.go) contains reduced adaptations
of the pinned upstream specs linked in the table. The fixtures preserve the
observable SQL behavior while replacing pg-mem's TypeScript helpers with the
public Go API and Ginkgo/Gomega tables. No upstream implementation source is
copied. See [`UPSTREAM.md`](./UPSTREAM.md) and
[`THIRD_PARTY_NOTICES`](./THIRD_PARTY_NOTICES) for the attribution and license
boundary.

## What to run against PostgreSQL

Use `pgmem` for fast unit tests, not as the final compatibility oracle. Run the
real application migrations and integration suite against every supported
PostgreSQL version before release, especially when relying on types, casts,
collations, timezones, JSONB, catalogs, extensions, constraints added by later
migrations, concurrent transactions, or exact error diagnostics.
