package db

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Migration file names: NNNN_description.sql (at least four digits, e.g. 0001_init.sql).
var migrationName = regexp.MustCompile(`^(\d{4,})_([A-Za-z0-9_.-]+)\.sql$`)

// moduleName restricts module identifiers recorded in schema_migrations.
var moduleName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Migration is one parsed migration file.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

func (d *DB) ensureMigrationsTable(ctx context.Context) error {
	_, err := d.SQL.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		module     TEXT    NOT NULL,
		version    INTEGER NOT NULL,
		name       TEXT    NOT NULL,
		applied_at TEXT    NOT NULL,
		PRIMARY KEY (module, version)
	) STRICT`)
	if err != nil {
		return fmt.Errorf("db: create schema_migrations: %w", err)
	}
	return nil
}

// LoadMigrations reads and orders the migrations in fsys. The .sql files must be at the root
// of fsys; as a convenience, if the root holds none but has a "migrations" directory, that
// directory is used (so an embed.FS of "migrations/*.sql" works without fs.Sub). Files that
// do not match NNNN_name.sql are rejected rather than skipped, to catch typos.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	if fsys == nil {
		return nil, nil
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	hasSQL, hasDir := false, false
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			hasSQL = true
		}
		if e.IsDir() && e.Name() == "migrations" {
			hasDir = true
		}
	}
	if !hasSQL && hasDir {
		sub, err := fs.Sub(fsys, "migrations")
		if err != nil {
			return nil, err
		}
		return LoadMigrations(sub)
	}
	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration file %q: name must match NNNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(m[1])
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("migration file %q: invalid version", e.Name())
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migration version %d defined twice (%s, %s)", v, prev, e.Name())
		}
		seen[v] = e.Name()
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: v, Name: e.Name(), SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Migrate applies the not-yet-applied migrations of module from fsys in version order. Each
// migration runs in its own transaction together with its schema_migrations row, so a
// failing migration leaves no partial state and is retried on the next start.
func (d *DB) Migrate(ctx context.Context, module string, fsys fs.FS) error {
	if !moduleName.MatchString(module) {
		return fmt.Errorf("db: invalid module name %q", module)
	}
	migs, err := LoadMigrations(fsys)
	if err != nil {
		return fmt.Errorf("db: migrations for %s: %w", module, err)
	}
	if len(migs) == 0 {
		return nil
	}
	applied := map[int]bool{}
	rows, err := d.SQL.QueryContext(ctx, `SELECT version FROM schema_migrations WHERE module = ?`, module)
	if err != nil {
		return fmt.Errorf("db: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, m := range migs {
		if applied[m.Version] {
			continue
		}
		err := d.Tx(ctx, func(tx *sql.Tx) error {
			// Re-check inside the write lock in case another process raced us.
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE module = ? AND version = ?`,
				module, m.Version).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return nil
			}
			if strings.TrimSpace(m.SQL) != "" {
				if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (module, version, name, applied_at) VALUES (?, ?, ?, ?)`,
				module, m.Version, m.Name, FormatTime(nowUTC()))
			return err
		})
		if err != nil {
			return fmt.Errorf("db: migration %s/%s: %w", module, m.Name, err)
		}
	}
	return nil
}

// AppliedVersions returns the applied migration versions for module (ascending).
func (d *DB) AppliedVersions(ctx context.Context, module string) ([]int, error) {
	rows, err := d.SQL.QueryContext(ctx, `SELECT version FROM schema_migrations WHERE module = ? ORDER BY version`, module)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
