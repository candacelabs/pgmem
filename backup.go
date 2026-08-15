package pgmem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	sqlite "modernc.org/sqlite"
)

var restoreSequence atomic.Uint64

type connectionSerializer interface {
	Serialize() ([]byte, error)
}

type connectionDeserializer interface {
	Deserialize([]byte) error
}

type connectionRestorer interface {
	NewRestore(sourceURI string) (*sqlite.Backup, error)
}

// Backup is an immutable restore point owned by one DB. A backup can be
// restored repeatedly, including concurrently with ordinary operations.
//
// Restore accepts data changes made after the backup, but rejects schema DDL
// and named-schema topology changes. That restriction matches pg-mem's backup
// contract and keeps existing Schema handles valid.
type Backup struct {
	database       *DB
	schemaNames    []string
	schemaVersions map[string]int64
	images         map[string][]byte
}

// Backup creates a restore point for the public schema and every named schema.
func (db *DB) Backup() (*Backup, error) {
	return db.BackupContext(context.Background())
}

// BackupContext creates a restore point for the public schema and every named
// schema. The returned backup owns defensive copies of all database images.
func (db *DB) BackupContext(ctx context.Context) (*Backup, error) {
	if db == nil {
		return nil, ErrClosed
	}

	db.topologyMu.Lock()
	defer db.topologyMu.Unlock()

	// Holding the state read lock prevents Close and CreateSchema while the
	// pinned sole SQL connection gives us a consistent cross-schema snapshot.
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, ErrClosed
	}

	connection, err := db.sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("pgmem: acquire connection for backup: %w", err)
	}
	defer connection.Close()

	names := db.schemaNamesLocked()
	versions, err := readSchemaVersions(ctx, connection, names)
	if err != nil {
		return nil, fmt.Errorf("pgmem: read schema versions for backup: %w", err)
	}

	images := make(map[string][]byte, len(names))
	mainImage, err := serializeConnection(connection)
	if err != nil {
		return nil, fmt.Errorf("pgmem: back up public schema: %w", err)
	}
	images["public"] = cloneBytes(mainImage)

	for _, name := range names {
		if name == "public" {
			continue
		}
		image, err := vacuumSchemaImage(ctx, connection, name)
		if err != nil {
			return nil, fmt.Errorf("pgmem: back up schema %q: %w", name, err)
		}
		images[name] = cloneBytes(image)
	}

	return &Backup{
		database:       db,
		schemaNames:    append([]string(nil), names...),
		schemaVersions: versions,
		images:         images,
	}, nil
}

// Restore resets all data to this restore point.
func (b *Backup) Restore() error {
	return b.RestoreContext(context.Background())
}

// RestoreContext resets all data to this restore point. It is repeatable. If
// any schema definition or the named-schema topology changed since Backup,
// RestoreContext returns ErrSchemaChanged without changing data.
func (b *Backup) RestoreContext(ctx context.Context) error {
	if b == nil || b.database == nil {
		return errors.New("pgmem: backup is nil")
	}
	db := b.database

	db.topologyMu.Lock()
	defer db.topologyMu.Unlock()

	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return ErrClosed
	}

	currentNames := db.schemaNamesLocked()
	if !equalStrings(currentNames, b.schemaNames) {
		return fmt.Errorf("%w: named schemas are now %v, backup has %v", ErrSchemaChanged, currentNames, b.schemaNames)
	}

	connection, err := db.sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pgmem: acquire connection for restore: %w", err)
	}
	defer connection.Close()

	currentVersions, err := readSchemaVersions(ctx, connection, currentNames)
	if err != nil {
		return fmt.Errorf("pgmem: read schema versions before restore: %w", err)
	}
	for _, name := range currentNames {
		if currentVersions[name] != b.schemaVersions[name] {
			return fmt.Errorf(
				"%w: schema %q version is %d, backup version is %d",
				ErrSchemaChanged,
				name,
				currentVersions[name],
				b.schemaVersions[name],
			)
		}
	}

	// Materialize replacement named databases before changing the live
	// connection. This validates every saved image while the live data is still
	// untouched. The holders keep shared in-memory databases alive until they
	// have been attached to the live connection.
	holders := make([]*restoredSchemaHolder, 0, len(currentNames)-1)
	defer func() {
		for _, holder := range holders {
			holder.close()
		}
	}()
	publicValidation, err := openRestoredSchema(ctx, "public", b.images["public"])
	if err != nil {
		return fmt.Errorf("pgmem: validate public schema for restore: %w", err)
	}
	publicValidation.close()
	for _, name := range currentNames {
		if name == "public" {
			continue
		}
		holder, err := openRestoredSchema(ctx, name, b.images[name])
		if err != nil {
			return fmt.Errorf("pgmem: prepare schema %q for restore: %w", name, err)
		}
		holders = append(holders, holder)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Every fallible input/shape check is complete. Finish the database-level
	// commit even if the caller cancels now, just as a storage commit may
	// succeed after its initiating context is canceled. This prevents context
	// cancellation between public and named-schema swaps from exposing a
	// partially restored snapshot.
	commitCtx := context.WithoutCancel(ctx)
	if err := deserializeConnection(connection, b.images["public"]); err != nil {
		return fmt.Errorf("pgmem: restore public schema: %w", err)
	}
	for _, holder := range holders {
		if _, err := connection.ExecContext(commitCtx, `DETACH DATABASE `+quoteIdentifier(holder.name)); err != nil {
			return fmt.Errorf("pgmem: detach schema %q for restore: %w", holder.name, err)
		}
		if _, err := connection.ExecContext(
			commitCtx,
			`ATTACH DATABASE ? AS `+quoteIdentifier(holder.name),
			holder.uri,
		); err != nil {
			return fmt.Errorf("pgmem: attach restored schema %q: %w", holder.name, err)
		}
	}
	return nil
}

