//go:build unix

package main

import (
	"path/filepath"
	"testing"
)

func TestLockFileExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "muxprune.lock")
	f, err := lockFile(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if _, err := lockFile(path); err != errLocked {
		t.Fatalf("second lock while held = %v, want errLocked", err)
	}
	f.Close()
	g, err := lockFile(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	g.Close()
}
