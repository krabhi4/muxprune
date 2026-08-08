# muxprune

`muxprune` is a lightweight, self-hosted Docker application that scans your *arr-managed media libraries (Sonarr/Radarr) and lets you surgically remove unwanted audio tracks and subtitle streams (both embedded and sidecar files), losslessly, without re-encoding.

Unlike heavy transcode pipelines (such as Tdarr or Unmanic) that focus on re-encoding video tracks, `muxprune` is designed solely to keep your library clean and save gigabytes of disk space by stripping out foreign languages, commentaries, duplicate tracks, and bloated subtitle lists instantly.

---

## Key Features

- **Lossless Container Remuxing:** Strips audio/subtitle streams using `mkvmerge` (for MKV containers) or `ffmpeg` stream-copy fallback (for MP4/others). Video is never touched, meaning processing is I/O-bound and takes seconds.
- **Sidecar Subtitle Support:** Automatically scans, groups, and cleans up sidecar files (SRT, ASS, VTT, etc.) matching Bazarr/release-group structures next to media files.
- **Safety Pipeline:** Writes to a temporary file in the same directory, verifies stream counts, duration, and output size before atomically swapping the original file to prevent corruption.
- **Hardlink Safety Guard:** Detects if a media file has multiple hardlinks (e.g. still seeding in your torrent client). Skips or warns by default to prevent breaking links or doubling space.
- **Recycle Bin:** Deleted sidecar files can be moved to a temporary recycle folder and automatically purged after a configurable number of days.
- **Incremental Scanner:** Crawls TV and Movie roots, parses basenames best-effort, tracks file size, modification times, link counts, and caches metadata in a CGO-free SQLite database.
- **Automatic Library Monitoring:** Keeps the catalog in sync without manual scans via a configurable per-library periodic auto-scan (default every 6 hours), plus an optional real-time `fsnotify` watcher that triggers a debounced rescan on local filesystem changes. The watcher auto-disables on network/union mounts (NFS/SMB/CIFS/FUSE) where inotify is unreliable, falling back to the periodic scan. A built-in guard means a transiently-unmounted volume can never wipe your catalog.
- **Webhooks:** Integrates with Sonarr/Radarr "On Import" webhooks to instantly scan newly imported files.
- **REST API & Utilitarian Web UI:** Utility-driven dark mode UI to browse inventory, inspect streams, queue single or batch jobs, see dry-run size estimates, and track active queue status.
- **Developer CLI Mode:** Can be run directly as a CLI tool (`muxprune inspect` / `muxprune strip`) without launching the web server.

---

## Docker Compose Quickstart

Deploy `muxprune` alongside your *arr stack. Mount the same media root folders and configure the matching `PUID`/`PGID` to preserve file ownership.

```yaml
services:
  muxprune:
    image: ghcr.io/krabhi4/muxprune:latest
    container_name: muxprune
    environment:
      - PUID=1000
      - PGID=1000
      - TZ=Etc/UTC
      - UMASK=022
      # - MUXPRUNE_API_KEY=your-secret-api-key # Requires X-Api-Key header on API endpoints
      # - MUXPRUNE_WORKERS=1                   # Concurrent remux jobs (1 is recommended for HDDs/SSDs)
      # - MUXPRUNE_RECYCLE_DAYS=7              # Keep deleted sidecars in config/recycle (0 = delete permanently)
      # - MUXPRUNE_AUTOSCAN_DEFAULT=21600     # Default auto-scan interval (sec) for new libraries (0 = off, min 60)
      # - MUXPRUNE_WATCH=1                     # Real-time fsnotify watcher (0 = periodic scans only)
    volumes:
      - ./config:/config
      - /data/media/tv:/tv
      - /data/media/movies:/movies
    ports:
      - "8484:8484"
    restart: unless-stopped
```

---

## Configuration & Environment Variables

| Variable | Description | Default |
| -------- | ----------- | ------- |
| `MUXPRUNE_PORT` | Port for the web interface and REST API | `8484` |
| `MUXPRUNE_CONFIG` | Directory for the SQLite database, logs, and recycle folder | `/config` (Docker) or `./data` |
| `MUXPRUNE_WORKERS` | Number of concurrent background remux workers | `1` |
| `MUXPRUNE_RECYCLE_DAYS` | Number of days to keep deleted sidecar subtitles in recycle | `7` |
| `MUXPRUNE_AUTOSCAN_DEFAULT` | Default auto-scan interval (seconds) for newly added libraries; `0` disables, minimum `60` | `21600` (6h) |
| `MUXPRUNE_WATCH` | Enable the real-time filesystem watcher (`0`/`false`/`off` uses periodic scans only) | `1` (on) |
| `MUXPRUNE_API_KEY` | Optional authorization key for the REST API | (none, open API) |
| `MUXPRUNE_BROWSE_ROOTS` | Colon-separated directories the folder picker may list, and the only places external merge inputs may come from. Unset falls back to your library paths plus conventional media mounts (`/media`, `/mnt`, `/data`, `/tv`, `/movies`, `/music`, `/srv`, `/storage`, `/Volumes`) and your home directory | (see left) |
| `MUXPRUNE_SECURE_COOKIE` | Force the `Secure` flag on the session cookie. Only needed behind a TLS-terminating proxy that does not set `X-Forwarded-Proto` | `0` (auto-detected) |
| `PUID` / `PGID` | User and Group ID mapping to match media folder ownership | `1000`/`1000` |
| `UMASK` | File permissions mask for newly created files | `022` |

---

## CLI Usage

If running outside Docker or inside the container terminal, you can interact with the app via the CLI:

### Serve the API & Web UI (Default)
```bash
muxprune serve -port 8484 -config ./data
```

### Inspect a File
Prints a full stream inventory of a file with index IDs, codec, language tags, channels, and layout:
```bash
muxprune inspect /media/movies/Movie.mkv
```

### Strip Tracks from a File
Remuxes a file to strip specified stream index IDs (use `inspect` to find the indices):
```bash
# Strip audio track 1 and subtitle track 3
muxprune strip --audio 1 --subs 3 /media/movies/Movie.mkv

# Dry-run mode: show the command and estimated size savings without modifying the file
muxprune strip --audio 1,2 --all-subs --dry-run /media/movies/Movie.mkv
```

---

## License

This project is licensed under the [MIT License](LICENSE).
