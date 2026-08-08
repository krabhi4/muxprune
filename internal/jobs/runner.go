// Package jobs runs the persistent job queue. Jobs survive restarts in
// SQLite; anything left 'running' by a crashed process is failed on startup.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/krabhi4/muxprune/internal/engine"
	"github.com/krabhi4/muxprune/internal/scan"
	"github.com/krabhi4/muxprune/internal/store"
)

type RemuxPayload struct {
	AudioIdx       []int `json:"audio_idx"`
	SubIdx         []int `json:"sub_idx"`
	AllowHardlink  bool  `json:"allow_hardlink"`
	AllowLastAudio bool  `json:"allow_last_audio"`
}

type SidecarPayload struct {
	SidecarID int64  `json:"sidecar_id"`
	Path      string `json:"path"`
}

type EditMetadataPayload struct {
	Edits []engine.MetadataEdit `json:"edits"`
}

type ReorderPayload struct {
	TrackOrder []int `json:"track_order"`
}

type MergePayload struct {
	ExternalFiles []string `json:"external_files"`
}

type ScanLibraryPayload struct {
	LibraryID int64 `json:"library_id"`
}

type Runner struct {
	Store         *store.Store
	Engine        *engine.Engine
	Scanner       *scan.Scanner
	Events        scan.Notifier
	ShutdownGrace time.Duration

	wake chan struct{}
	once sync.Once

	cmu       sync.Mutex
	cancels   map[int64]context.CancelFunc
	cancelled map[int64]bool
}

func (r *Runner) grace() time.Duration {
	if r.ShutdownGrace > 0 {
		return r.ShutdownGrace
	}
	return 30 * time.Second
}

func finalStatus(wasCancelled, killed bool, status, log string) (string, string) {
	if wasCancelled {
		return "cancelled", "cancelled by user"
	}
	if killed && status == "failed" {
		return "cancelled", "cancelled by shutdown"
	}
	return status, log
}

func (r *Runner) init() {
	r.once.Do(func() {
		r.wake = make(chan struct{}, 1)
		r.cancels = map[int64]context.CancelFunc{}
		r.cancelled = map[int64]bool{}
	})
}

func (r *Runner) Cancel(id int64) bool {
	r.init()
	r.cmu.Lock()
	defer r.cmu.Unlock()
	cancel, ok := r.cancels[id]
	if !ok {
		return false
	}
	r.cancelled[id] = true
	cancel()
	return true
}

// Wake nudges the workers; safe to call from any goroutine.
func (r *Runner) Wake() {
	r.init()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) notify(data any) {
	if r.Events != nil {
		r.Events.Notify("job", data)
	}
}

// Start launches n workers and blocks until ctx is cancelled.
func (r *Runner) Start(ctx context.Context, n int) {
	r.init()
	if n < 1 {
		n = 1
	}
	if failed, _ := r.Store.FailInterrupted(); failed > 0 {
		fmt.Printf("jobs: failed %d interrupted job(s) from previous run\n", failed)
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.worker(ctx)
		}()
	}
	wg.Wait()
}

func (r *Runner) worker(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second) // backstop poll; Wake() is the fast path
	defer ticker.Stop()
	for {
		for {
			if ctx.Err() != nil {
				return
			}
			job, err := r.Store.ClaimNextJob()
			if err != nil || job == nil {
				break
			}
			r.run(ctx, job)
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-ticker.C:
		}
	}
}

func (r *Runner) run(ctx context.Context, job *store.Job) {
	r.notify(map[string]any{"id": job.ID, "status": "running", "type": job.Type, "file_path": job.FilePath})
	jobCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopWatch := context.AfterFunc(ctx, func() {
		t := time.NewTimer(r.grace())
		defer t.Stop()
		select {
		case <-t.C:
			cancel()
		case <-jobCtx.Done():
		}
	})
	r.cmu.Lock()
	r.cancels[job.ID] = cancel
	r.cmu.Unlock()
	status, log, saved := r.execute(jobCtx, job)
	killed := jobCtx.Err() != nil
	stopWatch()
	r.cmu.Lock()
	wasCancelled := r.cancelled[job.ID]
	delete(r.cancels, job.ID)
	delete(r.cancelled, job.ID)
	r.cmu.Unlock()
	cancel()
	if status, log = finalStatus(wasCancelled, killed, status, log); status == "cancelled" {
		saved = 0
	}
	log = clampLog(log)
	if err := r.Store.FinishJob(job.ID, status, log, saved); err != nil {
		fmt.Printf("jobs: finish %d: %v\n", job.ID, err)
	}
	r.notify(map[string]any{"id": job.ID, "status": status, "type": job.Type,
		"file_path": job.FilePath, "bytes_saved": saved, "log": log})
}

// maxJobLog bounds what a job writes to the database and hands back over the
// API. Tool stderr is already tailed by the engine; this catches the rest.
const maxJobLog = 4000

func clampLog(s string) string {
	if len(s) <= maxJobLog {
		return s
	}
	return s[:maxJobLog] + " ...[truncated]"
}

