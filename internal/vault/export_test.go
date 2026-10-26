package vault

import (
	"context"
	"time"
)

// SetLimitsForTest lowers the per-user limits and returns a function restoring them.
func SetLimitsForTest(items, folders, bytes int64) func() {
	old := lim
	lim.items, lim.folders, lim.bytes = items, folders, bytes
	return func() { lim = old }
}

// PurgeTombstonesForTest runs the tombstone purge with an explicit cutoff.
func PurgeTombstonesForTest(m *Module, cutoff time.Time) (int64, error) {
	return m.purgeTombstones(context.Background(), cutoff)
}
