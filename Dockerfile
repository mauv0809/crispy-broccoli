# syntax=docker/dockerfile:1.7
# Cache mounts (npm, Go module, Go build) below need BuildKit + the
# 1.7 frontend. Coolify's recent buildkit defaults handle this; if a
# host runs a legacy builder, drop the --mount lines.

# ---- css stage ----
# Tailwind v4 uses platform-specific native binaries, so output bytes
# vary by platform. Building inside Docker (deterministic Linux image)
# avoids drift between local dev and CI; output.css is therefore not
# committed to git.
FROM node:22-alpine AS css
WORKDIR /src

COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci

# Tailwind scans assets/css/input.css (which @source's the templ files)
# and internal/views/**/*.templ. Copy only what's needed for the scan
# so this layer's cache only invalidates on relevant changes.
COPY assets ./assets
COPY internal/views ./internal/views

RUN npm run css:build

# ---- go build stage ----
FROM golang:1.24-alpine AS builder
WORKDIR /src

# Cache deps separately for fast rebuilds
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
# Overlay the freshly-built CSS into assets/ before the binary build
# (assets/ is bundled into the runtime image below).
COPY --from=css /src/assets/css/output.css ./assets/css/output.css

ARG BUILD_SHA=unknown
ARG BUILD_TIME=unknown
# /root/.cache/go-build is the big win: Go's incremental compile cache.
# After the first build, only changed packages recompile.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux \
    go build \
      -ldflags="-s -w -X main.buildSHA=${BUILD_SHA} -X main.buildTime=${BUILD_TIME}" \
      -o /out/app ./cmd/app

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=builder /out/app /app/app
COPY --from=builder /src/assets /app/assets

EXPOSE 8080
USER nonroot:nonroot

# Distroless has no curl/wget, so the binary probes itself. The flag
# short-circuits before DB or logger setup, so the probe is fast and
# side-effect-free. PORT is read from the env at probe time.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/app", "--healthcheck"]

ENTRYPOINT ["/app/app"]
