#!/bin/sh
# Run muxprune as PUID:PGID (linuxserver convention) so written files match
# the ownership of the rest of the *arr stack. Falls back to the current user
# when not started as root (e.g. docker run --user).
set -e

# If the command starts with a flag (e.g. -port) or is empty, default to "serve"
if [ "$#" -eq 0 ] || [ "${1#-}" != "$1" ]; then
    set -- serve "$@"
fi

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
        case "$UMASK" in
            [0-7][0-7][0-7]|[0-7][0-7][0-7][0-7]) umask "$UMASK" ;;
            *) echo "muxprune: ignoring invalid UMASK=$UMASK (expected octal like 022)" >&2 ;;
        esac
    fi
    mkdir -p "${MUXPRUNE_CONFIG:-/config}"
    chown "$PUID:$PGID" "${MUXPRUNE_CONFIG:-/config}"

    # If the subcommand is a known muxprune command, run it under su-exec
    case "$1" in
        serve|inspect|strip|mcp|version)
            exec su-exec "$PUID:$PGID" muxprune "$@"
            ;;
        *)
            # Otherwise execute the custom command directly (e.g. /bin/sh)
            exec su-exec "$PUID:$PGID" "$@"
            ;;
    esac
fi

case "$1" in
    serve|inspect|strip|mcp|version)
        exec muxprune "$@"
        ;;
    *)
        exec "$@"
        ;;
esac
