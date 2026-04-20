# Scorpion — Improvement Suggestions

A full code-review pass across every package. Items are grouped by theme and
ordered from highest to lowest impact.

---

## 1. Redis / Data Layer

### 1.1 `EventStore.Push` — RPUSH, not LPUSH
`Push` uses `LPUSH` (prepend) while `Drain` uses `LRANGE 0 N-1` + `LTRIM N -1`
(reads from the head). This means the newest event is always at index 0 and
older events are pushed further back — **events arrive out of order**.

**Fix:** use `RPUSH` so the list is a proper FIFO queue: oldest at index 0,
newest at the tail. `Drain` already reads from the head, so no other change is
needed.

```go
// internal/repository/events.go
func (s *EventStore) Push(ctx context.Context, clientID, eventJSON string) error {
    return s.rdb.RPush(ctx, eventsKey(clientID), eventJSON).Err()
}
```

### 1.2 `EventStore.Push` — missing queue cap (unbounded memory)
A slow or disconnected client causes the Redis list to grow forever.  
Add a `LTRIM` right after `RPUSH` to cap the list at a configurable max depth
(e.g. 10 000 events). A single pipeline keeps it one round-trip:

```go
func (s *EventStore) Push(ctx context.Context, clientID, eventJSON string, maxDepth int64) error {
    key := eventsKey(clientID)
    pipe := s.rdb.Pipeline()
    pipe.RPush(ctx, key, eventJSON)
    pipe.LTrim(ctx, key, -maxDepth, -1)
    _, err := pipe.Exec(ctx)
    return err
}
```

Expose `max_queue_depth` in `config.SSE`.

### 1.3 Rate limiter — extra RTT for TTL read
`limiter.Allow` runs a pipeline (INCR + EXPIRE) and then issues a **separate**
`TTL` call to compute `ResetAt`. That is a wasted round-trip on every ticket
request.

**Fix:** derive the reset time from `time.Now().Add(60s)` on the first
increment (`count == 1`) and store it in a second Redis key, **or** simply
compute `ResetAt = time.Now().Add(60 * time.Second)` as an approximation —
it is already an approximation anyway, and removes the extra call entirely.

### 1.4 Rate limiter — INCR+EXPIRE is not truly atomic
Between `INCR` and `EXPIRE` the key has no TTL; a crash between the two leaves
a persistent key. Use a Lua script (same pattern as the ticket validator) or
`SET key 1 EX 60 NX` on first hit + `INCR` afterwards via a script.

### 1.5 `ConnectionStore.CleanupInstance` — ignores `Del` errors
```go
s.rdb.Del(ctx, key)  // error silently dropped
```
Wrap it and log the error.

### 1.6 `ConnectionStore.DeleteWithValue` — two round-trips
`GET` + conditional `DEL` has a TOCTOU race in a multi-instance deployment.
Replace it with a short Lua script (same approach as the ticket validator):

```lua
-- atomic_conn_delete.lua
if redis.call('GET', KEYS[1]) == ARGV[1] then
    return redis.call('DEL', KEYS[1])
end
return 0
```

---

## 2. SSE / Streaming

### 2.1 Polling is inefficient — switch to Redis Pub/Sub or keyspace notifications
The event loop polls Redis every `PollInterval` (default 1 s) regardless of
whether there are any events. This means:

- **Latency:** up to 1 s delivery delay on every event.
- **Wasted work:** N connected clients × 1 poll/s even when idle.

**Recommended approach — hybrid fan-out:**
1. `Push` writes to the Redis list **and** publishes a lightweight "wake"
   message on a per-client Pub/Sub channel (`scorpion:wake:<clientID>`).
2. The SSE loop `SUBSCRIBE`s to that channel. On wake signal → drain
   immediately. Keep the 1 s poll as a safety fallback.

This reduces delivery latency to ~milliseconds and eliminates idle polls.

### 2.2 `fmt.Fprintf` in the hot path allocates
Every event write calls `fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", …)`.
`fmt.Fprintf` uses reflection and allocates a format buffer internally.

**Fix:** use a pooled `bytes.Buffer` or write the fixed strings directly:

```go
var sseWriteBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func writeSSEEvent(w io.Writer, id, eventType, data string) {
    buf := sseWriteBufPool.Get().(*bytes.Buffer)
    buf.Reset()
    buf.WriteString("id: "); buf.WriteString(id); buf.WriteByte('\n')
    buf.WriteString("event: "); buf.WriteString(eventType); buf.WriteByte('\n')
    buf.WriteString("data: "); buf.WriteString(data); buf.WriteString("\n\n")
    w.Write(buf.Bytes())
    sseWriteBufPool.Put(buf)
}
```

