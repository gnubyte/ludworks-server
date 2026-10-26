package auth

import (
	"context"
	"errors"

	"ludworks/internal/core"
)

// reauthHasher verifies passwords for other modules. Verification uses the parameters stored in
// each hash (and the process-wide hashing semaphore), so it matches the module's own hasher;
// the default parameters only shape the dummy hash burnt for unknown accounts.
var reauthHasher = newHasher(DefaultPasswordParams)

// VerifyPassword re-checks an account's login password before a sensitive action in another
// module (e.g. POST /api/v1/vault/reset, which needs the account password rather than the
// vault master password).
//
// It returns false with a nil error for a wrong password and for unknown or disabled accounts
// (those still cost one full hash, so timing reveals nothing). Errors are reserved for storage
// or context failures. It does not rate limit or audit: callers must guard it with their own
// httpx.Lockout keyed by user and record the outcome in the audit log.
func VerifyPassword(ctx context.Context, d *core.Deps, userID, password string) (bool, error) {
	h := reauthHasher
	if password == "" || len(password) > MaxPasswordBytes {
		return false, nil
	}
	u, err := getUser(ctx, d.DB.SQL, userID)
	if err != nil {
		if errors.Is(err, errUserNotFound) {
			h.Burn(ctx, password)
			return false, nil
		}
		return false, err
	}
	if u.Disabled {
		h.Burn(ctx, password)
		return false, nil
	}
	ok, _, err := h.Verify(ctx, u.PasswordHash, password)
	if err != nil {
		return false, err
	}
	return ok, nil
}
