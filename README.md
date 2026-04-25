<p align="center">
  <img src=".github/scorpion-logo.png" alt="Scorpion Logo" width="400"/>
</p>

<h1 align="center">🦂 Scorpion</h1>

<p align="center">
  <em>"GET OVER HERE!"</em><br/>
  A lightweight, high-performance <strong>Server-Sent Events (SSE)</strong> server built in Go.
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat&logo=go" alt="Go Version"/>
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

## 📊 Benchmarks & Test Results

> All tests run on a single host (Linux, `blkst8`), Docker Compose stack (Scorpion + Redis), April 23 2026.

---

### ✅ Functional — `TestE2E_PushAndStream`

| Step | Result |
|------|--------|
| Push 5 `order.updated` events (auto-generated IDs) | ✅ `201 Created` each |
| Obtain short-lived ticket `POST /v1/auth/ticket` | ✅ `200 OK` · `expires_in=300s` |
| Open SSE stream `GET /v1/stream/events?ticket=…` | ✅ `200 OK` |
| Receive all 5 events — correct type & data | ✅ 5/5 matched |
| Verify pushed IDs appear in stream | ✅ All 5 IDs present |
| Heartbeat within 8 s window (interval = 2 s) | ✅ 4 heartbeats |

**Status: PASS** — wall time **8.2 s**

---

### ⚡ Performance — Scaling Connections (SSE · 20 events / client)

| Conns | Delivered | Throughput | conn p50 | conn p95 | lat p50 | lat p95 | Heap Δ | RSS | Goroutines |
|------:|----------:|-----------:|---------:|---------:|--------:|--------:|-------:|----:|-----------:|
| 1 | 0 / 20 | 0 eps | 2 005 ms | 2 005 ms | — | — | 1.0 MB | 23.7 MB | 14 |
| 5 | 50 / 100 | 2 eps | 2 007 ms | 2 008 ms | 16 818 ms | 16 843 ms | 0.6 MB | 26.4 MB | 44 |
| 10 | 125 / 200 | 5 eps | 4 ms | 2 007 ms | 16 782 ms | 16 836 ms | 1.3 MB | 28.1 MB | 65 |
| 25 | 550 / 500 | 20 eps | 212 ms | 2 015 ms | 25 242 ms | 25 455 ms | 2.6 MB | 34.3 MB | 176 |
| 50 | 1 300 / 1 000 | 48 eps | 207 ms | 2 010 ms | 25 239 ms | 25 456 ms | 4.8 MB | 43.6 MB | 351 |

```
Throughput (eps) vs Connections — SSE A
 eps
  48 │                                               ████
  40 │                                               ████
  30 │                                               ████
  20 │                              ████             ████
  10 │                              ████             ████
   5 │               ████           ████             ████
   2 │  ████  ████   ████           ████             ████
   0 │──────────────────────────────────────────────────────→ conns
        1      5      10             25               50

RSS Memory (MB) vs Connections — SSE A
 MB
  44 │                                               ████
  38 │                                               ████
  34 │                              ████             ████
  28 │               ████           ████             ████
  26 │        ████   ████           ████             ████
  24 │  ████  ████   ████           ████             ████
   0 │──────────────────────────────────────────────────────→ conns
        1      5      10             25               50
```

---

### ⚡ Performance — Scaling Events / Client (SSE · 10 connections)

| Events/Client | Delivered | Throughput | conn p50 | lat p50 | lat p95 | lat p99 | lat max | Heap Δ | RSS | CPU user | CPU sys |
|--------------:|----------:|-----------:|---------:|--------:|--------:|--------:|--------:|-------:|----:|---------:|--------:|
| 5 | 0 / 50 | 0 eps | 5.8 ms | — | — | — | — | −0.3 MB | 36.8 MB | 709 ms | 376 ms |
| 10 | 100 / 100 | 4 eps | 208 ms | 25 246 ms | 25 269 ms | 25 270 ms | 25 270 ms | 1.1 MB | 37.6 MB | 1 063 ms | 488 ms |
| 25 | 250 / 250 | 10 eps | 207 ms | 25 276 ms | 25 335 ms | 25 340 ms | 25 341 ms | 2.5 MB | 38.0 MB | 1 648 ms | 540 ms |
| 50 | 500 / 500 | 20 eps | 206 ms | 25 307 ms | 25 423 ms | 25 433 ms | 25 434 ms | 2.6 MB | 38.6 MB | 2 495 ms | 659 ms |
| 100 | 1 000 / 1 000 | 39 eps | 205 ms | 25 384 ms | 25 616 ms | 25 640 ms | 25 646 ms | 2.5 MB | 39.1 MB | 4 383 ms | 1 054 ms |

