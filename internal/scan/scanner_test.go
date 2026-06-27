package scan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/krabhi4/muxprune/internal/probe"
	"github.com/krabhi4/muxprune/internal/store"
)

func seedCacheHit(t *testing.T, sc *Scanner, lib *store.Library, dir, name string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("realfilecontents"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	rec := &store.MediaFile{
		LibraryID: lib.ID, Path: p, Size: info.Size(), Mtime: info.ModTime().Unix(),
		Nlink: 1, SidecarSummary: "", ProbeJSON: "{}",
	}
	if err := sc.Store.UpsertMediaFile(rec); err != nil {
		t.Fatal(err)
	}
}

func seedGhost(t *testing.T, sc *Scanner, lib *store.Library, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		g := &store.MediaFile{LibraryID: lib.ID, Path: fmt.Sprintf("/gone/ghost%d.mkv", i), Size: 1, Mtime: 1, Nlink: 1}
		if err := sc.Store.UpsertMediaFile(g); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScanLibrary_RatioGuardSkipsMassPrune(t *testing.T) {
	sc := newTestScanner(t)
	dir := t.TempDir()
	lib := &store.Library{Name: "L", Path: dir, Kind: "other", HardlinkPolicy: "skip"}
	if err := sc.Store.AddLibrary(lib); err != nil {
		t.Fatal(err)
	}
	seedCacheHit(t, sc, lib, dir, "keep.mkv")
	seedGhost(t, sc, lib, 9)
	time.Sleep(1100 * time.Millisecond)

	if err := sc.ScanLibrary(context.Background(), lib); err != nil {
		t.Fatal(err)
	}
	if n, _ := sc.Store.CountFilesByLibrary(lib.ID); n != 10 {
		t.Errorf("records after guarded scan = %d, want 10 (prune skipped)", n)
	}
}

func TestScanLibrary_PrunesWhenUnderThreshold(t *testing.T) {
	sc := newTestScanner(t)
	sc.MaxPruneRatio = 1.0
	dir := t.TempDir()
	lib := &store.Library{Name: "L", Path: dir, Kind: "other", HardlinkPolicy: "skip"}
	if err := sc.Store.AddLibrary(lib); err != nil {
		t.Fatal(err)
	}
	seedCacheHit(t, sc, lib, dir, "keep.mkv")
	seedGhost(t, sc, lib, 9)
	time.Sleep(1100 * time.Millisecond)

	if err := sc.ScanLibrary(context.Background(), lib); err != nil {
		t.Fatal(err)
	}
	if n, _ := sc.Store.CountFilesByLibrary(lib.ID); n != 1 {
		t.Errorf("records after permissive scan = %d, want 1 (ghosts pruned)", n)
	}
}

func TestScanLibrary_ProbeErrorKeepsExistingRecord(t *testing.T) {
	sc := newTestScanner(t)
	sc.MaxPruneRatio = 1.0
	dir := t.TempDir()
	lib := &store.Library{Name: "L", Path: dir, Kind: "other", HardlinkPolicy: "skip"}
	if err := sc.Store.AddLibrary(lib); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.mkv")
	if err := os.WriteFile(bad, []byte("not media"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &store.MediaFile{LibraryID: lib.ID, Path: bad, Size: 1, Mtime: 1, Nlink: 1, ProbeJSON: ""}
	if err := sc.Store.UpsertMediaFile(rec); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)

	if err := sc.ScanLibrary(context.Background(), lib); err != nil {
		t.Fatal(err)
	}
	if n, _ := sc.Store.CountFilesByLibrary(lib.ID); n != 1 {
		t.Errorf("records after probe-error scan = %d, want 1 (record preserved)", n)
	}
}

func newTestScanner(t *testing.T) *Scanner {
	t.Helper()
	dir, err := os.MkdirTemp("", "muxprune-scan-test-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return &Scanner{Store: s, Prober: &probe.Prober{}}
}

// A library whose root has vanished (e.g. an unmounted volume) must NOT be
// treated as a mass deletion: ScanLibrary errors out and prunes nothing.
func TestScanLibrary_MissingRoot_AbortsWithoutPruning(t *testing.T) {
	sc := newTestScanner(t)
	lib := &store.Library{Name: "L", Path: "/tmp/muxprune-does-not-exist-xyz", Kind: "other", HardlinkPolicy: "skip"}
	if err := sc.Store.AddLibrary(lib); err != nil {
		t.Fatalf("add lib: %v", err)
	}
	seed := &store.MediaFile{LibraryID: lib.ID, Path: "/tmp/old/ghost.mkv", Size: 1, Mtime: 1}
	if err := sc.Store.UpsertMediaFile(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Ensure the seeded record predates the scan start so a prune *would*
	// delete it absent the guard.
	time.Sleep(1100 * time.Millisecond)

	err := sc.ScanLibrary(context.Background(), lib)
	if err == nil {
		t.Fatal("expected ScanLibrary to error on a missing root, got nil")
	}
	n, _ := sc.Store.CountFilesByLibrary(lib.ID)
	if n != 1 {
		t.Errorf("records after missing-root scan = %d, want 1 (no prune)", n)
	}
}

// An accessible-but-empty root while the DB still holds records is suspicious
// (likely a wrong/half-mounted path) and must skip pruning rather than wipe the
// library.
func TestScanLibrary_EmptyRootWithExistingRecords_SkipsPrune(t *testing.T) {
	sc := newTestScanner(t)
	dir, err := os.MkdirTemp("", "muxprune-emptylib-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	lib := &store.Library{Name: "L", Path: dir, Kind: "other", HardlinkPolicy: "skip"}
	if err := sc.Store.AddLibrary(lib); err != nil {
		t.Fatalf("add lib: %v", err)
	}
	seed := &store.MediaFile{LibraryID: lib.ID, Path: filepath.Join(dir, "gone.mkv"), Size: 1, Mtime: 1}
	if err := sc.Store.UpsertMediaFile(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)

	if err := sc.ScanLibrary(context.Background(), lib); err != nil {
		t.Fatalf("ScanLibrary returned error: %v", err)
	}
	n, _ := sc.Store.CountFilesByLibrary(lib.ID)
	if n != 1 {
		t.Errorf("records after empty-root scan = %d, want 1 (prune skipped)", n)
	}
}
