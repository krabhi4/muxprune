package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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

func TestSizeFloor(t *testing.T) {
	cases := []struct {
		name               string
		inSize, estRemoved int64
		want               int64
	}{
		{"no estimate", 1000, 0, 700},
		{"partial estimate", 1000, 200, 560},
		{"estimate exceeds input", 1000, 2000, 100},
		{"estimate equals input", 1000, 1000, 100},
	}
	for _, c := range cases {
		if got := sizeFloor(c.inSize, c.estRemoved); got != c.want {
			t.Errorf("%s: sizeFloor(%d,%d)=%d, want %d", c.name, c.inSize, c.estRemoved, got, c.want)
		}
	}
	if got := sizeFloor(1000, 1<<40); got < 0 {
		t.Errorf("floor went negative: %d", got)
	}
}

func TestVerifyRejectsCorruptOutput(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	tmp := filepath.Join(dir, "bad.mkv")
	if err := os.WriteFile(tmp, []byte("this is not a media file"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := &probe.Result{
		Path: "orig.mkv", Format: "matroska,webm", Duration: 3, Size: 100000,
		Streams: []probe.Stream{
			{Index: 0, Type: "video"},
			{Index: 1, Type: "audio"},
			{Index: 2, Type: "audio"},
		},
	}
	e := &Engine{Prober: &probe.Prober{}}
	if err := e.verify(context.Background(), in, RemovalSpec{AudioIdx: []int{2}}, tmp); err == nil {
		t.Fatal("verify must reject a corrupt output, but returned nil")
	}
}

func TestTempPathUniqueAndSkippable(t *testing.T) {
	dir := t.TempDir()
	a, err := tempPath(dir, "movie", ".mkv")
	if err != nil {
		t.Fatal(err)
	}
	b, err := tempPath(dir, "movie", ".mkv")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("temp paths must be unique, both = %s", a)
	}
	if name := filepath.Base(a); !strings.HasPrefix(name, ".movie.muxprune.tmp.") || !strings.HasSuffix(name, ".mkv") {
		t.Errorf("temp name must keep the scanner-skip marker and extension: %s", name)
	}
	if fi, err := os.Lstat(a); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("temp file must be created as a regular file: %v", err)
	}
}

func TestKeyedMutexSerializesSameKey(t *testing.T) {
	var km KeyedMutex
	var mu sync.Mutex
	var active, maxActive int
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := km.Lock("same/file.mkv")
			defer unlock()
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if maxActive != 1 {
		t.Fatalf("KeyedMutex allowed %d concurrent holders for the same key, want 1", maxActive)
	}
}

func TestKeyedMutexAllowsDifferentKeys(t *testing.T) {
	var km KeyedMutex
	unlockA := km.Lock("a")
	defer unlockA()
	done := make(chan struct{})
	go func() {
		unlockB := km.Lock("b")
		unlockB()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lock on a different key blocked")
	}
}

func TestUniqueDst(t *testing.T) {
	dir := t.TempDir()
	name := "20260628-120000_2_eng.srt"
	a := uniqueDst(dir, name)
	if a != filepath.Join(dir, name) {
		t.Fatalf("first call should return the plain name, got %s", a)
	}
	if err := os.WriteFile(a, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := uniqueDst(dir, name)
	if b == a {
		t.Fatal("uniqueDst returned a path that already exists (would overwrite)")
	}
	if _, err := os.Stat(b); err == nil {
		t.Fatalf("second path %s should not exist yet", b)
	}
}

func TestValidLanguageTag(t *testing.T) {
	tests := []struct {
		tag  string
		want bool
	}{
		{"eng", true},
		{"en", true},
		{"pt-BR", true},
		{"zh-Hant-HK", true},
		{"", false},
		{"en_US", false},
		{"en US", false},
		{"123", false},
		{"en-", false},
		{"-en", false},
		{"en--US", false},
		{"e\nn", false},
		{"en;rm -rf", false},
	}
	for _, tc := range tests {
		if got := validLanguageTag(tc.tag); got != tc.want {
			t.Errorf("validLanguageTag(%q) = %v, want %v", tc.tag, got, tc.want)
		}
	}
}

func TestStripControl(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Director's Cut", "Director's Cut"},
		{"bad\x00title", "badtitle"},
		{"line\nbreak", "linebreak"},
		{"tab\there", "tabhere"},
		{"del\x7fchar", "delchar"},
		{"\x01\x02\x03", ""},
	}
	for _, tc := range tests {
		if got := stripControl(tc.in); got != tc.want {
			t.Errorf("stripControl(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateExternalFilesRejectsHostile(t *testing.T) {
	dir := t.TempDir()
	mkv := filepath.Join(dir, "movie.mkv")
	os.WriteFile(mkv, []byte("x"), 0o644)

	dash := filepath.Join(dir, "-evil.srt")
	at := filepath.Join(dir, "@evil.srt")
	ok := filepath.Join(dir, "good.srt")
	os.WriteFile(dash, []byte("x"), 0o644)
	os.WriteFile(at, []byte("x"), 0o644)
	os.WriteFile(ok, []byte("x"), 0o644)

	tests := []struct {
		name    string
		files   []string
		wantErr bool
	}{
		{"dash prefix", []string{dash}, true},
		{"at prefix", []string{at}, true},
		{"normal", []string{ok}, false},
		{"mixed rejects", []string{ok, dash}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Engine{}).ValidateExternalFiles(mkv, tc.files)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %v, got nil", tc.files)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %v: %v", tc.files, err)
			}
		})
	}
}

