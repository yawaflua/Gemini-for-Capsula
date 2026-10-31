# The Marusya plugin catalog in one image: the Go API, which also serves the
# built Svelte site. Build from registry/: docker build -t marusya-registry .

FROM oven/bun:1.3.14-alpine AS site
WORKDIR /src
COPY frontend/package.json frontend/bun.lock ./
RUN bun install --frozen-lockfile
COPY frontend/ ./
ARG VITE_SITE_URL=https://marusya.ywfl.dev
ENV VITE_SITE_URL=$VITE_SITE_URL VITE_API_BASE_URL=
RUN bun run build

FROM golang:1.25.8-alpine AS api
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/registry ./cmd/app

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=api /out/registry /usr/local/bin/registry
COPY --from=site /src/dist /srv/site
ENV SITE_DIR=/srv/site HTTP_PORT=8080
EXPOSE 8080
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
CMD ["registry"]
