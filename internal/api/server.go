// Package api exposes the REST API, SSE event stream, and the embedded web UI.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/krabhi4/muxprune/internal/engine"
	"github.com/krabhi4/muxprune/internal/jobs"
	"github.com/krabhi4/muxprune/internal/probe"
	"github.com/krabhi4/muxprune/internal/scan"
	"github.com/krabhi4/muxprune/internal/store"
)

//go:embed web/static
var webFS embed.FS

type LibraryMonitor interface {
	Reconcile()
	Status(libraryID int64) string
}

type Server struct {
	Store   *store.Store
	Scanner *scan.Scanner
	Runner  *jobs.Runner
	Engine  *engine.Engine
	Hub     *Hub
	Monitor LibraryMonitor
	APIKey  string // empty disables auth

	WebhookSecret string
	BrowseRoots   []string

	DefaultAutoScanInterval int

	// SecureCookie forces the Secure flag on the session cookie even when the
	// request did not arrive over TLS. Needed behind proxies that terminate
	// TLS without setting X-Forwarded-Proto.
	SecureCookie bool

	mcpMu       sync.Mutex
	mcpSessions map[string]*mcpSession

	limiter authLimiter

	readLimiter  rateLimiter
	writeLimiter rateLimiter
	limiterOnce  sync.Once

	sessMu   sync.Mutex
	sessions map[string]*session
}

const sessionCookie = "mp_session"

// csrfHeader is required on cookie-authenticated state-changing requests. A
// browser cannot set a custom header on a cross-origin request without a
// preflight the server never approves, so its presence proves the request did
// not originate from an attacker's page.
const csrfHeader = "X-Requested-With"

const csrfValue = "muxprune"

// Request size ceilings. Every one of these is reachable by an authenticated
// caller and each item costs a query, a stat, or an argv entry.
const (
	maxBatchFileIDs   = 1000
	maxStreamIndexes  = 100
	maxMetadataEdits  = 100
	maxExternalFiles  = 100
	maxTrackOrder     = 1000
	maxSidecarDeletes = 1000
)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"status":      "ok",
			"ffprobe":     s.Scanner.Prober.HasFFprobe(),
			"mkvmerge":    s.Scanner.Prober.HasMkvmerge(),
			"mkvpropedit": s.Engine.HasMkvpropedit(),
		})
	})
	mux.HandleFunc("GET /api/v1/stats", s.handleStats)
	mux.HandleFunc("GET /api/v1/browse", s.handleBrowse)

	mux.HandleFunc("GET /api/v1/libraries", s.handleListLibraries)
	mux.HandleFunc("POST /api/v1/libraries", s.handleAddLibrary)
	mux.HandleFunc("PUT /api/v1/libraries/{id}", s.handleUpdateLibrary)
	mux.HandleFunc("DELETE /api/v1/libraries/{id}", s.handleDeleteLibrary)
	mux.HandleFunc("POST /api/v1/libraries/{id}/scan", s.handleScanLibrary)
	mux.HandleFunc("POST /api/v1/scan", s.handleScanAll)

	mux.HandleFunc("GET /api/v1/files", s.handleListFiles)
	mux.HandleFunc("GET /api/v1/files/{id}", s.handleGetFile)
	mux.HandleFunc("POST /api/v1/files/{id}/jobs", s.handleFileJobs)
	mux.HandleFunc("POST /api/v1/files/{id}/metadata", s.handleEditMetadata)
	mux.HandleFunc("POST /api/v1/files/{id}/reorder", s.handleReorderTracks)
	mux.HandleFunc("POST /api/v1/files/{id}/merge", s.handleMergeTracks)
	mux.HandleFunc("POST /api/v1/batch", s.handleBatch)

	mux.HandleFunc("GET /api/v1/jobs", s.handleListJobs)
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", s.handleCancelJob)
	mux.HandleFunc("POST /api/v1/jobs/{id}/retry", s.handleRetryJob)
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", s.handleDeleteJob)
	mux.HandleFunc("GET /api/v1/events", s.Hub.ServeSSE)
	mux.HandleFunc("POST /api/v1/webhooks/arr", s.handleArrWebhook)
	mux.HandleFunc("POST /api/v1/auth/session", s.handleAuthSession)

	mux.HandleFunc("GET /sse", s.handleMCPSSE)
	mux.HandleFunc("GET /api/v1/mcp/sse", s.handleMCPSSE)
	mux.HandleFunc("POST /api/v1/mcp/message", s.handleMCPMessage)

	static, _ := fs.Sub(webFS, "web/static")
	mux.Handle("GET /", http.FileServerFS(static))

	return s.wrap(s.auth(mux))
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func protectedPath(path string) bool {
	return strings.HasPrefix(path, "/api/") || path == "/sse"
}

