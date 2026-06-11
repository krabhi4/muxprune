//go:build unix

package scan

import (
	"io/fs"
	"syscall"
)

// Nlink returns the hardlink count; > 1 means a remux would silently break
// the link (typically a torrent seed) and double disk usage.
func Nlink(info fs.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Nlink)
	}
	return 1
}
