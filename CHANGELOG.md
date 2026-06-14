# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.0] - 2026-06-15

### Added
- In-place Matroska header editing with `mkvpropedit` integration, enabling instantaneous updates (<100ms) to track titles, languages, default flags, and forced flags without rewriting the file.
- Interactive multiplexing editor utilizing `mkvmerge` for track reordering and merging of external subtitle and audio files.
- Sleek and responsive UI controls in the file details dialog for reordering tracks (up/down arrows), editing stream metadata fields (language, title, default/forced flags), and specifying external files for merging.
- Low-priority CPU and disk I/O scheduling via `nice -n 19` and `ionice -c 3` wrapper to keep the host fully responsive during remuxing tasks.

### Changed
- Optimized scanner performance by replacing individual database updates for unchanged files with a bulk-touch operation (`TouchFilesBulk`) executed at the end of a scan.
- Reduced filesystem syscalls by caching sidecar file sizes during the directory walk, eliminating redundant `os.Stat` calls.
- Hardened API and engine layer validations to reject duplicate track order inputs and self-merging requests.

## [0.1.0] - 2026-06-12

### Added
- Initial release of **muxprune**, a self-hosted media track removal utility.
- Lightweight, CGO-free, single-binary architecture built with Go and SQLite.
- Lossless container remuxing engine using `mkvmerge` (MKV) and `ffmpeg` (MP4/other fallbacks) to strip streams without transcoding.
- Sidecar subtitle file scanner and cleaner matching Sonarr/Radarr/Bazarr structures.
- Incremental library scanning and media files cataloging with hardlink safety detection.
- SQLite-backed asynchronous job queue and single-threaded worker manager.
- REST API and embedded Web UI for browsing media inventory, inspecting tracks, and queuing strip jobs.
- Rich sorting (size, title, modification date, hardlink count, scan date) and advanced filters (media kind, hardlink status, subtitle/sidecar presence) in the Files view.
- Interactive, server-side folder browser in the Web UI for selecting library paths instead of typing them.
- Docker multi-arch support (`linux/amd64`, `linux/arm64`) with user ID mapping (`PUID`/`PGID`) and integrated health checks.
- Dependabot config to auto-group minor/patch dependency upgrades for Go, Docker, and GitHub Actions.
- Robust CI/CD workflows for automated linting (golangci-lint, hadolint, govulncheck), tests, and GHCR package publishing.

### Fixed
- Fixed duration drift verification failures on full-length movie files by replacing the hardcoded 1.5s limit with a percentage-based tolerance (allows up to 1% or max 60s change when removing longer trailing commentary or subtitle tracks).
