package watch

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/krabhi4/muxprune/internal/scan"
)

func isRelevantFile(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	if scan.IsVideo(name) {
		return true
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	return scan.IsSubtitleExt(ext)
}

type libWatcher struct {
	libID    int64
	root     string
	debounce time.Duration
	trigger  func(libID int64)
	onStatus func(libID int64, status string)

	fsw    *fsnotify.Watcher
	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	timer *time.Timer
	dead  bool
}

func newLibWatcher(libID int64, root string, debounce time.Duration, trigger func(int64), onStatus func(int64, string)) *libWatcher {
	if debounce <= 0 {
		debounce = 10 * time.Second
	}
	return &libWatcher{libID: libID, root: root, debounce: debounce, trigger: trigger, onStatus: onStatus}
}

func (w *libWatcher) start(parent context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.fsw = fsw
	ctx, cancel := context.WithCancel(parent)
	w.ctx, w.cancel = ctx, cancel
	if err := fsw.Add(w.root); err != nil {
		w.status("error: " + err.Error())
		fsw.Close()
		cancel()
		return err
	}
	w.addRecursive(w.root)
	go w.loop(ctx)
	return nil
}

func (w *libWatcher) stop() {
	w.mu.Lock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.mu.Unlock()
	if w.cancel != nil {
		w.cancel()
	}
}

func (w *libWatcher) isDead() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dead
}

func (w *libWatcher) died(reason string) {
	if w.ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	w.dead = true
	w.mu.Unlock()
	w.status("error: " + reason)
}

func (w *libWatcher) addRecursive(root string) {
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		_ = w.fsw.Add(path)
		return nil
	})
}

func (w *libWatcher) loop(ctx context.Context) {
	defer w.fsw.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				w.died("watcher event stream closed")
				return
			}
			w.handle(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				w.died("watcher error stream closed")
				return
			}
			if err != nil {
				w.status("error: " + err.Error())
				_ = w.fsw.Add(w.root)
			}
		}
	}
}

func (w *libWatcher) handle(ev fsnotify.Event) {
	name := filepath.Base(ev.Name)
	if ev.Op&fsnotify.Create != 0 {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			if !strings.HasPrefix(name, ".") {
				w.addRecursive(ev.Name)
				w.schedule()
			}
			return
		}
	}
	if isRelevantFile(name) {
		w.schedule()
	}
}

func (w *libWatcher) schedule() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Reset(w.debounce)
		return
	}
	w.timer = time.AfterFunc(w.debounce, w.fire)
}

func (w *libWatcher) fire() {
	w.mu.Lock()
	w.timer = nil
	w.mu.Unlock()
	if w.ctx.Err() != nil || w.isDead() {
		return
	}
	w.trigger(w.libID)
}

func (w *libWatcher) status(s string) {
	if w.onStatus != nil {
		w.onStatus(w.libID, s)
	}
}
