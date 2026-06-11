package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/krabhi4/muxprune/internal/probe"
	"github.com/krabhi4/muxprune/internal/store"
)

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true,
	".webm": true, ".ts": true, ".m2ts": true, ".wmv": true, ".flv": true,
}

func IsVideo(name string) bool { return videoExts[strings.ToLower(filepath.Ext(name))] }

// Notifier receives progress events; the API layer plugs its SSE hub in here.
type Notifier interface {
	Notify(event string, data any)
}

type Scanner struct {
	Store  *store.Store
	Prober *probe.Prober
	Events Notifier
}

type progress struct {
	LibraryID int64  `json:"library_id"`
	Phase     string `json:"phase"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
	Path      string `json:"path,omitempty"`
}

func (sc *Scanner) notify(p progress) {
	if sc.Events != nil {
		sc.Events.Notify("scan", p)
	}
}

// ScanLibrary walks the library root, probes new/changed videos, matches
// sidecar subtitles, and prunes records for files that disappeared.
// Probe results are cached by (path, size, mtime).
func (sc *Scanner) ScanLibrary(ctx context.Context, lib *store.Library) error {
	start := time.Now().Unix()

	type entry struct {
		path string
		info fs.FileInfo
	}
	var videos []entry
	dirFiles := map[string][]string{} // dir -> all file names (for sidecar matching)

	err := filepath.WalkDir(lib.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, do not abort the scan
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != lib.Path {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") {
			return nil
		}
		dirFiles[filepath.Dir(path)] = append(dirFiles[filepath.Dir(path)], name)
		if IsVideo(name) && !strings.Contains(name, ".muxprune.tmp") {
			info, err := d.Info()
			if err != nil {
				return nil
			}
			videos = append(videos, entry{path, info})
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(videos, func(i, j int) bool { return videos[i].path < videos[j].path })
	sc.notify(progress{LibraryID: lib.ID, Phase: "probing", Total: len(videos)})

	for i, v := range videos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := sc.scanOne(ctx, lib, v.path, v.info, dirFiles[filepath.Dir(v.path)]); err != nil {
			// Per-file failure (corrupt file, probe error) must not kill the scan.
			fmt.Fprintf(os.Stderr, "scan: %s: %v\n", v.path, err)
		}
		if (i+1)%25 == 0 || i+1 == len(videos) {
			sc.notify(progress{LibraryID: lib.ID, Phase: "probing", Done: i + 1, Total: len(videos), Path: v.path})
		}
	}

	pruned, err := sc.Store.PruneFiles(lib.ID, start)
	if err != nil {
		return err
	}
	sc.notify(progress{LibraryID: lib.ID, Phase: "done", Done: len(videos), Total: len(videos)})
	if pruned > 0 {
		fmt.Fprintf(os.Stderr, "scan: library %d: pruned %d stale records\n", lib.ID, pruned)
	}
	return nil
}

// ScanFile refreshes a single file's record (probe + sidecars), used after a
// job mutates it. A vanished file is pruned via the next full scan instead.
func (sc *Scanner) ScanFile(ctx context.Context, lib *store.Library, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	var siblings []string
	for _, ent := range entries {
		if !ent.IsDir() {
			siblings = append(siblings, ent.Name())
		}
	}
	return sc.scanOne(ctx, lib, path, info, siblings)
}

func (sc *Scanner) scanOne(ctx context.Context, lib *store.Library, path string, info fs.FileInfo, siblings []string) error {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	dir := filepath.Dir(path)

	var sidecars []store.Sidecar
	var scNames []string
	for _, name := range siblings {
		if m, ok := MatchSidecar(name, base); ok {
			scInfo, err := os.Stat(filepath.Join(dir, name))
			var size int64
			if err == nil {
				size = scInfo.Size()
			}
			sidecars = append(sidecars, store.Sidecar{
				Path: filepath.Join(dir, name), Name: m.Name, Lang: m.Lang,
				HI: m.HI, Forced: m.Forced, Ext: m.Ext, Size: size,
			})
			scNames = append(scNames, sidecarLabel(m))
		}
	}
	sort.Strings(scNames)
	scSummary := strings.Join(scNames, " ")
	nlink := Nlink(info)

	// Probe cache check: same size+mtime with a stored probe means only the
	// cheap bookkeeping (nlink, sidecars) needs refreshing.
	id, oldSize, oldMtime, hasProbe, err := sc.Store.GetFileByPathMeta(path)
	if err != nil {
		return err
	}
	if id != 0 && hasProbe && oldSize == info.Size() && oldMtime == info.ModTime().Unix() {
		if err := sc.Store.TouchFile(id, nlink, scSummary); err != nil {
			return err
		}
		return sc.Store.ReplaceSidecars(id, sidecars)
	}

	res, err := sc.Prober.Probe(ctx, path)
	if err != nil {
		return err
	}
	probeJSON, err := json.Marshal(res)
	if err != nil {
		return err
	}
	parsed := ParsePath(lib.Path, path)
	mf := &store.MediaFile{
		ID: id, LibraryID: lib.ID, Path: path,
		Size: info.Size(), Mtime: info.ModTime().Unix(), Nlink: nlink,
		Kind: parsed.Kind, Series: parsed.Series, Season: parsed.Season,
		Episode: parsed.Episode, Title: parsed.Title,
		VideoCodec:     summarizeVideo(res),
		AudioSummary:   summarizeStreams(res, "audio"),
		SubSummary:     summarizeStreams(res, "subtitle"),
		SidecarSummary: scSummary,
		ProbeJSON:      string(probeJSON),
	}
	if err := sc.Store.UpsertMediaFile(mf); err != nil {
		return err
	}
	return sc.Store.ReplaceSidecars(mf.ID, sidecars)
}

func sidecarLabel(m Sidecar) string {
	l := m.Lang
	if l == "" {
		l = "und"
	}
	if m.HI {
		l += ".hi"
	}
	if m.Forced {
		l += ".forced"
	}
	return l + "." + m.Ext
}

func summarizeVideo(res *probe.Result) string {
	for _, s := range res.Streams {
		if s.Type == "video" {
			return s.Codec
		}
	}
	return ""
}

// summarizeStreams renders "eng jpn und" style summaries for list views.
func summarizeStreams(res *probe.Result, typ string) string {
	var parts []string
	for _, s := range res.Streams {
		if s.Type != typ {
			continue
		}
		l := s.Lang
		if l == "" {
			l = "und"
		}
		parts = append(parts, l)
	}
	return strings.Join(parts, " ")
}
