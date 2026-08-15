package pgmem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	databaseSequence  atomic.Uint64
	schemaNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)
)

// DB is one isolated, in-memory database. A DB owns its SQL connection and
// must be closed when a test or process is finished with it.
type DB struct {
	mu         sync.RWMutex
	topologyMu sync.Mutex
	sqlDB      *sql.DB
	translator Translator
	schemas    map[string]*Schema
	dispatchID string
	functions  functionRegistry
	closed     bool
}

// New creates an isolated in-memory database.
func New(options ...Option) (*DB, error) {
	return NewContext(context.Background(), options...)
}

// NewContext creates an isolated in-memory database and validates its
// connection before returning.
func NewContext(ctx context.Context, options ...Option) (*DB, error) {
	cfg := config{translator: defaultTranslator{}}
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("pgmem: option must not be nil")
		}
		if err := option(&cfg); err != nil {
			return nil, err
		}
	}
	dispatchID, err := newDispatchID()
	if err != nil {
		return nil, fmt.Errorf("pgmem: %w", err)
	}

	id := databaseSequence.Add(1)
	dsn := "file:pgmem-" + strconv.FormatUint(id, 10) + "?mode=memory&cache=shared"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("pgmem: open internal database: %w", err)
	}
	// A single connection makes attached schemas and future snapshots apply to
	// every operation while retaining database/sql transaction semantics.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	if _, err := sqlDB.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("pgmem: enable foreign keys: %w", err)
	}

	database := &DB{
		sqlDB:      sqlDB,
		translator: cfg.translator,
		schemas:    make(map[string]*Schema),
		dispatchID: dispatchID,
		functions: functionRegistry{
			bySchema: make(map[string]map[string]*functionOverloads),
		},
	}
	database.schemas["public"] = &Schema{database: database, name: "public"}
	functionDatabases.Store(database.dispatchID, database)
	return database, nil
}

// MustNew creates a database or panics. It is intended for test setup.
func MustNew(options ...Option) *DB {
	database, err := New(options...)
	if err != nil {
		panic(err)
	}
	return database
}

// Public returns the default public schema.
func (db *DB) Public() *Schema {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.schemas["public"]
}

// CreateSchema creates and returns an isolated named schema.
func (db *DB) CreateSchema(ctx context.Context, name string) (*Schema, error) {
	requestedName := name
	if !schemaNamePattern.MatchString(name) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSchema, name)
	}
	// The API accepts PostgreSQL's ordinary (unquoted) identifier form, which
	// folds to lowercase. Quoted schema names are deliberately outside v0.1.
	name = strings.ToLower(name)
	if name == "main" || name == "temp" || name == "public" {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSchema, requestedName)
	}

	// Serialize topology changes without holding the state mutex while waiting
	// for the sole engine connection. Adapter transactions can legitimately pin
	// that connection and still need the state read lock for execution.
	db.topologyMu.Lock()
	defer db.topologyMu.Unlock()

	db.mu.RLock()
	if db.closed {
		db.mu.RUnlock()
		return nil, ErrClosed
	}
	if _, exists := db.schemas[name]; exists {
		db.mu.RUnlock()
		return nil, &Error{Code: "42P06", Message: fmt.Sprintf("schema %q already exists", name)}
	}
	db.mu.RUnlock()
	if _, err := db.sqlDB.ExecContext(ctx, `ATTACH DATABASE ':memory:' AS `+quoteIdentifier(name)); err != nil {
		return nil, db.executionError("", err)
	}
	schema := &Schema{database: db, name: name}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, ErrClosed
	}
	db.schemas[name] = schema
	return schema, nil
}

// Schema returns a named schema and whether it exists.
func (db *DB) Schema(name string) (*Schema, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	schema, exists := db.schemas[strings.ToLower(name)]
	return schema, exists
}

// SchemaNames returns a stable snapshot of known schema names.
func (db *DB) SchemaNames() []string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	names := make([]string, 0, len(db.schemas))
	for name := range db.schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SQLDB exposes the underlying database/sql pool. Direct calls currently use
// the internal dialect; prefer Schema methods when PostgreSQL translation is
// required.
func (db *DB) SQLDB() *sql.DB {
	return db.sqlDB
}

// Close releases all memory owned by the database. It is idempotent.
func (db *DB) Close() error {
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return nil
	}
	db.closed = true
	functionDatabases.Delete(db.dispatchID)
	db.mu.Unlock()
	return db.sqlDB.Close()
}

func (db *DB) isClosed() bool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.closed
}

func (db *DB) executionError(statement string, cause error) error {
	return &Error{
		Code:      sqlState(cause),
		Message:   cause.Error(),
		Statement: statement,
		Cause:     cause,
	}
}

func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func sqlState(cause error) string {
	var sqliteError *sqlite.Error
	if errors.As(cause, &sqliteError) {
		switch sqliteError.Code() {
		case sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, sqlite3.SQLITE_CONSTRAINT_UNIQUE:
			return "23505"
		case sqlite3.SQLITE_CONSTRAINT_NOTNULL:
			return "23502"
		case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
			return "23503"
		case sqlite3.SQLITE_CONSTRAINT_CHECK:
			return "23514"
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return "40001"
		}
	}

	message := strings.ToLower(cause.Error())
	switch {
	case strings.Contains(message, "pgmem function"):
		return "38000"
	case strings.Contains(message, "no such table"):
		return "42P01"
	case strings.Contains(message, "no such column"):
		return "42703"
	case strings.Contains(message, "syntax error"):
		return "42601"
	default:
		return "XX000"
	}
}
