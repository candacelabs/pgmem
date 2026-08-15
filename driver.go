package pgmem

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
)

// Connector returns a database/sql connector for the public schema.
//
// The returned connector does not own DB. Close the database/sql pool before
// closing DB.
func (db *DB) Connector() driver.Connector {
	return db.Public().Connector()
}

// Open returns a database/sql pool backed by the public schema. The returned
// pool and DB have independent lifetimes and both must be closed.
func (db *DB) Open() *sql.DB {
	return db.Public().Open()
}

// Connector returns a database/sql connector whose unqualified statements use
// this schema. The returned connector does not own the schema's DB.
func (s *Schema) Connector() driver.Connector {
	return &sqlConnector{database: s.database, schema: s}
}

// Open returns a database/sql pool whose unqualified statements use this
// schema. Close the pool before closing its DB.
func (s *Schema) Open() *sql.DB {
	pool := sql.OpenDB(s.Connector())
	// The embedded engine intentionally owns one physical connection. Keeping
	// the adapter pool to one logical connection avoids building up connector
	// calls that can only wait for that same engine connection.
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	return pool
}

type sqlConnector struct {
	database *DB
	schema   *Schema
}

func (c *sqlConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.database == nil || c.schema == nil || c.database.isClosed() {
		return nil, ErrClosed
	}
	return &sqlConnection{database: c.database, schema: c.schema}, nil
}

func (c *sqlConnector) Driver() driver.Driver {
	return &sqlDriver{connector: c}
}

type sqlDriver struct {
	connector *sqlConnector
}

func (d *sqlDriver) Open(string) (driver.Conn, error) {
	return d.connector.Connect(context.Background())
}

type sqlConnection struct {
	mu          sync.Mutex
	database    *DB
	schema      *Schema
	transaction *sqlTransaction
	closed      bool
}

func (c *sqlConnection) Prepare(statement string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), statement)
}

func (c *sqlConnection) PrepareContext(ctx context.Context, statement string) (driver.Stmt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	translated, err := c.translateLocked(ctx, statement)
	if err != nil {
		return nil, err
	}

	var prepared *sql.Stmt
	if c.transaction != nil {
		prepared, err = c.transaction.transaction.PrepareContext(ctx, translated)
	} else {
		prepared, err = c.database.sqlDB.PrepareContext(ctx, translated)
	}
	if err != nil {
		return nil, c.database.executionError(statement, err)
	}
	return &sqlStatement{
		database:  c.database,
		statement: statement,
		prepared:  prepared,
	}, nil
}

func (c *sqlConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.transaction == nil {
		return nil
	}

	transaction := c.transaction
	c.transaction = nil
	transaction.finished = true
	rollbackErr := transaction.transaction.Rollback()
	closeErr := transaction.connection.Close()
	if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
		return c.database.executionError("", rollbackErr)
	}
	if closeErr != nil {
		return c.database.executionError("", closeErr)
	}
	return nil
}

func (c *sqlConnection) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *sqlConnection) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.validLocked(ctx); err != nil {
		return nil, err
	}
	if c.transaction != nil {
		return nil, errors.New("pgmem: transaction already active")
	}

	connection, err := c.database.sqlDB.Conn(ctx)
	if err != nil {
		return nil, c.database.executionError("", err)
	}
	transaction, err := connection.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.IsolationLevel(options.Isolation),
		ReadOnly:  options.ReadOnly,
	})
	if err != nil {
		_ = connection.Close()
		return nil, c.database.executionError("", err)
	}

	active := &sqlTransaction{
		owner:       c,
		connection:  connection,
		transaction: transaction,
	}
	c.transaction = active
	return active, nil
}

func (c *sqlConnection) ExecContext(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	translated, err := c.translateLocked(ctx, statement)
	if err != nil {
		return nil, err
	}
	values := namedValueArguments(arguments)

	var result sql.Result
	if c.transaction != nil {
		result, err = c.transaction.transaction.ExecContext(ctx, translated, values...)
	} else {
		result, err = c.database.sqlDB.ExecContext(ctx, translated, values...)
	}
	if err != nil {
		return nil, c.database.executionError(statement, err)
	}
	return result, nil
}

func (c *sqlConnection) QueryContext(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	translated, err := c.translateLocked(ctx, statement)
	if err != nil {
		return nil, err
	}
	values := namedValueArguments(arguments)

	var result *sql.Rows
	if c.transaction != nil {
		result, err = c.transaction.transaction.QueryContext(ctx, translated, values...)
	} else {
		result, err = c.database.sqlDB.QueryContext(ctx, translated, values...)
	}
	if err != nil {
		return nil, c.database.executionError(statement, err)
	}
	return newSQLRows(c.database, statement, result)
}

func (c *sqlConnection) Ping(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.validLocked(ctx); err != nil {
		return err
	}

	var err error
	if c.transaction != nil {
		err = c.transaction.connection.PingContext(ctx)
	} else {
		err = c.database.sqlDB.PingContext(ctx)
	}
	if err != nil {
		return c.database.executionError("", err)
	}
	return nil
}

func (c *sqlConnection) ResetSession(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.closed || c.database == nil || c.schema == nil || c.database.isClosed() {
		return driver.ErrBadConn
	}
	if c.transaction != nil {
		return driver.ErrBadConn
	}
	return nil
}

func (c *sqlConnection) IsValid() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && c.transaction == nil && c.database != nil && !c.database.isClosed()
}

