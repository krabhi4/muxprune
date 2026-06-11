FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /muxprune ./cmd/muxprune

FROM alpine:3.21
RUN apk add --no-cache ffmpeg mkvtoolnix tzdata su-exec
COPY --from=build /muxprune /usr/local/bin/muxprune
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENV MUXPRUNE_CONFIG=/config \
    MUXPRUNE_PORT=8484 \
    PUID=1000 \
    PGID=1000

VOLUME /config
EXPOSE 8484
HEALTHCHECK --interval=30s --timeout=5s \
    CMD wget -qO- http://127.0.0.1:${MUXPRUNE_PORT}/api/v1/health >/dev/null || exit 1

ENTRYPOINT ["/entrypoint.sh"]
