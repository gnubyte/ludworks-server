//go:build !unix && !windows

package files

import "errors"

// diskFree is unsupported on this platform; the free-space check is then skipped.
func diskFree(string) (int64, error) {
	return 0, errors.New("free space not available on this platform")
}

func isWindowsNotSameDevice(error) bool { return false }

// renameBetween renames src (in area a) to dst (in area b) after verifying that every folder
// above both paths is a plain directory inside its area.
func renameBetween(a *area, src string, b *area, dst string) error {
	return renameChecked(a, src, b, dst)
}
