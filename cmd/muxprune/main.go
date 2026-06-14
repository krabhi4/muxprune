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
	"syscall"
	"time"

	"github.com/krabhi4/muxprune/internal/api"
	"github.com/krabhi4/muxprune/internal/engine"
	"github.com/krabhi4/muxprune/internal/jobs"
	"github.com/krabhi4/muxprune/internal/probe"
	"github.com/krabhi4/muxprune/internal/scan"
	"github.com/krabhi4/muxprune/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return def
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

	prober := &probe.Prober{}
	if !prober.HasFFprobe() {
		return fmt.Errorf("ffprobe not found in PATH; install ffmpeg")
	}
	hub := api.NewHub()
	eng := &engine.Engine{Prober: prober}
	if *recycleDays > 0 {
		eng.RecycleDir = filepath.Join(*configDir, "recycle")
	}
	scanner := &scan.Scanner{Store: st, Prober: prober, Events: hub}
	runner := &jobs.Runner{Store: st, Engine: eng, Scanner: scanner, Events: hub}
	srv := &api.Server{Store: st, Scanner: scanner, Runner: runner, Engine: eng, Hub: hub, APIKey: *apiKey}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go runner.Start(ctx, *workers)
	if *recycleDays > 0 {
		go purgeLoop(ctx, eng, time.Duration(*recycleDays)*24*time.Hour)
	}

	httpSrv := &http.Server{Addr: fmt.Sprintf(":%d", *port), Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()

	fmt.Printf("muxprune %s listening on :%d (config: %s, mkvmerge: %v)\n",
		version, *port, *configDir, prober.HasMkvmerge())
	if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
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

	prober := &probe.Prober{}
	if !prober.HasFFprobe() {
		return fmt.Errorf("ffprobe not found in PATH; install ffmpeg")
	}
	hub := api.NewHub()
	eng := &engine.Engine{Prober: prober}
	if *recycleDays > 0 {
		eng.RecycleDir = filepath.Join(*configDir, "recycle")
	}
	scanner := &scan.Scanner{Store: st, Prober: prober, Events: hub}
	runner := &jobs.Runner{Store: st, Engine: eng, Scanner: scanner, Events: hub}
	srv := &api.Server{Store: st, Scanner: scanner, Runner: runner, Engine: eng, Hub: hub}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go runner.Start(ctx, *workers)
	if *recycleDays > 0 {
		go purgeLoop(ctx, eng, time.Duration(*recycleDays)*24*time.Hour)
	}

	return srv.ServeMCP(ctx)
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
