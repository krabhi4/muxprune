package scan

import "testing"

func TestParsePath(t *testing.T) {
	cases := []struct {
		root, path string
		kind       string
		series     string
		season     int
		episode    string
		title      string
	}{
		{
			"/tv", "/tv/The Series Title! (2010) {tvdb-1520211}/Season 01/The Series Title! (2010) - S01E01 - Episode Title [WEBDL-1080p]-RlsGrp.mkv",
			"tv", "The Series Title! (2010)", 1, "S01E01", "The Series Title! (2010)",
		},
		{
			"/tv", "/tv/The Series Title! (2010)/Season 01/The Series Title! (2010) - S01E01-E03 - Episode Title.mkv",
			"tv", "The Series Title! (2010)", 1, "S01E01-E03", "The Series Title! (2010)",
		},
		{
			"/tv", "/tv/Daily Show (2010)/Season 2013/Daily Show - 2013-10-30 - Episode Title.mkv",
			"tv", "Daily Show (2010)", 2013, "2013-10-30", "Daily Show (2010)",
		},
		{
			"/tv", "/tv/Anime (2010) [tvdbid-1520211]/Specials/Anime - S00E01 - Special.mkv",
			"tv", "Anime (2010)", 0, "S00E01", "Anime (2010)",
		},
		{
			"/tv", "/tv/Flat Show/Flat.Show.S02E05.720p.HDTV.x264-GRP.mkv",
			"tv", "Flat Show", 2, "S02E05", "Flat Show",
		},
		{
			"/movies", "/movies/The Movie Title (2010) {tmdb-345691}/The Movie Title (2010) [Bluray-1080p]-RlsGrp.mkv",
			"movie", "", -1, "", "The Movie Title (2010)",
		},
		{
			"/movies", "/movies/loose-file.mkv",
			"other", "", -1, "", "loose-file",
		},
	}
	for _, c := range cases {
		got := ParsePath(c.root, c.path)
		if got.Kind != c.kind || got.Series != c.series || got.Season != c.season ||
			got.Episode != c.episode || got.Title != c.title {
			t.Errorf("%s:\n got  %+v\n want kind=%s series=%q season=%d episode=%q title=%q",
				c.path, got, c.kind, c.series, c.season, c.episode, c.title)
		}
	}
}
