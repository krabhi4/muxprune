package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Version is the build's version string, set from main so the MCP handshake
// reports what is actually running instead of a hand-maintained constant.
var Version = "dev"

// clientError carries a message authored in this package, safe to hand back to
// an API client. Errors that come from the database, the filesystem, or an
// external tool are not of this type and are replaced with a generic message
// before they leave the process.
type clientError struct{ msg string }

func (e *clientError) Error() string { return e.msg }

func cerr(format string, a ...any) error {
	return &clientError{msg: fmt.Sprintf(format, a...)}
}

func genericMsg(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "invalid request"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not found"
	case http.StatusConflict:
		return "conflict"
	default:
		return "internal error"
	}
}

// safeMessage decides what a client is allowed to see for err, and returns
// whether the full error still needs to be logged server-side.
func safeMessage(code int, err error) (msg string, logIt bool) {
	var ce *clientError
	if errors.As(err, &ce) {
		return ce.msg, false
	}
	return genericMsg(code), true
}

func logInternal(code int, err error) {
	fmt.Fprintf(os.Stderr, "api: %d: %v\n", code, err)
}

// ---- path allow-listing ----

// bootstrapBrowseRoots is where the folder picker may look when the operator
// has not set MUXPRUNE_BROWSE_ROOTS and no library exists yet. Without this the
// browse guard would either expose the whole filesystem or make the first
// library impossible to add.
var bootstrapBrowseRoots = []string{
	"/media", "/mnt", "/data", "/tv", "/movies", "/music", "/srv", "/storage", "/Volumes",
}

func defaultBrowseRoots() []string {
	var out []string
	for _, p := range bootstrapBrowseRoots {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, p)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		if fi, err := os.Stat(home); err == nil && fi.IsDir() {
			out = append(out, home)
		}
	}
	return out
}

// realPath resolves symlinks so that allow-list checks compare real locations
// rather than the names the caller happened to use. A path that cannot be
// resolved (it does not exist yet, or a component is unreadable) falls back to
// its lexical form, which still fails the allow-list check when it is outside.
func realPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

func resolveRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if r = strings.TrimSpace(r); r == "" {
			continue
		}
		if p := realPath(r); !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func pathAllowed(path string, roots []string) bool {
	for _, root := range roots {
		root = filepath.Clean(root)
		if path == root || strings.HasPrefix(path+string(filepath.Separator), root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// rootChildren lists the immediate child names of path that lead to an allowed
// root. It lets the folder picker walk down to a root through directories that
// are not themselves browsable, without revealing anything else they contain.
func rootChildren(path string, roots []string) []string {
	prefix := path
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	var out []string
	for _, root := range roots {
		if root == path || !strings.HasPrefix(root, prefix) {
			continue
		}
		rest := strings.TrimPrefix(root, prefix)
		if i := strings.Index(rest, string(filepath.Separator)); i >= 0 {
			rest = rest[:i]
		}
		if rest != "" && !slices.Contains(out, rest) {
			out = append(out, rest)
		}
	}
	slices.Sort(out)
	return out
}

// ---- transport security ----

// requestIsTLS reports whether the client's connection to the edge was
// encrypted. muxprune itself only speaks plain HTTP, so a reverse proxy's
// X-Forwarded-Proto is the only signal available in the usual deployment.
func requestIsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// ---- request rate limiting ----

type rateLimiter struct {
	limit  int
	window time.Duration

	mu   sync.Mutex
	hits map[string]*rateWindow
}

type rateWindow struct {
	count int
	start time.Time
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string]*rateWindow{}
	}
	rw, ok := l.hits[key]
	if !ok || time.Since(rw.start) > l.window {
		l.hits[key] = &rateWindow{count: 1, start: time.Now()}
		return true
	}
	rw.count++
	return rw.count <= l.limit
}

func (l *rateLimiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, rw := range l.hits {
		if time.Since(rw.start) > l.window {
			delete(l.hits, key)
		}
	}
}