// authMethod records how a request proved itself, because a cookie-borne
// identity is attacker-triggerable from another origin and an API key is not.
type authMethod int

const (
	authNone authMethod = iota
	authKey
	authCookie
)

func (s *Server) authenticate(r *http.Request) authMethod {
	if key := r.Header.Get("X-Api-Key"); key != "" {
		if constantTimeEqual(key, s.APIKey) {
			return authKey
		}
		return authNone
	}
	if c, err := r.Cookie(sessionCookie); err == nil && s.sessionValid(c.Value) {
		return authCookie
	}
	return authNone
}

const (
	sessionTTL     = 30 * 24 * time.Hour
	sessionIdleTTL = 24 * time.Hour
)

type session struct {
	expires  time.Time
	lastSeen time.Time
}

func (s *Server) newSession() string {
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	now := time.Now()
	s.sessMu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]*session{}
	}
	s.sessions[tok] = &session{expires: now.Add(sessionTTL), lastSeen: now}
	s.sessMu.Unlock()
	return tok
}

func (s *Server) sessionValid(tok string) bool {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	sess, ok := s.sessions[tok]
	if !ok {
		return false
	}
	now := time.Now()
	if now.After(sess.expires) || now.Sub(sess.lastSeen) > sessionIdleTTL {
		delete(s.sessions, tok)
		return false
	}
	sess.lastSeen = now
	return true
}

func (s *Server) sweepSessions() {
	now := time.Now()
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	for tok, sess := range s.sessions {
		if now.After(sess.expires) || now.Sub(sess.lastSeen) > sessionIdleTTL {
			delete(s.sessions, tok)
		}
	}
}

// StartJanitor drops expired sessions and stale rate-limiter entries that no
// request will ever touch again. Without it both maps only shrink when the
// same token or IP comes back.
func (s *Server) StartJanitor(ctx context.Context) {
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweepSessions()
				s.limiter.sweep()
				s.readLimiter.sweep()
				s.writeLimiter.sweep()
			}
		}
	}()
}

func mutatingMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

const (
	readRateLimit   = 600
	writeRateLimit  = 120
	rateLimitWindow = time.Minute
)

func (s *Server) initLimiters() {
	s.limiterOnce.Do(func() {
		s.readLimiter.limit, s.readLimiter.window = readRateLimit, rateLimitWindow
		s.writeLimiter.limit, s.writeLimiter.window = writeRateLimit, rateLimitWindow
	})
}

// rateAllowed throttles authenticated traffic. Failed logins are handled
// separately by authLimiter; this covers everything that gets past the door.
func (s *Server) rateAllowed(r *http.Request) bool {
	s.initLimiters()
	if mutatingMethod(r.Method) {
		return s.writeLimiter.allow(clientIP(r))
	}
	return s.readLimiter.allow(clientIP(r))
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !protectedPath(r.URL.Path) || r.URL.Path == "/api/v1/health" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/v1/webhooks/arr" && s.WebhookSecret != "" {
			if constantTimeEqual(r.Header.Get("X-Webhook-Secret"), s.WebhookSecret) {
				next.ServeHTTP(w, r)
				return
			}
			if s.APIKey == "" {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid webhook secret"})
				return
			}
		}
		if !s.rateAllowed(r) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded; slow down"})
			return
		}
		if s.APIKey == "" {
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r)
		if s.limiter.blocked(ip) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed auth attempts; try again later"})
			return
		}
		method := s.authenticate(r)
		if method == authNone {
			s.limiter.fail(ip)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid api key"})
			return
		}
		if method == authCookie && mutatingMethod(r.Method) && r.Header.Get(csrfHeader) != csrfValue {
			writeJSON(w, http.StatusForbidden,
				map[string]string{"error": "missing " + csrfHeader + " header on a cookie-authenticated request"})
			return
		}
		s.limiter.reset(ip)
		next.ServeHTTP(w, r)
	})
}

