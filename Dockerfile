# syntax=docker/dockerfile:1

# Build stage: cross-compiles a static binary for the target platform on the
# build platform, then assembles the complete mod filesystem under
# /root-layer. Go cross-compiles natively, so no emulation is needed.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY root /root-layer
RUN set -eu; \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
        go build -trimpath -buildvcs=false \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /root-layer/usr/local/bin/plex-4k-guard ./cmd/plex-4k-guard; \
    chmod 0755 /root-layer/usr/local/bin/plex-4k-guard \
        /root-layer/etc/s6-overlay/s6-rc.d/init-mod-plex-4k-guard/run \
        /root-layer/etc/s6-overlay/s6-rc.d/svc-mod-plex-4k-guard/run; \
    chown -R 0:0 /root-layer

# Final stage: a single-layer image. The LinuxServer.io docker-mods loader
# downloads only the first layer of the manifest and extracts it over "/",
# so everything must live in exactly one COPY.
FROM scratch

ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=1970-01-01T00:00:00Z

LABEL maintainer="bmanhuge" \
      org.opencontainers.image.title="plex-4k-transcode-guard" \
      org.opencontainers.image.description="LinuxServer.io Plex Docker Mod that terminates video sessions transcoding a 4K/UHD source with a configurable message" \
      org.opencontainers.image.source="https://github.com/bmanhuge/plex-4k-transcode-guard" \
      org.opencontainers.image.url="https://github.com/bmanhuge/plex-4k-transcode-guard" \
      org.opencontainers.image.documentation="https://github.com/bmanhuge/plex-4k-transcode-guard#readme" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.vendor="bmanhuge" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}" \
      io.linuxserver.docker-mod="true" \
      io.linuxserver.docker-mod.base="plex"

COPY --from=build /root-layer/ /
