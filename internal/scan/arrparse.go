package scan

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Parsed is best-effort display grouping derived from Sonarr/Radarr folder
// conventions. It must never be used for anything destructive; operations key
// on absolute paths only.
type Parsed struct {
	Kind    string `json:"kind"` // tv, movie, other
	Series  string `json:"series,omitempty"`
	Season  int    `json:"season"` // -1 when unknown
	Episode string `json:"episode,omitempty"`
	Title   string `json:"title"` // movie folder name or series name
}

var (
	seasonDirRe = regexp.MustCompile(`(?i)^season[ ._-]*(\d{1,4})$`)
	episodeRe   = regexp.MustCompile(`(?i)\bS(\d{1,2})E(\d{1,3})(?:-?E(\d{1,3}))?\b`)
	airDateRe   = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})\b`)
	// strip {tvdb-123} / {imdb-tt123} / [tmdbid-123] style ID tags from folder names
	idTagRe = regexp.MustCompile(`\s*[\[{](?:tvdb|tvdbid|imdb|imdbid|tmdb|tmdbid)-[^\]}]+[\]}]`)
)

// ParsePath classifies a video file relative to its library root.
func ParsePath(root, path string) Parsed {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return Parsed{Kind: "other", Season: -1, Title: filepath.Base(path)}
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	fileName := parts[len(parts)-1]
	dirs := parts[:len(parts)-1]

	// TV layout: Series/Season XX/file or Series/Specials/file
	if len(dirs) >= 2 {
		seasonDir := dirs[len(dirs)-1]
		if m := seasonDirRe.FindStringSubmatch(seasonDir); m != nil {
			n, _ := strconv.Atoi(m[1])
			return Parsed{
				Kind:    "tv",
				Series:  cleanFolderName(dirs[len(dirs)-2]),
				Season:  n,
				Episode: episodeLabel(fileName),
				Title:   cleanFolderName(dirs[len(dirs)-2]),
			}
		}
		if strings.EqualFold(seasonDir, "specials") {
			return Parsed{
				Kind:    "tv",
				Series:  cleanFolderName(dirs[len(dirs)-2]),
				Season:  0,
				Episode: episodeLabel(fileName),
				Title:   cleanFolderName(dirs[len(dirs)-2]),
			}
		}
	}

	// Episode marker in the file name but no season folder: still TV.
	if m := episodeRe.FindStringSubmatch(fileName); m != nil {
		season, _ := strconv.Atoi(m[1])
		series := ""
		if len(dirs) >= 1 {
			series = cleanFolderName(dirs[len(dirs)-1])
		}
		return Parsed{Kind: "tv", Series: series, Season: season, Episode: episodeLabel(fileName), Title: series}
	}

	// Movie layout: Movie Folder/file
	if len(dirs) >= 1 {
		return Parsed{Kind: "movie", Season: -1, Title: cleanFolderName(dirs[len(dirs)-1])}
	}
	return Parsed{Kind: "other", Season: -1, Title: strings.TrimSuffix(fileName, filepath.Ext(fileName))}
}

func episodeLabel(fileName string) string {
	if m := episodeRe.FindStringSubmatch(fileName); m != nil {
		label := "S" + pad2(m[1]) + "E" + pad2(m[2])
		if m[3] != "" {
			label += "-E" + pad2(m[3])
		}
		return label
	}
	if m := airDateRe.FindStringSubmatch(fileName); m != nil {
		return m[1]
	}
	return ""
}

func pad2(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

func cleanFolderName(name string) string {
	return strings.TrimSpace(idTagRe.ReplaceAllString(name, ""))
}
