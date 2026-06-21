//go:build linux

package watch

import "golang.org/x/sys/unix"

func isLocalFS(path string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false
	}
	return !isNetworkFSType(int64(st.Type))
}
