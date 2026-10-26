package auth

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/settings"
)

// Usernames are ASCII so that case-insensitive uniqueness (SQLite NOCASE) is exact and
// look-alike Unicode names are impossible. No "@", so "username or email" is unambiguous.
var usernameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

const (
	maxEmailLen       = 254
	maxDisplayNameLen = 100
	maxDeviceNameLen  = 128
	maxTokenNameLen   = 100
)

// checkUsername returns "" when valid, else a message.
func checkUsername(s string) string {
	if s == "" {
		return "required"
	}
	if !usernameRE.MatchString(s) {
		return "1-64 characters: letters, digits, '.', '_' or '-', starting with a letter or digit"
	}
	return ""
}

// normalizeEmail trims and validates a plain address (no display name, no CR/LF).
func normalizeEmail(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "required"
	}
	if len(s) > maxEmailLen || settings.HasControl(s) || !utf8.ValidString(s) {
		return "", "must be a valid email address"
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" || !strings.Contains(s, "@") {
		return "", "must be a valid email address"
	}
	return s, ""
}

// checkDisplayName validates a free-text single-line name.
func checkDisplayName(s string) string {
	switch {
	case !utf8.ValidString(s):
		return "invalid UTF-8"
	case settings.HasControl(s):
		return "must not contain control characters"
	case utf8.RuneCountInString(s) > maxDisplayNameLen:
		return fmt.Sprintf("must be at most %d characters", maxDisplayNameLen)
	}
	return ""
}

// cleanDeviceName trims a client-supplied device label, truncating overly long ones (they
// are cosmetic) but rejecting control characters.
func cleanDeviceName(s string) (string, string) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) || settings.HasControl(s) || len(s) > 4*maxDeviceNameLen {
		return "", "invalid device name"
	}
	if r := []rune(s); len(r) > maxDeviceNameLen {
		s = string(r[:maxDeviceNameLen])
	}
	return s, ""
}

// checkPassword applies the password policy: security.password_min_length characters, at
// most MaxPasswordBytes bytes, and not equal to the username or email.
func checkPassword(minLen int, pw, username, email string) string {
	if minLen < 1 {
		minLen = 12
	}
	switch {
	case len(pw) > MaxPasswordBytes:
		return fmt.Sprintf("must be at most %d bytes", MaxPasswordBytes)
	case !utf8.ValidString(pw):
		return "invalid UTF-8"
	case utf8.RuneCountInString(pw) < minLen:
		return fmt.Sprintf("must be at least %d characters", minLen)
	case strings.TrimSpace(pw) == "":
		return "must not be blank"
	case username != "" && strings.EqualFold(pw, username):
		return "must not be the same as the username"
	case email != "" && strings.EqualFold(pw, email):
		return "must not be the same as the email address"
	}
	return ""
}

// validRole reports whether r is a known role.
func validRole(r string) bool { return r == string(core.RoleAdmin) || r == string(core.RoleUser) }

// Scopes a personal access token may carry (API.md §0).
var AllScopes = []string{
	"files:read", "files:write", "media:read", "media:write", "tv:read", "tv:write",
	"notes:read", "notes:write", "vault:read", "vault:write", "sheets:read", "sheets:write",
	"account", "admin",
}

func knownScope(s string) bool {
	for _, a := range AllScopes {
		if a == s {
			return true
		}
	}
	return false
}

// fieldErrors collects 422 messages.
type fieldErrors map[string]string

func (f fieldErrors) err() error {
	if len(f) == 0 {
		return nil
	}
	return core.Validation(f)
}
