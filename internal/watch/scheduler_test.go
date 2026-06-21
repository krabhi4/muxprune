package watch

import (
	"sort"
	"testing"
	"time"

	"github.com/krabhi4/muxprune/internal/store"
)

func TestDueForScan(t *testing.T) {
	const now = int64(1_000_000)
	tests := []struct {
		name string
		lib  store.Library
		want bool
	}{
		{"disabled interval never due", store.Library{AutoScanInterval: 0, LastScanFinishedAt: 0}, false},
		{"never scanned is due", store.Library{AutoScanInterval: 3600, LastScanFinishedAt: 0}, true},
		{"recently scanned not due", store.Library{AutoScanInterval: 3600, LastScanFinishedAt: now - 100}, false},
		{"exactly at interval is due", store.Library{AutoScanInterval: 3600, LastScanFinishedAt: now - 3600}, true},
		{"past interval is due", store.Library{AutoScanInterval: 3600, LastScanFinishedAt: now - 5000}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dueForScan(tc.lib, now); got != tc.want {
				t.Errorf("dueForScan = %v, want %v", got, tc.want)
			}
		})
	}
}

type fakeStore struct {
	libs   []store.Library
	active map[int64]bool
}

func (f *fakeStore) ListLibraries() ([]store.Library, error) { return f.libs, nil }
func (f *fakeStore) IsScanActive(id int64) (bool, error)     { return f.active[id], nil }

func TestMonitor_tick_enqueuesDueSkipsActiveAndDisabled(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	fs := &fakeStore{
		active: map[int64]bool{3: true},
		libs: []store.Library{
			{ID: 1, AutoScanInterval: 3600, LastScanFinishedAt: now.Unix() - 4000}, // due
			{ID: 2, AutoScanInterval: 3600, LastScanFinishedAt: now.Unix() - 100},  // not due
			{ID: 3, AutoScanInterval: 3600, LastScanFinishedAt: now.Unix() - 4000}, // due but active
			{ID: 4, AutoScanInterval: 0, LastScanFinishedAt: 0},                    // disabled
		},
	}

	var enqueued []int64
	m := New(fs, func(id int64) { enqueued = append(enqueued, id) }, Config{
		Now: func() time.Time { return now },
	})

	m.tick()

	sort.Slice(enqueued, func(i, j int) bool { return enqueued[i] < enqueued[j] })
	want := []int64{1}
	if len(enqueued) != len(want) || (len(want) == 1 && enqueued[0] != want[0]) {
		t.Errorf("enqueued = %v, want %v", enqueued, want)
	}
}
