//go:build !unix

package scan

import "io/fs"

func Nlink(info fs.FileInfo) int { return 1 }