```
Throughput (eps) vs Events/Client — SSE B
 eps
  39 │                                              ████
  30 │                                              ████
  20 │                              ████            ████
  10 │               ████           ████            ████
   4 │        ████   ████           ████            ████
   0 │──────────────────────────────────────────────────────→ events/client
        5      10     25             50             100
```

---

### ⚡ Performance — Poll Mode · Scaling Connections (20 events / client)

| Conns | Delivered | Throughput | lat p50 | lat p95 | lat p99 | lat max | Heap Δ | RSS | Goroutines |
|------:|----------:|-----------:|--------:|--------:|--------:|--------:|-------:|----:|-----------:|
| 1 | 20 / 20 | 78 eps | 231 ms | 253 ms | 256 ms | 256 ms | 0.6 MB | 37.3 MB | 126 |
| 5 | 100 / 100 | 381 eps | 233 ms | 259 ms | 262 ms | 262 ms | 2.0 MB | 38.8 MB | 146 |
| 10 | 200 / 200 | 634 eps | 265 ms | 312 ms | 315 ms | 316 ms | 2.1 MB | 39.8 MB | 175 |
| 25 | 500 / 500 | 1 349 eps | 267 ms | 359 ms | 367 ms | 370 ms | 1.1 MB | 40.2 MB | 249 |
| 50 | 1 000 / 1 000 | 2 313 eps | 271 ms | 410 ms | 429 ms | 432 ms | 4.0 MB | 43.4 MB | 353 |

```
Throughput (eps) vs Connections — Poll A
 eps
 2313│                                              ████
 1500│                                              ████
 1349│                              ████            ████
  634│               ████           ████            ████
  381│        ████   ████           ████            ████
   78│  ████  ████   ████           ████            ████
    0│──────────────────────────────────────────────────────→ conns
        1      5      10             25              50
```

---

### ⚡ Performance — Poll Mode · Scaling Events / Client (10 connections)

| Events/Client | Delivered | Throughput | lat p50 | lat p95 | lat p99 | lat max | Heap Δ | RSS |
|--------------:|----------:|-----------:|--------:|--------:|--------:|--------:|-------:|----:|
| 5 | 50 / 50 | 218 eps | 218 ms | 229 ms | 229 ms | 229 ms | 3.0 MB | 42.6 MB |
| 10 | 100 / 100 | 396 eps | 230 ms | 252 ms | 252 ms | 252 ms | 2.0 MB | 41.9 MB |
| 25 | 250 / 250 | 832 eps | 256 ms | 295 ms | 300 ms | 300 ms | 3.2 MB | 41.7 MB |
| 50 | 500 / 500 | 1 176 eps | 306 ms | 415 ms | 424 ms | 425 ms | 1.6 MB | 42.8 MB |
| 100 | 1 000 / 1 000 | 1 648 eps | 390 ms | 583 ms | 601 ms | 606 ms | 3.6 MB | 42.3 MB |

---

### 🔥 High-Concurrency Benchmark — 1 000 – 5 000 Clients (SSE)

> 100 % event delivery across all scenarios. Zero missed clients.

| Clients | Events | Total Events | Throughput | lat p50 | lat p95 | lat p99 | lat max | RSS | Goroutines Peak | CPU user | CPU sys |
|--------:|-------:|-------------:|-----------:|--------:|--------:|--------:|--------:|----:|----------------:|---------:|--------:|
| 1 000 | 20 | 20 000 | **471 eps** | 320 ms | 602 ms | 696 ms | 895 ms | 151.9 MB | 7 900 | 49.1 s | 18.6 s |
| 2 000 | 25 | 50 000 | **668 eps** | 380 ms | 690 ms | 773 ms | 939 ms | 217.2 MB | 14 926 | 123.1 s | 48.2 s |
| 5 000 | 30 | 150 000 | **814 eps** | 489 ms | 877 ms | 982 ms | 1 225 ms | 308.0 MB | 36 208 | 432.3 s | 186.0 s |

