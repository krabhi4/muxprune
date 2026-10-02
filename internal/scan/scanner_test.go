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
		g := &store.MediaFile{LibraryID: lib.ID, Path: filepath.Join(lib.Path, "gone", fmt.Sprintf("ghost%d.mkv", i)), Size: 1, Mtime: 1, Nlink: 1}
		if err := sc.Store.UpsertMediaFile(g); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScanLibrary_WalkErrorSkipsPrune(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; permission errors cannot be induced")
	}
	sc := newTestScanner(t)
	sc.MaxPruneRatio = 1.0
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
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
		t.Errorf("records after walk-error scan = %d, want 10 (prune skipped)", n)
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

func TestScanLibrary_SidecarGoesToLongestVideoBase(t *testing.T) {
	sc := newTestScanner(t)
	dir := t.TempDir()
	lib := &store.Library{Name: "L", Path: dir, Kind: "other", HardlinkPolicy: "skip"}
	if err := sc.Store.AddLibrary(lib); err != nil {
		t.Fatal(err)
	}
	seedCacheHit(t, sc, lib, dir, "Movie.mkv")
	seedCacheHit(t, sc, lib, dir, "Movie.sample.mkv")
	for _, n := range []string{"Movie.en.srt", "Movie.sample.en.srt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("1"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := sc.ScanLibrary(context.Background(), lib); err != nil {
		t.Fatal(err)
	}
	files, _, err := sc.Store.ListFiles(store.FileFilter{LibraryID: lib.ID})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Movie.mkv": "Movie.en.srt", "Movie.sample.mkv": "Movie.sample.en.srt"}
	for _, f := range files {
		got, err := sc.Store.GetFile(f.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Sidecars) != 1 || got.Sidecars[0].Name != want[filepath.Base(f.Path)] {
			t.Errorf("%s sidecars = %+v, want only %s", filepath.Base(f.Path), got.Sidecars, want[filepath.Base(f.Path)])
		}
	}
}

func TestScanLibrary_SmallLibraryPrunesBelowFloor(t *testing.T) {
	sc := newTestScanner(t)
	dir := t.TempDir()
	lib := &store.Library{Name: "L", Path: dir, Kind: "other", HardlinkPolicy: "skip"}
	if err := sc.Store.AddLibrary(lib); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.mkv", "b.mkv", "c.mkv"} {
		seedCacheHit(t, sc, lib, dir, n)
	}
	seedGhost(t, sc, lib, 1)
	time.Sleep(1100 * time.Millisecond)

	if err := sc.ScanLibrary(context.Background(), lib); err != nil {
		t.Fatal(err)
	}
	if n, _ := sc.Store.CountFilesByLibrary(lib.ID); n != 3 {
		t.Errorf("records after 1-of-4 deletion = %d, want 3 (ghost pruned)", n)
	}
}

func TestScanLibrary_RemovesOldTempFiles(t *testing.T) {
	sc := newTestScanner(t)
	dir := t.TempDir()
	lib := &store.Library{Name: "L", Path: dir, Kind: "other", HardlinkPolicy: "skip"}
	if err := sc.Store.AddLibrary(lib); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, ".a.muxprune.tmp.1-1.mkv")
	fresh := filepath.Join(dir, ".a.muxprune.tmp.1-2.mkv")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	back := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(old, back, back); err != nil {
		t.Fatal(err)
	}
	if err := sc.ScanLibrary(context.Background(), lib); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old temp file still present: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh temp file removed: %v", err)
	}
}

