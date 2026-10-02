package jobs

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krabhi4/muxprune/internal/engine"
	"github.com/krabhi4/muxprune/internal/probe"
	"github.com/krabhi4/muxprune/internal/scan"
	"github.com/krabhi4/muxprune/internal/store"
)

// Completing a scan_library job must stamp last_scan_finished_at so the periodic
// scheduler knows when the next scan is due.
func TestRunner_ScanLibraryJob_MarksLibraryScanned(t *testing.T) {
	dir, err := os.MkdirTemp("", "muxprune-runner-test-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	libDir := filepath.Join(dir, "media")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("mkdir libdir: %v", err)
	}
	lib := &store.Library{Name: "L", Path: libDir, Kind: "other", HardlinkPolicy: "skip"}
	if err := st.AddLibrary(lib); err != nil {
		t.Fatalf("add lib: %v", err)
	}

	prober := &probe.Prober{}
	scanner := &scan.Scanner{Store: st, Prober: prober}
	r := &Runner{Store: st, Engine: &engine.Engine{Prober: prober}, Scanner: scanner}

	if _, err := st.CreateJob("scan_library", 0, lib.Path, ScanLibraryPayload{LibraryID: lib.ID}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	job, err := st.ClaimNextJob()
	if err != nil || job == nil {
		t.Fatalf("claim job: %v", err)
	}

	status, _, _ := r.execute(context.Background(), job)
	if status != "done" {
		t.Fatalf("scan_library status = %q, want done", status)
	}

	got, _ := st.GetLibrary(lib.ID)
	if got.LastScanFinishedAt == 0 {
		t.Error("LastScanFinishedAt = 0 after scan_library, want it stamped")
	}
}

func TestFinalStatus(t *testing.T) {
	cases := []struct {
		name                 string
		wasCancelled, killed bool
		status, log          string
		wantStatus, wantLog  string
	}{
		{"user cancel wins", true, true, "failed", "tool killed", "cancelled", "cancelled by user"},
		{"shutdown kill relabels failure", false, true, "failed", "signal: killed", "cancelled", "cancelled by shutdown"},
		{"natural failure kept", false, false, "failed", "bad file", "failed", "bad file"},
		{"done kept even when killed late", false, true, "done", "ok", "done", "ok"},
		{"done kept when user cancel lands late", true, true, "done", "ok", "done", "ok"},
		{"skip kept", false, false, "skipped", "hardlink", "skipped", "hardlink"},
	}
	for _, c := range cases {
		gs, gl := finalStatus(c.wasCancelled, c.killed, c.status, c.log)
		if gs != c.wantStatus || gl != c.wantLog {
			t.Errorf("%s: finalStatus=%q,%q want %q,%q", c.name, gs, gl, c.wantStatus, c.wantLog)
		}
	}
}

func TestRunner_StaleFingerprintFailsJob(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	lib := &store.Library{Name: "L", Path: dir, Kind: "other", HardlinkPolicy: "skip"}
	if err := st.AddLibrary(lib); err != nil {
		t.Fatalf("add lib: %v", err)
	}
	p := filepath.Join(dir, "a.mkv")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	f := &store.MediaFile{LibraryID: lib.ID, Path: p, Size: info.Size(), Mtime: info.ModTime().Unix()}
	if err := st.UpsertMediaFile(f); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateJob("remux", f.ID, p, RemuxPayload{AudioIdx: []int{1}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("rewritten by another job"), 0o644); err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimNextJob()
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	r := &Runner{Store: st, Engine: &engine.Engine{Prober: &probe.Prober{}}}
	status, log, _ := r.execute(context.Background(), job)
	if status != "failed" || !strings.Contains(log, "file changed since this job was queued") {
		t.Errorf("stale job = %q %q, want failed with file-changed log", status, log)
	}
}

func TestRunner_FingerprintCheckedUnderFileLock(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	p := filepath.Join(dir, "a.mkv")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateJob("remux", 0, p, RemuxPayload{AudioIdx: []int{1}}); err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimNextJob()
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	job.FileSize = sql.NullInt64{Int64: info.Size(), Valid: true}
	job.FileMtime = sql.NullInt64{Int64: info.ModTime().Unix(), Valid: true}
	r := &Runner{Store: st, Engine: &engine.Engine{Prober: &probe.Prober{}}}

	unlock := r.locks.Lock(p)
	done := make(chan string)
	go func() {
		_, log, _ := r.execute(context.Background(), job)
		done <- log
	}()
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(p, []byte("rewritten by the job holding the lock"), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock()
	if log := <-done; !strings.Contains(log, "file changed since this job was queued") {
		t.Errorf("job log = %q, want file-changed failure after waiting on the lock", log)
	}
}

func TestRunner_AllowHardlinkHonorsLibraryPolicy(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	r := &Runner{Store: st}
	for _, policy := range []string{"skip", "proceed"} {
		libDir := filepath.Join(dir, policy)
		lib := &store.Library{Name: policy, Path: libDir, Kind: "other", HardlinkPolicy: policy}
		if err := st.AddLibrary(lib); err != nil {
			t.Fatal(err)
		}
		f := &store.MediaFile{LibraryID: lib.ID, Path: filepath.Join(libDir, "a.mkv")}
		if err := st.UpsertMediaFile(f); err != nil {
			t.Fatal(err)
		}
		job := &store.Job{FileID: sql.NullInt64{Int64: f.ID, Valid: true}}
		if got, want := r.allowHardlink(job, false), policy == "proceed"; got != want {
			t.Errorf("policy %s: allowHardlink=%v want %v", policy, got, want)
		}
		if !r.allowHardlink(job, true) {
			t.Errorf("policy %s: payload allow_hardlink ignored", policy)
		}
	}
	if r.allowHardlink(&store.Job{}, false) {
		t.Error("job without a media file should not allow hardlinks")
	}
}