func TestMkvmergeArgsTerminator(t *testing.T) {
	res := &probe.Result{
		Path: "-tricky.mkv",
		Streams: []probe.Stream{
			{Index: 0, Type: "video", MkvID: 0},
			{Index: 1, Type: "audio", MkvID: 1},
			{Index: 2, Type: "audio", MkvID: 2},
		},
	}
	args := mkvmergeArgs(res, RemovalSpec{AudioIdx: []int{2}})
	want := "./-tricky.mkv"
	if len(args) < 1 || args[len(args)-1] != want {
		t.Fatalf("expected args to end with guarded path %q, got %v", want, args)
	}
	if slices.Contains(args, res.Path) {
		t.Fatalf("bare option-like path leaked into args: %v", args)
	}
	full := reorderOutput("mkvmerge", args, "out.mkv")
	if !slices.Contains(full, want) || slices.Contains(full, res.Path) {
		t.Fatalf("expected guarded path %q and no bare path in %v", want, full)
	}
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

func TestRemuxAdvancesModTime(t *testing.T) {
	dir := t.TempDir()
	path := makeFixture(t, dir)
	p := &probe.Prober{}
	e := &Engine{Prober: p}
	ctx := context.Background()

	res, err := p.Probe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	jpnIdx := -1
	for _, s := range res.Streams {
		if s.Lang == "jpn" {
			jpnIdx = s.Index
		}
	}
	if jpnIdx < 0 {
		t.Fatal("fixture missing jpn audio track")
	}

	backdate := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, backdate, backdate); err != nil {
		t.Fatal(err)
	}

	if _, err := e.RemoveTracks(ctx, path, RemovalSpec{AudioIdx: []int{jpnIdx}}, Options{}); err != nil {
		t.Fatalf("remux failed: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().After(backdate) {
		t.Fatalf("remux must advance mtime so downstream scanners detect the change: mtime=%v, backdated original=%v", after.ModTime(), backdate)
	}
}

func TestDeleteSidecarRecycle(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "movie.en.srt")
	os.WriteFile(sub, []byte("1\n00:00:00,000 --> 00:00:01,000\nx\n"), 0o644)
	old := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(sub, old, old)
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
	if n, _ := e.PurgeRecycle(time.Hour); n != 0 {
		t.Errorf("freshly recycled file purged by age: %d removed", n)
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
	res, err := e.ReorderTracks(ctx, path, ReorderSpec{TrackOrder: order}, false)
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

	res, err := e.MergeTracks(ctx, path, MergeSpec{ExternalFiles: []string{extSub}}, false)
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

func TestValidateExternalFiles_RootContainment(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mkv := filepath.Join(root, "show.mkv")
	if err := os.WriteFile(mkv, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "show.eng.srt")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(outside, "stray.srt")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	unrestricted := &Engine{}
	if err := unrestricted.ValidateExternalFiles(mkv, []string{stray}); err != nil {
		t.Errorf("no AllowedRoots provider should not restrict: %v", err)
	}

	e := &Engine{AllowedRoots: func() []string { return []string{root} }}
	if err := e.ValidateExternalFiles(mkv, []string{inside}); err != nil {
		t.Errorf("file inside an allowed root rejected: %v", err)
	}
	if err := e.ValidateExternalFiles(mkv, []string{stray}); err == nil {
		t.Error("file outside every allowed root was accepted")
	}
}

func TestValidateExternalFiles_SymlinkCannotEscapeRoots(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mkv := filepath.Join(root, "show.mkv")
	if err := os.WriteFile(mkv, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "secret.srt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "innocent.srt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	e := &Engine{AllowedRoots: func() []string { return []string{root} }}
	if err := e.ValidateExternalFiles(mkv, []string{link}); err == nil {
		t.Error("symlink pointing outside the allowed roots was accepted")
	}
}

func TestPathWithin(t *testing.T) {
	cases := []struct {
		path  string
		roots []string
		want  bool
	}{
		{"/media/tv/show.srt", []string{"/media/tv"}, true},
		{"/media/tv", []string{"/media/tv"}, true},
		{"/media/tvx/show.srt", []string{"/media/tv"}, false},
		{"/etc/passwd", []string{"/"}, true},
		{"/etc/passwd", nil, false},
	}
	for _, c := range cases {
		if got := pathWithin(c.path, c.roots); got != c.want {
			t.Errorf("pathWithin(%q, %v) = %v, want %v", c.path, c.roots, got, c.want)
		}
	}
}

func TestEditMetadataTargetsRightTrack(t *testing.T) {
	for _, bin := range []string{"mkvmerge", "mkvpropedit"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
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
	jpn := before.StreamsOfType("audio")[1]
	if _, err := e.EditMetadata(ctx, path, []MetadataEdit{{TrackIndex: jpn.Index, Language: "ger"}}, false); err != nil {
		t.Fatal(err)
	}
	after, err := p.Probe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if got := langs(after, "audio"); !slices.Equal(got, []string{"eng", "ger", "fre"}) {
		t.Errorf("audio langs after edit = %v, want [eng ger fre]", got)
	}
}

func TestReplaceGuards(t *testing.T) {
	if _, err := exec.LookPath("mkvmerge"); err != nil {
		t.Skip("mkvmerge not installed")
	}
	dir := t.TempDir()
	path := makeFixture(t, dir)
	p := &probe.Prober{}
	e := &Engine{Prober: p}
	ctx := context.Background()
	order := []int{0, 3, 2, 1, 4, 5}

	link := filepath.Join(dir, "link.mkv")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := e.ReorderTracks(ctx, link, ReorderSpec{TrackOrder: order}, true); !errors.Is(err, ErrSkipped) {
		t.Errorf("symlinked path: expected ErrSkipped, got %v", err)
	}
	if _, err := e.EditMetadata(ctx, link, []MetadataEdit{{TrackIndex: 1, Language: "ger"}}, true); !errors.Is(err, ErrSkipped) {
		t.Errorf("symlinked edit: expected ErrSkipped, got %v", err)
	}

	if err := os.Link(path, filepath.Join(dir, "seed.mkv")); err != nil {
		t.Skip("hardlinks unsupported here")
	}
	if _, err := e.ReorderTracks(ctx, path, ReorderSpec{TrackOrder: order}, false); !errors.Is(err, ErrSkipped) {
		t.Errorf("hardlinked reorder: expected ErrSkipped, got %v", err)
	}
	if _, err := e.EditMetadata(ctx, path, []MetadataEdit{{TrackIndex: 1, Language: "ger"}}, false); !errors.Is(err, ErrSkipped) {
		t.Errorf("hardlinked edit: expected ErrSkipped, got %v", err)
	}
	if _, err := e.ReorderTracks(ctx, path, ReorderSpec{TrackOrder: order}, true); err != nil {
		t.Errorf("hardlink override failed: %v", err)
	}
}

func TestRemoveTracksKeepsCoverAttachment(t *testing.T) {
	if _, err := exec.LookPath("mkvmerge"); err != nil {
		t.Skip("mkvmerge not installed")
	}
	dir := t.TempDir()
	src := makeFixture(t, dir)
	cover := filepath.Join(dir, "cover.jpg")
	if b, err := exec.Command("ffmpeg", "-y", "-v", "error", "-f", "lavfi", "-i", "color=red:size=64x64", "-frames:v", "1", cover).CombinedOutput(); err != nil {
		t.Fatalf("cover: %v: %s", err, b)
	}
	path := filepath.Join(dir, "cover.mkv")
	if b, err := exec.Command("mkvmerge", "-q", "-o", path, "--attachment-mime-type", "image/jpeg", "--attach-file", cover, src).CombinedOutput(); err != nil {
		t.Fatalf("attach: %v: %s", err, b)
	}
	p := &probe.Prober{}
	e := &Engine{Prober: p}
	ctx := context.Background()
	before, err := p.Probe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.StreamsOfType("video")) != 1 || len(before.StreamsOfType("attachment")) != 1 {
		t.Fatalf("cover art must probe as an attachment, got %+v", before.Streams)
	}
	res, err := e.RemoveTracks(ctx, path, RemovalSpec{AudioIdx: []int{before.StreamsOfType("audio")[1].Index}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tool != "mkvmerge" {
		t.Errorf("expected mkvmerge path, got %s", res.Tool)
	}
	after, err := p.Probe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.StreamsOfType("video")) != 1 || len(after.StreamsOfType("attachment")) != 1 {
		t.Errorf("after remux: want 1 video + 1 attachment, got %+v", after.Streams)
	}
}

func TestReplaceOriginalRefusesChangedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	os.WriteFile(path, []byte("old"), 0o644)
	orig, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	upgrade := filepath.Join(dir, "upgrade.mkv")
	os.WriteFile(upgrade, []byte("upgrade"), 0o644)
	if err := os.Rename(upgrade, path); err != nil {
		t.Fatal(err)
	}
	tmp, err := tempPath(dir, "movie", ".mkv")
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceOriginal(tmp, path, orig); err == nil {
		t.Fatal("replaceOriginal must refuse when the original was replaced mid-run")
	}
	if b, _ := os.ReadFile(path); string(b) != "upgrade" {
		t.Errorf("upgraded file was clobbered: %q", b)
	}
}

func TestMkvWarningsOnly(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ctx := context.Background()
	if err := exec.Command("sh", "-c", "exit 1").Run(); !mkvWarningsOnly(ctx, err) {
		t.Errorf("exit 1 must count as warnings-only, got %v", err)
	}
	if err := exec.Command("sh", "-c", "exit 2").Run(); mkvWarningsOnly(ctx, err) {
		t.Error("exit 2 must be a failure")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := exec.Command("sh", "-c", "exit 1").Run(); mkvWarningsOnly(cctx, err) {
		t.Error("a cancelled run must be a failure")
	}
	kctx, kcancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer kcancel()
	if err := exec.CommandContext(kctx, "sh", "-c", "sleep 5").Run(); mkvWarningsOnly(ctx, err) {
		t.Errorf("a killed run must be a failure, got %v", err)
	}
}

func TestDeleteSidecarConcurrentSameName(t *testing.T) {
	root := t.TempDir()
	recycle := filepath.Join(root, "recycle")
	e := &Engine{RecycleDir: recycle}
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		d := filepath.Join(root, strconv.Itoa(i))
		os.MkdirAll(d, 0o755)
		sub := filepath.Join(d, "movie.en.srt")
		os.WriteFile(sub, []byte(strconv.Itoa(i)), 0o644)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.DeleteSidecar(sub, false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if entries, _ := os.ReadDir(recycle); len(entries) != n {
		t.Fatalf("expected %d recycled files, got %d", n, len(entries))
	}
}

func TestDeleteSidecarRecycleLongName(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, strings.Repeat("字", 80)+".en.srt")
	if err := os.WriteFile(sub, []byte("1"), 0o644); err != nil {
		t.Skipf("long names unsupported: %v", err)
	}
	e := &Engine{RecycleDir: filepath.Join(dir, "recycle")}
	done := make(chan error, 1)
	go func() { _, err := e.DeleteSidecar(sub, false); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DeleteSidecar hung on a long filename")
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Errorf("original still present: %v", err)
	}
}

func TestTempPathLongBase(t *testing.T) {
	dir := t.TempDir()
	tmp, err := tempPath(dir, strings.Repeat("é", 120), ".mkv")
	if err != nil {
		t.Fatalf("long base name: %v", err)
	}
	if len(filepath.Base(tmp)) > 255 || !strings.HasSuffix(tmp, ".mkv") || !utf8.ValidString(tmp) {
		t.Errorf("bad temp name %q (%d bytes)", tmp, len(filepath.Base(tmp)))
	}
}

func TestEditMetadataClearsTitle(t *testing.T) {
	for _, bin := range []string{"mkvmerge", "mkvpropedit"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	dir := t.TempDir()
	path := makeFixture(t, dir)
	p := &probe.Prober{}
	e := &Engine{Prober: p}
	ctx := context.Background()
	res, err := p.Probe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	idx := res.StreamsOfType("audio")[0].Index
	titleOf := func() string {
		r, err := p.Probe(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range r.Streams {
			if s.Index == idx {
				return s.Title
			}
		}
		return "?"
	}
	named, empty := "Commentary", ""
	if _, err := e.EditMetadata(ctx, path, []MetadataEdit{{TrackIndex: idx, Title: &named}}, false); err != nil {
		t.Fatal(err)
	}
	if got := titleOf(); got != named {
		t.Fatalf("title after set = %q, want %q", got, named)
	}
	if _, err := e.EditMetadata(ctx, path, []MetadataEdit{{TrackIndex: idx, Title: &empty}}, false); err != nil {
		t.Fatal(err)
	}
	if got := titleOf(); got != "" {
		t.Errorf("title after clear = %q, want empty", got)
	}
}

func TestRemoveTracksDuplicateIndexes(t *testing.T) {
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
	a := before.StreamsOfType("audio")[1].Index
	if _, err := e.RemoveTracks(ctx, path, RemovalSpec{AudioIdx: []int{a, a}}, Options{}); err != nil {
		t.Fatalf("duplicate index: %v", err)
	}
	after, err := p.Probe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(after.StreamsOfType("audio")), len(before.StreamsOfType("audio"))-1; got != want {
		t.Errorf("audio tracks = %d, want %d", got, want)
	}
}

func TestEditMetadataKeepsUnchangedIETFLanguage(t *testing.T) {
	for _, bin := range []string{"mkvmerge", "mkvpropedit"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	dir := t.TempDir()
	path := makeFixture(t, dir)
	p := &probe.Prober{}
	e := &Engine{Prober: p}
	ctx := context.Background()
	res, err := p.Probe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	a := res.StreamsOfType("audio")[0]
	if out, err := exec.Command("mkvpropedit", path, "--edit", "track:"+strconv.Itoa(a.MkvID+1), "--set", "language=pt-BR").CombinedOutput(); err != nil {
		t.Fatalf("seed IETF tag: %v: %s", err, out)
	}
	res, err = p.Probe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	a = res.StreamsOfType("audio")[0]
	title := "Main"
	if _, err := e.EditMetadata(ctx, path, []MetadataEdit{{TrackIndex: a.Index, Language: a.Lang, Title: &title}}, false); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("mkvmerge", "-J", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var ident struct {
		Tracks []struct {
			ID         int `json:"id"`
			Properties struct {
				LanguageIETF string `json:"language_ietf"`
			} `json:"properties"`
		} `json:"tracks"`
	}
	if err := json.Unmarshal(out, &ident); err != nil {
		t.Fatal(err)
	}
	for _, tr := range ident.Tracks {
		if tr.ID == a.MkvID && tr.Properties.LanguageIETF != "pt-BR" {
			t.Errorf("language_ietf = %q after a title-only edit, want pt-BR", tr.Properties.LanguageIETF)
		}
	}
}

func TestRemovedBitratesKnown(t *testing.T) {
	res := &probe.Result{Duration: 60, Streams: []probe.Stream{
		{Index: 1, Type: "audio", BitRate: 640000},
		{Index: 2, Type: "audio"},
	}}
	if !removedBitratesKnown(res, RemovalSpec{AudioIdx: []int{1}}) {
		t.Error("stream with a bitrate reported unknown")
	}
	if removedBitratesKnown(res, RemovalSpec{AudioIdx: []int{1, 2}}) {
		t.Error("stream without a bitrate reported known; the 70% floor would refuse a valid removal")
	}
}
