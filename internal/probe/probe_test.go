package probe_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/krabhi4/muxprune/internal/probe"
)

func TestProbeTimeout(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "dummy.mkv")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &probe.Prober{Timeout: time.Nanosecond}
	if _, err := p.Probe(context.Background(), f); err == nil {
		t.Fatal("expected a timeout error from an already-expired probe deadline")
	}
}

func TestParseFFprobe_ZeroStreamsRejected(t *testing.T) {
	data := []byte(`{"streams":[],"format":{"format_name":"matroska,webm"}}`)
	if _, err := probe.ParseFFprobe(data, "x.mkv"); err == nil {
		t.Fatal("expected an error for a zero-stream probe result")
	}
}

func TestParseFFprobe_Valid(t *testing.T) {
	data := []byte(`{"streams":[
		{"index":0,"codec_type":"video","codec_name":"h264"},
		{"index":1,"codec_type":"audio","codec_name":"aac","tags":{"language":"eng"}}
	],"format":{"format_name":"matroska,webm","duration":"3.0","size":"1000"}}`)
	res, err := probe.ParseFFprobe(data, "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Streams) != 2 {
		t.Fatalf("want 2 streams, got %d", len(res.Streams))
	}
	if res.Streams[1].Lang != "eng" {
		t.Errorf("audio lang = %q, want eng", res.Streams[1].Lang)
	}
	if !res.IsMatroska() {
		t.Error("format should be detected as matroska")
	}
	if res.Size != 1000 || res.Duration != 3.0 {
		t.Errorf("format parse: size=%d duration=%v", res.Size, res.Duration)
	}
}