func (db *DB) schemaNamesLocked() []string {
	names := make([]string, 0, len(db.schemas))
	for name := range db.schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func readSchemaVersions(ctx context.Context, connection *sql.Conn, names []string) (map[string]int64, error) {
	versions := make(map[string]int64, len(names))
	for _, name := range names {
		var version int64
		if err := connection.QueryRowContext(
			ctx,
			`PRAGMA `+quoteIdentifier(engineSchemaName(name))+`.schema_version`,
		).Scan(&version); err != nil {
			return nil, fmt.Errorf("read schema %q version: %w", name, err)
		}
		versions[name] = version
	}
	return versions, nil
}

func engineSchemaName(name string) string {
	if name == "public" {
		return "main"
	}
	return name
}

func serializeConnection(connection *sql.Conn) ([]byte, error) {
	var image []byte
	err := connection.Raw(func(driverConnection any) error {
		serializer, ok := driverConnection.(connectionSerializer)
		if !ok {
			return errors.New("embedded SQLite connection does not support serialization")
		}
		serialized, err := serializer.Serialize()
		if err != nil {
			return err
		}
		image = cloneBytes(serialized)
		return nil
	})
	return image, err
}

func deserializeConnection(connection *sql.Conn, image []byte) error {
	return connection.Raw(func(driverConnection any) error {
		deserializer, ok := driverConnection.(connectionDeserializer)
		if !ok {
			return errors.New("embedded SQLite connection does not support deserialization")
		}
		return deserializer.Deserialize(cloneBytes(image))
	})
}

func vacuumSchemaImage(ctx context.Context, connection *sql.Conn, name string) ([]byte, error) {
	temporary, err := os.CreateTemp("", "pgmem-backup-*.sqlite")
	if err != nil {
		return nil, err
	}
	path := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	if err := os.Remove(path); err != nil {
		return nil, err
	}
	defer os.Remove(path)

	statement := `VACUUM ` + quoteIdentifier(name) + ` INTO ` + quoteStringLiteral(path)
	if _, err := connection.ExecContext(ctx, statement); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

type restoredSchemaHolder struct {
	name       string
	uri        string
	database   *sql.DB
	connection *sql.Conn
}

func openRestoredSchema(ctx context.Context, name string, image []byte) (*restoredSchemaHolder, error) {
	temporary, err := os.CreateTemp("", "pgmem-restore-source-*.sqlite")
	if err != nil {
		return nil, err
	}
	sourcePath := temporary.Name()
	defer os.Remove(sourcePath)
	if _, err := temporary.Write(cloneBytes(image)); err != nil {
		_ = temporary.Close()
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}

	id := restoreSequence.Add(1)
	uri := "file:pgmem-restore-" + strconv.FormatUint(id, 10) + "?mode=memory&cache=shared"
	database, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	connection, err := database.Conn(ctx)
	if err != nil {
		_ = database.Close()
		return nil, err
	}
	holder := &restoredSchemaHolder{
		name:       name,
		uri:        uri,
		database:   database,
		connection: connection,
	}
	if err := restoreConnectionFromURI(connection, sourcePath); err != nil {
		holder.close()
		return nil, err
	}
	return holder, nil
}

func restoreConnectionFromURI(connection *sql.Conn, sourceURI string) error {
	return connection.Raw(func(driverConnection any) error {
		restorer, ok := driverConnection.(connectionRestorer)
		if !ok {
			return errors.New("embedded SQLite connection does not support online restore")
		}
		restore, err := restorer.NewRestore(sourceURI)
		if err != nil {
			return err
		}
		_, stepErr := restore.Step(-1)
		finishErr := restore.Finish()
		if stepErr != nil {
			return stepErr
		}
		return finishErr
	})
}

func (h *restoredSchemaHolder) close() {
	if h == nil {
		return
	}
	if h.connection != nil {
		_ = h.connection.Close()
	}
	if h.database != nil {
		_ = h.database.Close()
	}
}

func cloneBytes(value []byte) []byte {
	return append([]byte(nil), value...)
}

func quoteStringLiteral(value string) string {
	return `'` + strings.ReplaceAll(value, `'`, `''`) + `'`
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
