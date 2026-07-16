package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestEscapeLike(t *testing.T) {
	cases := map[string]string{
		"plain":      "plain",
		"S01_E02":    `S01\_E02`,
		"50%":        `50\%`,
		`back\slash`: `back\\slash`,
		"a_b%c":      `a\_b\%c`,
	}
	for in, want := range cases {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClaimNextJob_ExactlyOnceUnderConcurrency(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const total = 60
	for i := 0; i < total; i++ {
		if _, err := s.CreateJob("remux", 0, fmt.Sprintf("/x/%d.mkv", i), map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	claimed := map[int64]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := s.ClaimNextJob()
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				claimed[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != total {
		t.Errorf("claimed %d distinct jobs, want %d", len(claimed), total)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("job %d claimed %d times", id, n)
		}
	}
}

func TestStats_PropagatesDBError(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := s.Stats(); err == nil {
		t.Fatal("Stats on closed store must return an error, got nil")
	}
}

func TestGetLibrary_PropagatesDBError(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	lib, err := s.GetLibrary(1)
	if err == nil {
		t.Fatal("GetLibrary on closed store must return an error, got nil")
	}
	if lib != nil {
		t.Errorf("GetLibrary on error must return nil library, got %+v", lib)
	}
}

func TestListFiles_SearchEscapesWildcards(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	lib := &Library{Name: "L", Path: "/x", Kind: "other", HardlinkPolicy: "skip"}
	if err := s.AddLibrary(lib); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/x/The_Matrix.mkv", "/x/TheXMatrix.mkv"} {
		if err := s.UpsertMediaFile(&MediaFile{LibraryID: lib.ID, Path: p, Size: 1, Mtime: 1}); err != nil {
			t.Fatal(err)
		}
	}
	files, total, err := s.ListFiles(FileFilter{Query: "The_Matrix"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(files) != 1 || files[0].Path != "/x/The_Matrix.mkv" {
		t.Errorf("query 'The_Matrix' should match only the literal underscore file, got total=%d files=%v", total, files)
	}
}

func TestRetryJob(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j, err := s.CreateJob("remux", 0, "/x/a.mkv", map[string]any{"audio_idx": []int{1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetryJob(j.ID); err == nil {
		t.Fatal("a queued job should not be retryable")
	}
	if err := s.FinishJob(j.ID, "failed", "boom", 0); err != nil {
		t.Fatal(err)
	}
	nj, err := s.RetryJob(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if nj.ID == j.ID || nj.Status != "queued" || nj.Type != "remux" || nj.FilePath != "/x/a.mkv" {
		t.Errorf("retry produced unexpected job: %+v", nj)
	}
}

func TestCountStaleFiles(t *testing.T) {
	dir, err := os.MkdirTemp("", "muxprune-stale-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	lib := &Library{Name: "L", Path: "/x", Kind: "other", HardlinkPolicy: "skip"}
	if err := s.AddLibrary(lib); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		f := &MediaFile{LibraryID: lib.ID, Path: fmt.Sprintf("/x/%d.mkv", i), Size: 1, Mtime: 1}
		if err := s.UpsertMediaFile(f); err != nil {
			t.Fatal(err)
		}
	}
	if stale, err := s.CountStaleFiles(lib.ID, time.Now().Unix()+100); err != nil || stale != 3 {
		t.Fatalf("stale (future cutoff) = %d, err=%v, want 3", stale, err)
	}
	if stale, _ := s.CountStaleFiles(lib.ID, time.Now().Unix()-100); stale != 0 {
		t.Errorf("stale (past cutoff) = %d, want 0", stale)
	}
}

func TestStore_ListFiles_FilteringAndSorting(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "muxprune-store-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	// 1. Add a library
	lib := &Library{
		Name:           "Test Library",
		Path:           "/tmp/test-lib",
		Kind:           "other",
		HardlinkPolicy: "skip",
	}
	if err := s.AddLibrary(lib); err != nil {
		t.Fatalf("failed to add library: %v", err)
	}

	// 2. Add media files with different fields
	// File A: size=1000, mtime=100, nlink=1, kind="movie", title="Movie A", sub_summary="eng", sidecar_summary=""
	fileA := &MediaFile{
		LibraryID:      lib.ID,
		Path:           "/tmp/test-lib/movie-a.mkv",
		Size:           1000,
		Mtime:          100,
		Nlink:          1,
		Kind:           "movie",
		Title:          "Movie A",
		SubSummary:     "eng",
		SidecarSummary: "",
	}
	if err := s.UpsertMediaFile(fileA); err != nil {
		t.Fatalf("failed to insert file A: %v", err)
	}

	// File B: size=500, mtime=200, nlink=2, kind="tv", series="Series B", title="Episode B", sub_summary="", sidecar_summary="fre"
	fileB := &MediaFile{
		LibraryID:      lib.ID,
		Path:           "/tmp/test-lib/tv-b.mkv",
		Size:           500,
		Mtime:          200,
		Nlink:          2,
		Kind:           "tv",
		Series:         "Series B",
		Title:          "Episode B",
		SubSummary:     "",
		SidecarSummary: "fre",
	}
	if err := s.UpsertMediaFile(fileB); err != nil {
		t.Fatalf("failed to insert file B: %v", err)
	}

	// File C: size=2000, mtime=150, nlink=1, kind="other", title="Video C", sub_summary="", sidecar_summary=""
	fileC := &MediaFile{
		LibraryID:      lib.ID,
		Path:           "/tmp/test-lib/other-c.mkv",
		Size:           2000,
		Mtime:          150,
		Nlink:          1,
		Kind:           "other",
		Title:          "Video C",
		SubSummary:     "",
		SidecarSummary: "",
	}
	if err := s.UpsertMediaFile(fileC); err != nil {
		t.Fatalf("failed to insert file C: %v", err)
	}

	// Test case structure
	tests := []struct {
		name          string
		filter        FileFilter
		expectedPaths []string
	}{
		{
			name:          "Filter kind=movie",
			filter:        FileFilter{Kind: "movie"},
			expectedPaths: []string{fileA.Path},
		},
		{
			name:          "Filter kind=tv",
			filter:        FileFilter{Kind: "tv"},
			expectedPaths: []string{fileB.Path},
		},
		{
			name:          "Filter hardlinks=yes (nlink > 1)",
			filter:        FileFilter{Hardlinks: "yes"},
			expectedPaths: []string{fileB.Path},
		},
		{
			name:          "Filter hardlinks=no (nlink == 1)",
			filter:        FileFilter{Hardlinks: "no"},
			expectedPaths: []string{fileA.Path, fileC.Path}, // Ordered by default (series/season/episode/title/path) -> Movie A, Video C
		},
		{
			name:          "Filter subs=embedded",
			filter:        FileFilter{Subs: "embedded"},
			expectedPaths: []string{fileA.Path},
		},
		{
			name:          "Filter subs=none_embedded",
			filter:        FileFilter{Subs: "none_embedded"},
			expectedPaths: []string{fileC.Path, fileB.Path},
		},
		{
			name:          "Filter subs=sidecar",
			filter:        FileFilter{Subs: "sidecar"},
			expectedPaths: []string{fileB.Path},
		},
		{
			name:          "Filter subs=none_sidecar",
			filter:        FileFilter{Subs: "none_sidecar"},
			expectedPaths: []string{fileA.Path, fileC.Path},
		},
		{
			name:          "Filter subs=any",
			filter:        FileFilter{Subs: "any"},
			expectedPaths: []string{fileA.Path, fileB.Path},
		},
		{
			name:          "Filter subs=none",
			filter:        FileFilter{Subs: "none"},
			expectedPaths: []string{fileC.Path},
		},
		{
			name:          "Sort by size ASC",
			filter:        FileFilter{Sort: "size", Order: "asc"},
			expectedPaths: []string{fileB.Path, fileA.Path, fileC.Path}, // 500, 1000, 2000
		},
		{
			name:          "Sort by size DESC",
			filter:        FileFilter{Sort: "size", Order: "desc"},
			expectedPaths: []string{fileC.Path, fileA.Path, fileB.Path}, // 2000, 1000, 500
		},
		{
			name:          "Sort by mtime ASC",
			filter:        FileFilter{Sort: "mtime", Order: "asc"},
			expectedPaths: []string{fileA.Path, fileC.Path, fileB.Path}, // 100, 150, 200
		},
		{
			name:          "Sort by mtime DESC",
			filter:        FileFilter{Sort: "mtime", Order: "desc"},
			expectedPaths: []string{fileB.Path, fileC.Path, fileA.Path}, // 200, 150, 100
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files, _, err := s.ListFiles(tc.filter)
			if err != nil {
				t.Fatalf("ListFiles failed: %v", err)
			}
			if len(files) != len(tc.expectedPaths) {
				t.Errorf("expected %d files, got %d", len(tc.expectedPaths), len(files))
			}
			for i, p := range tc.expectedPaths {
				if i < len(files) && files[i].Path != p {
					t.Errorf("at index %d, expected path %q, got %q", i, p, files[i].Path)
				}
			}
		})
	}
}

func TestStore_IsScanActive(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "muxprune-store-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	// Initially, scan is not active
	active, err := s.IsScanActive(1)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if active {
		t.Error("expected scan not to be active initially")
	}

	activeAll, err := s.IsScanAllActive()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if activeAll {
		t.Error("expected scan all not to be active initially")
	}

	// Queue a scan library job
	_, err = s.CreateJob("scan_library", 0, "/tmp/lib-1", map[string]any{"library_id": int64(1)})
	if err != nil {
		t.Fatalf("failed to create job: %v", err)
	}

	active, err = s.IsScanActive(1)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !active {
		t.Error("expected scan to be active for library 1")
	}

	active, err = s.IsScanActive(2)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if active {
		t.Error("expected scan not to be active for library 2")
	}

	// Queue a scan_all job
	_, err = s.CreateJob("scan_all", 0, "all libraries", map[string]any{})
	if err != nil {
		t.Fatalf("failed to create job: %v", err)
	}

	activeAll, err = s.IsScanAllActive()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !activeAll {
		t.Error("expected scan all to be active")
	}

	active, err = s.IsScanActive(2)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !active {
		t.Error("expected scan to be active for library 2 because scan_all is active")
	}
}

func TestStore_CancelJob(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "muxprune-store-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	// 1. Create a queued job
	job, err := s.CreateJob("scan_all", 0, "all libraries", map[string]any{})
	if err != nil {
		t.Fatalf("failed to create job: %v", err)
	}

	// Verify status is queued
	j1, err := s.GetJob(job.ID)
	if err != nil || j1 == nil {
		t.Fatalf("failed to get job: %v", err)
	}
	if j1.Status != "queued" {
		t.Errorf("expected status 'queued', got %q", j1.Status)
	}

	// 2. Cancel the job
	err = s.CancelJob(job.ID)
	if err != nil {
		t.Fatalf("failed to cancel job: %v", err)
	}

	// Verify status is cancelled and log is cancelled
	j2, err := s.GetJob(job.ID)
	if err != nil || j2 == nil {
		t.Fatalf("failed to get job: %v", err)
	}
	if j2.Status != "cancelled" {
		t.Errorf("expected status 'cancelled', got %q", j2.Status)
	}
	if j2.Log != "cancelled by user" {
		t.Errorf("expected log 'cancelled by user', got %q", j2.Log)
	}

	// 3. Trying to cancel again should fail
	err = s.CancelJob(job.ID)
	if err == nil {
		t.Error("expected error when cancelling an already cancelled job")
	}

	// 4. Try to cancel a running job (should fail)
	job2, err := s.CreateJob("scan_all", 0, "all libraries", map[string]any{})
	if err != nil {
		t.Fatalf("failed to create job2: %v", err)
	}
	_, err = s.ClaimNextJob() // moves job2 to running
	if err != nil {
		t.Fatalf("failed to claim job: %v", err)
	}
	err = s.CancelJob(job2.ID)
	if err == nil {
		t.Error("expected error when trying to cancel a running job")
	}
}

func TestRetryJob_IncrementsAttempts(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j, err := s.CreateJob("remux", 0, "/x/a.mkv", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if j.Attempts != 0 {
		t.Fatalf("new job attempts = %d, want 0", j.Attempts)
	}
	if _, err := s.ClaimNextJob(); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(j.ID, "failed", "boom", 0); err != nil {
		t.Fatal(err)
	}
	r1, err := s.RetryJob(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Attempts != 1 {
		t.Errorf("first retry attempts = %d, want 1", r1.Attempts)
	}
	if _, err := s.ClaimNextJob(); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(r1.ID, "failed", "boom", 0); err != nil {
		t.Fatal(err)
	}
	r2, err := s.RetryJob(r1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Attempts != 2 {
		t.Errorf("second retry attempts = %d, want 2", r2.Attempts)
	}
	got, err := s.GetJob(r2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 2 {
		t.Errorf("persisted attempts = %d, want 2", got.Attempts)
	}
}
