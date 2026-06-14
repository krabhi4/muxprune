package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/krabhi4/muxprune/internal/probe"
)

// makeFixture builds a small MKV with 3 audio tracks (eng/jpn/fre) and 2
// subtitle tracks (eng/spa) using ffmpeg synthetic sources.
func makeFixture(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	srt := "1\n00:00:00,000 --> 00:00:02,000\nhello\n"
	sub1 := filepath.Join(dir, "a.srt")
	sub2 := filepath.Join(dir, "b.srt")
	os.WriteFile(sub1, []byte(srt), 0o644)
	os.WriteFile(sub2, []byte(srt), 0o644)
	out := filepath.Join(dir, "fixture.mkv")
	cmd := exec.Command("ffmpeg", "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=duration=3:size=160x120:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=3",
		"-i", sub1, "-i", sub2,
		"-map", "0:v", "-map", "1:a", "-map", "2:a", "-map", "3:a", "-map", "4:s", "-map", "5:s",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-c:s", "srt",
		"-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=jpn",
		"-metadata:s:a:2", "language=fre",
		"-metadata:s:s:0", "language=eng", "-metadata:s:s:1", "language=spa",
		out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, b)
	}
	return out
}

func langs(res *probe.Result, typ string) []string {
	var out []string
	for _, s := range res.StreamsOfType(typ) {
		out = append(out, s.Lang)
	}
	return out
}

