// Package storage provides the Gateway's persistence primitives: a SQLite
// database with versioned migrations, bounded private and artifact object
// directories, an append-only event/audit log and idempotency records.
//
// The database uses a single connection (SQLite serialises writers anyway), so
// callers must never issue a query on the *DB while a transaction obtained
// from InTx is open on the same goroutine.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"
)

// Querier is implemented by *sql.DB and *sql.Tx.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB is a SQLite database.
type DB struct {
	sql *sql.DB
}

// Migration is one forward-only schema step of a component. Statements run in
// one transaction, in order; each is executed on its own so triggers (which
// contain semicolons) are safe.
type Migration struct {
	Version    int
	Name       string
	Statements []string
}

// OpenDB opens (creating it with mode 0600 when absent) the SQLite database
// at path with WAL, foreign keys and a busy timeout.
func OpenDB(path string) (*DB, error) {
	if path == "" {
		return nil, errors.New("storage: empty database path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("storage: create database directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("storage: create database file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("storage: close database file: %w", err)
	}
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	u := url.URL{Path: filepath.ToSlash(path)}
	dsn := "file:" + u.EscapedPath() + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: ping database: %w", err)
	}
	return &DB{sql: db}, nil
}

// SQL returns the underlying handle for reads and single statements.
func (d *DB) SQL() *sql.DB { return d.sql }

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// InTx runs fn in a transaction, committing when fn returns nil and rolling
// back on error or panic.
func (d *DB) InTx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit: %w", err)
	}
	return nil
}

// Migrate applies the not yet applied migrations of component. It refuses a
// database that records a newer version than the supplied migrations, so an
// older binary never runs against a newer schema (rollback must restore a
// compatible database snapshot).
func (d *DB) Migrate(ctx context.Context, component string, ms []Migration) error {
	if _, err := d.sql.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		component TEXT NOT NULL,
		version INTEGER NOT NULL,
		name TEXT NOT NULL,
		applied_at INTEGER NOT NULL,
		PRIMARY KEY (component, version))`); err != nil {
		return fmt.Errorf("storage: create schema_migrations: %w", err)
	}
	var current sql.NullInt64
	if err := d.sql.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations WHERE component = ?`, component).Scan(&current); err != nil {
		return fmt.Errorf("storage: read schema version: %w", err)
	}
	latest := 0
	for _, m := range ms {
		if m.Version > latest {
			latest = m.Version
		}
	}
	if int(current.Int64) > latest {
		return fmt.Errorf("storage: database schema for %q is version %d, newer than this binary (%d)", component, current.Int64, latest)
	}
	for _, m := range ms {
		if int64(m.Version) <= current.Int64 {
			continue
		}
		err := d.InTx(ctx, func(tx *sql.Tx) error {
			for _, stmt := range m.Statements {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("migration %s/%d %s: %w", component, m.Version, m.Name, err)
				}
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (component, version, name, applied_at) VALUES (?, ?, ?, ?)`,
				component, m.Version, m.Name, time.Now().UnixMilli())
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Checkpoint folds the write-ahead log into the main database file.
func (d *DB) Checkpoint(ctx context.Context) error {
	_, err := d.sql.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// Backup writes a consistent, compacted copy of the database to dest, which
// must not exist. The copy is created with mode 0600.
func (d *DB) Backup(ctx context.Context, dest string) error {
	if dest == "" {
		return errors.New("storage: empty backup path")
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("storage: backup destination %s already exists", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := d.sql.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("storage: backup: %w", err)
	}
	return os.Chmod(dest, 0o600)
}

// Config locates the three Gateway data directories.
type Config struct {
	DBPath      string // e.g. /data/db/lensdock.db
	PrivateDir  string // e.g. /data/private
	ArtifactDir string // e.g. /data/artifacts
}

// Store bundles the database and the object stores.
type Store struct {
	DB        *DB
	Private   *Objects
	Artifacts *Artifacts
	Events    *EventLog
}

// Open opens every store and applies the storage migrations.
func Open(ctx context.Context, c Config) (*Store, error) {
	db, err := OpenDB(c.DBPath)
	if err != nil {
		return nil, err
	}
	if err := Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	priv, err := OpenObjects(c.PrivateDir)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	art, err := OpenArtifacts(c.ArtifactDir)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{DB: db, Private: priv, Artifacts: art, Events: NewEventLog(db)}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.DB.Close() }

var storageMigrations = []Migration{{
	Version: 1,
	Name:    "events and idempotency",
	Statements: []string{
		`CREATE TABLE events (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			id TEXT NOT NULL UNIQUE,
			ts INTEGER NOT NULL,
			workspace_id TEXT NOT NULL,
			actor_id TEXT NOT NULL,
			actor_role TEXT NOT NULL,
			category TEXT NOT NULL,
			action TEXT NOT NULL,
			result TEXT NOT NULL,
			subject_type TEXT NOT NULL,
			subject_id TEXT NOT NULL,
			revision INTEGER NOT NULL DEFAULT 0,
			agent_id TEXT NOT NULL DEFAULT '',
			visibility TEXT NOT NULL,
			correlation_id TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX events_workspace ON events (workspace_id, seq)`,
		`CREATE TRIGGER events_no_update BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT, 'events are append-only'); END`,
		`CREATE TRIGGER events_no_delete BEFORE DELETE ON events BEGIN SELECT RAISE(ABORT, 'events are append-only'); END`,
		`CREATE TABLE idempotency (
			scope TEXT NOT NULL,
			key TEXT NOT NULL,
			fingerprint TEXT NOT NULL,
			response BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (scope, key)
		)`,
	},
}}

// Migrate applies the storage component's schema (events, idempotency). It
// touches only the database, so it is safe on a cloned database without the
// object directories (offline update migration).
func Migrate(ctx context.Context, db *DB) error {
	return db.Migrate(ctx, "storage", storageMigrations)
}

// SchemaVersions returns the highest applied migration version per component
// (empty before any migration ran).
func SchemaVersions(ctx context.Context, db *DB) (map[string]int, error) {
	out := map[string]int{}
	rows, err := db.sql.QueryContext(ctx, `SELECT component, MAX(version) FROM schema_migrations GROUP BY component`)
	if err != nil {
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var (
			c string
			v int
		)
		if err := rows.Scan(&c, &v); err != nil {
			return nil, err
		}
		out[c] = v
	}
	return out, rows.Err()
}
