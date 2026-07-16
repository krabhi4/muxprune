package probe

import (
	"encoding/json"
	"testing"
)

func parseIdent(t *testing.T, raw string) mkvIdentify {
	t.Helper()
	var ident mkvIdentify
	if err := json.Unmarshal([]byte(raw), &ident); err != nil {
		t.Fatalf("bad canned mkvmerge json: %v", err)
	}
	return ident
}

func TestAlignMkvIDs(t *testing.T) {
	tests := []struct {
		name      string
		streams   []Stream
		mkvJSON   string
		wantErr   bool
		wantMkvID []int
	}{
		{
			name: "normal aligned maps mkv ids in order",
			streams: []Stream{
				{Index: 0, Type: "video", Lang: "eng", MkvID: -1},
				{Index: 1, Type: "audio", Lang: "eng", MkvID: -1},
				{Index: 2, Type: "audio", Lang: "jpn", MkvID: -1},
				{Index: 3, Type: "subtitle", Lang: "eng", MkvID: -1},
			},
			mkvJSON: `{"tracks":[
				{"id":0,"type":"video","properties":{"language":"eng"}},
				{"id":1,"type":"audio","properties":{"language":"eng"}},
				{"id":2,"type":"audio","properties":{"language":"jpn"}},
				{"id":3,"type":"subtitles","properties":{"language":"eng"}}
			]}`,
			wantErr:   false,
			wantMkvID: []int{0, 1, 2, 3},
		},
		{
			name: "language mismatch on reordered same-type tracks errors",
			streams: []Stream{
				{Index: 0, Type: "video", Lang: "eng", MkvID: -1},
				{Index: 1, Type: "audio", Lang: "eng", MkvID: -1},
				{Index: 2, Type: "audio", Lang: "jpn", MkvID: -1},
			},
			mkvJSON: `{"tracks":[
				{"id":0,"type":"video","properties":{"language":"eng"}},
				{"id":1,"type":"audio","properties":{"language":"jpn"}},
				{"id":2,"type":"audio","properties":{"language":"eng"}}
			]}`,
			wantErr: true,
		},
		{
			name: "missing language on one side is tolerated",
			streams: []Stream{
				{Index: 0, Type: "video", Lang: "", MkvID: -1},
				{Index: 1, Type: "audio", Lang: "eng", MkvID: -1},
			},
			mkvJSON: `{"tracks":[
				{"id":0,"type":"video","properties":{"language":"und"}},
				{"id":1,"type":"audio","properties":{"language":""}}
			]}`,
			wantErr:   false,
			wantMkvID: []int{0, 1},
		},
		{
			name: "non-language properties do not block alignment",
			streams: []Stream{
				{Index: 0, Type: "video", Lang: "eng", MkvID: -1},
				{Index: 1, Type: "audio", Lang: "eng", MkvID: -1},
			},
			mkvJSON: `{"tracks":[
				{"id":0,"type":"video","properties":{"language":"eng","codec_id":"V_MPEG4/ISO/AVC"}},
				{"id":1,"type":"audio","properties":{"language":"eng","codec_id":"A_AAC"}}
			]}`,
			wantErr:   false,
			wantMkvID: []int{0, 1},
		},
		{
			name: "count mismatch still errors",
			streams: []Stream{
				{Index: 0, Type: "video", Lang: "eng", MkvID: -1},
				{Index: 1, Type: "audio", Lang: "eng", MkvID: -1},
			},
			mkvJSON: `{"tracks":[
				{"id":0,"type":"video","properties":{"language":"eng"}}
			]}`,
			wantErr: true,
		},
		{
			name: "codec mismatch on same-language reordered tracks errors",
			streams: []Stream{
				{Index: 0, Type: "video", Lang: "eng", Codec: "h264", MkvID: -1},
				{Index: 1, Type: "audio", Lang: "eng", Codec: "aac", MkvID: -1},
				{Index: 2, Type: "audio", Lang: "eng", Codec: "ac3", MkvID: -1},
			},
			mkvJSON: `{"tracks":[
				{"id":0,"type":"video","properties":{"language":"eng","codec_id":"V_MPEG4/ISO/AVC"}},
				{"id":1,"type":"audio","properties":{"language":"eng","codec_id":"A_AC3"}},
				{"id":2,"type":"audio","properties":{"language":"eng","codec_id":"A_AAC"}}
			]}`,
			wantErr: true,
		},
		{
			name: "matching codecs align",
			streams: []Stream{
				{Index: 0, Type: "video", Lang: "eng", Codec: "h264", MkvID: -1},
				{Index: 1, Type: "audio", Lang: "eng", Codec: "aac", MkvID: -1},
			},
			mkvJSON: `{"tracks":[
				{"id":0,"type":"video","properties":{"language":"eng","codec_id":"V_MPEG4/ISO/AVC"}},
				{"id":1,"type":"audio","properties":{"language":"eng","codec_id":"A_AAC"}}
			]}`,
			wantErr:   false,
			wantMkvID: []int{0, 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := &Result{Format: "matroska,webm", Streams: tt.streams}
			ident := parseIdent(t, tt.mkvJSON)
			err := alignMkvIDs(res, ident)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			for i, want := range tt.wantMkvID {
				if res.Streams[i].MkvID != want {
					t.Errorf("stream %d MkvID = %d, want %d", i, res.Streams[i].MkvID, want)
				}
			}
		})
	}
}