### 2.3 Batch flush only when `len(batch) > 0`
Already done correctly ✓ — but note that the heartbeat path calls
`flusher.Flush()` without checking if the writer is still alive. Wrap writes in
a recoverable helper that detects a closed connection.

### 2.4 `json.Unmarshal([]byte(raw), &e)` — double allocation
`raw` is already a `string`; converting it to `[]byte` allocates. Use
`json.Unmarshal(unsafe.Slice(unsafe.StringData(raw), len(raw)), &e)` or
store events as `[]byte` in Redis in the first place. Alternatively, use
`github.com/bytedance/sonic` or `github.com/goccy/go-json` as a drop-in
replacement for `encoding/json` (2–3× faster for this workload).

---

## 3. Security

### 3.1 `POST /v1/events/:client_id` is unauthenticated
The new event-push endpoint has **no authentication at all**. Any caller on the
network can inject arbitrary events into any client's queue.

**Fix:** protect it with the same `TokenMiddleware` used by the SSE route, or
introduce a separate internal API key / shared secret checked in `EventHandler`.
At minimum add it to your threat model documentation.

### 3.2 TLS minimum version is TLS 1.2
TLS 1.2 allows several weak cipher suites (e.g. RSA key exchange — no forward
secrecy). Raise to TLS 1.3 or explicitly allow only ECDHE suites:

```go
tlsCfg := &tls.Config{
    MinVersion:   tls.VersionTLS13, // or VersionTLS12 + explicit CipherSuites
    CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256},
}
```

### 3.3 `eventPayload` type is duplicated
`eventPayload` is defined independently in both `internal/stream/loop.go` and
`internal/http/handlers/event.go`. A future change to one that doesn't mirror
the other will cause silent deserialization bugs.

**Fix:** move the canonical type to a shared `internal/domain` (or similar)
package and import it everywhere.

### 3.4 Event `data` field not validated
`eventRequest.Data` accepts any `json.RawMessage`. A caller can push
`null`, a bare number, or deeply nested JSON with no size limit.  
Add a configurable `max_event_bytes` and reject oversized payloads in
`EventHandler.Handle`.

---

## 4. Observability

### 4.1 No tracing
The codebase has no distributed tracing (OpenTelemetry). Adding
`go.opentelemetry.io/otel` with a Jaeger/OTLP exporter would allow you to
trace the full lifecycle of an event: push → Redis queue → drain → SSE write.

### 4.2 `RedisLatencySeconds` metric is registered but never used
The metric exists in `Metrics` but is never `.Observe()`d anywhere.  
Wrap every Redis call site (Push, Drain, ValidateAndConsume, Allow…) with a
`time.Since` observation, or instrument at the `redis.Client` hook level using
`redis.AddHook`.

### 4.3 Add a metric for queue depth
Expose `scorpion_queue_depth{client_id}` as a Gauge (sampled periodically or
on each drain) so you can alert when a client's backlog grows.

### 4.4 Structured log fields are inconsistent
Some call sites use `"client_id"`, others use `"clientID"`, `"ip"` vs
`"client_ip"`. Standardise on `snake_case` keys and define constants (like the
existing `middleware.ClientID` / `middleware.ClientIP`) for every shared field
name.

---

## 5. Architecture / Design

### 5.1 `NewServer` signature will keep growing
`NewServer` already takes 7 parameters and has a `// restruct these handlers`
comment. Introduce a `Handlers` struct:

```go
type Handlers struct {
    Ticket *TicketHandler
    SSE    *SSEHandler
    Event  *EventHandler
}

func NewServer(cfg config.Config, log *slog.Logger, rdb *redis.Client,
    ipStrategy realclientip.Strategy, h Handlers) *Server
```

### 5.2 Business logic leaks into handlers
`TicketHandler.Handle` does rate-limit checking, IP extraction, ticket
generation, and Redis storage all in one method. Extract a `TicketService`
(or `TicketUsecase`) that owns the orchestration so the handler only deals
with HTTP concerns (bind, respond, errors).

### 5.3 No interface boundaries on stores
`TicketHandler`, `SSEHandler`, and `EventHandler` all take concrete
`*redisstore.TicketStore` / `*redisstore.EventStore` pointers. Defining
minimal interfaces makes unit testing trivial without spinning up Redis:

```go
type TicketStorer interface {
    Store(ctx context.Context, clientID, ip, jti string, ttl time.Duration) error
    ValidateAndConsume(ctx context.Context, clientID, ip, jti string) (bool, error)
}
```