type authLimiter struct {
	mu    sync.Mutex
	fails map[string]*failWindow
}

type failWindow struct {
	count int
	start time.Time
}

const (
	authFailLimit  = 10
	authFailWindow = time.Minute
)

func (l *authLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	fw, ok := l.fails[ip]
	if !ok {
		return false
	}
	if time.Since(fw.start) > authFailWindow {
		delete(l.fails, ip)
		return false
	}
	return fw.count >= authFailLimit
}

func (l *authLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fails == nil {
		l.fails = map[string]*failWindow{}
	}
	fw, ok := l.fails[ip]
	if !ok || time.Since(fw.start) > authFailWindow {
		l.fails[ip] = &failWindow{count: 1, start: time.Now()}
		return
	}
	fw.count++
}

func (l *authLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

func (l *authLimiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, fw := range l.fails {
		if time.Since(fw.start) > authFailWindow {
			delete(l.fails, ip)
		}
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// HSTS is only meaningful on a connection the browser already trusts;
		// muxprune serves plain HTTP and expects a TLS-terminating proxy.
		if requestIsTLS(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.newSession(),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.SecureCookie || requestIsTLS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// writeErr never echoes an error it did not author. Raw database, filesystem,
// and external-tool messages leak internal paths and schema, so they are
// logged here and replaced with a generic message for the client.
func writeErr(w http.ResponseWriter, code int, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return
	}
	msg, logIt := safeMessage(code, err)
	if logIt {
		logInternal(code, err)
	}
	writeJSON(w, code, map[string]string{"error": msg})
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, cerr("invalid id")
	}
	return id, nil
}

// ---- stats / libraries ----

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.Stats()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	// Resolve symlinks before the allow-list check: a link inside a root can
	// point anywhere, and os.ReadDir follows it.
	path = realPath(filepath.Clean(path))
	roots := s.resolvedBrowseRoots()

	if !pathAllowed(path, roots) {
		// Still let the picker walk down to a root through its ancestors,
		// listing only the segments that lead there.
		if dirs := rootChildren(path, roots); len(dirs) > 0 {
			writeJSON(w, 200, map[string]any{
				"current":     path,
				"parent":      browseParent(path),
				"directories": dirs,
			})
			return
		}
		writeErr(w, 403, cerr("path is outside the allowed roots"))
		return
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		writeErr(w, 400, cerr("path is not an accessible directory"))
		return
	}

	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			dirs = append(dirs, entry.Name())
		}
	}
	slices.Sort(dirs)

	writeJSON(w, 200, map[string]any{
		"current":     path,
		"parent":      browseParent(path),
		"directories": dirs,
	})
}

func browseParent(path string) string {
	if parent := filepath.Dir(path); parent != path {
		return parent
	}
	return ""
}

// effectiveBrowseRoots is the allow-list for the folder picker. It never
// returns an empty slice: an empty allow-list used to mean "no restriction",
// which exposed the whole filesystem on a fresh install.
func (s *Server) effectiveBrowseRoots() []string {
	if len(s.BrowseRoots) > 0 {
		return s.BrowseRoots
	}
	var roots []string
	if libs, err := s.Store.ListLibraries(); err == nil {
		for _, l := range libs {
			roots = append(roots, l.Path)
		}
	}
	return append(roots, defaultBrowseRoots()...)
}

func (s *Server) resolvedBrowseRoots() []string {
	return resolveRoots(s.effectiveBrowseRoots())
}

type libraryView struct {
	store.Library
	WatchStatus string `json:"watch_status"`
}

func (s *Server) libraryView(l store.Library) libraryView {
	v := libraryView{Library: l}
	if s.Monitor != nil {
		v.WatchStatus = s.Monitor.Status(l.ID)
	}
	return v
}

func (s *Server) reconcileMonitor() {
	if s.Monitor != nil {
		s.Monitor.Reconcile()
	}
}

func (s *Server) handleListLibraries(w http.ResponseWriter, r *http.Request) {
	libs, err := s.Store.ListLibraries()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	views := make([]libraryView, 0, len(libs))
	for _, l := range libs {
		views = append(views, s.libraryView(l))
	}
	writeJSON(w, 200, views)
}