func (r *Runner) execute(ctx context.Context, job *store.Job) (status, log string, saved int64) {
	switch job.Type {
	case "remux":
		var p RemuxPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return "failed", "bad payload: " + err.Error(), 0
		}
		res, err := r.Engine.RemoveTracks(ctx, job.FilePath,
			engine.RemovalSpec{AudioIdx: p.AudioIdx, SubIdx: p.SubIdx},
			engine.Options{AllowHardlink: p.AllowHardlink, AllowLastAudio: p.AllowLastAudio})
		if err != nil {
			if errors.Is(err, engine.ErrSkipped) {
				return "skipped", err.Error(), 0
			}
			return "failed", err.Error(), 0
		}
		r.refreshFile(ctx, job.MediaFileID())
		return "done", res.Tool + ": " + res.Command, res.BytesSaved

	case "delete_sidecar":
		var p SidecarPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return "failed", "bad payload: " + err.Error(), 0
		}
		res, err := r.Engine.DeleteSidecar(p.Path, false)
		if err != nil {
			return "failed", err.Error(), 0
		}
		warn := ""
		if p.SidecarID != 0 {
			if err := r.Store.DeleteSidecar(p.SidecarID); err != nil {
				fmt.Printf("jobs: delete sidecar row %d: %v\n", p.SidecarID, err)
				warn = " | warning: sidecar db row not removed: " + err.Error()
			}
		}
		r.refreshFile(ctx, job.MediaFileID())
		return "done", res.Command + warn, res.BytesSaved

	case "edit_metadata":
		var p EditMetadataPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return "failed", "bad payload: " + err.Error(), 0
		}
		res, err := r.Engine.EditMetadata(ctx, job.FilePath, p.Edits)
		if err != nil {
			return "failed", err.Error(), 0
		}
		r.refreshFile(ctx, job.MediaFileID())
		return "done", res.Tool + ": " + res.Command, res.BytesSaved

	case "reorder_tracks":
		var p ReorderPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return "failed", "bad payload: " + err.Error(), 0
		}
		res, err := r.Engine.ReorderTracks(ctx, job.FilePath, engine.ReorderSpec{TrackOrder: p.TrackOrder})
		if err != nil {
			return "failed", err.Error(), 0
		}
		r.refreshFile(ctx, job.MediaFileID())
		return "done", res.Tool + ": " + res.Command, res.BytesSaved

	case "merge_tracks":
		var p MergePayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return "failed", "bad payload: " + err.Error(), 0
		}
		res, err := r.Engine.MergeTracks(ctx, job.FilePath, engine.MergeSpec{ExternalFiles: p.ExternalFiles})
		if err != nil {
			return "failed", err.Error(), 0
		}
		r.refreshFile(ctx, job.MediaFileID())
		return "done", res.Tool + ": " + res.Command, res.BytesSaved

	case "scan_library":
		var p ScanLibraryPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return "failed", "bad payload: " + err.Error(), 0
		}
		lib, err := r.Store.GetLibrary(p.LibraryID)
		if err != nil {
			return "failed", "db error: " + err.Error(), 0
		}
		if lib == nil {
			return "failed", fmt.Sprintf("library %d not found", p.LibraryID), 0
		}
		serr := r.Scanner.ScanLibrary(ctx, lib)
		warn := ""
		if err := r.Store.MarkLibraryScanned(lib.ID, time.Now().Unix()); err != nil {
			fmt.Printf("jobs: mark library %d scanned: %v\n", lib.ID, err)
			warn = " | warning: last-scanned stamp failed: " + err.Error()
		}
		if serr != nil {
			return "failed", serr.Error() + warn, 0
		}
		return "done", fmt.Sprintf("scanned library: %s", lib.Name) + warn, 0

	case "scan_all":
		libs, err := r.Store.ListLibraries()
		if err != nil {
			return "failed", "db error: " + err.Error(), 0
		}
		var scanned, failedLibs, stampWarns []string
		for i := range libs {
			if ctx.Err() != nil {
				return "failed", ctx.Err().Error(), 0
			}
			serr := r.Scanner.ScanLibrary(ctx, &libs[i])
			if err := r.Store.MarkLibraryScanned(libs[i].ID, time.Now().Unix()); err != nil {
				fmt.Printf("jobs: mark library %d scanned: %v\n", libs[i].ID, err)
				stampWarns = append(stampWarns, libs[i].Name)
			}
			if serr != nil {
				failedLibs = append(failedLibs, fmt.Sprintf("%s: %v", libs[i].Name, serr))
				continue
			}
			scanned = append(scanned, libs[i].Name)
		}
		log := "scanned: " + strings.Join(scanned, ", ")
		if len(failedLibs) > 0 {
			log += " | failed: " + strings.Join(failedLibs, "; ")
		}
		if len(stampWarns) > 0 {
			log += " | warning: last-scanned stamp failed: " + strings.Join(stampWarns, ", ")
		}
		if len(scanned) == 0 && len(failedLibs) > 0 {
			return "failed", log, 0
		}
		return "done", log, 0

	default:
		return "failed", "unknown job type " + job.Type, 0
	}
}

// refreshFile re-probes a single file after a mutation so the UI reflects the
// new stream layout immediately.
func (r *Runner) refreshFile(ctx context.Context, fileID int64) {
	if fileID == 0 || r.Scanner == nil {
		return
	}
	f, err := r.Store.GetFile(fileID)
	if err != nil || f == nil {
		if err != nil {
			fmt.Printf("jobs: refresh file %d: %v\n", fileID, err)
		}
		return
	}
	lib, err := r.Store.GetLibrary(f.LibraryID)
	if err != nil || lib == nil {
		if err != nil {
			fmt.Printf("jobs: refresh library %d: %v\n", f.LibraryID, err)
		}
		return
	}
	if err := r.Scanner.ScanFile(ctx, lib, f.Path); err != nil {
		fmt.Printf("jobs: refresh %s: %v\n", f.Path, err)
	}
}
