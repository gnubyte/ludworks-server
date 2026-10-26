//go:build windows

package system

import (
	"strings"

	"golang.org/x/sys/windows"
)

// statDisk reports the size of the volume containing path (GetDiskFreeSpaceExW). The volume
// mount point (GetVolumePathNameW, e.g. `C:\` or a mounted-folder path) identifies the
// volume so directories on the same disk are reported once.
func statDisk(path string) (diskStat, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return diskStat{}, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return diskStat{}, err
	}
	id := strings.ToLower(path)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	if err := windows.GetVolumePathName(p, &buf[0], uint32(len(buf))); err == nil {
		id = strings.ToLower(windows.UTF16ToString(buf))
	}
	return diskStat{ID: "vol:" + id, Total: total, Free: free, Avail: avail}, nil
}
