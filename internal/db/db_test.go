package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestOpenPragmasAndFTS5(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	checks := map[string]string{"journal_mode": "wal", "foreign_keys": "1", "busy_timeout": "10000", "synchronous": "1"}
	for p, want := range checks {
		var got string
		if err := d.SQL.QueryRowContext(ctx, "PRAGMA "+p).Scan(&got); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if got != want {
			t.Errorf("PRAGMA %s = %q, want %q", p, got, want)
		}
	}
	if _, err := d.SQL.ExecContext(ctx, `CREATE VIRTUAL TABLE docs USING fts5(body)`); err != nil {
		t.Fatalf("fts5 unavailable: %v", err)
	}
	if _, err := d.SQL.ExecContext(ctx, `INSERT INTO docs(body) VALUES ('hello brave new world')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM docs WHERE docs MATCH ?`, FTS5Query("brav")).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("fts match = %d", n)
	}
	// User input full of FTS syntax must not produce an error.
	if err := d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM docs WHERE docs MATCH ?`, FTS5Query(`a" OR NEAR( * ^ -x`)).Scan(&n); err != nil {
		t.Fatalf("sanitized query failed: %v", err)
	}
}

func TestDSNEscaping(t *testing.T) {
	got := DSN(`C:\Program Data\a%b#c?.db`)
	if !strings.HasPrefix(got, "file:C:/Program%20Data/a%25b%23c%3F.db?_pragma=busy_timeout(10000)") {
		t.Fatalf("DSN = %s", got)
	}
	// A path with spaces works end to end.
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "dir with space", "..", "my db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
}

func TestMigrateOrderingIdempotencePerModule(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	// Versions deliberately out of lexical/insertion order; 0010 depends on 0002.
	fsA := fstest.MapFS{
		"0010_add_col.sql": {Data: []byte(`ALTER TABLE a ADD COLUMN extra TEXT NOT NULL DEFAULT '';`)},
		"0001_init.sql":    {Data: []byte(`CREATE TABLE a (id TEXT PRIMARY KEY); CREATE INDEX a_id ON a(id);`)},
		"0002_seed.sql":    {Data: []byte(`INSERT INTO a (id) VALUES ('x'); INSERT INTO a (id) VALUES ('y');`)},
	}
	if err := d.Migrate(ctx, "alpha", fsA); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, "alpha", fsA); err != nil {
		t.Fatalf("second run must be a no-op: %v", err)
	}
	var n int
	if err := d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM a WHERE extra = ''`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows = %d err=%v", n, err)
	}
	// A second module with the same version numbers is tracked independently.
	fsB := fstest.MapFS{"0001_init.sql": {Data: []byte(`CREATE TABLE b (id TEXT PRIMARY KEY);`)}}
	if err := d.Migrate(ctx, "beta", fsB); err != nil {
		t.Fatal(err)
	}
	va, _ := d.AppliedVersions(ctx, "alpha")
	vb, _ := d.AppliedVersions(ctx, "beta")
	if len(va) != 3 || va[0] != 1 || va[2] != 10 || len(vb) != 1 {
		t.Fatalf("applied alpha=%v beta=%v", va, vb)
	}
	// Adding a new migration later applies only that one.
	fsA["0011_more.sql"] = &fstest.MapFile{Data: []byte(`INSERT INTO a (id, extra) VALUES ('z', 'new');`)}
	if err := d.Migrate(ctx, "alpha", fsA); err != nil {
		t.Fatal(err)
	}
	d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM a`).Scan(&n)
	if n != 3 {
		t.Fatalf("rows after 0011 = %d", n)
	}
}

