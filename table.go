package pgmem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Table exposes a small raw-row API for test setup and inspection. SQL remains
// the primary interface; Table is useful when a fixture is clearer as Go data.
type Table struct {
	schema *Schema
	name   string
}

// Table returns a checked handle to a table or view in the schema.
func (s *Schema) Table(name string) (*Table, error) {
	return s.TableContext(context.Background(), name)
}

// GetTable is the pg-mem-compatible spelling of Table.
func (s *Schema) GetTable(name string) (*Table, error) {
	return s.Table(name)
}

// TableContext returns a checked handle to a table or view in the schema.
func (s *Schema) TableContext(ctx context.Context, name string) (*Table, error) {
	if s == nil || s.database == nil || s.database.isClosed() {
		return nil, ErrClosed
	}
	if strings.TrimSpace(name) == "" || strings.IndexByte(name, 0) >= 0 {
		return nil, fmt.Errorf("%w: %q", ErrRelationNotFound, name)
	}

	var exists int
	query := fmt.Sprintf(
		`SELECT 1 FROM %s.sqlite_schema WHERE type IN ('table', 'view') AND name = $1`,
		quoteIdentifier(s.internalName()),
	)
	if err := s.database.sqlDB.QueryRowContext(ctx, query, name).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return nil, &Error{
			Code:      "42P01",
			Message:   fmt.Sprintf("relation %q does not exist", name),
			Statement: query,
			Cause:     ErrRelationNotFound,
		}
	} else if err != nil {
		return nil, s.database.executionError(query, err)
	}
	return &Table{schema: s, name: name}, nil
}

// Name returns the unqualified relation name.
func (t *Table) Name() string {
	if t == nil {
		return ""
	}
	return t.name
}

// Schema returns the table's owning schema.
func (t *Table) Schema() *Schema {
	if t == nil {
		return nil
	}
	return t.schema
}

// Insert inserts a raw row and returns the stored row after defaults and
// generated values have been applied.
func (t *Table) Insert(row Row) (Row, error) {
	return t.InsertContext(context.Background(), row)
}

// InsertContext inserts a raw row and returns the stored row after defaults
// and generated values have been applied.
func (t *Table) InsertContext(ctx context.Context, row Row) (Row, error) {
	if t == nil || t.schema == nil {
		return nil, ErrRelationNotFound
	}
	columns := sortedRowKeys(row)
	statement := "INSERT INTO " + t.qualifiedName()
	arguments := make([]any, 0, len(columns))
	if len(columns) == 0 {
		statement += " DEFAULT VALUES"
	} else {
		quotedColumns := make([]string, 0, len(columns))
		placeholders := make([]string, 0, len(columns))
		for index, column := range columns {
			quotedColumns = append(quotedColumns, quoteIdentifier(column))
			placeholders = append(placeholders, fmt.Sprintf("$%d", index+1))
			arguments = append(arguments, row[column])
		}
		statement += " (" + strings.Join(quotedColumns, ", ") + ") VALUES (" +
			strings.Join(placeholders, ", ") + ")"
	}
	statement += " RETURNING *"
	return t.schema.OneContext(ctx, statement, arguments...)
}

// Find returns rows whose columns equal every non-nil template entry. A nil
// template value matches SQL NULL.
func (t *Table) Find(template Row) ([]Row, error) {
	return t.FindContext(context.Background(), template)
}

// FindContext returns rows whose columns equal every template entry.
func (t *Table) FindContext(ctx context.Context, template Row) ([]Row, error) {
	if t == nil || t.schema == nil {
		return nil, ErrRelationNotFound
	}
	columns := sortedRowKeys(template)
	statement := "SELECT * FROM " + t.qualifiedName()
	arguments := make([]any, 0, len(columns))
	if len(columns) > 0 {
		predicates := make([]string, 0, len(columns))
		for _, column := range columns {
			if template[column] == nil {
				predicates = append(predicates, quoteIdentifier(column)+" IS NULL")
				continue
			}
			arguments = append(arguments, template[column])
			predicates = append(predicates, fmt.Sprintf(
				"%s = $%d", quoteIdentifier(column), len(arguments),
			))
		}
		statement += " WHERE " + strings.Join(predicates, " AND ")
	}
	return t.schema.ManyContext(ctx, statement, arguments...)
}

func (s *Schema) internalName() string {
	if s.name == "public" {
		return "main"
	}
	return s.name
}

func (t *Table) qualifiedName() string {
	return quoteIdentifier(t.schema.internalName()) + "." + quoteIdentifier(t.name)
}

func sortedRowKeys(row Row) []string {
	keys := make([]string, 0, len(row))
	for key := range row {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
