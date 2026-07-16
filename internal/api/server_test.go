package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/krabhi4/muxprune/internal/engine"
	"github.com/krabhi4/muxprune/internal/probe"
	"github.com/krabhi4/muxprune/internal/scan"
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

func newAuthServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Server{Store: st, Hub: NewHub(), DefaultAutoScanInterval: 21600, APIKey: "secret"}
	return s, s.Handler()
}

func TestAuth_MutatingRouteRequiresCredentials(t *testing.T) {
	_, h := newAuthServer(t)
	dir := t.TempDir()
	body := `{"path":"` + dir + `","kind":"tv"}`

	rec := doJSON(t, h, "POST", "/api/v1/libraries", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without key: status = %d, want 401", rec.Code)
	}

	req := httptest.NewRequest("POST", "/api/v1/libraries", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("with key: status = %d body=%s, want 201", rec.Code, rec.Body.String())
	}
}

func TestAuth_SSERequiresCredentials(t *testing.T) {
	_, h := newAuthServer(t)
	rec := doJSON(t, h, "GET", "/sse", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuth_HealthIsOpen(t *testing.T) {
	s, h := newAuthServer(t)
	s.Scanner = &scan.Scanner{Prober: &probe.Prober{}}
	s.Engine = &engine.Engine{Prober: &probe.Prober{}}
	rec := doJSON(t, h, "GET", "/api/v1/health", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestAuth_SessionCookie(t *testing.T) {
	_, h := newAuthServer(t)

	req := httptest.NewRequest("POST", "/api/v1/auth/session", nil)
	req.Header.Set("X-Api-Key", "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("session: status = %d, want 200", rec.Code)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "mp_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("no mp_session cookie set")
	}
	if !cookie.HttpOnly {
		t.Fatalf("mp_session cookie is not HttpOnly")
	}
	if cookie.Value == "secret" {
		t.Fatalf("session cookie must not contain the raw api key")
	}

	req = httptest.NewRequest("GET", "/api/v1/jobs", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("cookie-authed request: status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest("GET", "/api/v1/jobs", nil)
	req.AddCookie(&http.Cookie{Name: "mp_session", Value: "bogus"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bogus cookie accepted: status = %d, want 401", rec.Code)
	}
}

func TestAuth_QueryParamNotAccepted(t *testing.T) {
	_, h := newAuthServer(t)
	rec := doJSON(t, h, "GET", "/api/v1/jobs?apikey=secret", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (apikey query must be rejected)", rec.Code)
	}
}

func TestAuth_WebhookSecret(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Server{Store: st, Hub: NewHub(), APIKey: "secret", WebhookSecret: "whsec"}
	h := s.Handler()

	req := httptest.NewRequest("POST", "/api/v1/webhooks/arr", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Secret", "whsec")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("valid webhook secret rejected: status = %d", rec.Code)
	}

	req = httptest.NewRequest("POST", "/api/v1/webhooks/arr", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Secret", "wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong webhook secret: status = %d, want 401", rec.Code)
	}
}

func TestHandleBrowse_JailRejectsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Server{Store: st, Hub: NewHub(), BrowseRoots: []string{root}}
	h := s.Handler()

	rec := doJSON(t, h, "GET", "/api/v1/browse?path="+outside, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("outside root: status = %d, want 403", rec.Code)
	}

	rec = doJSON(t, h, "GET", "/api/v1/browse?path="+root, "")
	if rec.Code != 200 {
		t.Fatalf("inside root: status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
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

func TestMCPSSE_NoWildcardCORS(t *testing.T) {
	s, h := newAuthServer(t)
	_ = s
	ts := httptest.NewServer(h)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "secret")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want unset", got)
	}
	cancel()
}

func TestAuth_WebhookSecretEnforcedWhenKeyless(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Server{Store: st, Hub: NewHub(), DefaultAutoScanInterval: 21600, WebhookSecret: "hook"}
	h := s.Handler()

	rec := doJSON(t, h, "POST", "/api/v1/webhooks/arr", `{}`)
	if rec.Code != 401 {
		t.Errorf("keyless webhook without secret = %d, want 401", rec.Code)
	}
	req := httptest.NewRequest("POST", "/api/v1/webhooks/arr", bytes.NewBufferString(`{}`))
	req.Header.Set("X-Webhook-Secret", "hook")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == 401 {
		t.Errorf("keyless webhook with secret = %d, want non-401", w.Code)
	}
}

func TestOversizedBodyRejected413(t *testing.T) {
	_, h := newTestServer(t)
	body := `{"file_ids":[` + strings.Repeat("1,", 6<<20) + `1]}`
	req := httptest.NewRequest("POST", "/api/v1/batch", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 413 {
		t.Errorf("%d byte body = %d, want 413", len(body), w.Code)
	}
}

func TestCSPHeaderPresent(t *testing.T) {
	_, h := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/v1/stats", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP header missing or weak: %q", csp)
	}
}

func TestAuth_RateLimitsFailures(t *testing.T) {
	_, h := newAuthServer(t)
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("GET", "/api/v1/stats", nil)
		req.RemoteAddr = "10.9.8.7:1234"
		req.Header.Set("X-Api-Key", "wrong")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	req := httptest.NewRequest("GET", "/api/v1/stats", nil)
	req.RemoteAddr = "10.9.8.7:1234"
	req.Header.Set("X-Api-Key", "secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 429 {
		t.Errorf("locked-out ip with valid key = %d, want 429", w.Code)
	}
	req = httptest.NewRequest("GET", "/api/v1/stats", nil)
	req.RemoteAddr = "10.1.1.1:9"
	req.Header.Set("X-Api-Key", "secret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == 429 || w.Code == 401 {
		t.Errorf("other ip with valid key = %d, want success", w.Code)
	}
}

func TestHandleBrowse_DefaultsToLibraryRoots(t *testing.T) {
	s, h := newTestServer(t)
	dir := t.TempDir()
	if err := s.Store.AddLibrary(&store.Library{Name: "L", Path: dir, Kind: "other", HardlinkPolicy: "skip"}); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, h, "GET", "/api/v1/browse?path="+dir, "")
	if rec.Code != 200 {
		t.Errorf("inside library root = %d, want 200", rec.Code)
	}
	rec = doJSON(t, h, "GET", "/api/v1/browse?path=/etc", "")
	if rec.Code != 403 {
		t.Errorf("outside library root = %d, want 403", rec.Code)
	}
}

func TestHandleBrowse_TraversalCannotEscape(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Server{Store: st, Hub: NewHub(), BrowseRoots: []string{root}}
	h := s.Handler()
	rec := doJSON(t, h, "GET", "/api/v1/browse?path="+url.QueryEscape(root+"/../../../etc"), "")
	if rec.Code != 403 {
		t.Errorf("traversal escape = %d, want 403", rec.Code)
	}
}

func TestHandleAddLibrary_JailedToBrowseRoots(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Server{Store: st, Hub: NewHub(), DefaultAutoScanInterval: 21600, BrowseRoots: []string{root}}
	h := s.Handler()
	rec := doJSON(t, h, "POST", "/api/v1/libraries", `{"path":"`+outside+`"}`)
	if rec.Code != 403 {
		t.Errorf("library outside roots = %d, want 403", rec.Code)
	}
	inside := filepath.Join(root, "media")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	rec = doJSON(t, h, "POST", "/api/v1/libraries", `{"path":"`+inside+`"}`)
	if rec.Code != 201 {
		t.Errorf("library inside roots = %d body=%s, want 201", rec.Code, rec.Body.String())
	}
}
