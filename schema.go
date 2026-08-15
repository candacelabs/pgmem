package pgmem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"
)

// Row is one result row keyed by column label.
type Row map[string]any

// Result contains materialized rows and statement metadata.
type Result struct {
	Rows     []Row
	RowCount int64
	Command  string
}

// Schema is a namespace inside a DB.
type Schema struct {
	database     *DB
	name         string
	interceptors interceptorRegistry
}

// Name returns the schema name.
func (s *Schema) Name() string {
	return s.name
}

// None executes a statement that is not expected to return rows.
func (s *Schema) None(statement string, arguments ...any) error {
	return s.NoneContext(context.Background(), statement, arguments...)
}

// NoneContext executes a statement that is not expected to return rows.
func (s *Schema) NoneContext(ctx context.Context, statement string, arguments ...any) error {
	_, err := s.ExecContext(ctx, statement, arguments...)
	return err
}

// Exec executes a statement and returns its affected-row count.
func (s *Schema) Exec(statement string, arguments ...any) (Result, error) {
	return s.ExecContext(context.Background(), statement, arguments...)
}

// ExecContext executes a statement and returns its affected-row count.
func (s *Schema) ExecContext(ctx context.Context, statement string, arguments ...any) (Result, error) {
	if result, handled, err := s.runInterceptors(ctx, statement, arguments); handled || err != nil {
		return result, err
	}
	statements, err := s.translateStatements(ctx, statement)
	if err != nil {
		return Result{}, err
	}
	if len(statements) == 1 {
		return s.execOne(ctx, s.database.sqlDB, statements[0], arguments...)
	}
	if len(arguments) > 0 {
		return Result{}, &Error{
			Code:      "0A000",
			Message:   "parameters in multi-statement scripts are not supported",
			Statement: statement,
			Cause:     ErrUnsupported,
		}
	}

	transaction, err := s.database.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, s.database.executionError(statement, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = transaction.Rollback()
		}
	}()

	result := Result{}
	for _, executable := range statements {
		result, err = s.execOne(ctx, transaction, executable)
		if err != nil {
			return Result{}, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return Result{}, s.database.executionError(statement, err)
	}
	committed = true
	return result, nil
}

// Query executes and fully materializes a row-returning statement.
func (s *Schema) Query(statement string, arguments ...any) (Result, error) {
	return s.QueryContext(context.Background(), statement, arguments...)
}

// QueryContext executes and fully materializes a row-returning statement.
func (s *Schema) QueryContext(ctx context.Context, statement string, arguments ...any) (Result, error) {
	if result, handled, err := s.runInterceptors(ctx, statement, arguments); handled || err != nil {
		return result, err
	}
	statements, err := s.translateStatements(ctx, statement)
	if err != nil {
		return Result{}, err
	}
	if len(statements) == 1 {
		return s.queryOne(ctx, s.database.sqlDB, statements[0], arguments...)
	}
	if len(arguments) > 0 {
		return Result{}, &Error{
			Code:      "0A000",
			Message:   "parameters in multi-statement scripts are not supported",
			Statement: statement,
			Cause:     ErrUnsupported,
		}
	}

	transaction, err := s.database.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, s.database.executionError(statement, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = transaction.Rollback()
		}
	}()

	for _, executable := range statements[:len(statements)-1] {
		if _, err := s.execOne(ctx, transaction, executable); err != nil {
			return Result{}, err
		}
	}
	result, err := s.queryOne(ctx, transaction, statements[len(statements)-1])
	if err != nil {
		return Result{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Result{}, s.database.executionError(statement, err)
	}
	committed = true
	return result, nil
}

// Many executes a query and returns every row.
func (s *Schema) Many(statement string, arguments ...any) ([]Row, error) {
	return s.ManyContext(context.Background(), statement, arguments...)
}

// ManyContext executes a query and returns every row.
func (s *Schema) ManyContext(ctx context.Context, statement string, arguments ...any) ([]Row, error) {
	result, err := s.QueryContext(ctx, statement, arguments...)
	return result.Rows, err
}

// One executes a query that must return exactly one row.
func (s *Schema) One(statement string, arguments ...any) (Row, error) {
	return s.OneContext(context.Background(), statement, arguments...)
}

// OneContext executes a query that must return exactly one row.
func (s *Schema) OneContext(ctx context.Context, statement string, arguments ...any) (Row, error) {
	rows, err := s.ManyContext(ctx, statement, arguments...)
	if err != nil {
		return nil, err
	}
	switch len(rows) {
	case 0:
		return nil, ErrNoRows
	case 1:
		return rows[0], nil
	default:
		return nil, fmt.Errorf("%w: got %d", ErrTooManyRows, len(rows))
	}
}

