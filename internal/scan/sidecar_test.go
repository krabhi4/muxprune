package scan

import "testing"

func TestMatchSidecar(t *testing.T) {
	base := "The Movie Title (2010) [Bluray-1080p][DTS 5.1][x264]-RlsGrp"
	cases := []struct {
		name   string
		ok     bool
		lang   string
		hi     bool
		forced bool
		ext    string
	}{
		{base + ".en.srt", true, "en", false, false, "srt"},
		{base + ".en.hi.srt", true, "en", true, false, "srt"},
		{base + ".en.sdh.srt", true, "en", true, false, "srt"},
		{base + ".fr.forced.srt", true, "fr", false, true, "srt"},
		{base + ".pt-BR.srt", true, "pt-br", false, false, "srt"},
		{base + ".srt", true, "", false, false, "srt"},
		{base + ".eng.ass", true, "eng", false, false, "ass"},
		{base + ".EN.SRT", true, "en", false, false, "srt"},
		{base + ".idx", true, "", false, false, "idx"},
		{base + ".sup", true, "", false, false, "sup"},
		// not sidecars of this base
		{base + ".mkv", false, "", false, false, ""},
		{base + ".nfo", false, "", false, false, ""},
		{"Other Movie (2011).en.srt", false, "", false, false, ""},
		{base + "x.en.srt", false, "", false, false, ""}, // prefix but no dot boundary
	}
	for _, c := range cases {
		got, ok := MatchSidecar(c.name, base)
		if ok != c.ok {
			t.Errorf("%s: ok=%v want %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Lang != c.lang || got.HI != c.hi || got.Forced != c.forced || got.Ext != c.ext {
			t.Errorf("%s: got %+v want lang=%s hi=%v forced=%v ext=%s", c.name, got, c.lang, c.hi, c.forced, c.ext)
		}
	}
}

func TestMatchSidecarDotSceneName(t *testing.T) {
	// {Original Title} naming: dots everywhere. The boundary rule still holds.
	base := "The.Series.Title.S01E01.1080p.AMZN.WEB-DL.DDP5.1.H.264-RlsGrp"
	got, ok := MatchSidecar(base+".en.srt", base)
	if !ok || got.Lang != "en" {
		t.Fatalf("scene name sidecar: ok=%v got=%+v", ok, got)
	}
}
