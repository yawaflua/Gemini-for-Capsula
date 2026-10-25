# Copyright 2026 The Marusya-Gemini Authors.
# SPDX-License-Identifier: Apache-2.0

# Builds marusya-server for the host platform and bundles the speaker agent
# (linux/arm64 by default), which speakers download from /v1/agent/<arch>
# into RAM on every boot.
#
#   docker build -f deploy/server/Dockerfile -t marusya-server .
#
# Works with the classic builder as well as BuildKit; no BuildKit-only
# features are used.

FROM golang:1.26 AS build
# Set by BuildKit for --platform builds; empty with the classic builder, in
# which case the server is built for the build machine.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG AGENT_ARCHS="arm64"
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-$(go env GOARCH)} go build -ldflags "-s -w -X main.version=$VERSION" \
        -o /out/marusya-server ./cmd/marusya-server && \
    mkdir -p /out/agent && \
    for arch in $AGENT_ARCHS; do \
        GOOS=linux GOARCH=$arch go build -ldflags "-s -w -X main.version=$VERSION" \
            -o /out/agent/marusya-agent-linux-$arch ./cmd/marusya-agent || exit 1; \
    done && \
    mkdir -p /out/data

# yt-dlp finds YouTube videos (and other sites) for speakers, ffmpeg decodes
# their sound; Node.js is the JavaScript runtime yt-dlp needs for YouTube.
# yt-dlp is taken from its latest release: YouTube changes often and old
# versions stop working. Rebuild the image (or run "yt-dlp -U" inside it) when
# playback breaks.
FROM debian:trixie-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl ffmpeg nodejs python3 && \
    curl -fsSL -o /usr/local/bin/yt-dlp https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp && \
    chmod 755 /usr/local/bin/yt-dlp && \
    apt-get purge -y curl && apt-get autoremove -y && rm -rf /var/lib/apt/lists/* && \
    useradd --system --uid 65532 --home-dir /data --shell /usr/sbin/nologin nonroot
COPY --from=build /out/marusya-server /marusya-server
COPY --from=build /out/agent /agent
# Volume for the settings and speaker profiles saved from the web panel.
COPY --from=build --chown=65532:65532 /out/data /data
ENV MARUSYA_LISTEN=:8080 \
    MARUSYA_AGENT_DIR=/agent \
    MARUSYA_YTDLP_ARGS="--js-runtimes node"
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/marusya-server", "-healthcheck"]
ENTRYPOINT ["/marusya-server"]
