package system

import "errors"

// diskStat describes the filesystem that contains a directory.
type diskStat struct {
	// ID identifies the filesystem so directories on the same volume are reported once
	// (device number on unix, volume mount point on Windows).
	ID string
	// Total is the filesystem size, Free the bytes not in use (including blocks reserved for
	// the superuser) and Avail the bytes this process may still write.
	Total, Free, Avail uint64
}

// errStatUnsupported is returned by statDisk on platforms without an implementation.
var errStatUnsupported = errors.New("system: disk statistics are not supported on this platform")

// nonNeg converts a block count of any integer kind to uint64, mapping negative values
// (FreeBSD reports a negative f_bavail when the reserve is in use) to 0.
func nonNeg[T ~int32 | ~int64 | ~uint32 | ~uint64](v T) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// mulSat multiplies without overflowing (a corrupt statfs answer must not wrap around).
func mulSat(a, b uint64) uint64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > ^uint64(0)/b {
		return ^uint64(0)
	}
	return a * b
}