func TestMigrateFailureRollsBack(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	fsys := fstest.MapFS{
		"0001_ok.sql":  {Data: []byte(`CREATE TABLE ok (id INTEGER);`)},
		"0002_bad.sql": {Data: []byte(`CREATE TABLE half (id INTEGER); THIS IS NOT SQL;`)},
	}
	if err := d.Migrate(ctx, "gamma", fsys); err == nil {
		t.Fatal("expected error")
	}
	var n int
	d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'half'`).Scan(&n)
	if n != 0 {
		t.Fatal("failed migration left partial schema")
	}
	v, _ := d.AppliedVersions(ctx, "gamma")
	if len(v) != 1 || v[0] != 1 {
		t.Fatalf("applied = %v", v)
	}
}

func TestMigrateRejectsBadInput(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if err := d.Migrate(ctx, "x", fstest.MapFS{"1_short.sql": {Data: []byte(`SELECT 1`)}}); err == nil {
		t.Error("expected bad file name error")
	}
	if err := d.Migrate(ctx, "x", fstest.MapFS{"0001_a.sql": {}, "01_b.sql": {}}); err == nil {
		t.Error("expected error for bad name")
	}
	if err := d.Migrate(ctx, "x", fstest.MapFS{"0001_a.sql": {}, "00001_b.sql": {}}); err == nil {
		t.Error("expected duplicate version error")
	}
	if err := d.Migrate(ctx, "Bad Module", fstest.MapFS{}); err == nil {
		t.Error("expected invalid module error")
	}
	if err := d.Migrate(ctx, "nilfs", nil); err != nil {
		t.Errorf("nil fs must be a no-op: %v", err)
	}
	// migrations/ subdirectory convenience
	sub := fstest.MapFS{"migrations/0001_init.sql": {Data: []byte(`CREATE TABLE subdir (id INTEGER);`)}}
	if err := d.Migrate(ctx, "subdir", sub); err != nil {
		t.Fatal(err)
	}
}

func TestTxCommitRollbackAndConcurrency(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if _, err := d.SQL.ExecContext(ctx, `CREATE TABLE c (id INTEGER PRIMARY KEY, n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		tx.ExecContext(ctx, `INSERT INTO c (id, n) VALUES (1, 1)`)
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	var n int
	d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM c`).Scan(&n)
	if n != 0 {
		t.Fatal("rollback failed")
	}
	if _, err := d.SQL.ExecContext(ctx, `INSERT INTO c (id, n) VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	// Many concurrent read-modify-write transactions must serialise without lost updates.
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := d.Tx(ctx, func(tx *sql.Tx) error {
				var cur int
				if err := tx.QueryRowContext(ctx, `SELECT n FROM c WHERE id = 1`).Scan(&cur); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, `UPDATE c SET n = ? WHERE id = 1`, cur+1)
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	d.SQL.QueryRowContext(ctx, `SELECT n FROM c WHERE id = 1`).Scan(&n)
	if n != 40 {
		t.Fatalf("n = %d, want 40", n)
	}
	_, err = d.SQL.ExecContext(ctx, `INSERT INTO c (id, n) VALUES (1, 0)`)
	if !IsUniqueViolation(err) {
		t.Fatalf("IsUniqueViolation(%v) = false", err)
	}
	if err := d.ReadTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO c (id, n) VALUES (2, 0)`)
		return err
	}); err == nil {
		t.Fatal("ReadTx allowed a write")
	}
}

func TestColumnHelpers(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if _, err := d.SQL.ExecContext(ctx, `CREATE TABLE h (t TEXT NOT NULL, nt TEXT, b INTEGER, j TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	loc := time.FixedZone("x", 2*3600)
	tm := time.Date(2026, 10, 26, 14, 0, 0, 123456789, loc)
	var tags []string // nil → "[]"
	if _, err := d.SQL.ExecContext(ctx, `INSERT INTO h VALUES (?, ?, ?, ?)`,
		FormatTime(tm), NullTime{}, Bool(true), JSON[[]string]{V: tags}); err != nil {
		t.Fatal(err)
	}
	var (
		ts   Time
		nt   NullTime
		b    bool
		j    JSON[[]string]
		jraw string
	)
	if err := d.SQL.QueryRowContext(ctx, `SELECT t, nt, b, j, j FROM h`).Scan(&ts, &nt, &b, &j, &jraw); err != nil {
		t.Fatal(err)
	}
	if want := "2026-10-26T12:00:00.123Z"; FormatTime(tm) != want || !ts.Equal(tm.Truncate(time.Millisecond)) {
		t.Fatalf("time = %s / %v", FormatTime(tm), ts)
	}
	if nt.Valid || !b || jraw != "[]" || len(j.V) != 0 {
		t.Fatalf("nt=%v b=%v jraw=%q j=%v", nt, b, jraw, j.V)
	}
	// Fixed width keeps lexical order == chronological order.
	a := FormatTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := FormatTime(time.Date(2026, 1, 1, 0, 0, 0, 500e6, time.UTC))
	if !(a < c) {
		t.Fatalf("%s !< %s", a, c)
	}
	if EscapeLike(`50%_a\b`) != `50\%\_a\\b` {
		t.Fatal(EscapeLike(`50%_a\b`))
	}
	if FTS5Query("  ") != "" || FTS5Query(`foo "bar`) != `"foo" """bar"*` {
		t.Fatalf("FTS5Query = %q", FTS5Query(`foo "bar`))
	}
}
