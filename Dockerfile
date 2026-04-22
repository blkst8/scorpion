# ─── Stage 1: builder ────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

WORKDIR /build

# Copy dependency manifests and vendored modules first (better layer caching).
COPY go.mod go.sum ./
COPY vendor/ vendor/

# Copy the rest of the source.
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -mod=vendor -trimpath \
    -ldflags="-s -w" \
    -o /out/scorpion .

# ─── Stage 2: runtime ────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

WORKDIR /app

# Copy compiled binary.
COPY --from=builder /out/scorpion /app/scorpion

# Copy default config (overridable via volume mount or env vars).
COPY config/config.yaml /app/config/config.yaml

# 8443 — HTTPS / HTTP/2 SSE server
# 9090 — Prometheus metrics
EXPOSE 8443 9090

ENTRYPOINT ["/app/scorpion", "start", "--config", "/app/config/config.yaml"]

