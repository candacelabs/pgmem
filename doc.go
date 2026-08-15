// Package pgmem provides a fast, process-local PostgreSQL emulator for Go
// tests. It follows pg-mem's database-and-schema model while exposing idiomatic
// context-aware Go APIs and a database/sql adapter.
//
// Pgmem is an emulator, not PostgreSQL. Applications should still run their
// migrations and integration tests against every PostgreSQL version they
// support before release.
package pgmem
