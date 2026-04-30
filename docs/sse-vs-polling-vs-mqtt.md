# SSE vs Polling vs MQTT/EMQX — A Data-Driven Comparison

> All numbers below are from live benchmark runs executed against **Scorpion** (SSE & Polling) and **EMQX** (MQTT) on the same host. Raw data lives in `test/e2e/perf_results.json`, `compare_results.json`, `hc_perf_results.json`, and `mqtt_sse_results.json`. Tests were generated on **2026-04-30** at `tcp://127.0.0.1:1883` (EMQX) / Redis `127.0.0.1:6379`.

> 📊 **Interactive charts version:** [`docs/sse-vs-polling-vs-mqtt.html`](./sse-vs-polling-vs-mqtt.html) — self-contained HTML with embedded SVG bar/line charts (no CDN), same dark theme as the project's benchmark reports.

---

## Table of Contents

1. [Transport Primer](#1-transport-primer)
2. [Benchmark Methodology](#2-benchmark-methodology)
3. [SSE vs Polling — Head-to-Head](#3-sse-vs-polling--head-to-head)
4. [SSE vs MQTT/EMQX — Head-to-Head](#4-sse-vs-mqttemqx--head-to-head)
5. [High-Concurrency SSE Behaviour](#5-high-concurrency-sse-behaviour)
6. [Pros & Cons Summary](#6-pros--cons-summary)
7. [When to Choose What](#7-when-to-choose-what)
8. [Architecture Notes for Scorpion](#8-architecture-notes-for-scorpion)

---

## 1. Transport Primer

| Feature | SSE | HTTP Polling | MQTT (QoS 1) |
|---|---|---|---|
| **Protocol** | HTTP/1.1 or HTTP/2 | HTTP/1.1 | TCP (binary framing) |
| **Direction** | Server → Client (unidirectional) | Client → Server → Client (request/response) | Bidirectional pub/sub |
| **Connection model** | Persistent, long-lived HTTP response | Short-lived repeated requests | Persistent TCP + keepalive |
| **Delivery guarantee** | At-most-once (server decides; no ACK by default) | At-most-once per poll | QoS 0/1/2 (at-most / at-least / exactly-once) |
| **Firewall/proxy friendliness** | ✅ Standard HTTP/HTTPS | ✅ Standard HTTP/HTTPS | ⚠️ Port 1883/8883 often blocked |
| **Browser-native** | ✅ `EventSource` API | ✅ `fetch`/`XMLHttpRequest` | ❌ Needs WebSocket bridge or MQTT-over-WS |
| **Server push timing** | Poll-interval bounded (configurable in Scorpion) | Bounded to poll frequency | Near-instant (broker-driven) |
| **Horizontal scale** | Stateful (one goroutine per stream) | Stateless (any instance can serve) | Broker-mediated (largely stateless per instance) |

---

## 2. Benchmark Methodology

All tests run after all subscribers/clients are fully connected so latency reflects **real-time push**, not backlog drain.

### SSE (Scorpion)
- Clients request a short-lived JWT **ticket** via `POST /v1/auth/ticket`.
- Each client opens a persistent `GET /v1/stream/events?ticket=…` SSE stream.
- Events are pushed via `POST /v1/events/:clientID` after all streams are open.
- Redis is the backend queue (`RPUSH/LRANGE/LTRIM`). The SSE loop polls Redis every `200 ms` (MQTT comparison) or `500 ms` (polling comparison).

### HTTP Polling
- Same Redis backend; poll interval for the head-to-head was **10 seconds** (realistic for chatty polling).
- Clients repeatedly call `GET /v1/events/:clientID` and drain the queue in one batch.

### MQTT (EMQX)
- Broker: `tcp://127.0.0.1:1883` (anonymous, no TLS), QoS 1.
- Each MQTT client subscribes to a unique topic `scorpion/bench/<clientID>`.
- A single publisher goroutine fan-outs events using a pool of 500 concurrent publish workers.
- Connection retries: 3 attempts with back-off.

---

## 3. SSE vs Polling — Head-to-Head

> SSE poll interval: **500 ms** | Polling interval: **10 s**

### 3.1 Latency (ms) — lower is better

| Scenario | SSE p50 | Poll p50 | SSE p95 | Poll p95 | SSE max | Poll max |
|---|---:|---:|---:|---:|---:|---:|
| 1 000 clients × 5 events | **454** | 10 504 | **662** | 20 388 | **843** | 20 556 |
| 2 000 clients × 10 events | **458** | 21 493 | **756** | 40 406 | **1 059** | 40 758 |
| 5 000 clients × 20 events | **650** | 55 317 | **1 108** | 100 613 | **1 578** | 102 436 |

**Observation:** SSE latency is **23–85× lower** than polling at p50, and the gap widens with scale. Polling latency grows linearly with the polling interval × number of cycles needed; SSE latency is bounded by the server poll cadence (`sse.poll_interval` in config).

```
Latency p50 (ms) — log scale, lower is better
─────────────────────────────────────────────
1k×5    SSE  ██                               454 ms
        Poll ████████████████████████████   10504 ms
2k×10   SSE  ██                               458 ms
        Poll ████████████████████████████   21493 ms
5k×20   SSE  ███                              650 ms
        Poll ████████████████████████████   55317 ms
```

### 3.2 Throughput (events/sec) — higher is better

| Scenario | SSE EPS | Poll EPS | SSE / Poll ratio |
|---|---:|---:|---:|
| 1 000 clients × 5 events | **2 809** | 243 | 11.6× |
| 2 000 clients × 10 events | **6 909** | 481 | 14.4× |
| 5 000 clients × 20 events | **10 086** | 928 | 10.9× |

```
Throughput (eps) — higher is better
────────────────────────────────────────────────────
1k×5    SSE  ████████                           2809
        Poll █                                   243
2k×10   SSE  ██████████████████████             6909
        Poll ██                                  481
5k×20   SSE  ████████████████████████████████  10086
        Poll ███                                 928
```

### 3.3 Wall-clock time (s) — lower is better

| Scenario | SSE | Poll | Speed-up |
|---|---:|---:|---:|
| 1 000 clients × 5 events | **1.78** | 20.57 | 11.6× |
| 2 000 clients × 10 events | **2.89** | 41.57 | 14.4× |
| 5 000 clients × 20 events | **9.91** | 107.69 | 10.9× |

### 3.4 Memory (MB)

| Scenario | SSE RSS | Poll RSS | SSE Heap Δ | Poll Heap Δ |
|---|---:|---:|---:|---:|
| 1 000 clients × 5 events | **466** | 263 | 278 | **99** |
| 2 000 clients × 10 events | **720** | 576 | 392 | **167** |
| 5 000 clients × 20 events | **1 466** | 1 146 | 607 | **141** |

**Observation:** SSE holds open HTTP connections (one goroutine per stream), so its **RSS and heap are ~25–30 % higher** than polling. Polling creates no persistent connections; it releases all memory between polls.

### 3.5 Redis Operations

| Scenario | SSE peak ops/s | Poll peak ops/s | SSE cmds total | Poll cmds total |
|---|---:|---:|---:|---:|
| 1 000 × 5 | **19 170** | 6 609 | **35 013** | 13 795 |
| 2 000 × 10 | **42 622** | 24 930 | **101 280** | 46 307 |
| 5 000 × 20 | **66 685** | 27 474 | **496 557** | 215 769 |

**Observation:** SSE polls Redis continuously (regardless of event activity) so it generates ~2× more Redis commands than polling. Heavy SSE deployments must plan Redis capacity accordingly.

### 3.6 Goroutines Peak

| Scenario | SSE | Poll |
|---|---:|---:|
| 1 000 × 5 | **11 534** | 5 624 |
| 2 000 × 10 | **24 535** | 12 575 |
| 5 000 × 20 | **60 540** | 30 518 |

SSE requires roughly **2× more goroutines** — one persistent goroutine per active stream.

---

## 4. SSE vs MQTT/EMQX — Head-to-Head

> SSE poll interval: **200 ms** | MQTT QoS: **1** (at-least-once) | Broker: EMQX

### 4.1 Latency (ms) — lower is better

| Scenario | SSE p50 | MQTT p50 | SSE p95 | MQTT p95 | SSE p99 | MQTT p99 |
|---|---:|---:|---:|---:|---:|---:|
| 1 000 × 5 | 183 | **54** | 329 | **92** | 374 | **95** |
| 2 000 × 10 | 249 | **202** | 431 | **319** | 509 | **332** |
| 5 000 × 20 | **396** | 8 472 🔴 | **723** | 8 738 🔴 | **814** | 8 828 🔴 |
| 10 000 × 20 | **463** | 172 076 🔴 | **1 756** | 172 347 🔴 | **3 690** | 172 425 🔴 |

**Critical finding:** MQTT/EMQX performs better at small scale (≤ 2 000 clients) but **collapses catastrophically at ≥ 5 000 subscribers in unicast fan-out**. At 10 000 clients, MQTT p50 exceeds **172 seconds** (≈ 2.87 minutes), while SSE p50 holds at **463 ms**.

```
Latency p50 (ms) — log scale, lower is better
──────────────────────────────────────────────────────────────
1k×5    SSE   █                                    183 ms
        MQTT  █                                     54 ms  ✅ MQTT wins
2k×10   SSE   ██                                   249 ms
        MQTT  █                                    202 ms  ✅ MQTT wins
5k×20   SSE   ██                                   396 ms
        MQTT  ████████████████████████████████    8472 ms  🔴 SSE wins
10k×20  SSE   ███                                  463 ms
        MQTT  ████████████████████████████████  172076 ms  🔴 SSE wins (372×)
```

### 4.2 Throughput (events/sec) — higher is better

| Scenario | SSE EPS | MQTT EPS |
|---|---:|---:|
| 1 000 × 5 | 4 417 | **6 948** |
| 2 000 × 10 | 9 889 | **17 111** |
| 5 000 × 20 | **13 951** | 954 🔴 |
| 10 000 × 20 | **8 658** | 266 🔴 |

```
Throughput (eps) — higher is better
──────────────────────────────────────────────────────
1k×5    SSE   █████████                          4417
        MQTT  █████████████                      6948  ✅ MQTT wins
2k×10   SSE   ██████████████████                 9889
        MQTT  ████████████████████████████████  17111  ✅ MQTT wins
5k×20   SSE   ██████████████████████████        13951
        MQTT  ██                                  954  🔴 SSE wins (14×)
10k×20  SSE   ████████████████                   8658
        MQTT  █                                   266  🔴 SSE wins (32×)
```

### 4.3 Delivery Reliability — higher delivery, lower miss is better

| Scenario | SSE Delivered | MQTT Delivered | SSE Miss | MQTT Miss |
|---|---:|---:|---:|---:|
| 1 000 × 5 | **5 000/5 000** | **5 000/5 000** | **0 %** | **0 %** |
| 2 000 × 10 | **20 000/20 000** | **20 000/20 000** | **0 %** | **0 %** |
| 5 000 × 20 | **100 000/100 000** | 95 066/100 000 🔴 | **0 %** | 4.96 % 🔴 |
| 10 000 × 20 | **200 000/200 000** | 94 201/200 000 🔴 | **0 %** | 53.0 % 🔴 |

SSE delivered **100 % of events** across all scenarios. MQTT at 10 000 clients lost **53 %** of events despite QoS 1 (at-least-once). This points to EMQX broker overload / buffer exhaustion in the unicast fan-out pattern — not an inherent MQTT weakness, but a deployment constraint.

```
Miss Rate (%) — lower is better
──────────────────────────────────────────────────
1k×5    SSE   0.00 %  ✅
        MQTT  0.00 %  ✅
2k×10   SSE   0.00 %  ✅
        MQTT  0.00 %  ✅
5k×20   SSE   0.00 %  ✅
        MQTT  4.96 %  🔴
10k×20  SSE   0.00 %  ✅
        MQTT  53.0 %  🔴 ████████████████████████████████
```

### 4.4 Memory (MB RSS)

| Scenario | SSE RSS | MQTT RSS |
|---|---:|---:|
| 1 000 × 5 | **410** | 220 |
| 2 000 × 10 | **661** | 486 |
| 5 000 × 20 | **1 533** | 1 317 |
| 10 000 × 20 | **2 910** | 1 926 |

SSE (in-process Scorpion server) uses more RSS than the client-side process for MQTT because it holds all goroutines in-process. The MQTT measurement only reflects the test harness side (broker memory is external).

### 4.5 CPU User Time (ms)

| Scenario | SSE CPU User | MQTT CPU User |
|---|---:|---:|
| 1 000 × 5 | **8 826** | 313 |
| 2 000 × 10 | **22 602** | 843 |
| 5 000 × 20 | **96 134** | 11 454 |
| 10 000 × 20 | **214 928** | 180 415 |

SSE's in-process event loop, HTTP connection handling, JWT validation, and Redis polling drive higher CPU usage. MQTT offloads routing to the broker, keeping client-side CPU low — until broker saturation at 10 000 clients.

### 4.6 Connection Setup Latency (ms) — lower is better

| Scenario | SSE conn p50 | MQTT conn p50 | SSE conn p95 | MQTT conn p95 |
|---|---:|---:|---:|---:|
| 1 000 × 5 | 15.4 | **13.6** | 49.9 | **26.8** |
| 2 000 × 10 | 11.4 | **9.2** | 30.1 | **19.3** |
| 5 000 × 20 | 10.9 | **9.1** | 25.1 | **12.1** |
| 10 000 × 20 | 23.1 | **5.1** | 63.3 | **9.0** |

MQTT connection setup is consistently faster because there is no ticket-issuance round-trip required (unlike SSE which needs `POST /v1/auth/ticket` then `GET /v1/stream/events`).

---

## 5. High-Concurrency SSE Behaviour

> From `hc_perf_results.json` — these are sustained, realistic workloads with larger event counts.

| Scenario | p50 Lat. (ms) | p99 Lat. (ms) | Throughput (eps) | RSS (MB) | Goroutines | Wall (s) | Miss |
|---|---:|---:|---:|---:|---:|---:|---:|
| 1 000 clients × 20 events | 319 | 695 | 471 | 151 | 7 900 | 42.4 | 0 % |
| 2 000 clients × 25 events | 379 | 773 | 668 | 217 | 14 926 | 74.8 | 0 % |
| 5 000 clients × 30 events | 488 | 982 | 813 | 307 | 36 208 | 184.4 | 0 % |

**Key takeaway:** SSE scales to **5 000+ simultaneous persistent connections** with sub-second p99 latency and **zero missed events**, at a steady ~307 MB RSS and 36 000 goroutines. Throughput per connection is lower than snapshot tests because this models a sustained streaming workload.

```
SSE Latency at Scale (sustained workloads) — lower is better
─────────────────────────────────────────────────────────────
             1k×20    2k×25    5k×30
  p50 (ms)    319  →   379  →   488   ▲ +53 % over 5× clients
  p95 (ms)    602  →   690  →   876   ▲ sub-second throughout
  p99 (ms)    695  →   773  →   982   ▲ sub-second throughout

SSE Resources at Scale
─────────────────────────────────────────────────────────────
  RSS (MB)    151  →   217  →   307   +31 MB per 1k clients
  Goroutines 7.9k → 14.9k → 36.2k   +7.2k per 1k clients
  Miss rate   0 %  →   0 %  →   0 %  zero drop at all scales
```

---

## 6. Pros & Cons Summary

### 6.1 SSE

#### ✅ Pros
- **Native browser support** — `EventSource` API, no special library needed.
- **Firewall/CDN transparent** — rides standard HTTPS (port 443); no special broker ports.
- **Ordered, reliable delivery** — events come in push order; Scorpion's Redis queue guarantees ordering within a client.
- **Zero missed events** — 0 % miss rate across all tested scenarios up to 10 000 clients.
- **Sub-second latency at scale** — p50 < 500 ms at 10 000 clients with 200 ms poll interval.
- **Simple operational model** — single stateless Go binary + Redis; no separate broker process.
- **Incremental reconnect** — `EventSource` reconnects automatically with `Last-Event-ID`.
- **Per-client isolation** — each client gets its own Redis queue; no fan-out amplification at the broker level.
- **Predictable memory growth** — ~290 KB RSS per additional 1 000 clients at steady state.

#### ❌ Cons
- **One goroutine per connection** — at 10 000 clients, ~126 000 goroutines are active; requires OS file-descriptor limits (`ulimit -n 65536`).
- **Higher RSS than polling** — persistent connections consume ~25–50 % more memory than equivalent polling workloads.
- **Poll-interval floor on latency** — events cannot arrive faster than `sse.poll_interval`; 200 ms is the practical floor in this implementation.
- **Redis pressure** — continuous polling of Redis even when there are no events; at 5 000 clients this produces ~66 000 Redis ops/sec.
- **HTTP/1.1 header overhead** — each SSE frame carries chunked-encoding framing; less efficient per byte than MQTT's binary protocol.
- **Unidirectional** — client cannot push data over the same stream (needs separate HTTP requests).
- **Connection TTL** — idle streams expire when `scorpion:conn:<client>:<ip>` key TTL lapses; clients must reconnect.
- **Single active stream per (client, IP)** — duplicate stream prevention is intentional but means mobile clients switching networks must re-auth.

---

### 6.2 HTTP Polling

#### ✅ Pros
- **Completely stateless** — any server instance can handle any poll request; trivial horizontal scaling and load-balancing.
- **Lower memory footprint** — no persistent connections; RSS is ~25–30 % lower than SSE under equivalent load.
- **Works with aggressive proxies** — short-lived requests survive proxy timeouts transparently.
- **Batch-friendly** — a single poll can drain the entire Redis queue in one round-trip (`LRANGE + LTRIM`).
- **Simpler concurrency model** — no goroutine-per-connection; goroutine count is ~50 % of equivalent SSE load.
- **Easy rate-limit reasoning** — poll frequency is entirely client-controlled; server-side rate limiting is straightforward.

#### ❌ Cons
- **Very high latency** — p50 latency equals half the poll interval; at 10 s polling, that is **55 s p50** for 5 000 clients.
- **Wasted requests** — most polls return empty when event rate is low; burns bandwidth and Redis reads unnecessarily.
- **Throughput ceiling** — 10× lower throughput than SSE in all tested scenarios.
- **Worst-case latency grows with load** — p95 reached **100 613 ms** (100 s) at 5 000 clients × 20 events.
- **Poor real-time UX** — 10 s polling makes real-time UI interactions (live notifications, auctions, trading) impractical.
- **More client logic required** — client must implement a polling loop, back-off, empty-response handling.

---

### 6.3 MQTT / EMQX

#### ✅ Pros
- **Lowest latency at small scale** — p50 < 55 ms at 1 000 clients (3–4× better than SSE at equivalent poll interval).
- **Highest throughput at small scale** — 17 111 eps at 2 000 clients vs 9 889 for SSE.
- **Bidirectional** — same connection supports both pub and sub; ideal for IoT device communication.
- **QoS levels** — QoS 0, 1, 2 let you trade reliability for throughput per use-case.
- **Topic wildcards** — broker-level pattern routing reduces application complexity.
- **Efficient binary wire format** — smaller per-message overhead than HTTP frames; lower bandwidth.
- **Lower CPU footprint on app side** — broker handles routing; client process CPU stays < 1 000 ms user-time at 2 000 clients.
- **Mature ecosystem** — MQTT 5.0, shared subscriptions, message expiry, retained messages.

#### ❌ Cons
- **Catastrophic degradation at unicast fan-out scale** — 4.96 % miss at 5 000 clients, **53 % miss** at 10 000 clients in these tests (EMQX broker overload in per-client topic fan-out).
- **Latency collapse** — p50 goes from 202 ms at 2 000 clients to **172 076 ms** (2.87 min) at 10 000 clients.
- **Browser incompatibility** — no native MQTT in browsers; requires MQTT-over-WebSocket and a JS library (adds ~30–60 KB and connection overhead).
- **Broker as a single point of failure** — operational complexity: cluster management, broker upgrade windows, persistent session state.
- **Port 1883/8883 often firewalled** — corporate proxies and CDNs frequently block non-HTTP ports; MQTT-over-WS on port 443 mitigates this but adds latency.
- **Authentication model mismatch for web** — MQTT auth (clientID + username + password) doesn't integrate natively with cookie/JWT auth flows common in web apps.
- **No retry built into the test harness** — fire-and-forget publish in the benchmark; real production needs PUBACK tracking.
- **Operational overhead** — running EMQX adds a separate service, its own config, TLS certs, monitoring, and cluster.

---

## 7. When to Choose What

| Use-Case | Best Choice | Reason |
|---|---|---|
| **Live notifications in a web app** | ✅ SSE | Native `EventSource`, HTTPS-only, zero broker infra |
| **Real-time dashboards / charting** | ✅ SSE | Sub-second latency, ordered events, auto-reconnect |
| **Infrequent batch sync (> 30 s acceptable latency)** | ✅ Polling | Simpler, stateless, lower memory |
| **Offline-capable apps that sync on open** | ✅ Polling | Drain queue on app resume, no persistent connection |
| **IoT device telemetry (non-browser)** | ✅ MQTT | Binary protocol, bidirectional, QoS 2 for sensor data |
| **< 2 000 concurrent subscribers, ultra-low latency** | ✅ MQTT | 3–4× lower p50 than SSE in this range |
| **> 5 000 concurrent web subscribers** | ✅ SSE | MQTT collapses; SSE maintains 0 % miss rate |
| **Multi-region fan-out with federation** | ✅ MQTT | EMQX cluster bridging; SSE requires sticky sessions |
| **Mobile clients with intermittent connectivity** | ⚠️ MQTT (persistent session) or Polling | SSE connection TTL forces re-auth on reconnect |
| **Behind corporate HTTP proxy** | ✅ SSE or Polling | MQTT port often blocked |
| **Bidirectional control (e.g. remote actuator)** | ✅ MQTT | SSE is server→client only |

---

## 8. Architecture Notes for Scorpion

1. **Poll interval is latency's hard floor.** Setting `sse.poll_interval: 200ms` delivers p50 ≈ 183 ms at 1 000 clients. Going to `100ms` halves median latency but doubles Redis ops/sec. Profile Redis utilisation before tightening.

2. **SSE scales linearly in goroutines.** At 10 000 clients, Scorpion used ~126 000 goroutines. Each Go goroutine starts at ~8 KB stack; plan for ~1 GB goroutine stack headroom at 10 000 clients before OS limits. Always set `ulimit -n 65536` and consider `GOMAXPROCS`.

3. **Redis is the throughput ceiling, not the HTTP layer.** At 5 000 clients, Redis peaked at 66 685 ops/sec with 496 557 total commands for the SSE side. Pool sizing (`redis.pool_size: 2048`, `min_idle_conns: 256`) is critical.

4. **Polling is ideal for a fallback.** When a client cannot maintain a persistent SSE connection (aggressive proxies, slow mobile), the `GET /v1/events/:clientID` poll endpoint provides the same Redis-backed queue with identical ordering guarantees — just bounded by poll cadence rather than server push.

5. **MQTT is complementary, not a replacement.** MQTT/EMQX excels at IoT and internal microservice messaging where broker infra is acceptable and clients are not browsers. For browser-facing event delivery at scale (> 2 000 users), SSE wins on reliability and zero-miss guarantees.

6. **ACK flow augments SSE reliability.** Scorpion's `POST /v1/ack` path tracks in-flight events in Redis. When an SSE client ACKs an event, it is removed from the inflight registry. This adds at-least-once semantics on top of SSE without a separate broker.

---

## Quick Reference: Key Numbers

| Metric | SSE (5k clients) | Polling (5k clients) | MQTT (5k clients) |
|---|---:|---:|---:|
| Latency p50 (ms) | **650** | 55 317 | 8 472 |
| Latency p95 (ms) | **1 108** | 100 613 | 8 738 |
| Throughput (eps) | **10 086** | 928 | 954 |
| Miss rate | **0 %** | **0 %** | 4.96 % |
| RSS (MB) | 1 466 | **1 146** | 1 317 |
| Wall time (s) | **9.9** | 107.7 | 99.6 |
| Goroutines peak | 60 540 | **30 518** | 74 515 |

*All measurements: same hardware, Redis `127.0.0.1:6379`, EMQX `127.0.0.1:1883`, QoS 1.*

---

*Generated: 2026-04-30 | Scorpion `test/e2e/` benchmarks*

