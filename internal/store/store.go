// Package store is the SQLite persistence layer. A single connection is used
// (SetMaxOpenConns(1)): every caller is low-frequency and this sidesteps
// SQLITE_BUSY entirely.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS libraries (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	path TEXT NOT NULL UNIQUE,
	kind TEXT NOT NULL DEFAULT 'other',
	hardlink_policy TEXT NOT NULL DEFAULT 'skip'
);
CREATE TABLE IF NOT EXISTS media_files (
	id INTEGER PRIMARY KEY,
	library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
	path TEXT NOT NULL UNIQUE,
	size INTEGER NOT NULL,
	mtime INTEGER NOT NULL,
	nlink INTEGER NOT NULL DEFAULT 1,
	kind TEXT NOT NULL DEFAULT 'other',
	series TEXT NOT NULL DEFAULT '',
	season INTEGER NOT NULL DEFAULT -1,
	episode TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	video_codec TEXT NOT NULL DEFAULT '',
	audio_summary TEXT NOT NULL DEFAULT '',
	sub_summary TEXT NOT NULL DEFAULT '',
	sidecar_summary TEXT NOT NULL DEFAULT '',
	probe_json TEXT NOT NULL DEFAULT '',
	scanned_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_files_library ON media_files(library_id);
CREATE TABLE IF NOT EXISTS sidecars (
	id INTEGER PRIMARY KEY,
	media_file_id INTEGER NOT NULL REFERENCES media_files(id) ON DELETE CASCADE,
	path TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL,
	lang TEXT NOT NULL DEFAULT '',
	hi INTEGER NOT NULL DEFAULT 0,
	forced INTEGER NOT NULL DEFAULT 0,
	ext TEXT NOT NULL DEFAULT '',
	size INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sidecars_file ON sidecars(media_file_id);
CREATE TABLE IF NOT EXISTS jobs (
	id INTEGER PRIMARY KEY,
	type TEXT NOT NULL,
	media_file_id INTEGER REFERENCES media_files(id) ON DELETE SET NULL,
	file_path TEXT NOT NULL DEFAULT '',
	payload_json TEXT NOT NULL DEFAULT '{}',
	status TEXT NOT NULL DEFAULT 'queued',
	log TEXT NOT NULL DEFAULT '',
	bytes_saved INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`)
	return err
}

// ---- libraries ----

type Library struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Path           string `json:"path"`
	Kind           string `json:"kind"`            // tv, movie, other
	HardlinkPolicy string `json:"hardlink_policy"` // skip, proceed
}

func (s *Store) AddLibrary(l *Library) error {
	res, err := s.db.Exec(`INSERT INTO libraries(name,path,kind,hardlink_policy) VALUES(?,?,?,?)`,
		l.Name, l.Path, l.Kind, l.HardlinkPolicy)
	if err != nil {
		return err
	}
	l.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) UpdateLibrary(l *Library) error {
	_, err := s.db.Exec(`UPDATE libraries SET name=?,path=?,kind=?,hardlink_policy=? WHERE id=?`,
		l.Name, l.Path, l.Kind, l.HardlinkPolicy, l.ID)
	return err
}

func (s *Store) DeleteLibrary(id int64) error {
	_, err := s.db.Exec(`DELETE FROM libraries WHERE id=?`, id)
	return err
}

func (s *Store) GetLibrary(id int64) (*Library, error) {
	l := &Library{}
	err := s.db.QueryRow(`SELECT id,name,path,kind,hardlink_policy FROM libraries WHERE id=?`, id).
		Scan(&l.ID, &l.Name, &l.Path, &l.Kind, &l.HardlinkPolicy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return l, err
}

func (s *Store) ListLibraries() ([]Library, error) {
	rows, err := s.db.Query(`SELECT id,name,path,kind,hardlink_policy FROM libraries ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Library
	for rows.Next() {
		var l Library
		if err := rows.Scan(&l.ID, &l.Name, &l.Path, &l.Kind, &l.HardlinkPolicy); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ---- media files ----

type MediaFile struct {
	ID             int64     `json:"id"`
	LibraryID      int64     `json:"library_id"`
	Path           string    `json:"path"`
	Size           int64     `json:"size"`
	Mtime          int64     `json:"mtime"`
	Nlink          int       `json:"nlink"`
	Kind           string    `json:"kind"`
	Series         string    `json:"series"`
	Season         int       `json:"season"`
	Episode        string    `json:"episode"`
	Title          string    `json:"title"`
	VideoCodec     string    `json:"video_codec"`
	AudioSummary   string    `json:"audio_summary"`
	SubSummary     string    `json:"sub_summary"`
	SidecarSummary string    `json:"sidecar_summary"`
	ProbeJSON      string    `json:"-"`
	Sidecars       []Sidecar `json:"sidecars,omitempty"`
}

type Sidecar struct {
	ID     int64  `json:"id"`
	FileID int64  `json:"media_file_id"`
	Path   string `json:"path"`
	Name   string `json:"name"`
	Lang   string `json:"lang"`
	HI     bool   `json:"hi"`
	Forced bool   `json:"forced"`
	Ext    string `json:"ext"`
	Size   int64  `json:"size"`
}

// GetFileByPathMeta returns id and probe staleness info for incremental scans.
func (s *Store) GetFileByPathMeta(path string) (id, size, mtime int64, hasProbe bool, err error) {
	var probeLen int
	err = s.db.QueryRow(`SELECT id,size,mtime,length(probe_json) FROM media_files WHERE path=?`, path).
		Scan(&id, &size, &mtime, &probeLen)
	if err == sql.ErrNoRows {
		return 0, 0, 0, false, nil
	}
	return id, size, mtime, probeLen > 0, err
}

func (s *Store) UpsertMediaFile(f *MediaFile) error {
	res, err := s.db.Exec(`
INSERT INTO media_files(library_id,path,size,mtime,nlink,kind,series,season,episode,title,
	video_codec,audio_summary,sub_summary,sidecar_summary,probe_json,scanned_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(path) DO UPDATE SET
	library_id=excluded.library_id, size=excluded.size, mtime=excluded.mtime,
	nlink=excluded.nlink, kind=excluded.kind, series=excluded.series, season=excluded.season,
	episode=excluded.episode, title=excluded.title, video_codec=excluded.video_codec,
	audio_summary=excluded.audio_summary, sub_summary=excluded.sub_summary,
	sidecar_summary=excluded.sidecar_summary, probe_json=excluded.probe_json,
	scanned_at=excluded.scanned_at`,
		f.LibraryID, f.Path, f.Size, f.Mtime, f.Nlink, f.Kind, f.Series, f.Season, f.Episode,
		f.Title, f.VideoCodec, f.AudioSummary, f.SubSummary, f.SidecarSummary, f.ProbeJSON,
		time.Now().Unix())
	if err != nil {
		return err
	}
	if f.ID == 0 {
		if id, err := res.LastInsertId(); err == nil && id != 0 {
			f.ID = id
		}
		// LastInsertId is unreliable on upsert-update; fetch explicitly.
		if f.ID == 0 {
			err = s.db.QueryRow(`SELECT id FROM media_files WHERE path=?`, f.Path).Scan(&f.ID)
		}
	}
	return err
}

// TouchFile refreshes scan bookkeeping for files whose probe cache is still valid.
func (s *Store) TouchFile(id int64, nlink int, sidecarSummary string) error {
	_, err := s.db.Exec(`UPDATE media_files SET scanned_at=?, nlink=?, sidecar_summary=? WHERE id=?`,
		time.Now().Unix(), nlink, sidecarSummary, id)
	return err
}

// PruneFiles removes records for files not seen since the given scan start.
func (s *Store) PruneFiles(libraryID int64, scanStart int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM media_files WHERE library_id=? AND scanned_at<?`, libraryID, scanStart)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) ReplaceSidecars(fileID int64, scs []Sidecar) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM sidecars WHERE media_file_id=?`, fileID); err != nil {
		return err
	}
	for _, sc := range scs {
		if _, err := tx.Exec(`INSERT INTO sidecars(media_file_id,path,name,lang,hi,forced,ext,size)
			VALUES(?,?,?,?,?,?,?,?)`,
			fileID, sc.Path, sc.Name, sc.Lang, sc.HI, sc.Forced, sc.Ext, sc.Size); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteSidecar(id int64) error {
	_, err := s.db.Exec(`DELETE FROM sidecars WHERE id=?`, id)
	return err
}

func (s *Store) GetSidecar(id int64) (*Sidecar, error) {
	sc := &Sidecar{}
	err := s.db.QueryRow(`SELECT id,media_file_id,path,name,lang,hi,forced,ext,size FROM sidecars WHERE id=?`, id).
		Scan(&sc.ID, &sc.FileID, &sc.Path, &sc.Name, &sc.Lang, &sc.HI, &sc.Forced, &sc.Ext, &sc.Size)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return sc, err
}

type FileFilter struct {
	LibraryID int64
	Query     string
	Kind      string
	Limit     int
	Offset    int
	Sort      string
	Order     string
	Hardlinks string
	Subs      string
}

func (s *Store) ListFiles(f FileFilter) ([]MediaFile, int, error) {
	where := []string{"1=1"}
	var args []any
	if f.LibraryID > 0 {
		where = append(where, "library_id=?")
		args = append(args, f.LibraryID)
	}
	if f.Kind != "" {
		where = append(where, "kind=?")
		args = append(args, f.Kind)
	}
	if f.Query != "" {
		where = append(where, "(path LIKE ? OR series LIKE ? OR title LIKE ?)")
		q := "%" + f.Query + "%"
		args = append(args, q, q, q)
	}
	if f.Hardlinks == "yes" {
		where = append(where, "nlink > 1")
	} else if f.Hardlinks == "no" {
		where = append(where, "nlink = 1")
	}

	switch f.Subs {
	case "embedded":
		where = append(where, "sub_summary != ''")
	case "none_embedded":
		where = append(where, "sub_summary = ''")
	case "sidecar":
		where = append(where, "sidecar_summary != ''")
	case "none_sidecar":
		where = append(where, "sidecar_summary = ''")
	case "any":
		where = append(where, "(sub_summary != '' OR sidecar_summary != '')")
	case "none":
		where = append(where, "(sub_summary = '' AND sidecar_summary = '')")
	}

	cond := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRow(`SELECT count(*) FROM media_files WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}

	// Dynamic sorting with whitelisting
	var orderBy string
	switch f.Sort {
	case "size":
		orderBy = "size"
	case "mtime":
		orderBy = "mtime"
	case "nlink":
		orderBy = "nlink"
	case "scanned_at":
		orderBy = "scanned_at"
	case "title":
		orderBy = "title"
	default:
		orderBy = "series,season,episode,title,path"
	}

	orderDir := "ASC"
	if strings.ToLower(f.Order) == "desc" {
		orderDir = "DESC"
	}

	parts := strings.Split(orderBy, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i]) + " " + orderDir
	}
	sortClause := strings.Join(parts, ", ")

	rows, err := s.db.Query(`SELECT id,library_id,path,size,mtime,nlink,kind,series,season,episode,title,
		video_codec,audio_summary,sub_summary,sidecar_summary
		FROM media_files WHERE `+cond+` ORDER BY `+sortClause+` LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []MediaFile
	for rows.Next() {
		var m MediaFile
		if err := rows.Scan(&m.ID, &m.LibraryID, &m.Path, &m.Size, &m.Mtime, &m.Nlink, &m.Kind,
			&m.Series, &m.Season, &m.Episode, &m.Title, &m.VideoCodec,
			&m.AudioSummary, &m.SubSummary, &m.SidecarSummary); err != nil {
			return nil, 0, err
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

func (s *Store) GetFile(id int64) (*MediaFile, error) {
	m := &MediaFile{}
	err := s.db.QueryRow(`SELECT id,library_id,path,size,mtime,nlink,kind,series,season,episode,title,
		video_codec,audio_summary,sub_summary,sidecar_summary,probe_json
		FROM media_files WHERE id=?`, id).
		Scan(&m.ID, &m.LibraryID, &m.Path, &m.Size, &m.Mtime, &m.Nlink, &m.Kind, &m.Series,
			&m.Season, &m.Episode, &m.Title, &m.VideoCodec, &m.AudioSummary, &m.SubSummary,
			&m.SidecarSummary, &m.ProbeJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id,media_file_id,path,name,lang,hi,forced,ext,size
		FROM sidecars WHERE media_file_id=? ORDER BY name`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sc Sidecar
		if err := rows.Scan(&sc.ID, &sc.FileID, &sc.Path, &sc.Name, &sc.Lang, &sc.HI, &sc.Forced,
			&sc.Ext, &sc.Size); err != nil {
			return nil, err
		}
		m.Sidecars = append(m.Sidecars, sc)
	}
	return m, rows.Err()
}

// ---- jobs ----

type Job struct {
	ID         int64           `json:"id"`
	Type       string          `json:"type"` // remux, delete_sidecar
	FileID     sql.NullInt64   `json:"-"`
	FilePath   string          `json:"file_path"`
	Payload    json.RawMessage `json:"payload"`
	Status     string          `json:"status"` // queued, running, done, failed, skipped
	Log        string          `json:"log"`
	BytesSaved int64           `json:"bytes_saved"`
	CreatedAt  int64           `json:"created_at"`
	FinishedAt int64           `json:"finished_at"`
}

func (j *Job) MediaFileID() int64 {
	if j.FileID.Valid {
		return j.FileID.Int64
	}
	return 0
}

func (s *Store) CreateJob(jobType string, fileID int64, filePath string, payload any) (*Job, error) {
	pj, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO jobs(type,media_file_id,file_path,payload_json,status,created_at)
		VALUES(?,?,?,?,'queued',?)`, jobType, nullable(fileID), filePath, string(pj), now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Job{ID: id, Type: jobType, FileID: sql.NullInt64{Int64: fileID, Valid: fileID != 0},
		FilePath: filePath, Payload: pj, Status: "queued", CreatedAt: now}, nil
}

func nullable(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// ClaimNextJob atomically moves the oldest queued job to running.
func (s *Store) ClaimNextJob() (*Job, error) {
	j := &Job{}
	var payload string
	err := s.db.QueryRow(`UPDATE jobs SET status='running'
		WHERE id=(SELECT id FROM jobs WHERE status='queued' ORDER BY id LIMIT 1)
		RETURNING id,type,media_file_id,file_path,payload_json,created_at`).
		Scan(&j.ID, &j.Type, &j.FileID, &j.FilePath, &payload, &j.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.Status = "running"
	j.Payload = json.RawMessage(payload)
	return j, nil
}

func (s *Store) FinishJob(id int64, status, log string, bytesSaved int64) error {
	_, err := s.db.Exec(`UPDATE jobs SET status=?, log=?, bytes_saved=?, finished_at=? WHERE id=?`,
		status, log, bytesSaved, time.Now().Unix(), id)
	return err
}

// FailInterrupted marks jobs left 'running' by a previous process as failed.
func (s *Store) FailInterrupted() (int64, error) {
	res, err := s.db.Exec(`UPDATE jobs SET status='failed', log='interrupted by restart', finished_at=?
		WHERE status='running'`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) ListJobs(status string, limit int) ([]Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id,type,media_file_id,file_path,payload_json,status,log,bytes_saved,created_at,finished_at
		FROM jobs`
	var args []any
	if status != "" {
		q += ` WHERE status=?`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		var payload string
		if err := rows.Scan(&j.ID, &j.Type, &j.FileID, &j.FilePath, &payload, &j.Status, &j.Log,
			&j.BytesSaved, &j.CreatedAt, &j.FinishedAt); err != nil {
			return nil, err
		}
		j.Payload = json.RawMessage(payload)
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) Stats() (map[string]int64, error) {
	out := map[string]int64{}
	var saved, files, queued int64
	s.db.QueryRow(`SELECT coalesce(sum(bytes_saved),0) FROM jobs WHERE status='done'`).Scan(&saved)
	s.db.QueryRow(`SELECT count(*) FROM media_files`).Scan(&files)
	s.db.QueryRow(`SELECT count(*) FROM jobs WHERE status IN ('queued','running')`).Scan(&queued)
	out["bytes_saved"] = saved
	out["files"] = files
	out["active_jobs"] = queued
	return out, nil
}

// ---- settings ----

func (s *Store) GetSetting(key, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
