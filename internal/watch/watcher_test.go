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
