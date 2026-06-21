package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/krabhi4/muxprune/internal/store"
)

func TestSanitizeInterval(t *testing.T) {
	cases := []struct{ in, want int }{
		{-5, 0}, {0, 0}, {30, 60}, {60, 60}, {21600, 21600},
	}
	for _, c := range cases {
		if got := sanitizeInterval(c.in); got != c.want {
			t.Errorf("sanitizeInterval(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Server{Store: st, Hub: NewHub(), DefaultAutoScanInterval: 21600}
	return s, s.Handler()
}

func doJSON(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandleAddLibrary_AppliesDefaults(t *testing.T) {
	_, h := newTestServer(t)
	dir := t.TempDir()
	rec := doJSON(t, h, "POST", "/api/v1/libraries", `{"path":"`+dir+`","kind":"tv"}`)
	if rec.Code != 201 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got store.Library
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AutoScanInterval != 21600 {
		t.Errorf("AutoScanInterval = %d, want 21600 (default)", got.AutoScanInterval)
	}
	if !got.WatchEnabled {
		t.Errorf("WatchEnabled = false, want true (default)")
	}
}

func TestHandleAddLibrary_ExplicitValues(t *testing.T) {
	_, h := newTestServer(t)
	dir := t.TempDir()
	rec := doJSON(t, h, "POST", "/api/v1/libraries", `{"path":"`+dir+`","auto_scan_interval":0,"watch_enabled":false}`)
	if rec.Code != 201 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got store.Library
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.AutoScanInterval != 0 {
		t.Errorf("AutoScanInterval = %d, want 0 (explicit off)", got.AutoScanInterval)
	}
	if got.WatchEnabled {
		t.Errorf("WatchEnabled = true, want false (explicit)")
	}
}

func TestHandleUpdateLibrary_PreservesOmittedMonitoringFields(t *testing.T) {
	s, h := newTestServer(t)
	dir := t.TempDir()
	lib := &store.Library{Name: "X", Path: dir, Kind: "tv", HardlinkPolicy: "skip", AutoScanInterval: 900, WatchEnabled: false}
	if err := s.Store.AddLibrary(lib); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := doJSON(t, h, "PUT", "/api/v1/libraries/"+strconv.FormatInt(lib.ID, 10),
		`{"path":"`+dir+`","name":"Renamed","kind":"tv"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := s.Store.GetLibrary(lib.ID)
	if got.Name != "Renamed" {
		t.Errorf("name = %q, want Renamed", got.Name)
	}
	if got.AutoScanInterval != 900 {
		t.Errorf("AutoScanInterval = %d, want 900 (preserved)", got.AutoScanInterval)
	}
	if got.WatchEnabled != false {
		t.Errorf("WatchEnabled = %v, want false (preserved)", got.WatchEnabled)
	}
}
