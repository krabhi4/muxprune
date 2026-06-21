# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.4.1] - 2026-06-21

### Fixed
- Remux no longer restores the original modification time onto the rewritten file. Previously, after pruning tracks, muxprune reset the file's `mtime` back to its pre-prune value, so external media servers (Jellyfin, Sonarr, Plex) never detected the content change: they kept displaying the stale, pre-prune track/subtitle list and, in Jellyfin's case, kept serving already-extracted (now-removed) subtitle tracks from its own cache. The remuxed file now keeps a fresh modification time so downstream scanners re-probe and reflect the real track layout. File ownership and permissions (`PUID`/`PGID`) are still preserved.

## [0.4.0] - 2026-06-21

### Added
- Automatic library monitoring so the catalog stays in sync without manual scans. Each library gets a configurable periodic auto-scan (default every 6 hours) plus an optional real-time filesystem watcher (`fsnotify`) that triggers a debounced rescan when files change. The watcher auto-disables on network/union mounts (NFS/SMB/CIFS/FUSE) where inotify is unreliable and falls back to the periodic scan; watcher failures surface as a status instead of dying silently.
- Per-library `auto_scan_interval` and `watch_enabled` settings, exposed in the Web UI library dialog (interval dropdown + watch toggle) and the libraries list (auto-scan cadence + live watch-status badge: watching/polling/disabled/error). New env vars `MUXPRUNE_AUTOSCAN_DEFAULT` (default interval for new libraries) and `MUXPRUNE_WATCH` (global watcher kill-switch).

### Fixed
- Mass-deletion guard for scans: a scan now aborts (instead of pruning) when a library root is missing or unreadable, and skips pruning when a completed walk finds zero files while records still exist. This stops a transiently-unmounted volume from wiping the library catalog, which matters now that scans can run automatically.

## [0.3.1] - 2026-06-16

### Added
- Collapsible TV series grouping in the Files list to consolidate episode lists under their respective show titles.
- Shift-click range checkbox selection support in the Files list to make mass editing more efficient.

### Fixed
- Forced ffmpeg to use the `mp4` muxer instead of `ipod` for `.m4v` and `.mp4` containers, preventing remux failures on HEVC video streams and non-standard data streams (e.g. `bin_data` with `text` tags).

## [0.3.0] - 2026-06-15

### Added
- Multi-view client-side URL routing and pagination across Files, Libraries, and Jobs views, ensuring persistent navigation states on page refresh.
- Persistent database-backed scanning queue converting background library scanning operations into robust `scan_library` and `scan_all` jobs processed by the runner.
- Job cancellation capability (`POST /api/v1/jobs/{id}/cancel` and a Cancel button in the Web UI) allowing users to safely abort queued jobs.
- Job deletion support (`DELETE /api/v1/jobs/{id}` and a Delete button in the Web UI) to remove completed/failed/cancelled jobs from the database history.
- Upgraded Model Context Protocol (MCP) server integration with new tools: `queue_scan_job`, `queue_scan_all_job`, `cancel_job`, `delete_job`, and `list_jobs`, plus paginated and sorted outputs in `list_files`.

### Changed
- Refactored scanning logic to run entirely as queued background jobs instead of using in-memory mutex locking.

## [0.2.3] - 2026-06-15

### Added
- Pagination support and status filtering to the Jobs view in the Web UI to prevent lists from getting cut off on large queues.
- Support for limit and offset parameters in the `/api/v1/jobs` API endpoint and `ListJobs` database store helper.

## [0.2.2] - 2026-06-15

### Added
- Native HTTP/SSE MCP transport support to the `serve` web server, permitting AI agents to connect directly to the running web application via `/sse` or `/api/v1/mcp/sse`.

## [0.2.1] - 2026-06-15

### Added
- Native Model Context Protocol (MCP) server subcommand (`muxprune mcp`) over standard input/output (stdio), exposing comprehensive media library control, searching, inspection, and job queue management capabilities directly to AI agents.
- New `GetJob` database store helper method to fetch logs and status metrics for individual jobs.

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
