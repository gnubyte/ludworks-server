package notes

import "context"

// Test hooks (compiled into tests only).

// FTSTable is the name of a user's search table.
var FTSTable = ftsTable

// SweepIndexes runs the housekeeping sweep of search tables of deleted accounts.
func (m *Module) SweepIndexes(ctx context.Context) error { return m.svc.sweepIndexes(ctx) }
