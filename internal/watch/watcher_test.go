package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/krabhi4/muxprune/internal/store"
)

func TestIsRelevantFile(t *testing.T) {
	relevant := []string{"Show.S01E01.mkv", "Movie.mp4", "Show.S01E01.en.srt", "Movie.eng.sup", "x.ass"}
	irrelevant := []string{"movie.nfo", "poster.jpg", ".hidden.mkv", "movie.mkv.muxprune.tmp", "download.part", "notes.txt"}
	for _, n := range relevant {
		if !isRelevantFile(n) {
			t.Errorf("%q should be relevant", n)
		}
	}
	for _, n := range irrelevant {
		if isRelevantFile(n) {
			t.Errorf("%q should NOT be relevant", n)
		}
	}
}

func TestWatcher_FileCreate_TriggersDebounced(t *testing.T) {
	dir := t.TempDir()
	fired := make(chan int64, 8)
	w := newLibWatcher(7, dir, 80*time.Millisecond, func(id int64) { fired <- id }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(120 * time.Millisecond) // let the watch establish

	if err := os.WriteFile(filepath.Join(dir, "show.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case id := <-fired:
		if id != 7 {
			t.Errorf("fired id = %d, want 7", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not fire on file create")
	}
}

// A file dropped into a subdirectory created AFTER the watcher started must
// still trigger, proving recursive watch registration.
func TestWatcher_NewSubdirFile_Triggers(t *testing.T) {
	dir := t.TempDir()
	fired := make(chan int64, 8)
	w := newLibWatcher(1, dir, 80*time.Millisecond, func(id int64) { fired <- id }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(120 * time.Millisecond)

	sub := filepath.Join(dir, "Season 01")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Wait for the mkdir-driven trigger; by the time it arrives the watcher has
	// already registered a watch on the new subdir.
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not fire on subdir create")
	}

	if err := os.WriteFile(filepath.Join(sub, "ep.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not fire for file in newly-created subdir (recursion broken)")
	}
}

func TestMonitor_Reconcile_StatusReflectsWatchEnabled(t *testing.T) {
	st := openReconcileStore(t)

	enabledDir := t.TempDir()
	disabledDir := t.TempDir()
	enabled := &store.Library{Name: "on", Path: enabledDir, Kind: "other", HardlinkPolicy: "skip", WatchEnabled: true, AutoScanInterval: 0}
	disabled := &store.Library{Name: "off", Path: disabledDir, Kind: "other", HardlinkPolicy: "skip", WatchEnabled: false, AutoScanInterval: 0}
	if err := st.AddLibrary(enabled); err != nil {
		t.Fatalf("add enabled: %v", err)
	}
	if err := st.AddLibrary(disabled); err != nil {
		t.Fatalf("add disabled: %v", err)
	}

	m := New(st, func(int64) {}, Config{Debounce: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Start(ctx)
	time.Sleep(300 * time.Millisecond) // allow Reconcile + watcher startup

	if got := m.Status(enabled.ID); got != "watching" {
		t.Errorf("enabled library status = %q, want watching", got)
	}
	if got := m.Status(disabled.ID); got != "disabled" {
		t.Errorf("disabled library status = %q, want disabled", got)
	}
}

func openReconcileStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWatcher_DirMovedOut_Triggers(t *testing.T) {
	dir := t.TempDir()
	show := filepath.Join(dir, "Show")
	if err := os.MkdirAll(show, 0o755); err != nil {
		t.Fatal(err)
	}
	fired := make(chan int64, 8)
	w := newLibWatcher(3, dir, 80*time.Millisecond, func(id int64) { fired <- id }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(120 * time.Millisecond)

	if err := os.Rename(show, filepath.Join(t.TempDir(), "Show")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not fire when a directory was moved out")
	}
}

func TestWatcher_MovedDirDropsStaleSubWatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Show", "Season 01"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := newLibWatcher(5, dir, 50*time.Millisecond, func(int64) {}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	if err := os.Rename(filepath.Join(dir, "Show"), filepath.Join(dir, "Show (2020)")); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "Show", "Season 01")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, p := range w.fsw.WatchList() {
			if p == stale {
				found = true
			}
		}
		if !found {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("stale watch %s still registered after its parent moved", stale)
}

func TestWatcher_RenamedDirStillWatched(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	fired := make(chan int64, 16)
	w := newLibWatcher(6, dir, 50*time.Millisecond, func(id int64) { fired <- id }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	if err := os.Rename(filepath.Join(dir, "a"), filepath.Join(dir, "b")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	for len(fired) > 0 {
		<-fired
	}
	if err := os.WriteFile(filepath.Join(dir, "b", "new.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("file created in a renamed directory did not trigger a scan")
	}
}
