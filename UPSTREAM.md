# Upstream relationship

This project is a Go port of the public behavior and testing model documented
by [`oguimbal/pg-mem`](https://github.com/oguimbal/pg-mem), pinned for the
initial compatibility effort at v3.0.14, commit
`0fcbf5c2b4826116d67f2fe5e30e3c7a3bbf4eb6`.
It is not an official pg-mem project and does not claim byte-for-byte or
bug-for-bug compatibility.

Upstream pg-mem is copyright Olivier Guimbal and contributors and is available
under the MIT License. The implementation in this repository is written in Go
against the documented API and PostgreSQL behavior. Compatibility fixtures
derived from upstream examples or tests retain their attribution when added.

The AST approach follows the operator's earlier
[`go-pg-sqlc-crud`](https://github.com/kaashmonee/go-pg-sqlc-crud) project:
parse PostgreSQL into a structural tree with `pg_query_go` and consume that
tree explicitly. `pg_query_go` and the embedded PostgreSQL parser retain their
own BSD-3-Clause and PostgreSQL licenses; see [THIRD_PARTY_NOTICES](./THIRD_PARTY_NOTICES).
