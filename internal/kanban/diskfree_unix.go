//go:build unix

package kanban

import "golang.org/x/sys/unix"

// diskFree reports the bytes available to unprivileged users on the volume holding path.
func diskFree(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	free := uint64(st.Bavail) * uint64(st.Bsize) //nolint:unconvert // field types differ per platform
	if free > 1<<62 {
		free = 1 << 62
	}
	return int64(free), nil
}
