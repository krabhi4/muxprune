// Package probe inspects media containers using ffprobe and, for Matroska
// files, mkvmerge's JSON identify output. ffprobe stream indexes and mkvmerge
// track IDs are independent numbering schemes; both are kept because the
// remux engine picks its tool at run time.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

type Stream struct {
	Index         int    `json:"index"`
	MkvID         int    `json:"mkv_id"` // -1 when unknown / not an MKV
	Type          string `json:"type"`   // video, audio, subtitle, attachment, data
	Codec         string `json:"codec"`
	Lang          string `json:"lang"` // ISO 639-2 tag as stored, "" when absent
	Title         string `json:"title"`
	Channels      int    `json:"channels,omitempty"`
	ChannelLayout string `json:"channel_layout,omitempty"`
	Default       bool   `json:"default"`
	Forced        bool   `json:"forced"`
	BitRate       int64  `json:"bit_rate,omitempty"` // bits/s, 0 when unknown
}

type Result struct {
	Path     string   `json:"path"`
	Format   string   `json:"format"` // ffprobe format_name, e.g. "matroska,webm"
	Duration float64  `json:"duration"`
	Size     int64    `json:"size"`
	Streams  []Stream `json:"streams"`
}

func (r *Result) IsMatroska() bool { return strings.Contains(r.Format, "matroska") }

func (r *Result) StreamsOfType(t string) []Stream {
	var out []Stream
	for _, s := range r.Streams {
		if s.Type == t {
			out = append(out, s)
		}
	}
	return out
}

// Prober shells out to ffprobe/mkvmerge. Binary paths are resolved once.
type Prober struct {
	once     sync.Once
	ffprobe  string
	mkvmerge string
}

func (p *Prober) resolve() {
	p.once.Do(func() {
		p.ffprobe, _ = exec.LookPath("ffprobe")
		p.mkvmerge, _ = exec.LookPath("mkvmerge")
	})
}

func (p *Prober) HasFFprobe() bool  { p.resolve(); return p.ffprobe != "" }
func (p *Prober) HasMkvmerge() bool { p.resolve(); return p.mkvmerge != "" }

type ffprobeOut struct {
	Streams []struct {
		Index         int    `json:"index"`
		CodecType     string `json:"codec_type"`
		CodecName     string `json:"codec_name"`
		Channels      int    `json:"channels"`
		ChannelLayout string `json:"channel_layout"`
		BitRate       string `json:"bit_rate"`
		Disposition   struct {
			Default int `json:"default"`
			Forced  int `json:"forced"`
		} `json:"disposition"`
		Tags map[string]string `json:"tags"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		Size       string `json:"size"`
	} `json:"format"`
}

func (p *Prober) Probe(ctx context.Context, path string) (*Result, error) {
	p.resolve()
	if p.ffprobe == "" {
		return nil, fmt.Errorf("ffprobe not found in PATH")
	}
	out, err := exec.CommandContext(ctx, p.ffprobe,
		"-v", "error", "-print_format", "json", "-show_format", "-show_streams", path).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe %s: %w (%s)", path, err, exitDetail(err))
	}
	var raw ffprobeOut
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("ffprobe %s: bad json: %w", path, err)
	}
	res := &Result{Path: path, Format: raw.Format.FormatName}
	res.Duration, _ = strconv.ParseFloat(raw.Format.Duration, 64)
	res.Size, _ = strconv.ParseInt(raw.Format.Size, 10, 64)
	for _, s := range raw.Streams {
		st := Stream{
			Index:         s.Index,
			MkvID:         -1,
			Type:          s.CodecType,
			Codec:         s.CodecName,
			Channels:      s.Channels,
			ChannelLayout: s.ChannelLayout,
			Default:       s.Disposition.Default == 1,
			Forced:        s.Disposition.Forced == 1,
			Lang:          s.Tags["language"],
			Title:         s.Tags["title"],
		}
		if st.Lang == "" {
			st.Lang = s.Tags["LANGUAGE"]
		}
		if st.Title == "" {
			st.Title = s.Tags["TITLE"]
		}
		if br, err := strconv.ParseInt(s.BitRate, 10, 64); err == nil {
			st.BitRate = br
		} else if bps, err := strconv.ParseInt(s.Tags["BPS"], 10, 64); err == nil {
			st.BitRate = bps
		}
		res.Streams = append(res.Streams, st)
	}
	if res.IsMatroska() && p.mkvmerge != "" {
		if err := p.attachMkvIDs(ctx, res); err != nil {
			// Non-fatal: engine falls back to ffmpeg for this file.
			for i := range res.Streams {
				res.Streams[i].MkvID = -1
			}
		}
	}
	return res, nil
}

type mkvIdentify struct {
	Tracks []struct {
		ID   int    `json:"id"`
		Type string `json:"type"` // video, audio, subtitles
	} `json:"tracks"`
}

// attachMkvIDs aligns mkvmerge tracks to ffprobe streams. Both list tracks in
// container order, but ffprobe additionally reports attachments/data streams,
// so alignment is done per type position, not by raw index.
func (p *Prober) attachMkvIDs(ctx context.Context, res *Result) error {
	out, err := exec.CommandContext(ctx, p.mkvmerge, "-J", res.Path).Output()
	if err != nil {
		return fmt.Errorf("mkvmerge -J: %w", err)
	}
	var ident mkvIdentify
	if err := json.Unmarshal(out, &ident); err != nil {
		return fmt.Errorf("mkvmerge -J: bad json: %w", err)
	}
	typeMap := map[string]string{"video": "video", "audio": "audio", "subtitles": "subtitle"}
	ids := map[string][]int{}
	for _, t := range ident.Tracks {
		ft, ok := typeMap[t.Type]
		if !ok {
			continue
		}
		ids[ft] = append(ids[ft], t.ID)
	}
	seen := map[string]int{}
	for i, s := range res.Streams {
		list := ids[s.Type]
		pos := seen[s.Type]
		if pos < len(list) {
			res.Streams[i].MkvID = list[pos]
		}
		seen[s.Type]++
	}
	// Count mismatch means our alignment assumption is off for this file;
	// report it so callers drop to the ffmpeg path.
	for _, t := range []string{"video", "audio", "subtitle"} {
		if len(ids[t]) != len(res.StreamsOfType(t)) {
			return fmt.Errorf("track count mismatch for %s: mkvmerge=%d ffprobe=%d",
				t, len(ids[t]), len(res.StreamsOfType(t)))
		}
	}
	return nil
}

func exitDetail(err error) string {
	if ee, ok := err.(*exec.ExitError); ok {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return err.Error()
}
