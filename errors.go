package pgmem

import (
	"errors"
	"fmt"
)

var (
	// ErrClosed is returned after a database has been closed.
	ErrClosed = errors.New("pgmem: database is closed")
	// ErrNoRows is returned when One expects a row and receives none.
	ErrNoRows = errors.New("pgmem: expected one row, got none")
	// ErrTooManyRows is returned when One receives more than one row.
	ErrTooManyRows = errors.New("pgmem: expected one row, got many")
	// ErrInvalidSchema is returned for an invalid or unknown schema name.
	ErrInvalidSchema = errors.New("pgmem: invalid schema")
	// ErrUnsupported is returned when valid PostgreSQL syntax is outside the
	// emulator's documented compatibility surface.
	ErrUnsupported = errors.New("pgmem: unsupported PostgreSQL feature")
	// ErrRelationNotFound is returned when a requested table or view does not
	// exist in a schema.
	ErrRelationNotFound = errors.New("pgmem: relation does not exist")
	// ErrSchemaChanged is returned when restoring a backup after the database's
	// schema definitions or named-schema topology changed.
	ErrSchemaChanged = errors.New("pgmem: schema changed since backup")
)

// Error adds a PostgreSQL-style SQLSTATE code and statement context to an
// execution error. Cause remains available through errors.Is and errors.As.
type Error struct {
	Code      string
	Message   string
	Statement string
	Cause     error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Code == "" {
		return e.Message
	}
	return fmt.Sprintf("%s (SQLSTATE %s)", e.Message, e.Code)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// SQLState returns the five-character PostgreSQL error code.
func (e *Error) SQLState() string {
	if e == nil {
		return ""
	}
	return e.Code
}
