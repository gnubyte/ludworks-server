//go:build !unix && !windows

package kanban

import "errors"

// diskFree is unsupported on this platform; the free-space check is then skipped (a full
// disk still answers 507 through ENOSPC).
func diskFree(string) (int64, error) {
	return 0, errors.New("free space not available on this platform")
}
