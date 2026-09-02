FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /muxprune ./cmd/muxprune

FROM alpine:3.24
RUN apk add --no-cache ffmpeg=8.1.2-r0 mkvtoolnix=99.0-r0 tzdata=2026c-r0 su-exec=0.3-r0
COPY --from=build /muxprune /usr/local/bin/muxprune
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENV MUXPRUNE_CONFIG=/config \
    MUXPRUNE_PORT=8484 \
    PUID=1000 \
    PGID=1000

VOLUME /config
EXPOSE 8484
HEALTHCHECK --interval=30s --timeout=5s CMD ["wget", "-qO-", "http://127.0.0.1:8484/api/v1/health"]

ENTRYPOINT ["/entrypoint.sh"]
