// muxprune removes unwanted audio tracks and subtitles (embedded or sidecar)
// from *arr-managed media libraries, losslessly.
//
//	muxprune serve            run the web UI + API (default)
//	muxprune inspect <file>   print stream inventory
//	muxprune strip <file>     remove tracks from one file (CLI mode)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/krabhi4/muxprune/internal/api"
	"github.com/krabhi4/muxprune/internal/engine"
	"github.com/krabhi4/muxprune/internal/jobs"
	"github.com/krabhi4/muxprune/internal/probe"
	"github.com/krabhi4/muxprune/internal/scan"
	"github.com/krabhi4/muxprune/internal/store"
	"github.com/krabhi4/muxprune/internal/watch"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "muxprune: invalid %s=%q, using default %d\n", key, v, def)
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "muxprune: invalid %s=%q, using default %g\n", key, v, def)
		return def
	}
	return f
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func startMonitor(ctx context.Context, st *store.Store, runner *jobs.Runner, hub *api.Hub, wg *sync.WaitGroup) *watch.Monitor {
	var enqMu sync.Mutex
	enqueue := func(libID int64) {
		enqMu.Lock()
		defer enqMu.Unlock()
		if active, _ := st.IsScanActive(libID); active {
			return
		}
		lib, err := st.GetLibrary(libID)
		if err != nil || lib == nil {
			return
		}
		if _, err := st.CreateJob("scan_library", 0, lib.Path, jobs.ScanLibraryPayload{LibraryID: libID}); err == nil {
			runner.Wake()
		}
	}
	m := watch.New(st, enqueue, watch.Config{
		WatchDisabled: !envBool("MUXPRUNE_WATCH", true),
		Events:        hub,
	})
	wg.Add(1)
	go func() { defer wg.Done(); m.Start(ctx) }()
	return m
}

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "inspect":
		err = runInspect(args)
	case "strip":
		err = runStrip(args)
	case "mcp":
		err = runMCP(args)
	case "version":
		fmt.Println("muxprune", version)
	default:
		err = fmt.Errorf("unknown command %q (expected serve, inspect, strip, mcp, version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "muxprune:", err)
		os.Exit(1)
	}
}

var version = "dev"

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", envInt("MUXPRUNE_PORT", 8484), "listen port")
	configDir := fs.String("config", env("MUXPRUNE_CONFIG", "./data"), "config/state directory")
	workers := fs.Int("workers", envInt("MUXPRUNE_WORKERS", 1), "concurrent job workers")
	recycleDays := fs.Int("recycle-days", envInt("MUXPRUNE_RECYCLE_DAYS", 7),
		"keep deleted sidecars this many days (0 = delete permanently)")
	apiKey := fs.String("api-key", env("MUXPRUNE_API_KEY", ""), "require this key on /api/* (empty = open)")
	fs.Parse(args)

	if err := os.MkdirAll(*configDir, 0o755); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(*configDir, "muxprune.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	prober := &probe.Prober{Timeout: time.Duration(envInt("MUXPRUNE_PROBE_TIMEOUT", 60)) * time.Second}
	if !prober.HasFFprobe() {
		return fmt.Errorf("ffprobe not found in PATH; install ffmpeg")
	}
	hub := api.NewHub()
	eng := &engine.Engine{Prober: prober}
	if *recycleDays > 0 {
		eng.RecycleDir = filepath.Join(*configDir, "recycle")
	}
	scanner := &scan.Scanner{Store: st, Prober: prober, Events: hub,
		MaxPruneRatio: envFloat("MUXPRUNE_PRUNE_MAX_RATIO", 0.2)}
	runner := &jobs.Runner{Store: st, Engine: eng, Scanner: scanner, Events: hub}
	srv := &api.Server{Store: st, Scanner: scanner, Runner: runner, Engine: eng, Hub: hub, APIKey: *apiKey,
		WebhookSecret:           env("MUXPRUNE_WEBHOOK_SECRET", ""),
		BrowseRoots:             browseRoots(),
		DefaultAutoScanInterval: envInt("MUXPRUNE_AUTOSCAN_DEFAULT", 21600)}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); runner.Start(ctx, *workers) }()
	srv.Monitor = startMonitor(ctx, st, runner, hub, &wg)
	if *recycleDays > 0 {
		wg.Add(1)
		go func() { defer wg.Done(); purgeLoop(ctx, eng, time.Duration(*recycleDays)*24*time.Hour) }()
	}

	bind := env("MUXPRUNE_BIND", "")
	if bind == "" {
		if *apiKey == "" {
			bind = "127.0.0.1"
			fmt.Println("muxprune: WARNING api key is empty; API is UNAUTHENTICATED and bound to loopback only (set MUXPRUNE_API_KEY, or MUXPRUNE_BIND to override)")
		} else {
			bind = "0.0.0.0"
		}
	}

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", bind, *port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()

	fmt.Printf("muxprune %s listening on %s (config: %s, mkvmerge: %v)\n",
		version, httpSrv.Addr, *configDir, prober.HasMkvmerge())
	srvErr := httpSrv.ListenAndServe()
	stop()
	wg.Wait()
	if srvErr != nil && srvErr != http.ErrServerClosed {
		return srvErr
	}
	return nil
}

