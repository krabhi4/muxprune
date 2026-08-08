package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krabhi4/muxprune/internal/store"
)

func newRootedServer(t *testing.T, roots ...string) (*Server, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Server{Store: st, Hub: NewHub(), DefaultAutoScanInterval: 21600, BrowseRoots: roots}
	return s, s.Handler()
}

// ---- #1 Secure cookie ----

func sessionCookieFrom(t *testing.T, h http.Handler, headers map[string]string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/auth/session", nil)
	req.Header.Set("X-Api-Key", "secret")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no session cookie issued (status %d)", rec.Code)
	return nil
}

func TestSessionCookie_SecureFollowsTransport(t *testing.T) {
	_, h := newAuthServer(t)

	if c := sessionCookieFrom(t, h, nil); c.Secure {
		t.Error("plain HTTP: Secure = true, want false (would break loopback deployments)")
	}
	if c := sessionCookieFrom(t, h, map[string]string{"X-Forwarded-Proto": "https"}); !c.Secure {
		t.Error("proxied HTTPS: Secure = false, want true")
	}
}

func TestSessionCookie_SecureForcedByConfig(t *testing.T) {
	s, h := newAuthServer(t)
	s.SecureCookie = true
	if c := sessionCookieFrom(t, h, nil); !c.Secure {
		t.Error("SecureCookie=true: Secure = false, want true")
	}
}

// ---- #11, #21, #32, #35 response headers ----

func TestSecurityHeaders(t *testing.T) {
	_, h := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/v1/stats", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP missing frame-ancestors: %q", csp)
	}
	if got := w.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS = %q over plain HTTP, want unset (browsers ignore it and it misleads operators)", got)
	}
}

func TestSecurityHeaders_HSTSOnlyBehindTLS(t *testing.T) {
	_, h := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/v1/stats", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Header().Get("Strict-Transport-Security"); !strings.Contains(got, "max-age=") {
		t.Errorf("HSTS = %q behind TLS, want a max-age directive", got)
	}
}

func TestSecurityHeaders_OnSSE(t *testing.T) {
	_, h := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/v1/events", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	cancel()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("SSE X-Content-Type-Options = %q, want nosniff", got)
	}
}

// ---- #15 CSRF ----

func TestCSRF_CookieAuthNeedsHeaderToMutate(t *testing.T) {
	_, h := newAuthServer(t)
	cookie := sessionCookieFrom(t, h, nil)
	dir := t.TempDir()
	body := `{"path":"` + dir + `","kind":"tv"}`

	req := httptest.NewRequest("POST", "/api/v1/libraries", bytes.NewBufferString(body))
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cookie POST without %s = %d, want 403", csrfHeader, w.Code)
	}

	req = httptest.NewRequest("POST", "/api/v1/libraries", bytes.NewBufferString(body))
	req.AddCookie(cookie)
	req.Header.Set(csrfHeader, csrfValue)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("cookie POST with %s = %d body=%s, want 201", csrfHeader, w.Code, w.Body.String())
	}
}

func TestCSRF_ReadsAndAPIKeyUnaffected(t *testing.T) {
	_, h := newAuthServer(t)
	cookie := sessionCookieFrom(t, h, nil)

	req := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("cookie GET = %d, want 200 (reads are not state-changing)", w.Code)
	}

	dir := t.TempDir()
	req = httptest.NewRequest("POST", "/api/v1/libraries", bytes.NewBufferString(`{"path":"`+dir+`"}`))
	req.Header.Set("X-Api-Key", "secret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Errorf("api-key POST without %s = %d, want 201 (not forgeable cross-site)", csrfHeader, w.Code)
	}
}

// ---- #3, #4 symlink traversal ----

func TestBrowse_SymlinkOutOfRootRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, h := newRootedServer(t, root)

	rec := doJSON(t, h, "GET", "/api/v1/browse?path="+link, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("symlink escape = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
}

func TestAddLibrary_SymlinkOutOfRootRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, h := newRootedServer(t, root)

	rec := doJSON(t, h, "POST", "/api/v1/libraries", `{"path":"`+link+`"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("library via symlink = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
}

func TestAddLibrary_StoresResolvedPath(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	s, h := newTestServer(t)

	rec := doJSON(t, h, "POST", "/api/v1/libraries", `{"path":"`+link+`"}`)
	if rec.Code != 201 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	libs, err := s.Store.ListLibraries()
	if err != nil || len(libs) != 1 {
		t.Fatalf("libraries: %v %d", err, len(libs))
	}
	if want := realPath(real); libs[0].Path != want {
		t.Errorf("stored path = %q, want resolved %q", libs[0].Path, want)
	}
}

// ---- #36 browse fails closed ----

func TestBrowse_FreshInstallDoesNotExposeSystemDirs(t *testing.T) {
	_, h := newTestServer(t) // no BrowseRoots, no libraries
	for _, p := range []string{"/etc", "/", "/usr"} {
		rec := doJSON(t, h, "GET", "/api/v1/browse?path="+p, "")
		if rec.Code == 200 && strings.Contains(rec.Body.String(), `"directories":[`) &&
			!strings.Contains(rec.Body.String(), `"directories":[]`) {
			// "/" may legitimately list the segments leading to allowed roots,
			// but must never return the real contents of a system directory.
			if p != "/" {
				t.Errorf("browse %s leaked a listing: %s", p, rec.Body.String())
			}
		}
		if p == "/etc" && rec.Code != http.StatusForbidden {
			t.Errorf("browse /etc = %d, want 403", rec.Code)
		}
	}
}

func TestBrowse_AncestorsListOnlyPathsToRoots(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "allowed", "deep")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "secret"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, h := newRootedServer(t, root)

	rec := doJSON(t, h, "GET", "/api/v1/browse?path="+base, "")
	if rec.Code != 200 {
		t.Fatalf("ancestor of root = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"allowed"`) {
		t.Errorf("ancestor listing missing the path to the root: %s", body)
	}
	if strings.Contains(body, `"secret"`) {
		t.Errorf("ancestor listing leaked a sibling directory: %s", body)
	}
}

// ---- #10 error sanitization ----

func TestWriteErr_DoesNotEchoInternalErrors(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, 500, errors.New("no such table: media_files; /var/lib/muxprune.db"))
	if body := rec.Body.String(); strings.Contains(body, "media_files") || strings.Contains(body, "/var/lib") {
		t.Errorf("internal error leaked to client: %s", body)
	}
}