func TestRemoveTracks(t *testing.T) {
	dir := t.TempDir()
	path := makeFixture(t, dir)
	p := &probe.Prober{}
	e := &Engine{Prober: p}
	ctx := context.Background()

	before, err := p.Probe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var jpnIdx, spaIdx int
	for _, s := range before.Streams {
		if s.Lang == "jpn" {
			jpnIdx = s.Index
		}
		if s.Lang == "spa" {
			spaIdx = s.Index
		}
	}

	res, err := e.RemoveTracks(ctx, path, RemovalSpec{AudioIdx: []int{jpnIdx}, SubIdx: []int{spaIdx}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.BytesSaved <= 0 {
		t.Errorf("expected positive bytes saved, got %d", res.BytesSaved)
	}

	after, err := p.Probe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if got := langs(after, "audio"); len(got) != 2 || got[0] != "eng" || got[1] != "fre" {
		t.Errorf("audio after removal: %v", got)
	}
	if got := langs(after, "subtitle"); len(got) != 1 || got[0] != "eng" {
		t.Errorf("subtitles after removal: %v", got)
	}
	// No temp leftovers
	matches, _ := filepath.Glob(filepath.Join(dir, "*muxprune.tmp*"))
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

func TestGuardrails(t *testing.T) {
	dir := t.TempDir()
	path := makeFixture(t, dir)
	p := &probe.Prober{}
	e := &Engine{Prober: p}
	ctx := context.Background()

	res, _ := p.Probe(ctx, path)
	var allAudio []int
	var videoIdx int
	for _, s := range res.Streams {
		if s.Type == "audio" {
			allAudio = append(allAudio, s.Index)
		}
		if s.Type == "video" {
			videoIdx = s.Index
		}
	}

	if _, err := e.RemoveTracks(ctx, path, RemovalSpec{AudioIdx: allAudio}, Options{}); err == nil {
		t.Error("removing all audio should fail without override")
	}
	if _, err := e.RemoveTracks(ctx, path, RemovalSpec{AudioIdx: []int{videoIdx}}, Options{}); err == nil {
		t.Error("targeting the video stream as audio should fail")
	}
	if _, err := e.RemoveTracks(ctx, path, RemovalSpec{}, Options{}); err == nil {
		t.Error("empty spec should fail")
	}
}

func TestHardlinkSkip(t *testing.T) {
	dir := t.TempDir()
	path := makeFixture(t, dir)
	if err := os.Link(path, filepath.Join(dir, "seed.mkv")); err != nil {
		t.Skip("hardlinks unsupported here")
	}
	p := &probe.Prober{}
	e := &Engine{Prober: p}
	ctx := context.Background()
	res, _ := p.Probe(ctx, path)
	idx := res.StreamsOfType("audio")[1].Index

	_, err := e.RemoveTracks(ctx, path, RemovalSpec{AudioIdx: []int{idx}}, Options{})
	if !errors.Is(err, ErrSkipped) {
		t.Fatalf("expected ErrSkipped for hardlinked file, got %v", err)
	}
	// With override it must succeed.
	if _, err := e.RemoveTracks(ctx, path, RemovalSpec{AudioIdx: []int{idx}}, Options{AllowHardlink: true}); err != nil {
		t.Fatalf("override failed: %v", err)
	}
}

func TestDryRun(t *testing.T) {
	dir := t.TempDir()
	path := makeFixture(t, dir)
	p := &probe.Prober{}
	e := &Engine{Prober: p}
	ctx := context.Background()
	res, _ := p.Probe(ctx, path)
	idx := res.StreamsOfType("audio")[1].Index
	before, _ := os.Stat(path)

	r, err := e.RemoveTracks(ctx, path, RemovalSpec{AudioIdx: []int{idx}}, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !r.DryRun || r.Command == "" {
		t.Errorf("dry run result incomplete: %+v", r)
	}
	after, _ := os.Stat(path)
	if before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		t.Error("dry run modified the file")
	}
}

func TestDeleteSidecarRecycle(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "movie.en.srt")
	os.WriteFile(sub, []byte("1\n00:00:00,000 --> 00:00:01,000\nx\n"), 0o644)
	recycle := filepath.Join(dir, "recycle")
	e := &Engine{RecycleDir: recycle}

	if _, err := e.DeleteSidecar(sub, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Error("sidecar still present after delete")
	}
	entries, _ := os.ReadDir(recycle)
	if len(entries) != 1 {
		t.Fatalf("expected 1 recycled file, got %d", len(entries))
	}
	// Refuse non-subtitle paths.
	video := filepath.Join(dir, "movie.mkv")
	os.WriteFile(video, []byte("x"), 0o644)
	if _, err := e.DeleteSidecar(video, false); err == nil {
		t.Error("deleting a video file via sidecar path should fail")
	}
}

func TestReorderTracks(t *testing.T) {
	if _, err := exec.LookPath("mkvmerge"); err != nil {
		t.Skip("mkvmerge not installed")
	}
	dir := t.TempDir()
	path := makeFixture(t, dir)
	p := &probe.Prober{}
	e := &Engine{Prober: p}
	ctx := context.Background()

	before, err := p.Probe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(langs(before, "audio")); got != 3 {
		t.Fatalf("expected 3 audio streams initially, got %d", got)
	}

	// We have 6 streams total:
	// 0: video
	// 1: audio (eng)
	// 2: audio (jpn)
	// 3: audio (fre)
	// 4: subtitle (eng)
	// 5: subtitle (spa)
	// Let's reorder the audio streams: fre (3), jpn (2), eng (1).
	// Track order should contain all streams in new order.
	order := []int{0, 3, 2, 1, 4, 5}
	res, err := e.ReorderTracks(ctx, path, ReorderSpec{TrackOrder: order})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tool != "mkvmerge" {
		t.Errorf("expected tool mkvmerge, got %s", res.Tool)
	}

	after, err := p.Probe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	gotLangs := langs(after, "audio")
	expectedLangs := []string{"fre", "jpn", "eng"}
	if len(gotLangs) != len(expectedLangs) {
		t.Fatalf("expected %d audio tracks, got %d", len(expectedLangs), len(gotLangs))
	}
	for i, l := range expectedLangs {
		if gotLangs[i] != l {
			t.Errorf("at pos %d, expected audio lang %s, got %s", i, l, gotLangs[i])
		}
	}
}

func TestMergeTracks(t *testing.T) {
	if _, err := exec.LookPath("mkvmerge"); err != nil {
		t.Skip("mkvmerge not installed")
	}
	dir := t.TempDir()
	path := makeFixture(t, dir)
	p := &probe.Prober{}
	e := &Engine{Prober: p}
	ctx := context.Background()

	extSub := filepath.Join(dir, "ext.srt")
	srtContent := "1\n00:00:00,000 --> 00:00:02,000\nexternal\n"
	os.WriteFile(extSub, []byte(srtContent), 0o644)

	res, err := e.MergeTracks(ctx, path, MergeSpec{ExternalFiles: []string{extSub}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tool != "mkvmerge" {
		t.Errorf("expected tool mkvmerge, got %s", res.Tool)
	}

	after, err := p.Probe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	// Before merge we had 2 subtitle tracks, now we should have 3.
	subLangs := langs(after, "subtitle")
	if len(subLangs) != 3 {
		t.Errorf("expected 3 subtitle tracks after merge, got %d: %v", len(subLangs), subLangs)
	}
}
