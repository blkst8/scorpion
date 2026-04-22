<p align="center">
  <img src=".github/scorpion-logo.png" alt="Scorpion Logo" width="400"/>
</p>

<h1 align="center">🦂 Scorpion</h1>

<p align="center">
  <em>"GET OVER HERE!"</em><br/>
  A lightweight, high-performance <strong>Server-Sent Events (SSE)</strong> server built in Go.
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go" alt="Go Version"/>
  <img src="https://img.shields.io/badge/Protocol-HTTP%2F2-blueviolet?style=flat" alt="HTTP/2"/>
  <img src="https://img.shields.io/badge/Redis-backed-red?style=flat&logo=redis" alt="Redis"/>
  <img src="https://img.shields.io/badge/version-0.3.0-green?style=flat" alt="Version"/>
</p>

---

## Overview

Scorpion is a real-time event streaming server that delivers events to authenticated clients via **Server-Sent Events (SSE)** over **HTTP/2 (TCP)**. It uses a **pre-auth ticket system** backed by Redis to ensure secure, low-latency event delivery at scale.

Each user is limited to a **single active stream per IP address**, preventing abuse and DDoS attacks.

---

## Key Features

- 🔐 **Pre-Auth Ticket System** — Clients first obtain a short-lived JWT ticket via a `POST /v1/auth/ticket` endpoint before establishing an SSE stream, keeping the stream URL stateless and secure.
- 📡 **SSE over HTTP/2** — Full-duplex streaming over a persistent connection with automatic heartbeats to keep connections alive.
- 🏎️ **High-Performance Redis Backend** — Events are stored in Redis lists and consumed atomically using Lua scripts, ensuring no double-delivery under concurrent load.
- 🛡️ **Rate Limiting** — Per-IP rate limiting on the ticket endpoint protects against brute-force and flooding attacks.
- 🌐 **Trusted Proxy IP Resolution** — Configurable IP extraction strategies (rightmost trusted count, trusted CIDR ranges, etc.) via the `realclientip-go` library.
- 📊 **Observability** — Prometheus metrics, structured logging (JSON/text), and OpenTelemetry tracing built-in.
- 🔄 **Graceful Shutdown** — In-flight streams are drained cleanly before the server exits.

---

## Architecture

```
┌────────────┐         ┌──────────────────────┐         ┌───────────┐
│   Client   │──HTTP2──│      Scorpion        │─────────│   Redis   │
│            │         │                      │         │           │
│ 1. POST /v1/auth/ticket (Bearer token)      │         │ - tickets │
│ 2. GET  /v1/stream/events?ticket=<jwt>      │         │ - events  │
│ 3. Receive SSE stream  │                    │         │ - conns   │
└────────────┘         └──────────────────────┘         └───────────┘
```

---

## API Endpoints

| Method | Path                  | Description                                      |
|--------|-----------------------|--------------------------------------------------|
| POST   | `/v1/auth/ticket`     | Authenticate and receive a short-lived JWT ticket |
| GET    | `/v1/stream/events`   | Connect to the SSE stream using the ticket       |
| POST   | `/v1/events`          | Push events into the stream (internal/backend)   |
| GET    | `/health`             | Health check                                     |
| GET    | `/metrics`            | Prometheus metrics (separate port)               |

---

## Quick Start

### Prerequisites

- Go 1.21+
- Redis 6+
- TLS certificate and key (required for HTTP/2)

### Run

```bash
go run main.go start --config config/config.yaml
```

### Configuration

Configuration is driven by `config/config.yaml`. Key settings:

```yaml
server:
  port: 8443
  tls_cert: "./certs/cert.pem"
  tls_key:  "./certs/key.pem"
  shutdown_timeout: 30s

sse:
  poll_interval:      100ms
  batch_size:         100
  heartbeat_interval: 15s

auth:
  token_secret:  "your-token-secret"
  ticket_secret: "your-ticket-secret"
  ticket_ttl:    5m

redis:
  address:  "localhost:6379"
  password: ""
  db:       0

ratelimit:
  ticket_rpm:   60
  ticket_burst: 10

observability:
  metrics_port: 9090
  log_level:    info
  log_format:   json
```

---

## Observability

- **Metrics** — Prometheus metrics exposed on the configured `metrics_port`.
- **Tracing** — OpenTelemetry tracing integrated throughout the request lifecycle.
- **Logging** — Structured logs in JSON or text format with configurable log levels.

---

## Security

- All connections require **TLS** (enforced by HTTP/2).
- JWT tickets are short-lived and bound to a `(user_id, ip)` pair.
- Atomic Lua scripts in Redis prevent race conditions on ticket validation and connection enforcement.
- Configurable trusted proxy resolution ensures accurate IP attribution behind load balancers.

---

## License

MIT

