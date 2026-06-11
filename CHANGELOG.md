# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-06-12

### Added
- Initial release of **muxprune**, a self-hosted media track removal utility.
- Lightweight, CGO-free, single-binary architecture built with Go and SQLite.
- Lossless container remuxing engine using `mkvmerge` (MKV) and `ffmpeg` (MP4/other fallbacks) to strip streams without transcoding.
- Sidecar subtitle file scanner and cleaner matching Sonarr/Radarr/Bazarr structures.
- Incremental library scanning and media files cataloging with hardlink safety detection.
- SQLite-backed asynchronous job queue and single-threaded worker manager.
- REST API and embedded Web UI for browsing media inventory, inspecting tracks, and queuing strip jobs.
- Docker multi-arch support (`linux/amd64`, `linux/arm64`) with user ID mapping (`PUID`/`PGID`) and integrated health checks.
- Dependabot config to auto-group minor/patch dependency upgrades for Go, Docker, and GitHub Actions.
- Robust CI/CD workflows for automated linting (golangci-lint, hadolint, govulncheck), tests, and GHCR package publishing.
