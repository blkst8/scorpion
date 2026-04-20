# 🦂 Scorpion — "GET OVER HERE!"

## AI Specification Document

**Project Name:** Scorpion  
**Language:** Go (Golang)  
**Protocol:** SSE over TCP (HTTP/2)  
**Version:** 0.3.0  
**Date:** 2026-04-17

---

## Table of Contents

1. [Overview](#1-overview)
2. [Architecture](#2-architecture)
3. [Configuration](#3-configuration)
4. [API Endpoints](#4-api-endpoints)
   - 4.1 [POST /v1/auth/ticket](#41-post-v1authticket)
   - 4.2 [GET /v1/stream/events](#42-get-v1streamevents)
5. [Authentication — Pre-Auth Ticket System](#5-authentication--pre-auth-ticket-system)
6. [Connection Policy — One Stream per User+IP](#6-connection-policy--one-stream-per-userip)
7. [Redis Data Model](#7-redis-data-model)
8. [SSE Event Loop](#8-sse-event-loop)
9. [Heartbeat / Keep-Alive](#9-heartbeat--keep-alive)
10. [Graceful Shutdown](#10-graceful-shutdown)
11. [Rate Limiting](#11-rate-limiting)
12. [Observability](#12-observability)
13. [IP Extraction — Trusted Proxy Resolution](#13-ip-extraction--trusted-proxy-resolution)
14. [Security Considerations](#14-security-considerations)
15. [Error Codes](#15-error-codes)
16. [Project Structure](#16-project-structure)

---

## 1. Overview

Scorpion is a lightweight, high-performance **Server-Sent Events (SSE)** server built in Go, operating over **HTTP/2 (TCP)**. It provides real-time event streaming to authenticated clients using a **pre-auth ticket system** backed by Redis. Each user is limited to a single active stream per IP address to prevent abuse and DDoS attacks.

---

## 2. Architecture

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

## 3. Configuration

All configuration values MUST be read from a config file (YAML/ENV). The following keys are required:

| Key                            | Type       | Description                                                     |
| ------------------------------ | ---------- | --------------------------------------------------------------- |
| `server.port`                  | `int`      | HTTP/2 server listen port                                       |
| `server.tls_cert`              | `string`   | Path to TLS certificate (required for HTTP/2)                   |
| `server.tls_key`               | `string`   | Path to TLS private key                                         |
| `server.shutdown_timeout`      | `duration` | Max wait time for graceful shutdown (e.g., `30s`)               |
| `sse.poll_interval`            | `duration` | Interval for polling Redis for new events                       |
| `sse.batch_size`               | `int`      | Max number of events to drain per tick (e.g., `100`)            |
| `sse.heartbeat_interval`       | `duration` | Interval for SSE keep-alive comments (e.g., `15s`)             |
| `auth.token_secret`            | `string`   | Secret key used to sign/verify the real token                   |
| `auth.ticket_secret`           | `string`   | Secret key used to sign JWT tickets                             |
| `auth.ticket_ttl`              | `duration` | Expiration time for issued JWT tickets                          |
| `ip.strategy`                  | `string`   | IP extraction strategy (see [Section 13](#13-ip-extraction--trusted-proxy-resolution)) |
| `ip.header`                    | `string`   | Header name to read IP from (default: `X-Forwarded-For`)       |
| `ip.trusted_count`             | `int`      | Number of trusted proxy hops (for `rightmost_trusted_count`)    |
| `ip.trusted_ranges`            | `[]string` | Trusted proxy CIDRs (for `rightmost_trusted_range`)            |
| `ratelimit.ticket_rpm`         | `int`      | Max requests per minute per IP on `/v1/auth/ticket`             |
| `ratelimit.ticket_burst`       | `int`      | Burst allowance for ticket rate limiter                         |
| `redis.address`                | `string`   | Redis server address                                            |
| `redis.password`               | `string`   | Redis password                                                  |
| `redis.db`                     | `int`      | Redis database index                                            |
| `observability.metrics_port`   | `int`      | Prometheus metrics exporter port                                |
| `observability.log_level`      | `string`   | Log level (`debug`, `info`, `warn`, `error`)                    |
| `observability.log_format`     | `string`   | Log format (`json`, `text`)                                     |

---

## 4. API Endpoints

### 4.1 POST `/v1/auth/ticket`

**Purpose:** Authenticate the client's real token and issue a short-lived JWT ticket for SSE access.

> ⚠️ This endpoint MUST be **rate-limited** per IP (see [Section 11 — Rate Limiting](#11-rate-limiting)).

#### Request

```
POST /v1/auth/ticket HTTP/2
Authorization: Bearer <real_token>
```

#### Logic

1. **Rate limit check:** If the client IP has exceeded `config.ratelimit.ticket_rpm`, reject with `429`.
2. Extract and validate the `Authorization: Bearer <real_token>` header against the authentication backend.
3. Extract `client_id` from the validated token claims.
4. Extract the client's IP address using the `realclientip-go` strategy (see [Section 13](#13-ip-extraction--trusted-proxy-resolution)).
5. Generate a JWT ticket containing:
   ```json
   {
     "sub": "<client_id>",
     "ip": "<client_ip>",
     "exp": "<now + config.auth.ticket_ttl>",
     "iat": "<now>",
     "jti": "<unique_ticket_id>"
   }
   ```
6. Store the ticket metadata in Redis (see [Redis Data Model](#7-redis-data-model)):
   - Key: `scorpion:ticket:<client_id>:<client_ip>`
   - Value: `<jti>`
   - TTL: `config.auth.ticket_ttl`
7. Return the ticket.

#### Response — `200 OK`

```json
{
  "ticket": "<jwt_ticket>",
  "expires_in": 300
}
```

#### Response — `401 Unauthorized`

```json
{
  "error": "invalid_token",
  "message": "The provided token is invalid or expired."
}
```

#### Response — `429 Too Many Requests`

```json
{
  "error": "rate_limit_exceeded",
  "message": "Too many ticket requests. Try again later.",
  "retry_after": 32
}
```

---

### 4.2 GET `/v1/stream/events`

**Purpose:** SSE endpoint that streams real-time events to authenticated clients.

#### Request

```
GET /v1/stream/events?ticket=<jwt_ticket> HTTP/2
Accept: text/event-stream
```

#### Logic (Connection Phase)

1. Extract the `ticket` query parameter.
2. Verify and decode the JWT ticket using `config.auth.ticket_secret`.
3. Extract `client_id` (`sub`), `ip`, and `jti` from the ticket claims.
4. **IP Validation:** Extract the requesting client's IP using the `realclientip-go` strategy and compare it with the `ip` claim in the ticket. If they differ → reject with `403`.
5. **Ticket Existence Check:** Look up `scorpion:ticket:<client_id>:<client_ip>` in Redis.
   - If key does not exist → reject with `403` (ticket expired or never issued).
   - If value does not match `jti` → reject with `403` (stale ticket).
6. **Invalidate Ticket (Single-Use):** Immediately delete `scorpion:ticket:<client_id>:<client_ip>` from Redis to prevent replay attacks. This MUST happen **before** registering the connection (see [Section 5](#5-authentication--pre-auth-ticket-system) for rationale).
7. **Atomic Duplicate Connection Check + Registration:** Use `SetNX` to atomically check and register the connection:
   ```
   SET scorpion:conn:<client_id>:<client_ip> "1" NX EX <conn_ttl>
   ```
   - If `SetNX` returns `false` → reject with `429` (already connected / DDoS prevention).
   - If `SetNX` returns `true` → connection registered, proceed.
8. Begin SSE stream.

#### Logic (Streaming Phase — Event Loop)

See [Section 8 — SSE Event Loop](#8-sse-event-loop) for the full specification.

#### SSE Message Format

```
id: <event_id>
event: <event_type>
data: <json_payload>

```

#### Response — `403 Forbidden`

```json
{
  "error": "access_denied",
  "message": "Ticket is invalid, expired, or IP mismatch."
}
```

#### Response — `429 Too Many Requests`

```json
{
  "error": "duplicate_connection",
  "message": "An active stream already exists for this user and IP."
}
```

---

## 5. Authentication — Pre-Auth Ticket System

The authentication model follows a **two-phase approach**:

```
Phase 1: Token → Ticket
┌────────┐                        ┌──────────┐
│ Client │── POST /v1/auth/ticket ─→│ Scorpion │
│        │   Authorization: Bearer  │          │
│        │◀── { ticket: <jwt> } ────│          │
└────────┘                        └──────────┘

Phase 2: Ticket → Stream
┌────────┐                                    ┌──────────┐
│ Client │── GET /v1/stream/events?ticket=jwt ─→│ Scorpion │
│        │◀── SSE stream ─────────────────────│          │
└────────┘                                    └──────────┘
```

**Why a ticket system?**
- SSE via `EventSource` in browsers does not support custom headers.
- Passing a short-lived JWT ticket in query params avoids exposing long-lived tokens in URLs/logs.
- Tickets are single-use, IP-bound, and short-lived — minimizing attack surface.

### Ticket Single-Use Enforcement

The ticket MUST be invalidated (deleted from Redis) immediately after the SSE connection is successfully established. This prevents a replay attack where an intercepted JWT ticket is reused before TTL expiry.

```
POST /v1/auth/ticket
  └─ Redis SET scorpion:ticket:user1:1.2.3.4 "jti-abc" EX 300

GET /v1/stream/events?ticket=<jwt>
  └─ Redis GET scorpion:ticket:user1:1.2.3.4 → "jti-abc" ✓
  └─ Redis DEL scorpion:ticket:user1:1.2.3.4        ← INVALIDATE
  └─ Redis SET scorpion:conn:user1:1.2.3.4 NX EX 60 ← REGISTER
  └─ Stream begins

GET /v1/stream/events?ticket=<same_jwt>  (replay attempt)
  └─ Redis GET scorpion:ticket:user1:1.2.3.4 → nil  ← BLOCKED
  └─ 403 Forbidden
```

> **Implementation Note:** The `GET` + `DEL` of the ticket key should be done atomically using a Lua script or Redis `GETDEL` command (Redis 6.2+) to prevent TOCTOU race conditions.

```lua
-- atomic_ticket_validate.lua
local key = KEYS[1]
local expected_jti = ARGV[1]

local stored_jti = redis.call('GET', key)
if stored_jti == false then
    return 0  -- ticket not found
end
if stored_jti ~= expected_jti then
    return -1 -- jti mismatch
end
redis.call('DEL', key)
return 1      -- success, ticket consumed
```

---

## 6. Connection Policy — One Stream per User+IP

To enforce **one stream per user per IP** and mitigate DDoS:

| Check                     | When                          | Action on Failure     |
| ------------------------- | ----------------------------- | --------------------- |
| Ticket exists in Redis    | SSE connection init           | `403 Forbidden`       |
| Ticket `jti` matches      | SSE connection init           | `403 Forbidden`       |
| Ticket consumed (DEL)     | SSE connection init           | — (replay prevented)  |
| IP matches ticket claim   | SSE connection init           | `403 Forbidden`       |
| Atomic SetNX conn key     | SSE connection init           | `429 Too Many Requests` |
| Refresh conn TTL          | Every SSE tick                | —                     |
| Remove conn from Redis    | SSE client disconnect/timeout | —                     |

### Atomic Connection Registration

> ⚠️ **Critical:** The connection existence check and registration MUST be a single atomic operation. A naive `EXISTS` + `SET` sequence has a race condition where two requests pass `EXISTS` before either writes `SET`.

```go
// ✅ CORRECT — Atomic SetNX
ok, err := rdb.SetNX(ctx, connKey, "1", connTTL).Result()
if !ok {
    // Connection already exists → 429
    http.Error(w, `{"error":"duplicate_connection"}`, http.StatusTooManyRequests)
    return
}
// Connection registered, proceed to stream

// ❌ WRONG — Race condition between EXISTS and SET
exists := rdb.Exists(ctx, connKey).Val()  // Two goroutines can both see 0
if exists > 0 {
    http.Error(w, "duplicate", 429)
    return
}
rdb.Set(ctx, connKey, "1", connTTL)       // Both goroutines reach here
```

### Flow Diagram

```
Client connects to /v1/stream/events?ticket=<jwt>
  │
  ├─ Decode JWT ticket
  │    └─ FAIL → 403
  │
  ├─ Extract IP via realclientip-go strategy
  ├─ Compare request IP == ticket.ip
  │    └─ MISMATCH → 403
  │
  ├─ Redis GETDEL scorpion:ticket:<client_id>:<ip> (atomic)
  │    └─ NOT FOUND or JTI MISMATCH → 403
  │
  ├─ Redis SET scorpion:conn:<client_id>:<ip> NX EX <ttl> (atomic)
  │    └─ NOT SET (already exists) → 429
  │
  ├─ BEGIN SSE STREAM
  │    ├─ Poll Redis every <poll_interval> — batch drain events
  │    ├─ Send heartbeat every <heartbeat_interval>
  │    └─ Refresh conn TTL every tick
  │
  └─ ON DISCONNECT
       └─ Redis DEL scorpion:conn:<client_id>:<ip>
```

---

## 7. Redis Data Model

### Keys

| Key Pattern                               | Type   | TTL                        | Description                              |
| ----------------------------------------- | ------ | -------------------------- | ---------------------------------------- |
| `scorpion:ticket:<client_id>:<ip>`        | String | `config.auth.ticket_ttl`   | Stores `jti` of the issued ticket        |
| `scorpion:conn:<client_id>:<ip>`          | String | Keepalive TTL (e.g., 60s)  | Tracks active SSE connections            |
| `scorpion:events:<client_id>`             | List   | Application-defined        | Event queue per client (LPUSH/LRANGE+LTRIM) |
| `scorpion:ratelimit:<ip>`                 | String | 60s (sliding window)       | Rate limit counter for ticket endpoint   |

### Example

```redis
# After POST /v1/auth/ticket
SET scorpion:ticket:user123:192.168.1.10 "abc-jti-456" EX 300

# After GET /v1/stream/events (ticket consumed + conn registered)
DEL scorpion:ticket:user123:192.168.1.10
SET scorpion:conn:user123:192.168.1.10 "1" NX EX 60

# Event published for user
LPUSH scorpion:events:user123 '{"type":"score_update","data":{"match_id":1,"score":"2-1"}}'

# Rate limit counter
INCR scorpion:ratelimit:192.168.1.10
EXPIRE scorpion:ratelimit:192.168.1.10 60
```

---

## 8. SSE Event Loop

### Batch Event Draining

> ⚠️ **Critical:** The event loop MUST drain **all available events** (up to `config.sse.batch_size`) per tick, not just a single event. A single `LPOP` per tick creates O(n × poll_interval) latency for burst events.

The drain operation MUST be atomic using a Redis pipeline to prevent partial reads:

```go
// Pseudocode — Atomic batch drain
func (h *SSEHandler) drainEvents(ctx context.Context, clientID string) ([]string, error) {
    key := fmt.Sprintf("scorpion:events:%s", clientID)
    batchSize := h.config.SSE.BatchSize // e.g., 100

    pipe := h.redis.Pipeline()
    lrangeCmd := pipe.LRange(ctx, key, 0, int64(batchSize-1))
    pipe.LTrim(ctx, key, int64(batchSize), -1)
    _, err := pipe.Exec(ctx)
    if err != nil {
        return nil, err
    }

    return lrangeCmd.Val(), nil
}
```

**Why `LRANGE` + `LTRIM` pipeline instead of multiple `LPOP`s?**

| Approach            | Events queued: 50, batch_size: 100 | Network roundtrips |
| ------------------- | ---------------------------------- | ------------------ |
| Single `LPOP`       | 1 event per tick (50 ticks total)  | 50                 |
| Loop of `LPOP`      | 50 events, 50 commands             | 50 (or 1 pipeline) |
| `LRANGE` + `LTRIM`  | 50 events, 2 commands (pipelined)  | 1                  |

### Connection TTL Refresh

> ⚠️ **Critical:** The connection TTL MUST be refreshed **every tick**, not only when events are delivered. If the event queue is empty for a period longer than `conn_ttl`, the connection key silently expires, allowing a duplicate connection.

```go
// Refresh MUST happen every tick, regardless of events
case <-ticker.C:
    // 1. Always refresh connection TTL
    h.redis.Expire(ctx, connKey, connTTL)

    // 2. Drain events
    events, err := h.drainEvents(ctx, clientID)
    // ...
```

### Full Event Loop

```go
func (h *SSEHandler) Stream(w http.ResponseWriter, r *http.Request) {
    flusher, ok := w.(http.Flusher)
    if !ok {
        http.Error(w, "streaming not supported", http.StatusInternalServerError)
        return
    }

    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("Connection", "keep-alive")

    connKey := fmt.Sprintf("scorpion:conn:%s:%s", clientID, clientIP)

    pollTicker := time.NewTicker(h.config.SSE.PollInterval)
    defer pollTicker.Stop()

    heartbeatTicker := time.NewTicker(h.config.SSE.HeartbeatInterval)
    defer heartbeatTicker.Stop()

    ctx := r.Context()

    // --- Metrics: increment active connections ---
    metrics.ActiveConnections.Inc()
    defer metrics.ActiveConnections.Dec()

    h.logger.Info("stream started",
        "client_id", clientID,
        "ip", clientIP,
    )

    defer func() {
        // Cleanup on disconnect
        h.redis.Del(context.Background(), connKey)
        h.logger.Info("stream closed",
            "client_id", clientID,
            "ip", clientIP,
        )
    }()

    for {
        select {
        case <-ctx.Done():
            // Client disconnected
            return

        case <-heartbeatTicker.C:
            // Send SSE heartbeat comment (see Section 9)
            fmt.Fprintf(w, ": heartbeat %d\n\n", time.Now().Unix())
            flusher.Flush()

        case <-pollTicker.C:
            // 1. Always refresh connection TTL — even if no events
            h.redis.Expire(ctx, connKey, connTTL)

            // 2. Batch drain events
            events, err := h.drainEvents(ctx, clientID)
            if err != nil {
                h.logger.Error("failed to drain events",
                    "client_id", clientID,
                    "error", err,
                )
                metrics.EventErrors.Inc()
                continue
            }

            // 3. Write all events to the SSE stream
            for _, event := range events {
                var e EventPayload
                if err := json.Unmarshal([]byte(event), &e); err != nil {
                    h.logger.Warn("malformed event", "raw", event, "error", err)
                    continue
                }

                fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n",
                    e.ID, e.Type, e.Data)

                metrics.EventsDelivered.Inc()
            }

            if len(events) > 0 {
                flusher.Flush()
            }
        }
    }
}
```

---

## 9. Heartbeat / Keep-Alive

SSE connections traveling through proxies, load balancers, CDNs, and cloud infrastructure can be silently terminated if idle. Scorpion MUST send periodic **SSE comment lines** as heartbeats.

### Specification

| Parameter                        | Value                               |
| -------------------------------- | ----------------------------------- |
| **Interval**                     | `config.sse.heartbeat_interval`     |
| **Default**                      | `15s`                               |
| **Format**                       | SSE comment (line starting with `:`) |
| **Impact on client**             | None — SSE comments are ignored by `EventSource` |

### Wire Format

```
: heartbeat 1713360000

```

The timestamp is included for debugging purposes. The line MUST end with `\n\n` (double newline) to be a valid SSE block.

### Why This Matters

```
Without heartbeat:
Client ──── SSE ──── [idle 60s] ──── LB/Proxy kills connection
                                      └─ Client sees no error, just silence
                                      └─ scorpion:conn key expires
                                      └─ Ghost state

With heartbeat (every 15s):
Client ──── SSE ──── : heartbeat ──── : heartbeat ──── event ────
                      (15s)            (30s)           (45s)
                      └─ Connection stays alive through all hops
```

---

## 10. Graceful Shutdown

When the Scorpion server receives a termination signal (`SIGINT`, `SIGTERM`), it MUST perform a graceful shutdown to prevent data loss and ghost connections.

### Shutdown Sequence

```
Signal received (SIGINT / SIGTERM)
  │
  ├─ 1. Stop accepting new connections
  │     └─ HTTP server stops listening
  │
  ├─ 2. Signal all active SSE goroutines to stop
  │     └─ Cancel the shared context
  │
  ├─ 3. Wait for active streams to flush & close (up to shutdown_timeout)
  │     └─ Each goroutine:
  │         a. Sends final heartbeat / flush
  │         b. DEL scorpion:conn:<client_id>:<ip> from Redis
  │         c. Returns
  │
  ├─ 4. Close Redis connection pool
  │
  ├─ 5. Flush pending metrics / logs
  │
  └─ 6. Exit with code 0
```

### Implementation

```go
func main() {
    // ... setup ...

    srv := &http.Server{
        Addr:    fmt.Sprintf(":%d", cfg.Server.Port),
        Handler: router,
    }

    // Channel to signal shutdown
    stop := make(chan os.Signal, 1)
    signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

    // Start server in goroutine
    go func() {
        log.Info("scorpion starting", "port", cfg.Server.Port)
        if err := srv.ListenAndServeTLS(cfg.Server.TLSCert, cfg.Server.TLSKey); err != nil &&
            err != http.ErrServerClosed {
            log.Fatal("server error", "error", err)
        }
    }()

    // Block until signal
    sig := <-stop
    log.Info("shutdown signal received", "signal", sig)

    // Create shutdown context with timeout
    ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
    defer cancel()

    // Graceful shutdown — waits for active connections to finish
    if err := srv.Shutdown(ctx); err != nil {
        log.Error("forced shutdown", "error", err)
    }

    // Cleanup: bulk remove connection keys
    connManager.CleanupAll(ctx)

    // Close Redis
    redisClient.Close()

    // Flush metrics
    metricsExporter.Shutdown(ctx)

    log.Info("scorpion stopped")
}
```

### Connection Cleanup on Shutdown

Each SSE handler goroutine should use the request context. When `srv.Shutdown(ctx)` is called, Go's HTTP server cancels all active request contexts, which triggers the `<-ctx.Done()` branch in the event loop, ensuring `DEL scorpion:conn:*` runs for every active connection.

> **Edge Case:** If the shutdown timeout expires before all goroutines complete, Scorpion MUST perform a bulk cleanup of all its connection keys. Use a **server instance ID** stored in each conn value to identify keys belonging to this instance:

```redis
SET scorpion:conn:user1:1.2.3.4 "instance-abc" NX EX 60
```

On forced shutdown, scan and delete:
```redis
SCAN 0 MATCH scorpion:conn:* COUNT 100
-- for each key where value == "instance-abc" → DEL
```

---

## 11. Rate Limiting

### Ticket Endpoint Rate Limiting

The `POST /v1/auth/ticket` endpoint MUST be rate-limited per IP address to prevent:
- Brute-force token validation attempts
- Ticket generation flooding
- Redis key exhaustion

### Strategy: Sliding Window Counter (Redis-backed)

```
Key:    scorpion:ratelimit:<client_ip>
TTL:    60 seconds
Limit:  config.ratelimit.ticket_rpm (e.g., 10 requests/minute)
Burst:  config.ratelimit.ticket_burst (e.g., 3 above limit)
```

### Implementation

```go
// Rate limit check using Redis INCR + EXPIRE
func (rl *RateLimiter) Allow(ctx context.Context, ip string) (bool, int, error) {
    key := fmt.Sprintf("scorpion:ratelimit:%s", ip)

    pipe := rl.redis.Pipeline()
    incrCmd := pipe.Incr(ctx, key)
    pipe.Expire(ctx, key, 60*time.Second) // reset window every 60s
    _, err := pipe.Exec(ctx)
    if err != nil {
        return false, 0, err
    }

    count := int(incrCmd.Val())
    limit := rl.config.RateLimit.TicketRPM + rl.config.RateLimit.TicketBurst

    remaining := limit - count
    if remaining < 0 {
        remaining = 0
    }

    return count <= limit, remaining, nil
}
```

### Response Headers

All responses from `/v1/auth/ticket` MUST include rate limit headers:

```
X-RateLimit-Limit: 10
X-RateLimit-Remaining: 7
X-RateLimit-Reset: 1713360060
```

When rate limit is exceeded:

```
HTTP/2 429 Too Many Requests
Retry-After: 32
X-RateLimit-Limit: 10
X-RateLimit-Remaining: 0
X-RateLimit-Reset: 1713360060

{
  "error": "rate_limit_exceeded",
  "message": "Too many ticket requests. Try again later.",
  "retry_after": 32
}
```

---

## 12. Observability

Scorpion MUST include observability from day one. A real-time streaming server without metrics and structured logging is a black box in production.

### 12.1 Metrics (Prometheus)

Expose a Prometheus-compatible metrics endpoint on a **separate port** (`config.observability.metrics_port`) to avoid exposing internal metrics to public traffic.

```
GET http://localhost:<metrics_port>/metrics
```

#### Required Metrics

| Metric Name                              | Type      | Labels                    | Description                                |
| ---------------------------------------- | --------- | ------------------------- | ------------------------------------------ |
| `scorpion_active_connections`            | Gauge     | —                         | Currently active SSE streams               |
| `scorpion_connections_total`             | Counter   | `status` (ok/rejected)    | Total SSE connection attempts              |
| `scorpion_events_delivered_total`        | Counter   | `event_type`              | Total events sent to clients               |
| `scorpion_events_drained_per_tick`       | Histogram | —                         | Batch size distribution per poll tick       |
| `scorpion_event_drain_errors_total`      | Counter   | —                         | Failed Redis drain operations              |
| `scorpion_tickets_issued_total`          | Counter   | —                         | Total tickets issued                       |
| `scorpion_tickets_rejected_total`        | Counter   | `reason` (invalid/expired/rate_limited) | Rejected ticket requests |
| `scorpion_auth_ip_mismatch_total`        | Counter   | —                         | SSE connections rejected due to IP mismatch |
| `scorpion_duplicate_connections_total`   | Counter   | —                         | Duplicate stream attempts blocked          |
| `scorpion_heartbeats_sent_total`         | Counter   | —                         | Total heartbeat comments sent              |
| `scorpion_redis_latency_seconds`         | Histogram | `operation`               | Redis operation latency                    |
| `scorpion_ratelimit_exceeded_total`      | Counter   | —                         | Rate limit hits on ticket endpoint         |

#### Example Prometheus Queries

```promql
# Active connections right now
scorpion_active_connections

# Events per second
rate(scorpion_events_delivered_total[5m])

# Connection rejection rate
rate(scorpion_connections_total{status="rejected"}[5m])

# P99 Redis latency
histogram_quantile(0.99, rate(scorpion_redis_latency_seconds_bucket[5m]))
```

### 12.2 Structured Logging

All log entries MUST be structured (JSON format in production) and include contextual fields.

#### Required Log Fields

Every log line from SSE handlers MUST include:

| Field        | Description                        |
| ------------ | ---------------------------------- |
| `client_id`  | User identifier from JWT           |
| `ip`         | Client IP address                  |
| `jti`        | Ticket ID                          |
| `trace_id`   | Request trace ID (for correlation) |

#### Log Events

| Event                  | Level  | Additional Fields                         |
| ---------------------- | ------ | ----------------------------------------- |
| Ticket issued          | `info` | `client_id`, `ip`, `expires_at`           |
| Ticket rejected        | `warn` | `ip`, `reason`                            |
| Stream started         | `info` | `client_id`, `ip`, `jti`                  |
| Stream closed          | `info` | `client_id`, `ip`, `duration`, `events_sent` |
| IP mismatch            | `warn` | `client_id`, `ticket_ip`, `request_ip`    |
| Duplicate connection   | `warn` | `client_id`, `ip`                         |
| Rate limit exceeded    | `warn` | `ip`, `count`, `limit`                    |
| Redis error            | `error`| `operation`, `key`, `error`               |
| Event drain failed     | `error`| `client_id`, `error`                      |
| Heartbeat sent         | `debug`| `client_id`, `ip`                         |
| Shutdown initiated     | `info` | `signal`, `active_connections`             |
| Shutdown complete      | `info` | `duration`                                 |

#### Library Recommendation

Use Go's built-in `log/slog` (Go 1.21+) for zero-dependency structured logging:

```go
logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
    Level: cfg.Observability.LogLevel,
}))

logger.Info("stream started",
    "client_id", clientID,
    "ip", clientIP,
    "jti", ticketJTI,
    "trace_id", traceID,
)
```

Output:
```json
{
  "time": "2026-04-17T10:30:00.000Z",
  "level": "INFO",
  "msg": "stream started",
  "client_id": "user123",
  "ip": "192.168.1.10",
  "jti": "abc-jti-456",
  "trace_id": "req-789"
}
```

### 12.3 Health Check Endpoint

```
GET /healthz
```

Returns `200 OK` if the server is healthy and Redis is reachable:

```json
{
  "status": "ok",
  "redis": "connected",
  "active_connections": 142,
  "uptime_seconds": 86400
}
```

Returns `503 Service Unavailable` if Redis is unreachable:

```json
{
  "status": "degraded",
  "redis": "disconnected",
  "error": "dial tcp: connection refused"
}
```

---

## 13. IP Extraction — Trusted Proxy Resolution

Scorpion uses the [`realclientip-go`](https://github.com/realclientip/realclientip-go) library for secure, production-grade client IP extraction. This replaces any custom IP parsing logic and handles all edge cases around `X-Forwarded-For` header trust, spoofing prevention, and multi-hop proxy chains.

### Why `realclientip-go`?

| Concern | DIY Implementation | `realclientip-go` |
|---|---|---|
| XFF spoofing prevention | Manual right-to-left parsing | Built-in, battle-tested |
| Multi-hop proxy chains | Custom CIDR matching | Declarative strategies |
| Edge cases (IPv6, malformed headers) | Easy to miss | Fully handled |
| Maintenance burden | You own it forever | Community-maintained |
| Strategy flexibility | Hardcoded logic | Swappable at config level |

### Installation

```bash
go get github.com/realclientip/realclientip-go
```

### Available Strategies

The library provides multiple strategies. Choose one based on your deployment topology:

| Strategy | Config Value | Use When | Description |
|---|---|---|---|
| `RightmostTrustedCount` | `rightmost_trusted_count` | You know the exact number of proxy hops | Counts N IPs from the right of XFF and returns the next one |
| `RightmostTrustedRange` | `rightmost_trusted_range` | You know your proxy CIDRs | Walks right-to-left, skipping trusted CIDRs, returns first untrusted |
| `RightmostNonPrivate` | `rightmost_non_private` | Quick setup, proxies use private IPs | Returns the rightmost non-private (public) IP from XFF |
| `SingleIPHeader` | `single_ip_header` | Proxy sets a single-IP header (e.g., `X-Real-IP`) | Reads IP from a header that contains exactly one IP |
| `RemoteAddr` | `remote_addr` | No proxy, direct connections | Uses `RemoteAddr` directly |

### Configuration

```yaml
# config/config.yaml

ip:
  # Strategy to use (see table above)
  strategy: "rightmost_trusted_count"

  # Header to read IPs from (used by most strategies)
  header: "X-Forwarded-For"

  # For "rightmost_trusted_count" — number of trusted proxy hops
  # Example: Client → Cloudflare → Nginx → Scorpion = 2 hops
  trusted_count: 2

  # For "rightmost_trusted_range" — trusted proxy CIDRs
  trusted_ranges:
    - "10.0.0.0/8"
    - "172.16.0.0/12"
    - "173.245.48.0/20"   # Cloudflare
    - "103.21.244.0/22"   # Cloudflare
```

### Strategy Selection by Deployment

```
Deployment: Client → Cloudflare → Nginx → Scorpion
  └─ strategy: rightmost_trusted_count
  └─ trusted_count: 2  (Cloudflare + Nginx)

Deployment: Client → AWS ALB → Scorpion
  └─ strategy: rightmost_trusted_count
  └─ trusted_count: 1  (ALB only)

Deployment: Client → Nginx (same machine) → Scorpion
  └─ strategy: single_ip_header
  └─ header: "X-Real-IP"  (Nginx sets this)

Deployment: Client → Scorpion (direct, no proxy)
  └─ strategy: remote_addr
```

### Implementation

#### Strategy Factory

```go
// internal/appmiddleware/ip.go
package middleware

import (
    "fmt"
    "net"

    "github.com/realclientip/realclientip-go"
)

type IPConfig struct {
    Strategy      string   `yaml:"strategy"`
    Header        string   `yaml:"header"`
    TrustedCount  int      `yaml:"trusted_count"`
    TrustedRanges []string `yaml:"trusted_ranges"`
}

// NewIPStrategy creates a realclientip strategy from config.
// This is called once at startup — the returned strategy is safe for concurrent use.
func NewIPStrategy(cfg IPConfig) (realclientip.Strategy, error) {
    headerName := cfg.Header
    if headerName == "" {
        headerName = "X-Forwarded-For"
    }

    switch cfg.Strategy {

    case "rightmost_trusted_count":
        if cfg.TrustedCount <= 0 {
            return nil, fmt.Errorf("trusted_count must be > 0 for rightmost_trusted_count strategy")
        }
        return realclientip.NewRightmostTrustedCountStrategy(headerName, cfg.TrustedCount)

    case "rightmost_trusted_range":
        if len(cfg.TrustedRanges) == 0 {
            return nil, fmt.Errorf("trusted_ranges must not be empty for rightmost_trusted_range strategy")
        }
        ranges := make([]net.IPNet, 0, len(cfg.TrustedRanges))
        for _, cidr := range cfg.TrustedRanges {
            _, network, err := net.ParseCIDR(cidr)
            if err != nil {
                return nil, fmt.Errorf("invalid trusted range %q: %w", cidr, err)
            }
            ranges = append(ranges, *network)
        }
        return realclientip.NewRightmostTrustedRangeStrategy(headerName, ranges)

    case "rightmost_non_private":
        return realclientip.NewRightmostNonPrivateStrategy(headerName)

    case "single_ip_header":
        return realclientip.NewSingleIPHeaderStrategy(headerName)

    case "remote_addr", "":
        return realclientip.NewRemoteAddrStrategy()

    default:
        return nil, fmt.Errorf("unknown IP extraction strategy: %q", cfg.Strategy)
    }
}
```

#### Server Initialization

```go
// cmd/scorpion/main.go
func main() {
    cfg := config.Load()

    // Build IP strategy once at startup
    ipStrategy, err := middleware.NewIPStrategy(cfg.IP)
    if err != nil {
        log.Fatal("failed to initialize IP strategy", "error", err)
    }

    // Inject into handlers
    ticketHandler := auth.NewTicketHandler(cfg, redisClient, ipStrategy)
    streamHandler := stream.NewSSEHandler(cfg, redisClient, ipStrategy)

    // ...
}
```

#### Usage in Handlers

```go
// internal/auth/handler.go
func (h *TicketHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // Extract real client IP — safe, spoofing-resistant
    clientIP, err := h.ipStrategy.ClientIP(r.Header, r.RemoteAddr)
    if err != nil {
        h.logger.Error("failed to extract client IP", "error", err)
        http.Error(w, `{"error":"internal_error"}`, http.StatusInternalServerError)
        return
    }

    h.logger.Debug("client IP extracted",
        "ip", clientIP,
        "remote_addr", r.RemoteAddr,
        "xff", r.Header.Get("X-Forwarded-For"),
    )

    // ... use clientIP in JWT ticket claims and Redis key
}
```

```go
// internal/stream/handler.go
func (h *SSEHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // Same strategy, same result
    clientIP, err := h.ipStrategy.ClientIP(r.Header, r.RemoteAddr)
    if err != nil {
        http.Error(w, `{"error":"internal_error"}`, http.StatusInternalServerError)
        return
    }

    // Compare with ticket claim
    if clientIP != ticketClaims.IP {
        h.logger.Warn("IP mismatch",
            "ticket_ip", ticketClaims.IP,
            "request_ip", clientIP,
        )
        metrics.AuthIPMismatch.Inc()
        http.Error(w, `{"error":"access_denied"}`, http.StatusForbidden)
        return
    }

    // ... proceed with connection
}
```

### How Spoofing Is Prevented

Example with `rightmost_trusted_count: 2` (Cloudflare + Nginx):

```
# Legitimate request:
Client (1.2.3.4) → Cloudflare → Nginx → Scorpion
XFF: "1.2.3.4, 172.16.0.1"
RemoteAddr: 10.0.0.1

Strategy: skip 2 from the right → 1.2.3.4 ✅

# Attacker tries to forge:
curl -H "X-Forwarded-For: 6.6.6.6" → Cloudflare → Nginx → Scorpion
XFF: "6.6.6.6, attacker-ip, 172.16.0.1"
RemoteAddr: 10.0.0.1

Strategy: skip 2 from the right → attacker-ip ✅ (real IP, not forged 6.6.6.6)
```

The forged `6.6.6.6` is pushed further left and ignored. The library always resolves the correct IP.

### Testing

```go
// internal/appmiddleware/ip_test.go
func TestIPExtraction(t *testing.T) {
    strategy, _ := realclientip.NewRightmostTrustedCountStrategy("X-Forwarded-For", 1)

    tests := []struct {
        name       string
        xff        string
        remoteAddr string
        wantIP     string
    }{
        {
            name:       "normal proxy chain",
            xff:        "1.2.3.4, 10.0.0.1",
            remoteAddr: "10.0.0.1:8080",
            wantIP:     "1.2.3.4",
        },
        {
            name:       "forged XFF",
            xff:        "6.6.6.6, 5.5.5.5, 10.0.0.1",
            remoteAddr: "10.0.0.1:8080",
            wantIP:     "5.5.5.5", // real client, not forged 6.6.6.6
        },
        {
            name:       "no XFF header",
            xff:        "",
            remoteAddr: "1.2.3.4:8080",
            wantIP:     "1.2.3.4",
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            h := http.Header{}
            if tt.xff != "" {
                h.Set("X-Forwarded-For", tt.xff)
            }
            gotIP, err := strategy.ClientIP(h, tt.remoteAddr)
            require.NoError(t, err)
            assert.Equal(t, tt.wantIP, gotIP)
        })
    }
}
```

---

## 14. Security Considerations

| Concern                  | Mitigation                                                                 |
| ------------------------ | -------------------------------------------------------------------------- |
| Token leak in URL/logs   | Short-lived JWT ticket (not the real token) passed in query params         |
| IP spoofing              | `realclientip-go` library with configurable strategy; never blindly trusts `X-Forwarded-For` |
| Ticket replay            | Ticket deleted from Redis immediately after SSE connection (single-use via `GETDEL` or Lua script) |
| DDoS / connection flood  | `SetNX` on `scorpion:conn` key blocks duplicate streams per user+IP       |
| Race condition           | Atomic `SetNX` for connection registration; Lua script for ticket validation+deletion |
| Stale connections        | Redis TTL on `scorpion:conn` keys, refreshed every tick; heartbeats keep proxies alive |
| Event queue overflow     | Apply max-length on `scorpion:events:<client_id>` lists (`LTRIM`)         |
| Ticket endpoint abuse    | Per-IP rate limiting with sliding window counter in Redis                  |
| Ghost connections        | Graceful shutdown cleans all `scorpion:conn` keys; instance ID enables bulk cleanup on crash |

---

## 15. Error Codes

| HTTP Status | Error Code              | Description                                    |
| ----------- | ----------------------- | ---------------------------------------------- |
| `401`       | `invalid_token`         | Bearer token is invalid or expired             |
| `403`       | `access_denied`         | Ticket invalid, expired, IP mismatch, or missing |
| `429`       | `duplicate_connection`  | Active stream already exists for user+IP       |
| `429`       | `rate_limit_exceeded`   | Too many ticket requests from this IP          |
| `500`       | `internal_error`        | Unexpected server error                        |
| `503`       | `service_unavailable`   | Redis unavailable or server shutting down       |

---

## 16. Project Structure

```
scorpion/
├── cmd/
│   └── scorpion/
│       └── main.go                 # Entry point — HTTP/2 server bootstrap, signal handling, graceful shutdown
├── internal/
│   ├── config/
│   │   └── config.go               # Configuration loading (YAML/ENV), trusted proxy parsing
│   ├── auth/
│   │   ├── handler.go              # POST /v1/auth/ticket handler
│   │   ├── jwt.go                  # JWT ticket generation & validation
│   │   └── middleware.go           # Token verification middleware
│   ├── stream/
│   │   ├── handler.go              # GET /v1/stream/events SSE handler
│   │   ├── loop.go                 # Redis polling event loop + batch drain
│   │   └── heartbeat.go           # SSE heartbeat / keep-alive logic
│   ├── redis/
│   │   ├── client.go               # Redis client initialization
│   │   ├── ticket.go               # Ticket store/check/consume operations (Lua scripts)
│   │   ├── connection.go           # Connection tracking (SetNX, Expire, Del)
│   │   └── events.go               # Event queue operations (LRANGE+LTRIM pipeline)
│   ├── ratelimit/
│   │   └── limiter.go              # Per-IP sliding window rate limiter
│   ├── middleware/
│   │   ├── ip.go                   # realclientip-go strategy factory & initialization
│   │   └── ip_test.go              # IP extraction tests (spoofing, edge cases)
│   └── observability/
│       ├── metrics.go              # Prometheus metric definitions & registration
│       ├── logger.go               # Structured logger setup (slog)
│       └── health.go               # GET /healthz handler
├── scripts/
│   └── redis/
│       └── atomic_ticket_validate.lua  # Lua script for atomic ticket consumption
├── config/
│   └── config.yaml                 # Default configuration file
├── go.mod
├── go.sum
└── README.md
```

---

> 🦂 *Scorpion — "*
