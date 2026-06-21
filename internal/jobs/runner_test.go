package jobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
