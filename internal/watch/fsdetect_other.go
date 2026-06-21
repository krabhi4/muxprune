//go:build !linux

package watch

func isLocalFS(string) bool { return true }
