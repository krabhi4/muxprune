#!/bin/sh
# Run muxprune as PUID:PGID (linuxserver convention) so written files match
# the ownership of the rest of the *arr stack. Falls back to the current user
# when not started as root (e.g. docker run --user).
set -e

if [ "$(id -u)" = "0" ]; then
    PUID="${PUID:-1000}"
    PGID="${PGID:-1000}"
    if ! getent group "$PGID" >/dev/null 2>&1; then
        addgroup -g "$PGID" muxprune
    fi
    if ! getent passwd "$PUID" >/dev/null 2>&1; then
        adduser -D -H -u "$PUID" -G "$(getent group "$PGID" | cut -d: -f1)" muxprune
    fi
    if [ -n "$UMASK" ]; then
        umask "$UMASK"
    fi
    mkdir -p "${MUXPRUNE_CONFIG:-/config}"
    chown "$PUID:$PGID" "${MUXPRUNE_CONFIG:-/config}"
    exec su-exec "$PUID:$PGID" muxprune serve "$@"
fi

exec muxprune serve "$@"
