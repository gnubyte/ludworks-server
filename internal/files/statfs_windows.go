//go:build windows

package files

import (
	"errors"

	"golang.org/x/sys/windows"
)

// diskFree reports the bytes available to the caller on the volume holding path.
func diskFree(path string) (int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	if avail > 1<<62 {
		avail = 1 << 62
	}
	return int64(avail), nil
}

// isWindowsNotSameDevice reports ERROR_NOT_SAME_DEVICE (a move across volumes).
func isWindowsNotSameDevice(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE)
}

// renameBetween renames src (in area a) to dst (in area b). Windows has no renameat, so every
// folder above both paths is first verified to be a plain directory (no symlink, junction or
// other reparse point) inside its area.
func renameBetween(a *area, src string, b *area, dst string) error {
	return renameChecked(a, src, b, dst)
}
