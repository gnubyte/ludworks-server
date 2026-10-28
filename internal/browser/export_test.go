package browser

import (
	"context"
	"time"
)

// SetLimitsForTest lowers the per-user limits and returns a function restoring them.
func SetLimitsForTest(records, bytes int64) func() {
	old := lim
	lim.records, lim.bytes = records, bytes
	return func() { lim = old }
}

// SetPageBytesForTest lowers the byte budget of a sync page and returns a function restoring it.
func SetPageBytesForTest(n int) func() {
	old := lim.pageBytes
	lim.pageBytes = n
	return func() { lim.pageBytes = old }
}

// PurgeTombstonesForTest runs the tombstone purge with an explicit cutoff.
func PurgeTombstonesForTest(m *Module, cutoff time.Time) (int64, error) {
	return m.purgeTombstones(context.Background(), cutoff)
}