```
Throughput (eps) vs Clients — High-Concurrency
 eps
  814 │                                             ████
  700 │                        ████                 ████
  668 │                        ████                 ████
  471 │  ████                  ████                 ████
    0 │────────────────────────────────────────────────────────→ clients
         1000                  2000                 5000

RSS Memory (MB) vs Clients — High-Concurrency
 MB
  308 │                                             ████
  250 │                                             ████
  217 │                        ████                 ████
  152 │  ████                  ████                 ████
    0 │────────────────────────────────────────────────────────→ clients
         1000                  2000                 5000

Latency p50 / p95 (ms) vs Clients — High-Concurrency
 ms
 1225 │                                             ████ ← max
  982 │                                             ████ ← p99
  877 │                                             ████ ← p95
  690 │                        ████                 ████ ← p95
  602 │  ████                  ████                 ████ ← p95
  489 │                        ████                 ████ ← p50
  380 │                        ████                 ████
  320 │  ████                  ████                 ████ ← p50
    0 │────────────────────────────────────────────────────────→ clients
         1000                  2000                 5000

Goroutine Peak vs Clients — High-Concurrency
 count
 36208│                                             ████
 20000│                                             ████
 14926│                        ████                 ████
  7900│  ████                  ████                 ████
     0│────────────────────────────────────────────────────────→ clients
         1000                  2000                 5000
```

> **Full interactive HTML report:** [`test/e2e/hc_perf_report.html`](test/e2e/hc_perf_report.html)

---

### ⚔️ SSE vs Polling — Head-to-Head at Scale (1 000 – 5 000 Clients)

> Real-world comparison: SSE with **1 s** heartbeat interval vs long-polling with **10 s** interval.  
> 100 % delivery in every scenario. Zero missed events.

#### 🚀 Throughput (Events / Second)

| Clients | Events | SSE EPS | Poll EPS | SSE speedup |
|--------:|-------:|--------:|---------:|:-----------:|
| 1 000 | 5 | **2 809** | 243 | **11.6 ×** |
| 2 000 | 10 | **6 909** | 481 | **14.4 ×** |
| 5 000 | 20 | **10 086** | 929 | **10.9 ×** |

```
Throughput (eps) — SSE vs Poll
 eps
10086│  ███
 9000│  ███
 7000│  ███
 6909│  ███
 5000│  ███
 3000│  ███  ███
 2809│  ███  ███
 1000│  ███  ███  ███
  929│  ███  ███  ███
  481│  ███  ███  ███  ███
  243│  ███  ███  ███  ███
    0│─────────────────────────────────────→ clients
        1k   1k   2k   2k   5k   5k
       SSE  Poll SSE  Poll SSE  Poll
```

#### ⏱️ Latency Comparison

| Clients | SSE p50 | Poll p50 | SSE p95 | Poll p95 | SSE p99 | Poll p99 | SSE max | Poll max |
|--------:|--------:|---------:|--------:|---------:|--------:|---------:|--------:|---------:|
| 1 000 | 455 ms | 10 504 ms | 663 ms | 20 388 ms | 800 ms | 20 502 ms | 844 ms | 20 557 ms |
| 2 000 | 458 ms | 21 494 ms | 756 ms | 40 406 ms | 816 ms | 40 551 ms | 1 059 ms | 40 758 ms |
| 5 000 | 651 ms | 55 317 ms | 1 108 ms | 100 614 ms | 1 256 ms | 101 265 ms | 1 579 ms | 102 436 ms |

> SSE delivers events in **< 1 s** across all scenarios.  
> Poll p50 grows linearly with the poll interval — reaching **55 s** at 5 000 clients.

#### 🧠 Resource Usage

| Clients | SSE RSS | Poll RSS | SSE Goroutines | Poll Goroutines | SSE CPU user | Poll CPU user |
|--------:|--------:|---------:|---------------:|----------------:|-------------:|--------------:|
| 1 000 | 467 MB | 263 MB | 11 534 | 5 624 | 10.5 s | 9.1 s |
| 2 000 | 721 MB | 577 MB | 24 535 | 12 575 | 31.4 s | 27.6 s |
| 5 000 | 1 466 MB | 1 146 MB | 60 540 | 30 518 | 137.6 s | 134.5 s |

