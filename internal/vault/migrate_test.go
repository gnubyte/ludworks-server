package vault_test

import (
	"context"
	"io/fs"
	"path/filepath"
	"regexp"
	"testing"
	"testing/fstest"

	"ludworks/internal/db"
	"ludworks/internal/vault"
)

// Vaults created before key identities existed get a random v4 key_id and no key_check.
func TestMigrationAssignsKeyIDsToExistingVaults(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.SQL.ExecContext(ctx, `CREATE TABLE users (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	all := new(vault.Module).Migrations()
	first, err := fs.ReadFile(all, "0001_vault.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, "vault", fstest.MapFS{"0001_vault.sql": {Data: first}}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"u1", "u2"} {
		if _, err := d.SQL.ExecContext(ctx, `INSERT INTO users (id) VALUES (?)`, u); err != nil {
			t.Fatal(err)
		}
		if _, err := d.SQL.ExecContext(ctx, `INSERT INTO vault_keys (user_id, kdf, kdf_iterations, kdf_salt, wrapped_key,
			sync_horizon, created_at, updated_at) VALUES (?, 'pbkdf2-sha256', 600000, 's', 'w', 't', 't', 't')`, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Migrate(ctx, "vault", all); err != nil {
		t.Fatal(err)
	}
	v4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	rows, err := d.SQL.QueryContext(ctx, `SELECT key_id, key_check IS NULL FROM vault_keys`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		var noCheck bool
		if err := rows.Scan(&id, &noCheck); err != nil {
			t.Fatal(err)
		}
		if !v4.MatchString(id) || !noCheck || seen[id] {
			t.Fatalf("migrated row: key_id %q key_check null %v (seen %v)", id, noCheck, seen)
		}
		seen[id] = true
	}
	if len(seen) != 2 {
		t.Fatalf("rows: %d", len(seen))
	}
}