func TestLangsCompatible(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"eng", "eng", true},
		{"eng", "jpn", false},
		{"", "eng", true},
		{"eng", "", true},
		{"", "", true},
		{"und", "eng", false},
	}
	for _, tt := range tests {
		if got := langsCompatible(tt.a, tt.b); got != tt.want {
			t.Errorf("langsCompatible(%q,%q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestCodecsCompatible(t *testing.T) {
	cases := []struct {
		mkv, ff string
		want    bool
	}{
		{"V_MPEG4/ISO/AVC", "h264", true},
		{"V_MPEGH/ISO/HEVC", "hevc", true},
		{"A_AAC", "aac", true},
		{"A_AC3", "ac3", true},
		{"A_EAC3", "eac3", true},
		{"A_DTS", "dts", true},
		{"A_TRUEHD", "truehd", true},
		{"A_OPUS", "opus", true},
		{"A_FLAC", "flac", true},
		{"S_TEXT/UTF8", "subrip", true},
		{"S_TEXT/ASS", "ass", true},
		{"S_HDMV/PGS", "hdmv_pgs_subtitle", true},
		{"S_VOBSUB", "dvd_subtitle", true},
		{"A_AAC", "ac3", false},
		{"V_MPEG4/ISO/AVC", "hevc", false},
		{"", "aac", true},
		{"A_AAC", "", true},
		{"X_UNKNOWN_CODEC", "somecodec", true},
	}
	for _, c := range cases {
		if got := codecsCompatible(c.mkv, c.ff); got != c.want {
			t.Errorf("codecsCompatible(%q,%q) = %v, want %v", c.mkv, c.ff, got, c.want)
		}
	}
}

func TestSafePathArg(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"absolute path unchanged", "/media/movies/film.mkv", "/media/movies/film.mkv"},
		{"dash-leading relative path guarded", "-foo.mkv", "./-foo.mkv"},
		{"at-leading relative path guarded", "@foo.mkv", "./@foo.mkv"},
		{"plain relative path guarded", "foo.mkv", "./foo.mkv"},
		{"absolute dash-containing path unchanged", "/tmp/-weird.mkv", "/tmp/-weird.mkv"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := safePathArg(tt.in)
			if got != tt.want {
				t.Errorf("safePathArg(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if got[0] == '-' || got[0] == '@' {
				t.Errorf("safePathArg(%q) = %q still begins with an option-like prefix", tt.in, got)
			}
		})
	}
}
