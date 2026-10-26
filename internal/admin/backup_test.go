package admin

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestPruneBackups(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"ludworks-20260101T000000Z.db",
		"ludworks-20260101T000000Z-2.db",
		"ludworks-20260101T000000Z-10.db",
		"ludworks-20260102T000000Z.db",
		"ludworks-20260103T000000Z.db",
		"notes.db",                    // unrelated: never touched
		"ludworks-20260101T000000Z.x", // unrelated
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneBackups(dir, 2); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	want := []string{"ludworks-20260101T000000Z.x", "ludworks-20260102T000000Z.db", "ludworks-20260103T000000Z.db", "notes.db"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	// The numeric suffix orders same-second backups (-10 is newer than -2).
	if !backupLess("ludworks-20260101T000000Z-2.db", "ludworks-20260101T000000Z-10.db") ||
		!backupLess("ludworks-20260101T000000Z.db", "ludworks-20260101T000000Z-2.db") {
		t.Fatal("ordering")
	}
}