func TestScanLibrary_SymlinkUsesTargetStat(t *testing.T) {
	sc := newTestScanner(t)
	sc.MaxPruneRatio = 1.0
	dir := t.TempDir()
	lib := &store.Library{Name: "L", Path: dir, Kind: "other", HardlinkPolicy: "skip"}
	if err := sc.Store.AddLibrary(lib); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "real.mkv")
	if err := os.WriteFile(target, []byte("a much longer body than the link text"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.mkv")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "nowhere.mkv"), filepath.Join(dir, "dead.mkv")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "link.en.srt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &store.MediaFile{LibraryID: lib.ID, Path: link, Size: info.Size(), Mtime: info.ModTime().Unix(), Nlink: 1, ProbeJSON: "{}"}
	if err := sc.Store.UpsertMediaFile(rec); err != nil {
		t.Fatal(err)
	}
	seedGhost(t, sc, lib, 1)
	time.Sleep(1100 * time.Millisecond)

	if err := sc.ScanLibrary(context.Background(), lib); err != nil {
		t.Fatal(err)
	}
	got, err := sc.Store.GetFile(rec.ID)
	if err != nil || got == nil {
		t.Fatalf("link record: %v", err)
	}
	if got.SidecarSummary != "en.srt" {
		t.Errorf("link sidecar summary = %q, want en.srt (cache hit on target stat)", got.SidecarSummary)
	}
	if n, _ := sc.Store.CountFilesByLibrary(lib.ID); n != 1 {
		t.Errorf("records = %d, want 1 (ghost pruned despite dangling link)", n)
	}
}

func TestScanLibrary_PathChangedMidScan(t *testing.T) {
	sc := newTestScanner(t)
	oldDir, newDir := t.TempDir(), t.TempDir()
	lib := &store.Library{Name: "L", Path: oldDir, Kind: "other", HardlinkPolicy: "skip"}
	if err := sc.Store.AddLibrary(lib); err != nil {
		t.Fatal(err)
	}
	held := *lib
	lib.Path = newDir
	if err := sc.Store.UpdateLibrary(lib); err != nil {
		t.Fatal(err)
	}
	seedCacheHit(t, sc, &held, oldDir, "keep.mkv")
	seedGhost(t, sc, &held, 9)
	time.Sleep(1100 * time.Millisecond)

	if err := sc.ScanLibrary(context.Background(), &held); err != nil {
		t.Fatal(err)
	}
	if n, _ := sc.Store.CountFilesByLibrary(lib.ID); n != 10 {
		t.Fatalf("records after scan with stale library = %d, want 10 (no prune)", n)
	}
	seedCacheHit(t, sc, lib, newDir, "new.mkv")
	if err := sc.ScanLibrary(context.Background(), lib); err != nil {
		t.Fatal(err)
	}
	if n, _ := sc.Store.CountFilesByLibrary(lib.ID); n != 1 {
		t.Errorf("records after rescan at new path = %d, want 1 (old-path rows removed despite ratio guard)", n)
	}
}

func TestScanLibrary_SymlinkedRootIsWalked(t *testing.T) {
	sc := newTestScanner(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "tv")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	lib := &store.Library{Name: "L", Path: link, Kind: "other", HardlinkPolicy: "skip"}
	if err := sc.Store.AddLibrary(lib); err != nil {
		t.Fatal(err)
	}
	seedCacheHit(t, sc, lib, link, "keep.mkv")
	seedGhost(t, sc, lib, 1)
	time.Sleep(1100 * time.Millisecond)

	if err := sc.ScanLibrary(context.Background(), lib); err != nil {
		t.Fatal(err)
	}
	if n, _ := sc.Store.CountFilesByLibrary(lib.ID); n != 1 {
		t.Errorf("records after scanning symlinked root = %d, want 1 (keep.mkv found, ghost pruned)", n)
	}
}

func TestScanLibrary_BrokenNonVideoSymlinkKeepsPruning(t *testing.T) {
	sc := newTestScanner(t)
	sc.MaxPruneRatio = 1.0
	dir := t.TempDir()
	if err := os.Symlink("loop.nfo", filepath.Join(dir, "loop.nfo")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	lib := &store.Library{Name: "L", Path: dir, Kind: "other", HardlinkPolicy: "skip"}
	if err := sc.Store.AddLibrary(lib); err != nil {
		t.Fatal(err)
	}
	seedCacheHit(t, sc, lib, dir, "keep.mkv")
	seedGhost(t, sc, lib, 1)
	time.Sleep(1100 * time.Millisecond)

	if err := sc.ScanLibrary(context.Background(), lib); err != nil {
		t.Fatal(err)
	}
	if n, _ := sc.Store.CountFilesByLibrary(lib.ID); n != 1 {
		t.Errorf("records = %d, want 1 (a looping .nfo symlink must not disable pruning)", n)
	}
}
