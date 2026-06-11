// Package api exposes the REST API, SSE event stream, and the embedded web UI.
package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/krabhi4/muxprune/internal/engine"
	"github.com/krabhi4/muxprune/internal/jobs"
	"github.com/krabhi4/muxprune/internal/probe"
	"github.com/krabhi4/muxprune/internal/scan"
	"github.com/krabhi4/muxprune/internal/store"
)

//go:embed web/static
var webFS embed.FS

type Server struct {
	Store   *store.Store
	Scanner *scan.Scanner
	Runner  *jobs.Runner
	Engine  *engine.Engine
	Hub     *Hub
	APIKey  string // empty disables auth

	scanMu  sync.Mutex
	scanSet map[int64]bool // libraries with a scan in flight
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"status":   "ok",
			"ffprobe":  s.Scanner.Prober.HasFFprobe(),
			"mkvmerge": s.Scanner.Prober.HasMkvmerge(),
		})
	})
	mux.HandleFunc("GET /api/v1/stats", s.handleStats)

	mux.HandleFunc("GET /api/v1/libraries", s.handleListLibraries)
	mux.HandleFunc("POST /api/v1/libraries", s.handleAddLibrary)
	mux.HandleFunc("PUT /api/v1/libraries/{id}", s.handleUpdateLibrary)
	mux.HandleFunc("DELETE /api/v1/libraries/{id}", s.handleDeleteLibrary)
	mux.HandleFunc("POST /api/v1/libraries/{id}/scan", s.handleScanLibrary)
	mux.HandleFunc("POST /api/v1/scan", s.handleScanAll)

	mux.HandleFunc("GET /api/v1/files", s.handleListFiles)
	mux.HandleFunc("GET /api/v1/files/{id}", s.handleGetFile)
	mux.HandleFunc("POST /api/v1/files/{id}/jobs", s.handleFileJobs)
	mux.HandleFunc("POST /api/v1/batch", s.handleBatch)

	mux.HandleFunc("GET /api/v1/jobs", s.handleListJobs)
	mux.HandleFunc("GET /api/v1/events", s.Hub.ServeSSE)
	mux.HandleFunc("POST /api/v1/webhooks/arr", s.handleArrWebhook)

	static, _ := fs.Sub(webFS, "web/static")
	mux.Handle("GET /", http.FileServerFS(static))

	return s.auth(mux)
}

// auth enforces the optional API key on /api/* (static UI stays open; it
// holds no data). Accepts X-Api-Key header or ?apikey= for webhook senders.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.APIKey != "" && strings.HasPrefix(r.URL.Path, "/api/") {
			key := r.Header.Get("X-Api-Key")
			if key == "" {
				key = r.URL.Query().Get("apikey")
			}
			if key != s.APIKey {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid api key"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
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

func (s *Server) handleListLibraries(w http.ResponseWriter, r *http.Request) {
	libs, err := s.Store.ListLibraries()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if libs == nil {
		libs = []store.Library{}
	}
	writeJSON(w, 200, libs)
}

func decodeLibrary(r *http.Request) (*store.Library, error) {
	var l store.Library
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
		return nil, err
	}
	l.Path = filepath.Clean(l.Path)
	if l.Name == "" {
		l.Name = filepath.Base(l.Path)
	}
	if !slices.Contains([]string{"tv", "movie", "other"}, l.Kind) {
		l.Kind = "other"
	}
	if !slices.Contains([]string{"skip", "proceed"}, l.HardlinkPolicy) {
		l.HardlinkPolicy = "skip"
	}
	info, err := os.Stat(l.Path)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("path is not an accessible directory: %s", l.Path)
	}
	return &l, nil
}

func (s *Server) handleAddLibrary(w http.ResponseWriter, r *http.Request) {
	l, err := decodeLibrary(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.Store.AddLibrary(l); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, l)
}

func (s *Server) handleUpdateLibrary(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	l, err := decodeLibrary(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	l.ID = id
	if err := s.Store.UpdateLibrary(l); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, l)
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
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- scanning ----

func (s *Server) startScan(lib *store.Library) bool {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if s.scanSet == nil {
		s.scanSet = map[int64]bool{}
	}
	if s.scanSet[lib.ID] {
		return false
	}
	s.scanSet[lib.ID] = true
	go func() {
		defer func() {
			s.scanMu.Lock()
			delete(s.scanSet, lib.ID)
			s.scanMu.Unlock()
		}()
		if err := s.Scanner.ScanLibrary(context.Background(), lib); err != nil {
			s.Hub.Notify("scan", map[string]any{"library_id": lib.ID, "phase": "error", "error": err.Error()})
		}
	}()
	return true
}

func (s *Server) handleScanLibrary(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	lib, err := s.Store.GetLibrary(id)
	if err != nil || lib == nil {
		writeErr(w, 404, errors.New("library not found"))
		return
	}
	if !s.startScan(lib) {
		writeJSON(w, 409, map[string]string{"error": "scan already running for this library"})
		return
	}
	writeJSON(w, 202, map[string]bool{"started": true})
}

func (s *Server) handleScanAll(w http.ResponseWriter, r *http.Request) {
	libs, err := s.Store.ListLibraries()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	started := 0
	for i := range libs {
		if s.startScan(&libs[i]) {
			started++
		}
	}
	writeJSON(w, 202, map[string]int{"started": started})
}

// ---- files ----

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	libID, _ := strconv.ParseInt(q.Get("library"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	files, total, err := s.Store.ListFiles(store.FileFilter{
		LibraryID: libID, Query: q.Get("q"), Kind: q.Get("kind"), Limit: limit, Offset: offset,
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
		writeErr(w, 404, errors.New("file not found"))
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
	d, _, err := s.loadDetail(id)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if d == nil {
		writeErr(w, 404, errors.New("file not found"))
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
			return nil, fmt.Errorf("sidecar %d does not belong to file %d", scID, d.ID)
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
		return nil, errors.New("nothing to do")
	}
	return created, nil
}

// ---- batch ----

type batchRequest struct {
	FileIDs            []int64  `json:"file_ids"`
	RemoveAudioLangs   []string `json:"remove_audio_langs"`    // langs to REMOVE
	KeepAudioLangs     []string `json:"keep_audio_langs"`      // alternative: langs to KEEP (others removed)
	RemoveSubLangs     []string `json:"remove_sub_langs"`      // embedded
	RemoveAllSubs      bool     `json:"remove_all_subs"`       // embedded
	DeleteSidecarLangs []string `json:"delete_sidecar_langs"`  // sidecar files
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
		writeErr(w, 400, errors.New("file_ids required"))
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
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.Store.ListJobs(r.URL.Query().Get("status"), limit)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if list == nil {
		list = []store.Job{}
	}
	writeJSON(w, 200, list)
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
			s.startScan(&libs[i])
			writeJSON(w, 202, map[string]any{"library": libs[i].Name, "scan": "started"})
			return
		}
	}
	writeJSON(w, 200, map[string]string{"status": "no matching library"})
}