func TestWriteErr_KeepsAuthoredMessages(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, 404, cerr("file not found"))
	if !strings.Contains(rec.Body.String(), "file not found") {
		t.Errorf("authored message dropped: %s", rec.Body.String())
	}
}

func TestBrowse_ErrorDoesNotRevealFilesystem(t *testing.T) {
	root := t.TempDir()
	_, h := newRootedServer(t, root)
	missing := filepath.Join(root, "does-not-exist")
	rec := doJSON(t, h, "GET", "/api/v1/browse?path="+missing, "")
	if strings.Contains(rec.Body.String(), "no such file") {
		t.Errorf("raw os error echoed: %s", rec.Body.String())
	}
}

// ---- #30 id validation ----

func TestPathID_RejectsNonPositive(t *testing.T) {
	_, h := newTestServer(t)
	for _, id := range []string{"0", "-1", "abc"} {
		rec := doJSON(t, h, "GET", "/api/v1/files/"+id, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET /files/%s = %d, want 400", id, rec.Code)
		}
	}
}

// ---- #17, #18, #27 array ceilings ----

func TestArrayLimits(t *testing.T) {
	s, h := newTestServer(t)
	dir := t.TempDir()
	lib := &store.Library{Name: "L", Path: dir, Kind: "other", HardlinkPolicy: "skip"}
	if err := s.Store.AddLibrary(lib); err != nil {
		t.Fatal(err)
	}
	f := &store.MediaFile{LibraryID: lib.ID, Path: filepath.Join(dir, "a.mkv"), Size: 1}
	if err := s.Store.UpsertMediaFile(f); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ name, path, body string }{
		{"batch file_ids", "/api/v1/batch", `{"file_ids":[` + repeatCSV("1", maxBatchFileIDs+1) + `]}`},
		{"merge external_files", "/api/v1/files/1/merge", `{"external_files":[` + repeatCSV(`"/x"`, maxExternalFiles+1) + `]}`},
		{"metadata edits", "/api/v1/files/1/metadata", `{"edits":[` + repeatCSV(`{"track_index":0}`, maxMetadataEdits+1) + `]}`},
		{"remove_audio", "/api/v1/files/1/jobs", `{"remove_audio":[` + repeatCSV("1", maxStreamIndexes+1) + `]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doJSON(t, h, "POST", c.path, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("oversized %s = %d body=%s, want 400", c.name, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "limit") {
				t.Errorf("%s: error should name the limit, got %s", c.name, rec.Body.String())
			}
		})
	}
}

func repeatCSV(item string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = item
	}
	return strings.Join(parts, ",")
}

// ---- #13 rate limiting ----

func TestRateLimit_ThrottlesAuthenticatedWrites(t *testing.T) {
	_, h := newTestServer(t)
	var last int
	for i := 0; i < writeRateLimit+5; i++ {
		req := httptest.NewRequest("POST", "/api/v1/batch", strings.NewReader(`{}`))
		req.RemoteAddr = "203.0.113.9:5000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		last = w.Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("after %d writes = %d, want 429", writeRateLimit+5, last)
	}

	req := httptest.NewRequest("POST", "/api/v1/batch", strings.NewReader(`{}`))
	req.RemoteAddr = "203.0.113.10:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusTooManyRequests {
		t.Error("a different IP was throttled; the limit must be per-client")
	}
}

// ---- #6, #34 session lifetime ----

func TestSession_ExpiresWhenIdle(t *testing.T) {
	s, _ := newAuthServer(t)
	tok := s.newSession()

	s.sessMu.Lock()
	s.sessions[tok].lastSeen = time.Now().Add(-sessionIdleTTL - time.Minute)
	s.sessMu.Unlock()

	if s.sessionValid(tok) {
		t.Error("idle session still valid past the inactivity window")
	}
	s.sessMu.Lock()
	_, present := s.sessions[tok]
	s.sessMu.Unlock()
	if present {
		t.Error("rejected session was left in the map")
	}
}

func TestSession_SweepDropsUntouchedSessions(t *testing.T) {
	s, _ := newAuthServer(t)
	live := s.newSession()
	stale := s.newSession()

	s.sessMu.Lock()
	s.sessions[stale].expires = time.Now().Add(-time.Hour)
	s.sessMu.Unlock()

	s.sweepSessions()

	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	if _, ok := s.sessions[stale]; ok {
		t.Error("expired session survived the sweep")
	}
	if _, ok := s.sessions[live]; !ok {
		t.Error("sweep dropped a live session")
	}
}

// ---- #28 auth limiter cleanup ----

func TestAuthLimiter_SweepDropsExpiredWindows(t *testing.T) {
	var l authLimiter
	l.fail("198.51.100.1")
	l.mu.Lock()
	l.fails["198.51.100.1"].start = time.Now().Add(-authFailWindow - time.Minute)
	l.mu.Unlock()

	l.sweep()

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.fails) != 0 {
		t.Errorf("sweep left %d stale entries", len(l.fails))
	}
}