type libraryRequest struct {
	Name             string `json:"name"`
	Path             string `json:"path"`
	Kind             string `json:"kind"`
	HardlinkPolicy   string `json:"hardlink_policy"`
	AutoScanInterval *int   `json:"auto_scan_interval"`
	WatchEnabled     *bool  `json:"watch_enabled"`
}

func sanitizeInterval(secs int) int {
	if secs <= 0 {
		return 0
	}
	if secs < 60 {
		return 60
	}
	return secs
}

func decodeLibraryReq(r *http.Request) (*libraryRequest, error) {
	var req libraryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	// Store the resolved path: os.Stat and the scanner both follow symlinks,
	// so validating the link name instead of its target would let a library
	// escape the configured roots.
	req.Path = realPath(filepath.Clean(req.Path))
	if req.Name == "" {
		req.Name = filepath.Base(req.Path)
	}
	if !slices.Contains([]string{"tv", "movie", "other"}, req.Kind) {
		req.Kind = "other"
	}
	if !slices.Contains([]string{"skip", "proceed"}, req.HardlinkPolicy) {
		req.HardlinkPolicy = "skip"
	}
	info, err := os.Stat(req.Path)
	if err != nil || !info.IsDir() {
		return nil, cerr("path is not an accessible directory")
	}
	return &req, nil
}

// libraryPathAllowed jails library paths to the configured browse roots. An
// unset MUXPRUNE_BROWSE_ROOTS keeps the historical behaviour of allowing any
// path, because the operator has not expressed a boundary to enforce.
func (s *Server) libraryPathAllowed(path string) bool {
	if len(s.BrowseRoots) == 0 {
		return true
	}
	return pathAllowed(realPath(path), resolveRoots(s.BrowseRoots))
}

