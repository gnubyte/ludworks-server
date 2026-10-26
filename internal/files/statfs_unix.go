//go:build unix

package files

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

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

func isWindowsNotSameDevice(error) bool { return false }

// renameBetween renames src (in area a) to dst (in area b) with renameat(2) on directory
// handles opened through each area's own os.Root: no component of either path is resolved
// outside its area, not even through a symlink swapped in after the handles were opened. The
// final components are not followed (a symlink is moved as a link).
func renameBetween(a *area, src string, b *area, dst string) error {
	sdir, sname, err := splitRel(src)
	if err != nil {
		return err
	}
	ddir, dname, err := splitRel(dst)
	if err != nil {
		return err
	}
	sd, err := a.root.Open(sdir)
	if err != nil {
		return err
	}
	defer sd.Close()
	dd, err := b.root.Open(ddir)
	if err != nil {
		return err
	}
	defer dd.Close()
	err = withFd(sd, func(sfd int) error {
		return withFd(dd, func(dfd int) error { return unix.Renameat(sfd, sname, dfd, dname) })
	})
	if err != nil {
		return &os.LinkError{Op: "renameat", Old: filepath.Join(a.base, src), New: filepath.Join(b.base, dst), Err: err}
	}
	return nil
}

// withFd runs fn with f's descriptor (without switching f to blocking mode as Fd would).
func withFd(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}
