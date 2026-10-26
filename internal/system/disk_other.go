//go:build !linux && !darwin && !freebsd && !windows

package system

func statDisk(string) (diskStat, error) { return diskStat{}, errStatUnsupported }
