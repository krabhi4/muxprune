//go:build !unix

package engine

// wrapNice is a no-op on non-unix platforms.
func wrapNice(name string, args []string) (string, []string) {
	return name, args
}
