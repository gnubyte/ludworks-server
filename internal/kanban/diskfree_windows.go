//go:build windows

package kanban

import "golang.org/x/sys/windows"

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