func (s *Schema) translate(ctx context.Context, statement string) (string, error) {
	if s == nil || s.database == nil || s.database.isClosed() {
		return "", ErrClosed
	}
	translated, err := s.database.translator.Translate(ctx, s.name, statement)
	if err != nil {
		return "", fmt.Errorf("pgmem: translate statement: %w", err)
	}
	if strings.TrimSpace(translated) == "" {
		return "", errors.New("pgmem: statement must not be empty")
	}
	translated, err = s.database.rewriteRegisteredFunctions(ctx, s.name, statement, translated)
	if err != nil {
		return "", err
	}
	return translated, nil
}

type statementExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type statementQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type executableStatement struct {
	source     string
	translated string
}

func (s *Schema) execOne(ctx context.Context, executor statementExecutor, statement executableStatement, arguments ...any) (Result, error) {
	execution, err := executor.ExecContext(ctx, statement.translated, arguments...)
	if err != nil {
		return Result{}, s.database.executionError(statement.source, err)
	}
	rowCount, err := execution.RowsAffected()
	if err != nil {
		return Result{}, s.database.executionError(statement.source, err)
	}
	return Result{RowCount: rowCount, Command: statementCommand(statement.source)}, nil
}

func (s *Schema) queryOne(ctx context.Context, querier statementQuerier, statement executableStatement, arguments ...any) (Result, error) {
	rows, err := querier.QueryContext(ctx, statement.translated, arguments...)
	if err != nil {
		return Result{}, s.database.executionError(statement.source, err)
	}
	defer rows.Close()

	materialized, err := materializeRows(rows)
	if err != nil {
		return Result{}, s.database.executionError(statement.source, err)
	}
	return Result{
		Rows:     materialized,
		RowCount: int64(len(materialized)),
		Command:  statementCommand(statement.source),
	}, nil
}

func (s *Schema) translateStatements(ctx context.Context, script string) ([]executableStatement, error) {
	if !strings.ContainsRune(script, ';') {
		translated, err := s.translate(ctx, script)
		if err != nil {
			return nil, err
		}
		return []executableStatement{{source: script, translated: translated}}, nil
	}

	parts, err := pgquery.SplitWithParser(script, true)
	if err != nil {
		// The configured Translator owns syntax diagnostics and may intentionally
		// accept a project-specific language, so let it produce the public error.
		translated, translateErr := s.translate(ctx, script)
		if translateErr != nil {
			return nil, translateErr
		}
		return []executableStatement{{source: script, translated: translated}}, nil
	}
	statements := make([]executableStatement, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		translated, err := s.translate(ctx, part)
		if err != nil {
			return nil, err
		}
		statements = append(statements, executableStatement{source: part, translated: translated})
	}
	if len(statements) == 0 {
		return nil, errors.New("pgmem: statement must not be empty")
	}
	return statements, nil
}

func materializeRows(rows *sql.Rows) ([]Row, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := make([]Row, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for index := range values {
			destinations[index] = &values[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, err
		}
		row := make(Row, len(columns))
		for index, column := range columns {
			row[column] = cloneDriverValue(values[index])
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func cloneDriverValue(value any) any {
	bytes, ok := value.([]byte)
	if !ok {
		return value
	}
	cloned := make([]byte, len(bytes))
	copy(cloned, bytes)
	return cloned
}

func statementCommand(statement string) string {
	if parsed, err := pgquery.Parse(statement); err == nil && len(parsed.Stmts) > 0 {
		node := parsed.Stmts[0].Stmt
		switch {
		case node.GetSelectStmt() != nil:
			return "SELECT"
		case node.GetInsertStmt() != nil:
			return "INSERT"
		case node.GetUpdateStmt() != nil:
			return "UPDATE"
		case node.GetDeleteStmt() != nil:
			return "DELETE"
		case node.GetCreateStmt() != nil, node.GetCreateTableAsStmt() != nil:
			return "CREATE"
		case node.GetAlterTableStmt() != nil:
			return "ALTER"
		case node.GetDropStmt() != nil:
			return "DROP"
		case node.GetTruncateStmt() != nil:
			return "TRUNCATE"
		}
	}
	fields := strings.Fields(statement)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToUpper(fields[0])
}
