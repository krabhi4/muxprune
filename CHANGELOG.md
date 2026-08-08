# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.6.0] - 2026-08-08

Follow-up release to the 0.5.0 hardening pass. A full-codebase security audit produced 35
findings; re-verifying each against the code confirmed 33 (3 were false positives) and
surfaced 1 more the audit had missed. All 33 are addressed here.

### Security
- Filesystem browsing and library paths now resolve symlinks before the allow-list check. Previously a symlink inside an allowed root was validated by its own name while `os.ReadDir` and the scanner followed it to the target, so a link placed in a media folder could enumerate arbitrary directories or attach a library rooted outside the jail. Library paths are stored resolved.
- Browsing no longer fails open. The jail was skipped entirely whenever the allow-list came back empty, which is the state of every fresh install (no `MUXPRUNE_BROWSE_ROOTS`, no libraries yet), leaving the whole filesystem enumerable through the folder picker. With nothing configured it now falls back to conventional media mounts (`/media`, `/mnt`, `/data`, `/tv`, `/movies`, `/music`, `/srv`, `/storage`, `/Volumes`) plus the home directory, and walking down to a root through a parent directory lists only the segments that lead there.
- External merge inputs are confined to the allowed roots (browse roots plus every library path), resolved through symlinks first. Track merging previously accepted any path on the host that mkvmerge could read. The REST handler, the MCP tool, and the job runner now share one validator instead of three drifting copies.
- The session cookie carries `Secure` when the request arrived over TLS or through a proxy setting `X-Forwarded-Proto: https`, and `MUXPRUNE_SECURE_COOKIE=1` forces it for proxies that set neither. It is not set unconditionally, which would make the cookie unusable on the plain-HTTP loopback default.
- Cookie-authenticated state-changing requests must carry `X-Requested-With: muxprune`. `SameSite=Strict` already blocked the classic cross-site form post; this closes the gap for clients that do not enforce it. API-key requests and all reads are exempt.
- API and MCP responses no longer echo raw database, filesystem, or external-tool error text. Messages authored by muxprune are returned verbatim; everything else is logged server-side and replaced with a generic message, so `os.Stat` and `os.ReadDir` failures stop working as a filesystem probe.
- Authenticated traffic is rate limited per client IP (600 reads and 120 writes per minute) rather than only failed logins.
- Sessions expire after 24 hours of inactivity in addition to the 30-day absolute TTL, so an abandoned or stolen token is not usable for a month.
- Every array input is bounded on both the REST API and the MCP tools: 1000 batch file ids, 100 stream indexes, 100 metadata edits, 100 external merge files, 1000 track-order entries. Deep pagination is clamped (offset capped at 100,000) so a large `OFFSET` cannot make SQLite walk and discard the whole table, and job logs are capped at 4000 characters before they reach the database.
- Response headers: `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, and CSP `frame-ancestors 'none'` on every response including the SSE streams. `Strict-Transport-Security` is emitted only when the request came over TLS, since a browser ignores it on a plain-HTTP origin and setting it unconditionally would only mislead operators.

### Changed
- **Breaking:** cookie-authenticated `POST`/`PUT`/`DELETE` requests without `X-Requested-With: muxprune` are rejected with `403`. The bundled Web UI sends it. Third-party scripts using the session cookie must add the header or switch to `X-Api-Key`, which is unaffected.
- **Breaking:** error responses for internal failures now read `internal error` or `invalid request` instead of the underlying error text. Clients matching on database or filesystem error strings need updating; validation messages authored by the API are unchanged.
- Browsing on a fresh install is restricted to the bootstrap roots listed above instead of the entire filesystem. Set `MUXPRUNE_BROWSE_ROOTS` to browse anywhere else.
- The MCP handshake reports the real build version instead of a hardcoded `v0.2.0` that had not tracked a release since 0.2.

### Fixed
- The stdio MCP server (`muxprune mcp`) no longer blocks shutdown until a newline arrives on stdin. Cancellation was only checked between reads, so an idle session ignored SIGTERM until the client happened to send something; it now exits immediately.
- MCP requests detached from a disconnected HTTP client no longer run forever. Work is bound to the SSE session's lifetime instead of being made uncancellable.
- The Sonarr/Radarr webhook no longer queues a duplicate scan when the "is a scan already running" check fails: the error was discarded and treated as "not running".
- Non-positive and unparseable path ids (`/api/v1/files/0`, `/-1`, `/abc`) return `400` instead of falling through to a database lookup and a misleading `404`.
- The session and authentication rate-limiter maps no longer grow without bound. Both only shrank when the same token or IP came back; a background sweep now drops entries that nothing will touch again.
- The library watcher's watch-limit check and counter increment happen under a single lock. The pair was split across an unlock/relock window that no current caller could interleave, but the invariant no longer depends on that.

## [0.5.0] - 2026-07-17

### Security
- Unified authentication wall over every protected route, including the MCP endpoints (`/sse`, `/api/v1/mcp/*`) that previously bypassed the API key check. Key comparison is constant-time, and the key is accepted via the `X-Api-Key` header only; the `?apikey=` query parameter is no longer read anywhere.
- The Web UI now authenticates SSE and all requests through an httpOnly session cookie carrying a random 30-day token, issued by `POST /api/v1/auth/session` after a one-time header auth. The raw API key is no longer stored in the cookie or in `localStorage`.
- When no API key is configured, the server binds to loopback only instead of exposing an unauthenticated control plane to the network (`MUXPRUNE_BIND` overrides for trusted networks).
- Repeated authentication failures from an IP are rate limited (10 failures per minute locks the IP out temporarily).
- Filesystem browsing is jailed: to `MUXPRUNE_BROWSE_ROOTS` when set, otherwise to the configured library roots; `..` traversal cannot escape. Library add/edit paths are validated against the explicit roots as well.
- Sonarr/Radarr webhook supports a dedicated shared secret (`MUXPRUNE_WEBHOOK_SECRET`, header `X-Webhook-Secret`), enforced even when no API key is set.
- Closed the mkvmerge/ffmpeg argument-injection class: positional paths handed to mkvmerge/ffprobe are guarded with a `./` prefix whenever they could parse as options or option-files (mkvmerge itself has no `--` terminator support), external merge files whose names begin with `-` or `@` are rejected at validation time, and metadata language tags are validated with control characters stripped from titles.
- HTTP server hardening: read/header/idle timeouts, 1 MB header cap, and a 10 MB request-body limit that returns `413`. A restrictive `Content-Security-Policy` header is set on all responses.
- MCP: removed the wildcard `Access-Control-Allow-Origin` header from the SSE transport, and a disconnecting client can no longer crash the server via a send on a closed session channel.

### Fixed
- The remux verification size floor can no longer go negative and silently disable itself when the estimated removed bytes exceed the input size; it is clamped to a hard minimum of 10% of the input, so a truncated output can never atomically replace good media.
- A transient probe error (mid-import write, NFS hiccup, corrupt container) no longer prunes the still-present file's catalog record and sidecars; the record is kept alive and re-probed on a later pass.
- A partially available mount can no longer mass-delete a library's catalog: pruning is refused when the walk hit unreadable subtrees, and when a single pass would delete more than `MUXPRUNE_PRUNE_MAX_RATIO` (default 20%) of the library's records.
- Concurrent mutating jobs on the same file can no longer corrupt each other: each mutation holds a per-file lock and writes to a unique temp path (`MUXPRUNE_WORKERS` > 1 is now safe).
- Each probe is bounded by a timeout (`MUXPRUNE_PROBE_TIMEOUT`, default 60s) so a stalled network mount cannot wedge a scan, and probe results with zero streams are rejected instead of being ingested as real, prunable files.
- Recycled sidecars with the same basename deleted in the same second no longer overwrite each other, and cross-filesystem recycle moves are atomic (copy to a temp name, rename, delete source last).
- mkvmerge track IDs are aligned to ffprobe streams with language and codec cross-checks; on any ambiguity the file falls back to the ffmpeg path instead of risking a wrong-track edit.
- Graceful shutdown drains workers, the monitor, and the purge loop before closing the database, so a job that finishes during shutdown keeps its terminal status. A grace window (`MUXPRUNE_SHUTDOWN_GRACE`, default 30s) lets an almost-finished remux complete, and jobs killed by shutdown are recorded as `cancelled` rather than failed.
- The filesystem watcher self-heals: transient errors re-add the full subtree and surface as `reconnecting`, dead watchers are rebuilt, a vanished mount downgrades to periodic scanning, and recursive watches are capped (default 8192) with an explicit degraded status instead of going silently blind. Fixed a data race on the watcher context.
- Read-path correctness: `Stats` propagates query errors instead of returning zeros, `GetLibrary` no longer returns a half-populated struct alongside an error, and file search treats `%`/`_` literally.
- Malformed or out-of-range numeric environment values produce a startup warning and fall back to defaults instead of being silently ignored; an invalid `UMASK` no longer crash-loops the container.

### Added
- Running jobs can be cancelled from the UI (per-job cancel with a distinct `cancelled` terminal status), and failed/skipped/cancelled jobs can be retried (`POST /api/v1/jobs/{id}/retry`); retries track an attempt count.
- Job logs surface side-effect failures (sidecar record cleanup, last-scanned stamping) as warnings instead of swallowing them.
- The effective configuration is logged at startup (workers, recycle retention, probe timeout, prune ratio, shutdown grace, watch, auth posture).
- The `mcp` subcommand is available through the container entrypoint.

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