### 5.4 `metrics.NewMetrics()` uses `promauto` (global registry)
Multiple calls (e.g. in tests) will panic with "duplicate metrics collector"
because `promauto` registers into the global Prometheus registry.

**Fix:** accept a `prometheus.Registerer` parameter and use `prometheus.MustRegister`
on it, or use `prometheus.NewRegistry()` per test.

### 5.5 `start.go` should use a DI container or wire
The `runStart` function manually wires every dependency. As the graph grows,
this becomes fragile. Consider `google/wire` (compile-time) or
`uber-go/fx` (runtime) for dependency injection.

---

## 6. Resilience

### 6.1 No Redis reconnect / retry policy
If Redis goes down briefly, all in-flight polls and pushes fail silently with
no retry. Use `go-redis` `MaxRetries`, `MinRetryBackoff`, and `MaxRetryBackoff`
options, and add a circuit breaker (e.g. `sony/gobreaker`) around the event
drain path.

### 6.2 No backpressure on the SSE writer
If the underlying TCP write buffer fills up (slow client), `fmt.Fprintf` will
block, holding the goroutine indefinitely. Add a write deadline:

```go
if rc, ok := w.(interface{ SetWriteDeadline(time.Time) error }); ok {
    rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
}
```

### 6.3 Graceful drain on shutdown
On `SIGTERM`, `Shutdown()` closes the HTTP server which cancels all SSE
contexts. Any events still in the Redis list will be left undelivered.
Consider draining the in-flight batch to the writer before returning from
`RunLoop` when `ctx.Done()` fires.

---

## 7. Testing

### 7.1 No unit tests beyond `appmiddleware/ip_test.go`
The entire handler, store, and stream layer has zero unit tests.
Adding table-driven tests with mock stores (see §5.3) would catch regressions
immediately without needing Redis.

### 7.2 E2E test — `readSSEEvents` blocks on scanner when context is done
`bufio.Scanner.Scan()` blocks on `Read`. When the `readCtx` deadline fires the
context is cancelled but the scanner still blocks waiting for bytes from the
server. The context check (`select { case <-ctx.Done() }`) runs only
**between** lines, not during the blocking `Read`.

**Fix:** close the response body from a separate goroutine when the context
expires:

```go
go func() {
    <-readCtx.Done()
    streamResp.Body.Close()
}()
events, heartbeats := readSSEEvents(readCtx, streamResp.Body)
```

### 7.3 Prometheus global registry causes parallel test panics
`metrics.NewMetrics()` panics on the second call because `promauto` registers
into the global registry (see §5.4). Add `TestMain` or use a custom registry
per test instance.

---

## 8. Configuration

### 8.1 Secrets in plain YAML
`token_secret` and `ticket_secret` are stored in `config/config.yaml` in plain
text. Use environment variable overrides (`SCORPION_AUTH_TOKEN_SECRET`) or
integrate with a secrets manager (Vault, AWS SSM) for production deployments.

### 8.2 No read/write timeouts on the main HTTP server
`mainSrv` has no `ReadTimeout`, `WriteTimeout`, or `IdleTimeout`. A slow
connection can hold a goroutine open forever (Slowloris attack).  
**Note:** `WriteTimeout` must be disabled or set very high for SSE routes —
handle it per-handler with a context deadline instead.

```go
mainSrv := &http.Server{
    Addr:        fmt.Sprintf(":%d", cfg.Server.Port),
    Handler:     e,
    TLSConfig:   tlsCfg,
    ReadTimeout: 10 * time.Second,
    IdleTimeout: 60 * time.Second,
    // WriteTimeout intentionally omitted for SSE
}
```

---

## Quick-win priority list

| Priority | Item |
|---|---|
| 🔴 Critical | §1.1 LPUSH→RPUSH (events out of order) |
| 🔴 Critical | §3.1 Unauthenticated event-push endpoint |
| 🟠 High | §1.2 Unbounded queue cap |
| 🟠 High | §2.1 Polling → Pub/Sub (latency + CPU) |
| 🟠 High | §1.3 Extra TTL round-trip in rate limiter |
| 🟡 Medium | §3.3 Duplicate `eventPayload` type |
| 🟡 Medium | §4.2 `RedisLatencySeconds` never observed |
| 🟡 Medium | §5.3 Interface boundaries on stores |
| 🟡 Medium | §7.2 E2E scanner blocks on cancelled context |
| 🟢 Low | §5.1 `NewServer` handlers struct |
| 🟢 Low | §2.2 Pool SSE write buffer |
| 🟢 Low | §8.2 HTTP server timeouts |

