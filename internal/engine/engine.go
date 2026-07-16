// Package engine performs the destructive operations: lossless track removal
// via container remux, and sidecar subtitle deletion. Every remux goes through
// the same safety pipeline: guardrails, hardlink policy, free-space check,
// write to a temp file in the same directory, verify the output, preserve file
// attributes, then atomically rename over the original.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/krabhi4/muxprune/internal/probe"
)

// ErrSkipped marks a job that was refused by policy (not a failure).
var ErrSkipped = errors.New("skipped by policy")

type RemovalSpec struct {
	AudioIdx []int `json:"audio_idx"` // ffprobe stream indexes to remove
	SubIdx   []int `json:"sub_idx"`
}

func (r RemovalSpec) Empty() bool { return len(r.AudioIdx) == 0 && len(r.SubIdx) == 0 }

type Options struct {
	AllowHardlink  bool `json:"allow_hardlink"`
	AllowLastAudio bool `json:"allow_last_audio"`
	DryRun         bool `json:"dry_run"`
}

type Result struct {
	Tool       string `json:"tool"`
	Command    string `json:"command"`
	BytesSaved int64  `json:"bytes_saved"`
	DryRun     bool   `json:"dry_run"`
}

type MetadataEdit struct {
	TrackIndex int    `json:"track_index"` // ffprobe stream index
	Language   string `json:"language,omitempty"`
	Title      string `json:"title,omitempty"`
	Default    *bool  `json:"default,omitempty"`
	Forced     *bool  `json:"forced,omitempty"`
}

type Engine struct {
	Prober     *probe.Prober
	RecycleDir string // "" deletes sidecars permanently

	once        sync.Once
	ffmpeg      string
	mkvmerge    string
	mkvpropedit string

	locks keyedMutex
}

func (e *Engine) resolve() {
	e.once.Do(func() {
		e.ffmpeg, _ = exec.LookPath("ffmpeg")
		e.mkvmerge, _ = exec.LookPath("mkvmerge")
		e.mkvpropedit, _ = exec.LookPath("mkvpropedit")
	})
}

// HasMkvpropedit reports whether the mkvpropedit binary is available.
func (e *Engine) HasMkvpropedit() bool { e.resolve(); return e.mkvpropedit != "" }

// EditMetadata performs in-place header edits on a Matroska file using
// mkvpropedit. This is orders of magnitude faster than a full remux for
// metadata-only changes (language, title, default/forced flags).
func (e *Engine) EditMetadata(ctx context.Context, path string, edits []MetadataEdit) (*Result, error) {
	e.resolve()
	unlock := e.locks.lock(absKey(path))
	defer unlock()
	if e.mkvpropedit == "" {
		return nil, errors.New("mkvpropedit not found in PATH")
	}
	if len(edits) == 0 {
		return nil, errors.New("no edits specified")
	}

	res, err := e.Prober.Probe(ctx, path)
	if err != nil {
		return nil, err
	}
	if !res.IsMatroska() {
		return nil, errors.New("mkvpropedit requires a Matroska file")
	}

	// Build a lookup from ffprobe stream index to Stream.
	byIdx := map[int]probe.Stream{}
	for _, s := range res.Streams {
		byIdx[s.Index] = s
	}

	args := []string{path}
	for _, edit := range edits {
		st, ok := byIdx[edit.TrackIndex]
		if !ok {
			return nil, fmt.Errorf("stream index %d not found", edit.TrackIndex)
		}
		if st.MkvID < 0 {
			return nil, fmt.Errorf("stream index %d has no mkvmerge track ID (MkvID unknown)", edit.TrackIndex)
		}
		args = append(args, "--edit", "track:="+strconv.Itoa(st.MkvID))
		if edit.Language != "" {
			if !validLanguageTag(edit.Language) {
				return nil, fmt.Errorf("invalid language tag %q", edit.Language)
			}
			args = append(args, "--set", "language="+edit.Language)
		}
		if title := stripControl(edit.Title); title != "" {
			args = append(args, "--set", "name="+title)
		}
		if edit.Default != nil {
			args = append(args, "--set", "flag-default="+boolFlag(*edit.Default))
		}
		if edit.Forced != nil {
			args = append(args, "--set", "flag-forced="+boolFlag(*edit.Forced))
		}
	}

	cmdline := e.mkvpropedit + " " + strings.Join(args, " ")
	tool, full := wrapNice(e.mkvpropedit, args)
	cmd := exec.CommandContext(ctx, tool, full...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("mkvpropedit failed: %w: %s", err, tail(stderr.String(), 1000))
	}
	return &Result{Tool: "mkvpropedit", Command: cmdline, BytesSaved: 0}, nil
}

