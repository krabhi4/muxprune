//go:build !unix

package engine

import (
	"io/fs"
	"math"
	"os"
)

func nlink(info fs.FileInfo) int { return 1 }

func freeSpace(dir string) (uint64, error) { return math.MaxUint64, nil }

func preserveAttrs(tmp string, orig fs.FileInfo) {
	os.Chmod(tmp, orig.Mode().Perm())
	os.Chtimes(tmp, orig.ModTime(), orig.ModTime())
}
