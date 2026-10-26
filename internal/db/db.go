// Package db opens the SQLite database (pure-Go ncruces driver with FTS5), provides
// transaction helpers with SQLITE_BUSY retry, a per-module migration runner and small
// helpers for the column conventions used across Ludworks (UTC times as fixed-width TEXT,
// booleans as INTEGER 0/1, JSON as TEXT).
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

// DB wraps the connection pool.
type DB struct {
	// SQL is the underlying pool. Plain queries can use it directly; multi-statement writes
	// should go through Tx so they are atomic and retried on SQLITE_BUSY.
	SQL  *sql.DB
	path string
}

// Options tunes Open. The zero value is fine.
type Options struct {
	MaxOpenConns int // default min(max(NumCPU,4),8)
}

// Open opens (creating if needed) the database at path. Every pooled connection gets the
// FTS5 extension and the mandatory pragmas.
func Open(ctx context.Context, path string, opts ...Options) (*DB, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	// Pre-create the file owner-only; SQLite would otherwise create it subject to the umask.
	if f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600); err == nil {
		f.Close()
	}
	sqldb, err := driver.Open(DSN(path), func(c *sqlite3.Conn) error { return fts5.Register(c) })
	if err != nil {
		return nil, fmt.Errorf("db: open: %w", err)
	}
	n := o.MaxOpenConns
	if n <= 0 {
		n = min(max(runtime.NumCPU(), 4), 8)
	}
	// WAL allows concurrent readers with a single writer; write transactions start IMMEDIATE
	// (see DSN) so they queue on busy_timeout instead of failing on lock upgrade.
	sqldb.SetMaxOpenConns(n)
	sqldb.SetMaxIdleConns(n)
	sqldb.SetConnMaxIdleTime(10 * time.Minute)
	if err := sqldb.PingContext(ctx); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	var mode string
	if err := sqldb.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("db: journal_mode: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		sqldb.Close()
		return nil, fmt.Errorf("db: journal_mode is %q, want wal", mode)
	}
	d := &DB{SQL: sqldb, path: path}
	if err := d.ensureMigrationsTable(ctx); err != nil {
		sqldb.Close()
		return nil, err
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			_ = os.Chmod(p, 0o600) // WAL/SHM may have been created with umask defaults
		}
	}
	return d, nil
}

// DSN builds the SQLite URI for path. busy_timeout comes first so it applies while the
// remaining pragmas (journal_mode needs a lock) run. Write transactions are IMMEDIATE.
func DSN(path string) string {
	return "file:" + escapeURIPath(path) +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(wal)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(normal)" +
		"&_txlock=immediate"
}

// escapeURIPath turns an OS path into the path part of an SQLite "file:" URI. Backslashes
// become slashes (Windows) and the URI metacharacters are percent-encoded. No leading "//"
// is added: "file:C:/x/y.db" is passed to the VFS as "C:/x/y.db", which Go opens fine.
func escapeURIPath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '%', '?', '#', ' ', '&', '=':
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Path returns the database file path.
func (d *DB) Path() string { return d.path }

// Close closes the pool.
func (d *DB) Close() error { return d.SQL.Close() }

const maxBusyRetries = 4

// Tx runs fn inside an IMMEDIATE write transaction, committing when fn returns nil and
// rolling back otherwise. If the transaction fails with SQLITE_BUSY (after SQLite's own
// busy_timeout) it is retried with backoff, so fn may run more than once and must not have
// side effects outside the transaction.
func (d *DB) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return d.tx(ctx, nil, fn)
}

// ReadTx runs fn in a read-only (deferred) transaction for a consistent multi-query snapshot.
func (d *DB) ReadTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return d.tx(ctx, &sql.TxOptions{ReadOnly: true}, fn)
}

func (d *DB) tx(ctx context.Context, opts *sql.TxOptions, fn func(tx *sql.Tx) error) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = d.txOnce(ctx, opts, fn)
		if err == nil || !IsBusy(err) || attempt >= maxBusyRetries {
			return err
		}
		backoff := time.Duration(25<<attempt)*time.Millisecond + time.Duration(rand.Int64N(int64(25*time.Millisecond)))
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.Join(err, ctx.Err())
		case <-t.C:
		}
	}
}

func (d *DB) txOnce(ctx context.Context, opts *sql.TxOptions, fn func(tx *sql.Tx) error) (err error) {
	tx, err := d.SQL.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, rbErr)
		}
		return err
	}
	return tx.Commit()
}

// IsBusy reports whether err is SQLITE_BUSY (any extended code).
func IsBusy(err error) bool { return errors.Is(err, sqlite3.BUSY) }

// IsUniqueViolation reports whether err is a UNIQUE or PRIMARY KEY constraint failure, so
// callers can map it to 409 conflict.
func IsUniqueViolation(err error) bool {
	return errors.Is(err, sqlite3.CONSTRAINT_UNIQUE) || errors.Is(err, sqlite3.CONSTRAINT_PRIMARYKEY)
}

// IsForeignKeyViolation reports whether err is a FOREIGN KEY constraint failure.
func IsForeignKeyViolation(err error) bool { return errors.Is(err, sqlite3.CONSTRAINT_FOREIGNKEY) }

// Querier is satisfied by *sql.DB, *sql.Tx and *sql.Conn, so helpers can run in or out of a
// transaction.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var (
	_ Querier = (*sql.DB)(nil)
	_ Querier = (*sql.Tx)(nil)
)
