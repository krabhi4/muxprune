package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krabhi4/muxprune/internal/engine"
	"github.com/krabhi4/muxprune/internal/probe"
	"github.com/krabhi4/muxprune/internal/scan"
	"github.com/krabhi4/muxprune/internal/store"
)

func newScanRunner(t *testing.T) (*Runner, *store.Store) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muxprune-runscan-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	prober := &probe.Prober{}
	scanner := &scan.Scanner{Store: st, Prober: prober}
	return &Runner{Store: st, Engine: &engine.Engine{Prober: prober}, Scanner: scanner}, st
}

func execOne(t *testing.T, r *Runner) (status, log string) {
	t.Helper()
	job, err := r.Store.ClaimNextJob()
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	s, l, _ := r.execute(context.Background(), job)
	return s, l
}

// A failed scan must still stamp last_scan_finished_at so the scheduler backs
// off instead of re-enqueuing the same broken library every tick.
func TestRunner_ScanLibraryJob_StampsEvenOnFailure(t *testing.T) {
	r, st := newScanRunner(t)
	lib := &store.Library{Name: "gone", Path: "/tmp/muxprune-missing-xyz-123", Kind: "other", HardlinkPolicy: "skip"}
	if err := st.AddLibrary(lib); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := st.CreateJob("scan_library", 0, lib.Path, ScanLibraryPayload{LibraryID: lib.ID}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	status, _ := execOne(t, r)
	if status != "failed" {
		t.Fatalf("status = %q, want failed (missing root)", status)
	}
	got, _ := st.GetLibrary(lib.ID)
	if got.LastScanFinishedAt == 0 {
		t.Error("LastScanFinishedAt = 0 after failed scan, want it stamped (backoff)")
	}
}

// One inaccessible library must not abort scan_all for the others.
func TestRunner_ScanAllJob_ContinuesPastInaccessibleLibrary(t *testing.T) {
	r, st := newScanRunner(t)
	goodDir, err := os.MkdirTemp("", "muxprune-good-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(goodDir)

	// "aaa" sorts first and is broken; "bbb" is healthy. ListLibraries is name-ordered.
	bad := &store.Library{Name: "aaa", Path: "/tmp/muxprune-missing-aaa-999", Kind: "other", HardlinkPolicy: "skip"}
	good := &store.Library{Name: "bbb", Path: goodDir, Kind: "other", HardlinkPolicy: "skip"}
	if err := st.AddLibrary(bad); err != nil {
		t.Fatalf("add bad: %v", err)
	}
	if err := st.AddLibrary(good); err != nil {
		t.Fatalf("add good: %v", err)
	}
	if _, err := st.CreateJob("scan_all", 0, "all libraries", map[string]any{}); err != nil {
		t.Fatalf("create job: %v", err)
	}

	status, log := execOne(t, r)
	if status != "done" {
		t.Fatalf("scan_all status = %q, want done (the healthy library should succeed)", status)
	}
	gotGood, _ := st.GetLibrary(good.ID)
	if gotGood.LastScanFinishedAt == 0 {
		t.Error("healthy library was not scanned (scan_all aborted on the broken one)")
	}
	if !strings.Contains(log, "aaa") {
		t.Errorf("log %q should mention the failed library", log)
	}
}
