package watch

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestWatcher_StartThenImmediateCancel_NoRace(t *testing.T) {
	dir := t.TempDir()
	w := newLibWatcher(3, dir, 50*time.Millisecond, func(int64) {}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	if err := w.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); cancel() }()
	go func() { defer wg.Done(); w.died("synthetic") }()
	go func() { defer wg.Done(); _ = w.ctxErr() }()
	wg.Wait()
	w.stop()
}

func TestWatcher_StartEarlyError_NoRace(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	var mu sync.Mutex
	var statuses []string
	w := newLibWatcher(4, missing, 50*time.Millisecond, func(int64) {}, func(_ int64, s string) {
		mu.Lock()
		statuses = append(statuses, s)
		mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := w.start(ctx)
	if err == nil {
		t.Fatal("expected start to fail on a non-existent root")
	}
	if w.ctxErr() == nil {
		t.Error("ctx should be cancelled after a failed start")
	}
	mu.Lock()
	got := len(statuses)
	mu.Unlock()
	if got == 0 {
		t.Error("expected an error status to be surfaced on early-error path")
	}
}

func TestWatcher_WatchCap_SurfacesDegradedStatus(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 10; i++ {
		sub := filepath.Join(dir, "d"+strconv.Itoa(i))
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	var mu sync.Mutex
	var sawLimit int
	w := newLibWatcher(5, dir, 50*time.Millisecond, func(int64) {}, func(_ int64, s string) {
		if s == "watch-limit" {
			mu.Lock()
			sawLimit++
			mu.Unlock()
		}
	})
	w.watchLimit = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.stop()

	if !w.isDegraded() {
		t.Error("watcher should be degraded once the watch cap is reached")
	}
	w.mu.Lock()
	watches := w.watches
	w.mu.Unlock()
	if watches > w.watchLimit {
		t.Errorf("watches = %d exceeds cap %d", watches, w.watchLimit)
	}
	mu.Lock()
	got := sawLimit
	mu.Unlock()
	if got != 1 {
		t.Errorf("watch-limit status emitted %d times, want exactly 1", got)
	}
}

func TestWatcher_WatchCap_NotDegradedUnderCap(t *testing.T) {
	dir := t.TempDir()
	w := newLibWatcher(6, dir, 50*time.Millisecond, func(int64) {}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.stop()
	if w.isDegraded() {
		t.Error("watcher should not be degraded for a small tree under the default cap")
	}
}
