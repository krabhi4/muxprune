// Package watch keeps the media library in sync with the filesystem.
package watch

import (
	"context"
	"sync"
	"time"

	"github.com/krabhi4/muxprune/internal/store"
)

type Store interface {
	ListLibraries() ([]store.Library, error)
	IsScanActive(libraryID int64) (bool, error)
}

type Notifier interface {
	Notify(event string, data any)
}

type Config struct {
	TickEvery     time.Duration
	Debounce      time.Duration
	WatchDisabled bool
	Now           func() time.Time
	Events        Notifier
}

type Monitor struct {
	store       Store
	enqueue     func(libraryID int64)
	now         func() time.Time
	tickEvery   time.Duration
	debounce    time.Duration
	watchGlobal bool
	events      Notifier

	reconcileMu sync.Mutex
	mu          sync.Mutex
	ctx         context.Context
	watchers    map[int64]*libWatcher
	statuses    map[int64]string
}

func New(st Store, enqueue func(int64), cfg Config) *Monitor {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TickEvery <= 0 {
		cfg.TickEvery = 60 * time.Second
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = 10 * time.Second
	}
	return &Monitor{
		store:       st,
		enqueue:     enqueue,
		now:         cfg.Now,
		tickEvery:   cfg.TickEvery,
		debounce:    cfg.Debounce,
		watchGlobal: !cfg.WatchDisabled,
		events:      cfg.Events,
		watchers:    map[int64]*libWatcher{},
		statuses:    map[int64]string{},
	}
}

func dueForScan(lib store.Library, now int64) bool {
	if lib.AutoScanInterval <= 0 {
		return false
	}
	if lib.LastScanFinishedAt == 0 {
		return true
	}
	return now-lib.LastScanFinishedAt >= int64(lib.AutoScanInterval)
}

func (m *Monitor) tick() {
	libs, err := m.store.ListLibraries()
	if err != nil {
		return
	}
	now := m.now().Unix()
	for _, lib := range libs {
		if !dueForScan(lib, now) {
			continue
		}
		if active, err := m.store.IsScanActive(lib.ID); err != nil || active {
			continue
		}
		m.enqueue(lib.ID)
	}
}

func (m *Monitor) Start(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.Reconcile()

	ticker := time.NewTicker(m.tickEvery)
	defer ticker.Stop()
	for {
		m.tick()
		select {
		case <-ctx.Done():
			m.stopAll()
			return
		case <-ticker.C:
			m.Reconcile()
		}
	}
}

func (m *Monitor) Status(libID int64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statuses[libID]
}

func (m *Monitor) setStatus(libID int64, status string) {
	m.mu.Lock()
	m.statuses[libID] = status
	m.mu.Unlock()
	if m.events != nil {
		m.events.Notify("watch", map[string]any{"library_id": libID, "status": status})
	}
}

func (m *Monitor) shouldWatch(lib store.Library) bool {
	return lib.WatchEnabled && m.watchGlobal && isLocalFS(lib.Path)
}

func (m *Monitor) Reconcile() {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	libs, err := m.store.ListLibraries()
	if err != nil {
		return
	}
	m.mu.Lock()
	ctx := m.ctx
	m.mu.Unlock()
	if ctx == nil {
		return
	}

	want := make(map[int64]store.Library, len(libs))
	for _, lib := range libs {
		want[lib.ID] = lib
	}

	m.mu.Lock()
	var toStop []*libWatcher
	for id, w := range m.watchers {
		if lib, ok := want[id]; !ok || !m.shouldWatch(lib) {
			toStop = append(toStop, w)
			delete(m.watchers, id)
		}
	}
	for id := range m.statuses {
		if _, ok := want[id]; !ok {
			delete(m.statuses, id)
		}
	}
	m.mu.Unlock()
	for _, w := range toStop {
		w.stop()
	}

	for _, lib := range libs {
		switch {
		case !lib.WatchEnabled:
			m.setStatus(lib.ID, "disabled")
			continue
		case !m.watchGlobal:
			m.setStatus(lib.ID, "polling")
			continue
		case !isLocalFS(lib.Path):
			m.setStatus(lib.ID, "polling")
			continue
		}

		m.mu.Lock()
		existing := m.watchers[lib.ID]
		m.mu.Unlock()
		if existing != nil {
			if !existing.isDead() {
				continue
			}
			existing.stop()
			m.mu.Lock()
			delete(m.watchers, lib.ID)
			m.mu.Unlock()
		}

		w := newLibWatcher(lib.ID, lib.Path, m.debounce, m.enqueue, m.setStatus)
		if err := w.start(ctx); err != nil {
			m.setStatus(lib.ID, "error: "+err.Error())
			continue
		}
		m.mu.Lock()
		m.watchers[lib.ID] = w
		m.mu.Unlock()
		if w.isDegraded() {
			m.setStatus(lib.ID, "watch-limit")
		} else {
			m.setStatus(lib.ID, "watching")
		}
	}
}

func (m *Monitor) stopAll() {
	m.mu.Lock()
	ws := make([]*libWatcher, 0, len(m.watchers))
	for id, w := range m.watchers {
		ws = append(ws, w)
		delete(m.watchers, id)
	}
	m.mu.Unlock()
	for _, w := range ws {
		w.stop()
	}
}
