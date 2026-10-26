//go:build windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The service must not run a program that a non-administrator can replace: a file in a folder
// the current (test) user owns is refused, a system binary under %SystemRoot% is accepted.
func TestCheckServiceProgram(t *testing.T) {
	dir := t.TempDir() // in the user's profile: owned by and writable for the current user
	exe := filepath.Join(dir, "ludworks.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := CheckServiceProgram(exe)
	if err == nil || !strings.Contains(err.Error(), "could replace the program") {
		t.Fatalf("user-owned program accepted: %v", err)
	}
	if err := CheckServiceProgram(dir); err == nil {
		t.Fatal("a directory was accepted as the service program")
	}
	sys := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	if err := CheckServiceProgram(sys); err != nil {
		t.Fatalf("%s refused: %v", sys, err)
	}
}

// A data directory holding something another account can access (e.g. planted by whoever
// created the directory first) is refused by service install, and left unchanged. Under a
// test temp dir the user-owned profile parents may be refused first; either way nothing is
// adopted, and the tree check that guards the data itself names the planted file.
func TestPrepareServiceDataDirRefusesForeignEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(dir, "secret.key")
	if err := os.WriteFile(planted, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	const everyone = "S-1-1-0"
	if err := setPrivateDACL(planted, "", everyone); err != nil {
		t.Fatal(err)
	}
	err := PrepareServiceDataDir(dir, ServiceSID(WindowsServiceName))
	if err == nil {
		t.Fatalf("data directory with a world-accessible file adopted: %v", err)
	}
	o, err := inspect(planted, false)
	if err != nil {
		t.Fatal(err)
	}
	if o.problem(map[string]bool{sidSystem: true, sidAdministrators: true}, false) == "" {
		t.Fatal("planted file unexpectedly private")
	}
}

func TestVerifyTreeNamesPlantedEntry(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(dir, "backups")
	if err := os.Mkdir(planted, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := setPrivateDACL(planted, "", "S-1-1-0"); err != nil { // Everyone: full control
		t.Fatal(err)
	}
	trusted, err := trustedSIDs(ServiceSID(WindowsServiceName))
	if err != nil {
		t.Fatal(err)
	}
	err = verifyTree(dir, trusted)
	if err == nil || !strings.Contains(err.Error(), planted) || !strings.Contains(err.Error(), "grants access to") {
		t.Fatalf("verifyTree = %v", err)
	}
}
