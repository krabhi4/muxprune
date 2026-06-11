package scan

import (
	"regexp"
	"strings"
)

// Sidecar is an external subtitle file living next to a video file, following
// the Bazarr/Plex convention:
//
//	<video basename>[.<lang>[-<region>]][.hi|.sdh|.cc|.forced].<ext>
type Sidecar struct {
	Name   string `json:"name"` // file name only
	Lang   string `json:"lang"` // "" when no language token present
	HI     bool   `json:"hi"`
	Forced bool   `json:"forced"`
	Ext    string `json:"ext"`
	Size   int64  `json:"size"`
}

var subtitleExts = map[string]bool{
	"srt": true, "ass": true, "ssa": true, "sub": true, "idx": true,
	"vtt": true, "smi": true, "sup": true,
}

var langToken = regexp.MustCompile(`^[a-zA-Z]{2,3}(-[a-zA-Z]{2,4})?$`)

var flagTokens = map[string]string{
	"hi": "hi", "sdh": "hi", "cc": "hi",
	"forced": "forced", "foreign": "forced",
}

func IsSubtitleExt(ext string) bool { return subtitleExts[strings.ToLower(ext)] }

// MatchSidecar reports whether name is a sidecar subtitle for a video with
// the given basename (file name without extension), and parses its tokens.
func MatchSidecar(name, videoBase string) (Sidecar, bool) {
	if len(name) <= len(videoBase) || !strings.HasPrefix(name, videoBase) || name[len(videoBase)] != '.' {
		return Sidecar{}, false
	}
	rest := name[len(videoBase)+1:] // tokens after "basename."
	parts := strings.Split(rest, ".")
	ext := strings.ToLower(parts[len(parts)-1])
	if !subtitleExts[ext] {
		return Sidecar{}, false
	}
	sc := Sidecar{Name: name, Ext: ext}
	for _, tok := range parts[:len(parts)-1] {
		lower := strings.ToLower(tok)
		switch {
		case flagTokens[lower] == "hi":
			sc.HI = true
		case flagTokens[lower] == "forced":
			sc.Forced = true
		case sc.Lang == "" && langToken.MatchString(tok):
			sc.Lang = lower
		default:
			// Unknown middle token (e.g. release junk). Still a subtitle by
			// extension, keep it with what we parsed so far.
		}
	}
	return sc, true
}
