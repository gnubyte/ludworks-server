package files

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Both cross-area rename paths (renameat on unix, check-then-rename elsewhere) must refuse
// to follow a folder that was swapped for a symlink out of the area.
func TestCrossAreaRenameStaysConfined(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs privileges on Windows")
	}
	dir := t.TempDir()
	for _, d := range []string{"a/sub", "b", "victim"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a/sub/x.txt", "mine")
	write("victim/v.txt", "VICTIM")
	data, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	open := func(base string) *area {
		r, err := data.OpenRoot(base)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		return &area{root: r, data: data, base: base}
	}
	a, b := open("a"), open("b")

	for name, rename := range map[string]func(*area, string, *area, string) error{"renameBetween": renameBetween, "renameChecked": renameChecked} {
		// Plain folders: the move works.
		if err := rename(a, filepath.Join("sub", "x.txt"), b, "x.txt"); err != nil {
			t.Fatalf("%s: plain move: %v", name, err)
		}
		if err := rename(b, "x.txt", a, filepath.Join("sub", "x.txt")); err != nil {
			t.Fatalf("%s: move back: %v", name, err)
		}
		// sub -> ../victim: the move must not reach the victim's file.
		if err := os.Rename(filepath.Join(dir, "a", "sub"), filepath.Join(dir, "a", "sub.real")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../victim", filepath.Join(dir, "a", "sub")); err != nil {
			t.Fatal(err)
		}
		if err := rename(a, filepath.Join("sub", "v.txt"), b, "stolen.txt"); err == nil {
			t.Fatalf("%s followed a symlink out of the area", name)
		}
		write("b/p.txt", "planted")
		if err := rename(b, "p.txt", a, filepath.Join("sub", "planted.txt")); err == nil {
			t.Fatalf("%s wrote through a symlink out of the area", name)
		}
		if _, err := os.Lstat(filepath.Join(dir, "victim", "planted.txt")); err == nil {
			t.Fatalf("%s planted a file in the victim's folder", name)
		}
		_ = os.Remove(filepath.Join(dir, "b", "p.txt"))
		if _, err := os.Stat(filepath.Join(dir, "victim", "v.txt")); err != nil {
			t.Fatalf("%s moved the victim's file: %v", name, err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "b", "stolen.txt")); err == nil {
			t.Fatalf("%s: victim file arrived in b", name)
		}
		_ = os.Remove(filepath.Join(dir, "a", "sub"))
		if err := os.Rename(filepath.Join(dir, "a", "sub.real"), filepath.Join(dir, "a", "sub")); err != nil {
			t.Fatal(err)
		}
	}
	// renameChecked reports the link as a 409.
	_ = os.Rename(filepath.Join(dir, "a", "sub"), filepath.Join(dir, "a", "sub.real"))
	_ = os.Symlink("../victim", filepath.Join(dir, "a", "sub"))
	if err := renameChecked(a, filepath.Join("sub", "v.txt"), b, "s.txt"); !errors.Is(err, errLinkOnDisk) {
		t.Fatalf("renameChecked: %v", err)
	}
}