func (c *sqlConnection) translateLocked(ctx context.Context, statement string) (string, error) {
	if err := c.validLocked(ctx); err != nil {
		return "", err
	}
	return c.schema.translate(ctx, statement)
}

func (c *sqlConnection) validLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.closed || c.database == nil || c.schema == nil || c.database.isClosed() {
		return ErrClosed
	}
	return nil
}

type sqlTransaction struct {
	owner       *sqlConnection
	connection  *sql.Conn
	transaction *sql.Tx
	finished    bool
}

func (t *sqlTransaction) Commit() error {
	return t.finish(true)
}

func (t *sqlTransaction) Rollback() error {
	return t.finish(false)
}

func (t *sqlTransaction) finish(commit bool) error {
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	if t.finished || t.owner.transaction != t {
		return sql.ErrTxDone
	}
	t.finished = true
	t.owner.transaction = nil

	var operationErr error
	if commit {
		operationErr = t.transaction.Commit()
	} else {
		operationErr = t.transaction.Rollback()
	}
	closeErr := t.connection.Close()
	if operationErr != nil {
		return t.owner.database.executionError("", operationErr)
	}
	if closeErr != nil {
		return t.owner.database.executionError("", closeErr)
	}
	return nil
}

type sqlStatement struct {
	database  *DB
	statement string
	prepared  *sql.Stmt
}

func (s *sqlStatement) Close() error {
	return s.prepared.Close()
}

func (s *sqlStatement) NumInput() int {
	return -1
}

func (s *sqlStatement) Exec(arguments []driver.Value) (driver.Result, error) {
	values := make([]any, len(arguments))
	for index := range arguments {
		values[index] = arguments[index]
	}
	return s.execContext(context.Background(), values)
}

func (s *sqlStatement) ExecContext(ctx context.Context, arguments []driver.NamedValue) (driver.Result, error) {
	return s.execContext(ctx, namedValueArguments(arguments))
}

func (s *sqlStatement) execContext(ctx context.Context, arguments []any) (driver.Result, error) {
	result, err := s.prepared.ExecContext(ctx, arguments...)
	if err != nil {
		return nil, s.database.executionError(s.statement, err)
	}
	return result, nil
}

func (s *sqlStatement) Query(arguments []driver.Value) (driver.Rows, error) {
	values := make([]any, len(arguments))
	for index := range arguments {
		values[index] = arguments[index]
	}
	return s.queryContext(context.Background(), values)
}

func (s *sqlStatement) QueryContext(ctx context.Context, arguments []driver.NamedValue) (driver.Rows, error) {
	return s.queryContext(ctx, namedValueArguments(arguments))
}

func (s *sqlStatement) queryContext(ctx context.Context, arguments []any) (driver.Rows, error) {
	rows, err := s.prepared.QueryContext(ctx, arguments...)
	if err != nil {
		return nil, s.database.executionError(s.statement, err)
	}
	return newSQLRows(s.database, s.statement, rows)
}

type sqlRows struct {
	database  *DB
	statement string
	rows      *sql.Rows
	columns   []string
}

func newSQLRows(database *DB, statement string, rows *sql.Rows) (*sqlRows, error) {
	columns, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		return nil, database.executionError(statement, err)
	}
	return &sqlRows{
		database:  database,
		statement: statement,
		rows:      rows,
		columns:   columns,
	}, nil
}

func (r *sqlRows) Columns() []string {
	return r.columns
}

func (r *sqlRows) Close() error {
	if err := r.rows.Close(); err != nil {
		return r.database.executionError(r.statement, err)
	}
	return nil
}

func (r *sqlRows) Next(destination []driver.Value) error {
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return r.database.executionError(r.statement, err)
		}
		return io.EOF
	}
	values := make([]any, len(destination))
	for index := range destination {
		values[index] = &destination[index]
	}
	if err := r.rows.Scan(values...); err != nil {
		return r.database.executionError(r.statement, err)
	}
	for index := range destination {
		destination[index] = cloneDriverValue(destination[index])
	}
	return nil
}

func namedValueArguments(arguments []driver.NamedValue) []any {
	values := make([]any, len(arguments))
	for index, argument := range arguments {
		if argument.Name != "" {
			values[index] = sql.Named(argument.Name, argument.Value)
			continue
		}
		values[index] = argument.Value
	}
	return values
}

var (
	_ driver.Connector          = (*sqlConnector)(nil)
	_ driver.Driver             = (*sqlDriver)(nil)
	_ driver.Conn               = (*sqlConnection)(nil)
	_ driver.ConnBeginTx        = (*sqlConnection)(nil)
	_ driver.ConnPrepareContext = (*sqlConnection)(nil)
	_ driver.ExecerContext      = (*sqlConnection)(nil)
	_ driver.QueryerContext     = (*sqlConnection)(nil)
	_ driver.Pinger             = (*sqlConnection)(nil)
	_ driver.SessionResetter    = (*sqlConnection)(nil)
	_ driver.Validator          = (*sqlConnection)(nil)
	_ driver.Tx                 = (*sqlTransaction)(nil)
	_ driver.Stmt               = (*sqlStatement)(nil)
	_ driver.StmtExecContext    = (*sqlStatement)(nil)
	_ driver.StmtQueryContext   = (*sqlStatement)(nil)
	_ driver.Rows               = (*sqlRows)(nil)
)