func (s *Server) handleAddLibrary(w http.ResponseWriter, r *http.Request) {
	req, err := decodeLibraryReq(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if !s.libraryPathAllowed(req.Path) {
		writeErr(w, 403, cerr("library path is outside the allowed roots"))
		return
	}
	interval := s.DefaultAutoScanInterval
	if req.AutoScanInterval != nil {
		interval = *req.AutoScanInterval
	}
	watch := true
	if req.WatchEnabled != nil {
		watch = *req.WatchEnabled
	}
	l := &store.Library{
		Name: req.Name, Path: req.Path, Kind: req.Kind, HardlinkPolicy: req.HardlinkPolicy,
		AutoScanInterval: sanitizeInterval(interval), WatchEnabled: watch,
	}
	if err := s.Store.AddLibrary(l); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.reconcileMonitor()
	writeJSON(w, 201, s.libraryView(*l))
}

func (s *Server) handleUpdateLibrary(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	existing, err := s.Store.GetLibrary(id)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if existing == nil {
		writeErr(w, 404, cerr("library not found"))
		return
	}
	req, err := decodeLibraryReq(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if !s.libraryPathAllowed(req.Path) {
		writeErr(w, 403, cerr("library path is outside the allowed roots"))
		return
	}
	l := &store.Library{
		ID: id, Name: req.Name, Path: req.Path, Kind: req.Kind, HardlinkPolicy: req.HardlinkPolicy,
		AutoScanInterval:   existing.AutoScanInterval,
		WatchEnabled:       existing.WatchEnabled,
		LastScanFinishedAt: existing.LastScanFinishedAt,
	}
	if req.AutoScanInterval != nil {
		l.AutoScanInterval = sanitizeInterval(*req.AutoScanInterval)
	}
	if req.WatchEnabled != nil {
		l.WatchEnabled = *req.WatchEnabled
	}
	if err := s.Store.UpdateLibrary(l); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.reconcileMonitor()
	writeJSON(w, 200, s.libraryView(*l))
}

func (s *Server) handleDeleteLibrary(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.Store.DeleteLibrary(id); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.reconcileMonitor()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- scanning ----

func (s *Server) handleScanLibrary(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	lib, err := s.Store.GetLibrary(id)
	if err != nil || lib == nil {
		writeErr(w, 404, cerr("library not found"))
		return
	}
	active, err := s.Store.IsScanActive(lib.ID)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if active {
		writeJSON(w, 409, map[string]string{"error": "scan already running or queued for this library"})
		return
	}
	_, err = s.Store.CreateJob("scan_library", 0, lib.Path, jobs.ScanLibraryPayload{LibraryID: lib.ID})
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	s.Runner.Wake()
	writeJSON(w, 202, map[string]bool{"started": true})
}

func (s *Server) handleScanAll(w http.ResponseWriter, r *http.Request) {
	active, err := s.Store.IsScanAllActive()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if active {
		writeJSON(w, 409, map[string]string{"error": "scan all already queued or running"})
		return
	}
	_, err = s.Store.CreateJob("scan_all", 0, "all libraries", map[string]any{})
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	s.Runner.Wake()
	writeJSON(w, 202, map[string]int{"started": 1})
}

// ---- files ----

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	libID, _ := strconv.ParseInt(q.Get("library"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	files, total, err := s.Store.ListFiles(store.FileFilter{
		LibraryID: libID,
		Query:     q.Get("q"),
		Kind:      q.Get("kind"),
		Limit:     limit,
		Offset:    offset,
		Sort:      q.Get("sort"),
		Order:     q.Get("order"),
		Hardlinks: q.Get("hardlinks"),
		Subs:      q.Get("subs"),
	})
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if files == nil {
		files = []store.MediaFile{}
	}
	writeJSON(w, 200, map[string]any{"total": total, "files": files})
}

type fileDetail struct {
	*store.MediaFile
	Streams []probe.Stream `json:"streams"`
	Format  string         `json:"format"`
}

func (s *Server) loadDetail(id int64) (*fileDetail, *probe.Result, error) {
	f, err := s.Store.GetFile(id)
	if err != nil {
		return nil, nil, err
	}
	if f == nil {
		return nil, nil, nil
	}
	var res probe.Result
	if f.ProbeJSON != "" {
		if err := json.Unmarshal([]byte(f.ProbeJSON), &res); err != nil {
			return nil, nil, err
		}
	}
	return &fileDetail{MediaFile: f, Streams: res.Streams, Format: res.Format}, &res, nil
}

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	d, _, err := s.loadDetail(id)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if d == nil {
		writeErr(w, 404, cerr("file not found"))
		return
	}
	writeJSON(w, 200, d)
}

// ---- job submission ----

type fileJobRequest struct {
	RemoveAudio    []int   `json:"remove_audio"` // ffprobe stream indexes
	RemoveSubs     []int   `json:"remove_subs"`
	DeleteSidecars []int64 `json:"delete_sidecars"` // sidecar row ids
	AllowHardlink  bool    `json:"allow_hardlink"`
	AllowLastAudio bool    `json:"allow_last_audio"`
	DryRun         bool    `json:"dry_run"`
}

func (s *Server) handleFileJobs(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var req fileJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if len(req.RemoveAudio) > maxStreamIndexes || len(req.RemoveSubs) > maxStreamIndexes {
		writeErr(w, 400, cerr("remove_audio/remove_subs exceed the limit of %d entries", maxStreamIndexes))
		return
	}
	if len(req.DeleteSidecars) > maxSidecarDeletes {
		writeErr(w, 400, cerr("delete_sidecars exceeds the limit of %d entries", maxSidecarDeletes))
		return
	}
	d, _, err := s.loadDetail(id)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if d == nil {
		writeErr(w, 404, cerr("file not found"))
		return
	}

	if req.DryRun {
		out := map[string]any{}
		if len(req.RemoveAudio) > 0 || len(req.RemoveSubs) > 0 {
			res, err := s.Engine.RemoveTracks(r.Context(), d.Path,
				engine.RemovalSpec{AudioIdx: req.RemoveAudio, SubIdx: req.RemoveSubs},
				engine.Options{AllowHardlink: req.AllowHardlink, AllowLastAudio: req.AllowLastAudio, DryRun: true})
			if err != nil {
				writeErr(w, 400, err)
				return
			}
			out["remux"] = res
		}
		var sidecars []string
		for _, scID := range req.DeleteSidecars {
			if sc, _ := s.Store.GetSidecar(scID); sc != nil && sc.FileID == id {
				sidecars = append(sidecars, sc.Path)
			}
		}
		out["delete_sidecars"] = sidecars
		writeJSON(w, 200, out)
		return
	}

	created, err := s.enqueueForFile(d, req)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.Runner.Wake()
	writeJSON(w, 201, map[string]any{"jobs": created})
}

func (s *Server) enqueueForFile(d *fileDetail, req fileJobRequest) ([]*store.Job, error) {
	var created []*store.Job
	if len(req.RemoveAudio) > 0 || len(req.RemoveSubs) > 0 {
		j, err := s.Store.CreateJob("remux", d.ID, d.Path, jobs.RemuxPayload{
			AudioIdx: req.RemoveAudio, SubIdx: req.RemoveSubs,
			AllowHardlink: req.AllowHardlink, AllowLastAudio: req.AllowLastAudio,
		})
		if err != nil {
			return nil, err
		}
		created = append(created, j)
	}
	for _, scID := range req.DeleteSidecars {
		sc, err := s.Store.GetSidecar(scID)
		if err != nil {
			return nil, err
		}
		if sc == nil || sc.FileID != d.ID {
			return nil, cerr("sidecar %d does not belong to file %d", scID, d.ID)
		}
		j, err := s.Store.CreateJob("delete_sidecar", d.ID, d.Path, jobs.SidecarPayload{
			SidecarID: sc.ID, Path: sc.Path,
		})
		if err != nil {
			return nil, err
		}
		created = append(created, j)
	}
	if len(created) == 0 {
		return nil, cerr("nothing to do")
	}
	return created, nil
}

// ---- metadata editing ----

type editMetadataRequest struct {
	Edits []engine.MetadataEdit `json:"edits"`
}

func (s *Server) handleEditMetadata(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var req editMetadataRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if len(req.Edits) == 0 {
		writeErr(w, 400, cerr("edits required"))
		return
	}
	if len(req.Edits) > maxMetadataEdits {
		writeErr(w, 400, cerr("edits exceeds the limit of %d", maxMetadataEdits))
		return
	}
	d, res, err := s.loadDetail(id)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if d == nil {
		writeErr(w, 404, cerr("file not found"))
		return
	}
	if res == nil || !res.IsMatroska() {
		writeErr(w, 400, cerr("file is not a Matroska container; metadata editing requires MKV"))
		return
	}
	j, err := s.Store.CreateJob("edit_metadata", d.ID, d.Path, jobs.EditMetadataPayload{
		Edits: req.Edits,
	})
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	s.Runner.Wake()
	writeJSON(w, 201, map[string]any{"job": j})
}

// ---- track reordering and merging ----

type reorderTracksRequest struct {
	TrackOrder []int `json:"track_order"`
}

func (s *Server) handleReorderTracks(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var req reorderTracksRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if len(req.TrackOrder) == 0 {
		writeErr(w, 400, cerr("track_order is required"))
		return
	}
	if len(req.TrackOrder) > maxTrackOrder {
		writeErr(w, 400, cerr("track_order exceeds the limit of %d entries", maxTrackOrder))
		return
	}
	d, res, err := s.loadDetail(id)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if d == nil {
		writeErr(w, 404, cerr("file not found"))
		return
	}
	if res == nil || !res.IsMatroska() {
		writeErr(w, 400, cerr("file is not a Matroska container; track reordering requires MKV"))
		return
	}
	byIdx := map[int]probe.Stream{}
	for _, s := range res.Streams {
		byIdx[s.Index] = s
	}
	expectedCount := 0
	for _, s := range res.Streams {
		if s.MkvID >= 0 {
			expectedCount++
		}
	}
	if len(req.TrackOrder) != expectedCount {
		writeErr(w, 400, cerr("track order must contain exactly %d tracks, got %d", expectedCount, len(req.TrackOrder)))
		return
	}
	seen := map[int]bool{}
	for _, idx := range req.TrackOrder {
		if seen[idx] {
			writeErr(w, 400, cerr("duplicate stream index %d in track order", idx))
			return
		}
		seen[idx] = true

		st, ok := byIdx[idx]
		if !ok {
			writeErr(w, 400, cerr("stream index %d not found", idx))
			return
		}
		if st.MkvID < 0 {
			writeErr(w, 400, cerr("stream index %d has no mkvmerge track ID", idx))
			return
		}
	}

	j, err := s.Store.CreateJob("reorder_tracks", d.ID, d.Path, jobs.ReorderPayload{
		TrackOrder: req.TrackOrder,
	})
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	s.Runner.Wake()
	writeJSON(w, 201, map[string]any{"job": j})
}

type mergeTracksRequest struct {
	ExternalFiles []string `json:"external_files"`
}

// validateExternalFiles defers to the engine so the REST API, the MCP tools,
// and the job runner all apply the same rules, including containment in the
// allowed roots. Engine messages here are authored, not wrapped OS errors.
func (s *Server) validateExternalFiles(target string, files []string) error {
	if s.Engine == nil {
		return nil
	}
	if err := s.Engine.ValidateExternalFiles(target, files); err != nil {
		return cerr("%s", err.Error())
	}
	return nil
}

func (s *Server) handleMergeTracks(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var req mergeTracksRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if len(req.ExternalFiles) == 0 {
		writeErr(w, 400, cerr("external_files is required"))
		return
	}
	if len(req.ExternalFiles) > maxExternalFiles {
		writeErr(w, 400, cerr("external_files exceeds the limit of %d", maxExternalFiles))
		return
	}
	d, res, err := s.loadDetail(id)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if d == nil {
		writeErr(w, 404, cerr("file not found"))
		return
	}
	if res == nil || !res.IsMatroska() {
		writeErr(w, 400, cerr("file is not a Matroska container; merging tracks requires MKV"))
		return
	}
	if err := s.validateExternalFiles(d.Path, req.ExternalFiles); err != nil {
		writeErr(w, 400, err)
		return
	}
	j, err := s.Store.CreateJob("merge_tracks", d.ID, d.Path, jobs.MergePayload{
		ExternalFiles: req.ExternalFiles,
	})
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	s.Runner.Wake()
	writeJSON(w, 201, map[string]any{"job": j})
}

// ---- batch ----

type batchRequest struct {
	FileIDs            []int64  `json:"file_ids"`
	RemoveAudioLangs   []string `json:"remove_audio_langs"`   // langs to REMOVE
	KeepAudioLangs     []string `json:"keep_audio_langs"`     // alternative: langs to KEEP (others removed)
	RemoveSubLangs     []string `json:"remove_sub_langs"`     // embedded
	RemoveAllSubs      bool     `json:"remove_all_subs"`      // embedded
	DeleteSidecarLangs []string `json:"delete_sidecar_langs"` // sidecar files
	DeleteAllSidecars  bool     `json:"delete_all_sidecars"`
	AllowHardlink      bool     `json:"allow_hardlink"`
	DryRun             bool     `json:"dry_run"`
}

type batchFileResult struct {
	FileID         int64    `json:"file_id"`
	Path           string   `json:"path"`
	RemoveAudio    []int    `json:"remove_audio"`
	RemoveSubs     []int    `json:"remove_subs"`
	DeleteSidecars []int64  `json:"delete_sidecars"`
	SidecarNames   []string `json:"sidecar_names,omitempty"`
	Notes          []string `json:"notes,omitempty"`
	Jobs           int      `json:"jobs"`
}

func matchLang(lang string, list []string) bool {
	if lang == "" {
		lang = "und"
	}
	for _, l := range list {
		if strings.EqualFold(l, lang) {
			return true
		}
	}
	return false
}

// resolveBatch turns language-based intent into concrete per-file stream
// indexes, applying the never-remove-all-audio guard per file.
func resolveBatch(d *fileDetail, res *probe.Result, req batchRequest) batchFileResult {
	out := batchFileResult{FileID: d.ID, Path: d.Path}
	audio := res.StreamsOfType("audio")
	for _, st := range audio {
		remove := false
		if len(req.KeepAudioLangs) > 0 {
			remove = !matchLang(st.Lang, req.KeepAudioLangs)
		} else if len(req.RemoveAudioLangs) > 0 {
			remove = matchLang(st.Lang, req.RemoveAudioLangs)
		}
		if remove {
			out.RemoveAudio = append(out.RemoveAudio, st.Index)
		}
	}
	if len(out.RemoveAudio) >= len(audio) && len(out.RemoveAudio) > 0 {
		out.RemoveAudio = nil
		out.Notes = append(out.Notes, "audio removal skipped: would remove every audio track")
	}
	for _, st := range res.StreamsOfType("subtitle") {
		if req.RemoveAllSubs || matchLang(st.Lang, req.RemoveSubLangs) {
			out.RemoveSubs = append(out.RemoveSubs, st.Index)
		}
	}
	for _, sc := range d.Sidecars {
		if req.DeleteAllSidecars || matchLang(sc.Lang, req.DeleteSidecarLangs) {
			out.DeleteSidecars = append(out.DeleteSidecars, sc.ID)
			out.SidecarNames = append(out.SidecarNames, sc.Name)
		}
	}
	return out
}

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	var req batchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if len(req.FileIDs) == 0 {
		writeErr(w, 400, cerr("file_ids required"))
		return
	}
	if len(req.FileIDs) > maxBatchFileIDs {
		writeErr(w, 400, cerr("file_ids exceeds the limit of %d", maxBatchFileIDs))
		return
	}
	var results []batchFileResult
	totalJobs := 0
	for _, id := range req.FileIDs {
		d, res, err := s.loadDetail(id)
		if err != nil || d == nil {
			results = append(results, batchFileResult{FileID: id, Notes: []string{"file not found"}})
			continue
		}
		plan := resolveBatch(d, res, req)
		if !req.DryRun {
			created, err := s.enqueueForFile(d, fileJobRequest{
				RemoveAudio: plan.RemoveAudio, RemoveSubs: plan.RemoveSubs,
				DeleteSidecars: plan.DeleteSidecars, AllowHardlink: req.AllowHardlink,
			})
			if err != nil && err.Error() != "nothing to do" {
				plan.Notes = append(plan.Notes, err.Error())
			}
			plan.Jobs = len(created)
			totalJobs += len(created)
		}
		results = append(results, plan)
	}
	if !req.DryRun {
		s.Runner.Wake()
	}
	writeJSON(w, 200, map[string]any{"dry_run": req.DryRun, "total_jobs": totalJobs, "results": results})
}

// ---- jobs / webhook ----

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	list, total, err := s.Store.ListJobs(q.Get("status"), limit, offset)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if list == nil {
		list = []store.Job{}
	}
	writeJSON(w, 200, map[string]any{"total": total, "jobs": list})
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if s.Runner.Cancel(id) {
		writeJSON(w, 200, map[string]string{"status": "cancelling"})
		return
	}
	if err := s.Store.CancelJob(id); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.Hub.Notify("job", map[string]any{"id": id, "status": "cancelled", "log": "cancelled by user"})
	writeJSON(w, 200, map[string]string{"status": "cancelled"})
}

func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	j, err := s.Store.RetryJob(id)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.Runner.Wake()
	writeJSON(w, 201, map[string]any{"job": j})
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.Store.DeleteJob(id); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.Hub.Notify("job", map[string]any{"id": id, "action": "deleted"})
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// handleArrWebhook accepts Sonarr/Radarr "On Import" webhooks and rescans the
// library containing the imported file.
func (s *Server) handleArrWebhook(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		EventType string `json:"eventType"`
		Series    struct {
			Path string `json:"path"`
		} `json:"series"`
		Movie struct {
			FolderPath string `json:"folderPath"`
		} `json:"movie"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeErr(w, 400, err)
		return
	}
	target := payload.Series.Path
	if target == "" {
		target = payload.Movie.FolderPath
	}
	if target == "" { // e.g. Test event
		writeJSON(w, 200, map[string]string{"status": "ignored"})
		return
	}
	libs, err := s.Store.ListLibraries()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	for i := range libs {
		if strings.HasPrefix(filepath.Clean(target)+string(filepath.Separator),
			filepath.Clean(libs[i].Path)+string(filepath.Separator)) {
			active, err := s.Store.IsScanActive(libs[i].ID)
			if err != nil {
				writeErr(w, 500, err)
				return
			}
			if !active {
				_, _ = s.Store.CreateJob("scan_library", 0, libs[i].Path, jobs.ScanLibraryPayload{LibraryID: libs[i].ID})
				s.Runner.Wake()
			}
			writeJSON(w, 202, map[string]any{"library": libs[i].Name, "scan": "started"})
			return
		}
	}
	writeJSON(w, 200, map[string]string{"status": "no matching library"})
}