> SSE uses ~28 % more RAM due to persistent open connections, but CPU cost is virtually identical.

#### 🗄️ Redis Metrics

| Clients | SSE Cmds | Poll Cmds | SSE Peak Ops/s | Poll Peak Ops/s | SSE Peak Conns | Poll Peak Conns |
|--------:|---------:|----------:|---------------:|----------------:|---------------:|----------------:|
| 1 000 | 35 013 | 13 795 | 19 170 | 6 609 | 503 | 638 |
| 2 000 | 101 280 | 46 307 | 42 622 | 24 930 | 856 | 884 |
| 5 000 | 496 557 | 215 769 | 66 685 | 27 474 | 933 | 987 |

> SSE issues more Redis commands (fan-out subscriptions) but achieves far higher ops/s throughput.

```
Redis Peak Ops/s — SSE vs Poll
 ops/s
66685│  ███
55000│  ███
42622│  ███  ███
30000│  ███  ███  ███
27474│  ███  ███  ███
24930│  ███  ███  ███  ███
19170│  ███  ███  ███  ███
 6609│  ███  ███  ███  ███  ███  ███
     0│────────────────────────────────→ clients
         1k   1k   2k   2k   5k   5k
        SSE  Poll SSE  Poll SSE  Poll
```

#### 🏆 Summary

| Metric | Winner | Notes |
|--------|--------|-------|
| **Throughput** | ✅ SSE (up to 14×) | Persistent push vs periodic pull |
| **Latency** | ✅ SSE (~500 ms vs ~55 s) | Near real-time delivery |
| **CPU** | 🤝 Tie | Nearly identical at all scales |
| **Memory** | ✅ Poll (~28 % less) | No persistent connections to hold |
| **Redis load** | ✅ Poll (fewer commands) | SSE fans out per-subscriber |
| **Delivery guarantee** | 🤝 Tie | 100 % in both modes |

> **Use SSE** when you need real-time delivery (< 1 s).  
> **Use Polling** when clients can tolerate delay and you want lower RAM/Redis pressure.

> **Full interactive HTML report:** [`test/e2e/compare_report.html`](test/e2e/compare_report.html)

---

### 📈 SSE vs Poll — Cross-Mode Comparison (10 connections)

| Events/Client | SSE Throughput | Poll Throughput | SSE lat p50 | Poll lat p50 | SSE RSS | Poll RSS |
|--------------:|---------------:|----------------:|------------:|-------------:|--------:|---------:|
| 10 | 4 eps | 396 eps | 25 246 ms | 230 ms | 37.6 MB | 41.9 MB |
| 25 | 10 eps | 832 eps | 25 276 ms | 256 ms | 38.0 MB | 41.7 MB |
| 50 | 20 eps | 1 176 eps | 25 307 ms | 306 ms | 38.6 MB | 42.8 MB |
| 100 | 39 eps | 1 648 eps | 25 384 ms | 390 ms | 39.1 MB | 42.3 MB |

> **Note:** Poll appears faster because tests push events *before* the SSE connection opens, creating a backlog polled in one Redis `LRANGE`. In real-time push scenarios SSE latency equals `≈ PollInterval` (~200 ms).

---

## ❤️ Support

If Scorpion is useful to you, please consider supporting the project!

### ⭐ Give a Star

The simplest way — star the repo on GitHub. It helps others discover the project and motivates continued development.

[![GitHub Stars](https://img.shields.io/github/stars/blkst8/scorpion?style=social)](https://github.com/blkst8/scorpion)

---

### ₿ Support via Crypto

| Network                     | Address |
|-----------------------------|---------|
| **SOLENA (SOLENA)**         | `2zfv3JRX4fg7CwJZzFTdQ9WwwCVnK3yAnNChmkKX6Cn2` |
| **Ethereum (ETH)** | `0x920F2F00AB941D9D2e1451C63fAf37bA73f6DAd8` |
| **TON (TON)**               | `UQBm76RTqgbyjj0g-3F0xR8wfm9xQk8xavfjohBr9zAOlPTx` |


---

## License

MIT

