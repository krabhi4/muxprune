package watch

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/krabhi4/muxprune/internal/store"
)

// A watcher whose loop died must be rebuilt by the next Reconcile (self-heal),
// not left as a dead entry reporting "watching" forever.
func TestMonitor_Reconcile_RebuildsDeadWatcher(t *testing.T) {
	st := openReconcileStore(t)
	dir := t.TempDir()
	lib := &store.Library{Name: "L", Path: dir, Kind: "other", HardlinkPolicy: "skip", WatchEnabled: true}
	if err := st.AddLibrary(lib); err != nil {
		t.Fatalf("add: %v", err)
	}
	m := New(st, func(int64) {}, Config{Debounce: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.Reconcile()

	m.mu.Lock()
	old := m.watchers[lib.ID]
	m.mu.Unlock()
	if old == nil {
		t.Fatal("no watcher started for enabled local library")
	}

	// Simulate the watcher goroutine having died unexpectedly.
	old.mu.Lock()
	old.dead = true
	old.mu.Unlock()

	m.Reconcile()

	m.mu.Lock()
	cur := m.watchers[lib.ID]
	m.mu.Unlock()
	if cur == nil || cur == old {
		t.Fatal("dead watcher was not rebuilt by Reconcile")
	}
	if cur.isDead() {
		t.Error("rebuilt watcher is already dead")
	}
	if s := m.Status(lib.ID); s != "watching" {
		t.Errorf("status after rebuild = %q, want watching", s)
	}
}

// Concurrent Reconcile calls for a not-yet-watched library must create exactly
// one watcher, not leak orphaned watcher goroutines (TOCTOU regression guard).
func TestMonitor_Reconcile_ConcurrentNoWatcherLeak(t *testing.T) {
	st := openReconcileStore(t)
	dir := t.TempDir()
	lib := &store.Library{Name: "L", Path: dir, Kind: "other", HardlinkPolicy: "skip", WatchEnabled: true}
	if err := st.AddLibrary(lib); err != nil {
		t.Fatalf("add: %v", err)
	}
	m := New(st, func(int64) {}, Config{Debounce: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()

	runtime.GC()
	base := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.Reconcile() }()
	}
	wg.Wait()
	time.Sleep(150 * time.Millisecond) // let any started loops show up

	m.mu.Lock()
	n := len(m.watchers)
	m.mu.Unlock()
	if n != 1 {
		t.Errorf("tracked watchers = %d, want 1", n)
	}
	if delta := runtime.NumGoroutine() - base; delta > 2 {
		t.Errorf("watcher goroutine leak: +%d goroutines after concurrent Reconcile (want <=1)", delta)
	}
}
