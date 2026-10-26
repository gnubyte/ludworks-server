//go:build linux

package system

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// statDisk reports the size of the filesystem containing path (statfs(2)). Block counts are
// in units of f_frsize, which is what df(1) uses; f_bsize is only a fallback.
func statDisk(path string) (diskStat, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return diskStat{}, err
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return diskStat{}, err
	}
	bs := nonNeg(fs.Frsize)
	if bs == 0 {
		bs = nonNeg(fs.Bsize)
	}
	return diskStat{
		ID:    "dev:" + strconv.FormatUint(uint64(st.Dev), 10),
		Total: mulSat(nonNeg(fs.Blocks), bs),
		Free:  mulSat(nonNeg(fs.Bfree), bs),
		Avail: mulSat(nonNeg(fs.Bavail), bs),
	}, nil
}
