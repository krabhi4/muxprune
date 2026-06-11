//go:build unix

package engine

import (
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func nlink(info fs.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Nlink)
	}
	return 1
}

func freeSpace(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// preserveAttrs copies mode, mtime and ownership from the original onto the
// temp file before the rename, so the replacement is invisible to PUID/PGID
// based setups. Chown failures (non-root) are ignored.
func preserveAttrs(tmp string, orig fs.FileInfo) {
	os.Chmod(tmp, orig.Mode().Perm())
	if st, ok := orig.Sys().(*syscall.Stat_t); ok {
		os.Chown(tmp, int(st.Uid), int(st.Gid))
	}
	os.Chtimes(tmp, orig.ModTime(), orig.ModTime())
}