func browseRoots() []string {
	raw := env("MUXPRUNE_BROWSE_ROOTS", "")
	if raw == "" {
		return nil
	}
	var roots []string
	for _, p := range strings.Split(raw, ":") {
		if p = strings.TrimSpace(p); p != "" {
			roots = append(roots, p)
		}
	}
	return roots
}

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	configDir := fs.String("config", env("MUXPRUNE_CONFIG", "./data"), "config/state directory")
	workers := fs.Int("workers", envInt("MUXPRUNE_WORKERS", 1), "concurrent job workers")
	recycleDays := fs.Int("recycle-days", envInt("MUXPRUNE_RECYCLE_DAYS", 7),
		"keep deleted sidecars this many days (0 = delete permanently)")
	fs.Parse(args)

	if err := os.MkdirAll(*configDir, 0o755); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(*configDir, "muxprune.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	prober := &probe.Prober{Timeout: time.Duration(envInt("MUXPRUNE_PROBE_TIMEOUT", 60)) * time.Second}
	if !prober.HasFFprobe() {
		return fmt.Errorf("ffprobe not found in PATH; install ffmpeg")
	}
	hub := api.NewHub()
	eng := &engine.Engine{Prober: prober}
	if *recycleDays > 0 {
		eng.RecycleDir = filepath.Join(*configDir, "recycle")
	}
	scanner := &scan.Scanner{Store: st, Prober: prober, Events: hub,
		MaxPruneRatio: envFloat("MUXPRUNE_PRUNE_MAX_RATIO", 0.2)}
	runner := &jobs.Runner{Store: st, Engine: eng, Scanner: scanner, Events: hub}
	srv := &api.Server{Store: st, Scanner: scanner, Runner: runner, Engine: eng, Hub: hub,
		WebhookSecret:           env("MUXPRUNE_WEBHOOK_SECRET", ""),
		BrowseRoots:             browseRoots(),
		DefaultAutoScanInterval: envInt("MUXPRUNE_AUTOSCAN_DEFAULT", 21600)}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); runner.Start(ctx, *workers) }()
	srv.Monitor = startMonitor(ctx, st, runner, hub, &wg)
	if *recycleDays > 0 {
		wg.Add(1)
		go func() { defer wg.Done(); purgeLoop(ctx, eng, time.Duration(*recycleDays)*24*time.Hour) }()
	}

	err = srv.ServeMCP(ctx)
	stop()
	wg.Wait()
	return err
}

func purgeLoop(ctx context.Context, eng *engine.Engine, maxAge time.Duration) {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()
	for {
		if n, err := eng.PurgeRecycle(maxAge); err == nil && n > 0 {
			fmt.Printf("recycle: purged %d file(s) older than %s\n", n, maxAge)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "raw JSON output")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: muxprune inspect [-json] <file>")
	}
	prober := &probe.Prober{}
	res, err := prober.Probe(context.Background(), fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	fmt.Printf("%s\n  format: %s  duration: %.1fs  size: %s\n", res.Path, res.Format, res.Duration, human(res.Size))
	for _, s := range res.Streams {
		extra := ""
		if s.Type == "audio" {
			extra = fmt.Sprintf(" %dch %s", s.Channels, s.ChannelLayout)
		}
		flags := ""
		if s.Default {
			flags += " default"
		}
		if s.Forced {
			flags += " forced"
		}
		lang := s.Lang
		if lang == "" {
			lang = "und"
		}
		fmt.Printf("  #%-2d %-10s %-12s %-6s%s%s  %s\n", s.Index, s.Type, s.Codec, lang, extra, flags, s.Title)
	}
	return nil
}

func runStrip(args []string) error {
	fs := flag.NewFlagSet("strip", flag.ExitOnError)
	audio := fs.String("audio", "", "comma-separated ffprobe stream indexes of audio tracks to remove")
	subs := fs.String("subs", "", "comma-separated ffprobe stream indexes of subtitle tracks to remove")
	allSubs := fs.Bool("all-subs", false, "remove every embedded subtitle track")
	dryRun := fs.Bool("dry-run", false, "show the plan without touching the file")
	allowHardlink := fs.Bool("allow-hardlink", false, "proceed even if the file has hardlinks")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: muxprune strip [flags] <file> (see muxprune inspect for indexes)")
	}
	path := fs.Arg(0)
	prober := &probe.Prober{}
	eng := &engine.Engine{Prober: prober}

	spec := engine.RemovalSpec{}
	var err error
	if spec.AudioIdx, err = parseIdx(*audio); err != nil {
		return err
	}
	if spec.SubIdx, err = parseIdx(*subs); err != nil {
		return err
	}
	if *allSubs {
		res, err := prober.Probe(context.Background(), path)
		if err != nil {
			return err
		}
		spec.SubIdx = nil
		for _, s := range res.StreamsOfType("subtitle") {
			spec.SubIdx = append(spec.SubIdx, s.Index)
		}
	}
	res, err := eng.RemoveTracks(context.Background(), path, spec,
		engine.Options{DryRun: *dryRun, AllowHardlink: *allowHardlink})
	if err != nil {
		return err
	}
	if res.DryRun {
		fmt.Printf("dry run: %s\nestimated savings: %s\n", res.Command, human(res.BytesSaved))
		return nil
	}
	fmt.Printf("done via %s, saved %s\n", res.Tool, human(res.BytesSaved))
	return nil
}

func parseIdx(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	var out []int
	for _, p := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("bad stream index %q", p)
		}
		out = append(out, n)
	}
	return out, nil
}

func human(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