func boolFlag(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func validLanguageTag(s string) bool {
	subtags := strings.Split(s, "-")
	for _, sub := range subtags {
		if sub == "" {
			return false
		}
		for _, r := range sub {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
				return false
			}
		}
	}
	for _, r := range subtags[0] {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// ReorderSpec describes the desired track order for a Matroska remux.
type ReorderSpec struct {
	TrackOrder []int `json:"track_order"` // ffprobe stream indexes in desired order
}

// MergeSpec describes external files to merge into a Matroska container.
type MergeSpec struct {
	ExternalFiles []string `json:"external_files"` // paths to subtitle/audio files to merge in
}

// ReorderTracks remuxes a Matroska file with tracks in the specified order
// using mkvmerge's --track-order flag.
func (e *Engine) ReorderTracks(ctx context.Context, path string, spec ReorderSpec) (*Result, error) {
	e.resolve()
	unlock := e.locks.lock(absKey(path))
	defer unlock()
	if e.mkvmerge == "" {
		return nil, errors.New("mkvmerge not found in PATH")
	}
	if len(spec.TrackOrder) == 0 {
		return nil, errors.New("no track order specified")
	}

	res, err := e.Prober.Probe(ctx, path)
	if err != nil {
		return nil, err
	}
	if !res.IsMatroska() {
		return nil, errors.New("track reordering requires a Matroska file")
	}

	// Build lookup from ffprobe index to stream.
	byIdx := map[int]probe.Stream{}
	for _, s := range res.Streams {
		byIdx[s.Index] = s
	}

	// Validate all indexes exist, have known MkvIDs, and have no duplicates.
	// Also ensure that we are reordering the exact set of tracks present in the file.
	expectedCount := 0
	for _, s := range res.Streams {
		if s.MkvID >= 0 {
			expectedCount++
		}
	}
	if len(spec.TrackOrder) != expectedCount {
		return nil, fmt.Errorf("track order must contain exactly %d tracks, got %d", expectedCount, len(spec.TrackOrder))
	}

	seen := map[int]bool{}
	for _, idx := range spec.TrackOrder {
		if seen[idx] {
			return nil, fmt.Errorf("duplicate stream index %d in track order", idx)
		}
		seen[idx] = true

		st, ok := byIdx[idx]
		if !ok {
			return nil, fmt.Errorf("stream index %d not found", idx)
		}
		if st.MkvID < 0 {
			return nil, fmt.Errorf("stream index %d has no mkvmerge track ID", idx)
		}
	}

	// Build --track-order value: 0:mkvID1,0:mkvID2,...
	var orderParts []string
	for _, idx := range spec.TrackOrder {
		orderParts = append(orderParts, fmt.Sprintf("0:%d", byIdx[idx].MkvID))
	}
	trackOrder := strings.Join(orderParts, ",")

	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if free, err := freeSpace(dir); err == nil && free < uint64(info.Size()) {
		return nil, fmt.Errorf("not enough free space in %s: need %d, have %d", dir, info.Size(), free)
	}

	ext := filepath.Ext(path)
	base := strings.TrimSuffix(filepath.Base(path), ext)
	tmp := tempPath(dir, base, ext)
	defer os.Remove(tmp)

	args := []string{"-q", "-o", tmp, "--track-order", trackOrder, probe.SafePathArg(path)}
	cmdline := e.mkvmerge + " " + strings.Join(args, " ")
	tool, full := wrapNice(e.mkvmerge, args)
	cmd := exec.CommandContext(ctx, tool, full...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("mkvmerge failed: %w: %s", err, tail(stderr.String(), 1000))
	}

	// Verify: all stream type counts must match exactly (reorder, not remove).
	if err := e.verify(ctx, res, RemovalSpec{}, tmp); err != nil {
		return nil, fmt.Errorf("output verification failed, original untouched: %w", err)
	}

	outInfo, err := os.Stat(tmp)
	if err != nil {
		return nil, err
	}
	preserveAttrs(tmp, info)
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("atomic rename: %w", err)
	}
	saved := info.Size() - outInfo.Size()
	if saved < 0 {
		saved = 0
	}
	return &Result{
		Tool: "mkvmerge", Command: cmdline,
		BytesSaved: saved,
	}, nil
}

// MergeTracks merges external subtitle/audio files into a Matroska container
// using mkvmerge.
func (e *Engine) MergeTracks(ctx context.Context, path string, spec MergeSpec) (*Result, error) {
	e.resolve()
	unlock := e.locks.lock(absKey(path))
	defer unlock()
	if e.mkvmerge == "" {
		return nil, errors.New("mkvmerge not found in PATH")
	}
	if len(spec.ExternalFiles) == 0 {
		return nil, errors.New("no external files specified")
	}

	res, err := e.Prober.Probe(ctx, path)
	if err != nil {
		return nil, err
	}
	if !res.IsMatroska() {
		return nil, errors.New("track merging requires a Matroska file")
	}

	if err := validateExternalFiles(path, spec.ExternalFiles); err != nil {
		return nil, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if free, err := freeSpace(dir); err == nil && free < uint64(info.Size()) {
		return nil, fmt.Errorf("not enough free space in %s: need %d, have %d", dir, info.Size(), free)
	}

	ext := filepath.Ext(path)
	base := strings.TrimSuffix(filepath.Base(path), ext)
	tmp := tempPath(dir, base, ext)
	defer os.Remove(tmp)

	args := []string{"-q", "-o", tmp, probe.SafePathArg(path)}
	for _, f := range spec.ExternalFiles {
		args = append(args, probe.SafePathArg(f))
	}
	cmdline := e.mkvmerge + " " + strings.Join(args, " ")
	tool, full := wrapNice(e.mkvmerge, args)
	cmd := exec.CommandContext(ctx, tool, full...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("mkvmerge failed: %w: %s", err, tail(stderr.String(), 1000))
	}

	// Verify: video same, audio >= original, subtitle >= original.
	if err := e.verifyMerge(ctx, res, tmp); err != nil {
		return nil, fmt.Errorf("output verification failed, original untouched: %w", err)
	}

	outInfo, err := os.Stat(tmp)
	if err != nil {
		return nil, err
	}
	preserveAttrs(tmp, info)
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("atomic rename: %w", err)
	}
	saved := info.Size() - outInfo.Size()
	if saved < 0 {
		saved = 0
	}
	return &Result{
		Tool: "mkvmerge", Command: cmdline,
		BytesSaved: saved,
	}, nil
}

func validateExternalFiles(path string, files []string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	seenExt := map[string]bool{}
	for _, ext := range files {
		absExt, err := filepath.Abs(ext)
		if err != nil {
			return fmt.Errorf("external file %s path: %w", ext, err)
		}
		if absExt == absPath {
			return fmt.Errorf("cannot merge a file into itself: %s", ext)
		}
		if seenExt[absExt] {
			return fmt.Errorf("duplicate external file specified: %s", ext)
		}
		seenExt[absExt] = true

		fi, err := os.Stat(ext)
		if err != nil {
			return fmt.Errorf("external file %s: %w", ext, err)
		}
		if fi.IsDir() {
			return fmt.Errorf("external file %s is a directory", ext)
		}
		if b := filepath.Base(ext); strings.HasPrefix(b, "-") || strings.HasPrefix(b, "@") {
			return fmt.Errorf("refusing external file %s: name begins with %q", ext, b[:1])
		}
	}
	return nil
}

// verifyMerge checks that a merge output has at least as many tracks as the
// input: video count must match, audio and subtitle counts must be >=.
func (e *Engine) verifyMerge(ctx context.Context, in *probe.Result, tmp string) error {
	out, err := e.Prober.Probe(ctx, tmp)
	if err != nil {
		return fmt.Errorf("probing output: %w", err)
	}
	if got := len(out.StreamsOfType("video")); got != len(in.StreamsOfType("video")) {
		return fmt.Errorf("video stream count changed: %d -> %d", len(in.StreamsOfType("video")), got)
	}
	if got := len(out.StreamsOfType("audio")); got < len(in.StreamsOfType("audio")) {
		return fmt.Errorf("audio stream count decreased: %d -> %d", len(in.StreamsOfType("audio")), got)
	}
	if got := len(out.StreamsOfType("subtitle")); got < len(in.StreamsOfType("subtitle")) {
		return fmt.Errorf("subtitle stream count decreased: %d -> %d", len(in.StreamsOfType("subtitle")), got)
	}
	if in.Duration > 0 && out.Duration > 0 {
		diff := in.Duration - out.Duration
		if diff < 0 {
			diff = -diff
		}
		tolerance := in.Duration * 0.01
		if tolerance < 2.0 {
			tolerance = 2.0
		}
		if tolerance > 60.0 {
			tolerance = 60.0
		}
		if diff > tolerance {
			return fmt.Errorf("duration drifted: %.2fs -> %.2fs (tolerance %.2fs)", in.Duration, out.Duration, tolerance)
		}
	}
	return nil
}

// RemoveTracks losslessly remuxes path without the specified streams.
func (e *Engine) RemoveTracks(ctx context.Context, path string, spec RemovalSpec, opts Options) (*Result, error) {
	e.resolve()
	unlock := e.locks.lock(absKey(path))
	defer unlock()
	if spec.Empty() {
		return nil, errors.New("nothing to remove")
	}
	res, err := e.Prober.Probe(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := validate(res, spec, opts); err != nil {
		return nil, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !opts.AllowHardlink {
		if n := nlink(info); n > 1 {
			return nil, fmt.Errorf("%w: file has %d hardlinks (likely still seeding); enable hardlink override to proceed", ErrSkipped, n)
		}
	}
	dir := filepath.Dir(path)
	if free, err := freeSpace(dir); err == nil && free < uint64(info.Size()) {
		return nil, fmt.Errorf("not enough free space in %s: need %d, have %d", dir, info.Size(), free)
	}

	tool, args, err := e.buildArgs(res, spec)
	if err != nil {
		return nil, err
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(filepath.Base(path), ext)
	tmp := tempPath(dir, base, ext)
	cmdline := tool + " " + strings.Join(args, " ")

	if opts.DryRun {
		return &Result{Tool: filepath.Base(tool), Command: cmdline, DryRun: true,
			BytesSaved: estimateRemoved(res, spec)}, nil
	}

	defer os.Remove(tmp) // no-op after successful rename
	full := append(slices.Clone(args), outputArgs(tool, tmp)...)
	full = reorderOutput(tool, full, tmp)
	tool, full = wrapNice(tool, full)
	cmd := exec.CommandContext(ctx, tool, full...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s failed: %w: %s", filepath.Base(tool), err, tail(stderr.String(), 1000))
	}

	if err := e.verify(ctx, res, spec, tmp); err != nil {
		return nil, fmt.Errorf("output verification failed, original untouched: %w", err)
	}

	outInfo, err := os.Stat(tmp)
	if err != nil {
		return nil, err
	}
	preserveAttrs(tmp, info)
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("atomic rename: %w", err)
	}
	return &Result{
		Tool: filepath.Base(tool), Command: cmdline,
		BytesSaved: info.Size() - outInfo.Size(),
	}, nil
}

func validate(res *probe.Result, spec RemovalSpec, opts Options) error {
	byIdx := map[int]probe.Stream{}
	for _, s := range res.Streams {
		byIdx[s.Index] = s
	}
	for _, i := range spec.AudioIdx {
		s, ok := byIdx[i]
		if !ok || s.Type != "audio" {
			return fmt.Errorf("stream %d is not an audio stream", i)
		}
	}
	for _, i := range spec.SubIdx {
		s, ok := byIdx[i]
		if !ok || s.Type != "subtitle" {
			return fmt.Errorf("stream %d is not a subtitle stream", i)
		}
	}
	audio := res.StreamsOfType("audio")
	if len(spec.AudioIdx) >= len(audio) && len(audio) > 0 && !opts.AllowLastAudio {
		return errors.New("refusing to remove the last audio track (override available)")
	}
	if len(res.StreamsOfType("video")) == 0 {
		return errors.New("no video stream found; not a video file?")
	}
	return nil
}

// buildArgs prefers mkvmerge for Matroska (fastest, most correct); falls back
// to ffmpeg negative mapping with stream copy.
func (e *Engine) buildArgs(res *probe.Result, spec RemovalSpec) (tool string, args []string, err error) {
	if res.IsMatroska() && e.mkvmerge != "" && mkvIDsKnown(res, spec) {
		return e.mkvmerge, mkvmergeArgs(res, spec), nil
	}
	if e.ffmpeg == "" {
		return "", nil, errors.New("no remux tool available (need mkvmerge or ffmpeg)")
	}
	return e.ffmpeg, ffmpegArgs(res, spec), nil
}

func mkvIDsKnown(res *probe.Result, spec RemovalSpec) bool {
	for _, s := range res.Streams {
		if (s.Type == "audio" || s.Type == "subtitle") && s.MkvID < 0 {
			return false
		}
	}
	return true
}

// mkvmergeArgs uses explicit keep-lists, which read unambiguously in job logs.
func mkvmergeArgs(res *probe.Result, spec RemovalSpec) []string {
	var args []string
	keep := func(typ string, removed []int) []string {
		var ids []string
		for _, s := range res.StreamsOfType(typ) {
			if !slices.Contains(removed, s.Index) {
				ids = append(ids, strconv.Itoa(s.MkvID))
			}
		}
		return ids
	}
	if len(spec.AudioIdx) > 0 {
		if ids := keep("audio", spec.AudioIdx); len(ids) == 0 {
			args = append(args, "--no-audio")
		} else {
			args = append(args, "--audio-tracks", strings.Join(ids, ","))
		}
	}
	if len(spec.SubIdx) > 0 {
		if ids := keep("subtitle", spec.SubIdx); len(ids) == 0 {
			args = append(args, "--no-subtitles")
		} else {
			args = append(args, "--subtitle-tracks", strings.Join(ids, ","))
		}
	}
	return append(args, probe.SafePathArg(res.Path))
}

func ffmpegArgs(res *probe.Result, spec RemovalSpec) []string {
	args := []string{"-y", "-v", "error", "-i", res.Path, "-map", "0"}
	// ffmpeg negative maps use per-type positions, not global indexes.
	pos := func(typ string, idx int) int {
		p := 0
		for _, s := range res.StreamsOfType(typ) {
			if s.Index == idx {
				return p
			}
			p++
		}
		return -1
	}
	var negs []string
	for _, i := range spec.AudioIdx {
		negs = append(negs, fmt.Sprintf("-0:a:%d", pos("audio", i)))
	}
	for _, i := range spec.SubIdx {
		negs = append(negs, fmt.Sprintf("-0:s:%d", pos("subtitle", i)))
	}
	sort.Strings(negs)
	for _, n := range negs {
		args = append(args, "-map", n)
	}
	ext := strings.ToLower(filepath.Ext(res.Path))
	if ext == ".m4v" || ext == ".mp4" {
		return append(args, "-c", "copy", "-f", "mp4")
	}
	return append(args, "-c", "copy")
}

// outputArgs / reorderOutput: mkvmerge wants `-o out` before the input,
// ffmpeg wants the output last. buildArgs returns input-terminated args, so
// fix up per tool here.
func outputArgs(tool, tmp string) []string {
	if strings.Contains(filepath.Base(tool), "mkvmerge") {
		return nil
	}
	return []string{tmp}
}

func reorderOutput(tool string, args []string, tmp string) []string {
	if strings.Contains(filepath.Base(tool), "mkvmerge") {
		return append([]string{"-q", "-o", tmp}, args...)
	}
	return args
}

func (e *Engine) verify(ctx context.Context, in *probe.Result, spec RemovalSpec, tmp string) error {
	out, err := e.Prober.Probe(ctx, tmp)
	if err != nil {
		return fmt.Errorf("probing output: %w", err)
	}
	wantAudio := len(in.StreamsOfType("audio")) - len(spec.AudioIdx)
	wantSubs := len(in.StreamsOfType("subtitle")) - len(spec.SubIdx)
	if got := len(out.StreamsOfType("video")); got != len(in.StreamsOfType("video")) {
		return fmt.Errorf("video stream count changed: %d -> %d", len(in.StreamsOfType("video")), got)
	}
	if got := len(out.StreamsOfType("audio")); got != wantAudio {
		return fmt.Errorf("audio stream count: want %d, got %d", wantAudio, got)
	}
	if got := len(out.StreamsOfType("subtitle")); got != wantSubs {
		return fmt.Errorf("subtitle stream count: want %d, got %d", wantSubs, got)
	}
	if in.Duration > 0 && out.Duration > 0 {
		diff := in.Duration - out.Duration
		if diff < 0 {
			diff = -diff
		}
		// Allow up to 1% drift, with a minimum tolerance of 2.0 seconds and a maximum tolerance of 60.0 seconds.
		tolerance := in.Duration * 0.01
		if tolerance < 2.0 {
			tolerance = 2.0
		}
		if tolerance > 60.0 {
			tolerance = 60.0
		}
		if diff > tolerance {
			return fmt.Errorf("duration drifted: %.2fs -> %.2fs (tolerance %.2fs)", in.Duration, out.Duration, tolerance)
		}
	}
	floor := sizeFloor(in.Size, estimateRemoved(in, spec))
	if out.Size < floor {
		return fmt.Errorf("output suspiciously small: %d < floor %d (input %d)", out.Size, floor, in.Size)
	}
	return nil
}

func sizeFloor(inSize, estRemoved int64) int64 {
	floor := int64(float64(inSize-estRemoved) * 0.7)
	if minFloor := inSize / 10; floor < minFloor {
		floor = minFloor
	}
	return floor
}

func estimateRemoved(res *probe.Result, spec RemovalSpec) int64 {
	var sum int64
	removed := append(slices.Clone(spec.AudioIdx), spec.SubIdx...)
	for _, s := range res.Streams {
		if slices.Contains(removed, s.Index) && s.BitRate > 0 && res.Duration > 0 {
			sum += int64(float64(s.BitRate) / 8 * res.Duration)
		}
	}
	return sum
}

// DeleteSidecar removes (or recycles) an external subtitle file.
func (e *Engine) DeleteSidecar(path string, dryRun bool) (*Result, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() || !isSubtitlePath(path) {
		return nil, fmt.Errorf("refusing to delete %s: not a subtitle file", path)
	}
	r := &Result{Tool: "fs", BytesSaved: info.Size(), DryRun: dryRun}
	if dryRun {
		r.Command = "delete " + path
		return r, nil
	}
	if e.RecycleDir != "" {
		if err := os.MkdirAll(e.RecycleDir, 0o755); err != nil {
			return nil, err
		}
		dst := uniqueDst(e.RecycleDir,
			time.Now().UTC().Format("20060102-150405")+"_"+filepath.Base(path))
		if err := moveFile(path, dst); err != nil {
			return nil, err
		}
		r.Command = "recycle " + path + " -> " + dst
		return r, nil
	}
	if err := os.Remove(path); err != nil {
		return nil, err
	}
	r.Command = "delete " + path
	return r, nil
}

// PurgeRecycle deletes recycled files older than the given age.
func (e *Engine) PurgeRecycle(olderThan time.Duration) (int, error) {
	if e.RecycleDir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(e.RecycleDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	cutoff := time.Now().Add(-olderThan)
	for _, ent := range entries {
		info, err := ent.Info()
		if err != nil || info.IsDir() {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if os.Remove(filepath.Join(e.RecycleDir, ent.Name())) == nil {
				n++
			}
		}
	}
	return n, nil
}

var subtitleExts = map[string]bool{
	".srt": true, ".ass": true, ".ssa": true, ".sub": true, ".idx": true,
	".vtt": true, ".smi": true, ".sup": true,
}

func isSubtitlePath(path string) bool {
	return subtitleExts[strings.ToLower(filepath.Ext(path))]
}

func uniqueDst(dir, name string) string {
	dst := filepath.Join(dir, name)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			return dst
		}
		dst = filepath.Join(dir, fmt.Sprintf("%s_%d%s", stem, i, ext))
	}
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Remove(src)
}

var tmpSeq atomic.Int64

func tempPath(dir, base, ext string) string {
	n := tmpSeq.Add(1)
	return filepath.Join(dir, fmt.Sprintf(".%s.muxprune.tmp.%d-%d%s", base, os.Getpid(), n, ext))
}

func absKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*refMutex
}

type refMutex struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = make(map[string]*refMutex)
	}
	rm := k.m[key]
	if rm == nil {
		rm = &refMutex{}
		k.m[key] = rm
	}
	rm.refs++
	k.mu.Unlock()

	rm.mu.Lock()
	return func() {
		rm.mu.Unlock()
		k.mu.Lock()
		rm.refs--
		if rm.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
