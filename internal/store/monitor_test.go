package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "muxprune-mon-test-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStore_LibraryMonitoringFields_RoundTrip(t *testing.T) {
	s := openTestStore(t)

	lib := &Library{
		Name:             "TV",
		Path:             "/tmp/tv",
		Kind:             "tv",
		HardlinkPolicy:   "skip",
		AutoScanInterval: 900,
		WatchEnabled:     false,
	}
	if err := s.AddLibrary(lib); err != nil {
		t.Fatalf("add: %v", err)
	}

	got, err := s.GetLibrary(lib.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.AutoScanInterval != 900 {
		t.Errorf("AutoScanInterval = %d, want 900", got.AutoScanInterval)
	}
	if got.WatchEnabled != false {
		t.Errorf("WatchEnabled = %v, want false", got.WatchEnabled)
	}
	if got.LastScanFinishedAt != 0 {
		t.Errorf("LastScanFinishedAt = %d, want 0", got.LastScanFinishedAt)
	}

	// Update flips the fields.
	got.AutoScanInterval = 0
	got.WatchEnabled = true
	if err := s.UpdateLibrary(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, _ := s.GetLibrary(lib.ID)
	if after.AutoScanInterval != 0 || after.WatchEnabled != true {
		t.Errorf("after update: interval=%d watch=%v, want 0/true", after.AutoScanInterval, after.WatchEnabled)
	}

	// ListLibraries carries the fields too.
	list, err := s.ListLibraries()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].AutoScanInterval != 0 || list[0].WatchEnabled != true {
		t.Errorf("list mismatch: %+v", list)
	}
}

// A database created before this feature (libraries table without the new
// columns) must be upgraded in place with the documented defaults.
func TestStore_Migration_UpgradesOldLibrariesTable(t *testing.T) {
	dir, err := os.MkdirTemp("", "muxprune-mig-test-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "old.db")

	// Build an old-schema libraries table and seed a row.
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)", dbPath)
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	_, err = raw.Exec(`CREATE TABLE libraries (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		path TEXT NOT NULL UNIQUE,
		kind TEXT NOT NULL DEFAULT 'other',
		hardlink_policy TEXT NOT NULL DEFAULT 'skip'
	);`)
	if err != nil {
		t.Fatalf("create old table: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO libraries(name,path,kind,hardlink_policy) VALUES('Old','/tmp/old','movie','skip')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	raw.Close()

	// Open through the store: migrate() must add the columns with defaults.
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	libs, err := s.ListLibraries()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(libs) != 1 {
		t.Fatalf("want 1 lib, got %d", len(libs))
	}
	l := libs[0]
	if l.AutoScanInterval != 21600 {
		t.Errorf("migrated AutoScanInterval = %d, want 21600", l.AutoScanInterval)
	}
	if l.WatchEnabled != true {
		t.Errorf("migrated WatchEnabled = %v, want true", l.WatchEnabled)
	}
	if l.LastScanFinishedAt != 0 {
		t.Errorf("migrated LastScanFinishedAt = %d, want 0", l.LastScanFinishedAt)
	}
}

func TestStore_MarkLibraryScanned(t *testing.T) {
	s := openTestStore(t)
	lib := &Library{Name: "L", Path: "/tmp/l", Kind: "other", HardlinkPolicy: "skip"}
	if err := s.AddLibrary(lib); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := s.MarkLibraryScanned(lib.ID, 1700000000); err != nil {
		t.Fatalf("mark: %v", err)
	}
	got, _ := s.GetLibrary(lib.ID)
	if got.LastScanFinishedAt != 1700000000 {
		t.Errorf("LastScanFinishedAt = %d, want 1700000000", got.LastScanFinishedAt)
	}
}

func TestStore_CountFilesByLibrary(t *testing.T) {
	s := openTestStore(t)
	a := &Library{Name: "A", Path: "/tmp/a", Kind: "other", HardlinkPolicy: "skip"}
	b := &Library{Name: "B", Path: "/tmp/b", Kind: "other", HardlinkPolicy: "skip"}
	if err := s.AddLibrary(a); err != nil {
		t.Fatalf("add a: %v", err)
	}
	if err := s.AddLibrary(b); err != nil {
		t.Fatalf("add b: %v", err)
	}

	if n, err := s.CountFilesByLibrary(a.ID); err != nil || n != 0 {
		t.Fatalf("empty count = %d (err %v), want 0", n, err)
	}

	for i := 0; i < 2; i++ {
		f := &MediaFile{LibraryID: a.ID, Path: fmt.Sprintf("/tmp/a/f%d.mkv", i), Size: 1, Mtime: 1}
		if err := s.UpsertMediaFile(f); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	if n, err := s.CountFilesByLibrary(a.ID); err != nil || n != 2 {
		t.Errorf("count(a) = %d (err %v), want 2", n, err)
	}
	if n, err := s.CountFilesByLibrary(b.ID); err != nil || n != 0 {
		t.Errorf("count(b) = %d (err %v), want 0", n, err)
	}
}
