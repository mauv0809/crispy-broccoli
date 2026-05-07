# ---- build stage ----
FROM golang:1.24-alpine AS builder
WORKDIR /src

# Cache deps separately for fast rebuilds
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BUILD_SHA=unknown
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 GOOS=linux \
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
ENTRYPOINT ["/app/app"]
