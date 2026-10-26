//go:build !windows

package config

import (
	"errors"
	"fmt"
	"os"
)

// restrictDir makes the directory owner-only (MkdirAll's mode is subject to the umask).
func restrictDir(path string) error { return os.Chmod(path, 0o700) }

// RestrictDirTo is the POSIX equivalent of the Windows DACL helper: owner-only access (the
// SIDs are ignored; ownership is managed by the package's maintainer scripts).
func RestrictDirTo(path string, _ ...string) error { return restrictDir(path) }

// ensureDataDir creates the data directory owner-only. An existing directory is left as it
// is: on Linux its ownership and mode are managed by the package's maintainer scripts.
func ensureDataDir(path string) error {
	created := false
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		created = true
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if created {
		if err := restrictDir(path); err != nil {
			return fmt.Errorf("restrict data dir permissions: %w", err)
		}
	}
	return nil
}

// checkDataEntries checks the security of the files the server trusts (Windows only; POSIX
// modes are enforced where the files are created).
func checkDataEntries(*Config) error { return nil }

// checkConfigFile checks who can change the configuration file (Windows only; on Linux the
// package installs it root-owned, mode 0640).
func checkConfigFile(string) error { return nil }

// CheckPrivate reports whether a file is private to the accounts that run and administer
// Ludworks (Windows only; see settings.CheckKeyFile for the POSIX mode check).
func CheckPrivate(string) error { return nil }
