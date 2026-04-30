# AGENTS.md

## Big Picture (Scorpion service map)
- `main.go` -> Cobra CLI (`cmd/scorpion/start.go`) -> wires config, logger, metrics, Redis, repositories, handlers, then starts Echo server + metrics server.
- Core runtime boundary is `internal/http/server.go`: API routing, middleware stacks, and separate `/metrics` listener.
- Persistence/state is Redis-only (`internal/repository/*`): per-client event queues, one-time tickets, active-connection locks, and in-flight ACK tracking.
- Streaming loop (`internal/stream/loop.go`) is pull-based: every `sse.poll_interval` it drains Redis and writes SSE frames; heartbeats are independent (`internal/stream/heartbeat.go`).

## Request/Data Flows You Need To Preserve
- Auth ticket flow: `POST /v1/auth/ticket` (`internal/http/handlers/ticket.go`) requires Bearer token, applies per-IP limiter, creates short-lived JWT ticket + Redis JTI (`scorpion:ticket:<client>:<ip>`).
- SSE flow: `GET /v1/stream/events?ticket=...` (`internal/http/handlers/stream.go`) validates ticket JWT, enforces request IP == ticket IP, atomically consumes ticket, then `SetNX` registers one active stream per `(client,ip)`.
- Event ingest: `POST /v1/events/:client_id` (`internal/http/handlers/event.go`) only allows `sub == :client_id`, server generates event ID, pushes JSON payload to Redis list.
- Poll mode exists for benchmarks: `GET /v1/events/:client_id` (`internal/http/handlers/poll.go`) drains up to batch size in one request; this is intentionally different from SSE timing.
- ACK flow: `POST /v1/ack` (`internal/http/handlers/ack.go`) validates against in-flight registry, publishes via `ack.Publisher`, then marks acked in Redis.

## Redis Model / Atomicity Patterns
- Event queue key: `scorpion:events:<client>` (`internal/repository/events.go`) with `RPUSH + LTRIM` on write, `LRANGE + LTRIM + LLEN` pipeline on drain.
- Ticket consume is atomic Lua (`internal/repository/scripts/atomic_ticket_validate.lua`): validates JTI and deletes key in one round-trip.
- Connection lock key: `scorpion:conn:<client>:<ip>` (`internal/repository/connection.go`) uses `SetArgs{Mode:NX, TTL}` to block duplicate streams.
- Disconnect safety: `delete_if_owner.lua` deletes conn key only if value matches instance ID (multi-instance shutdown protection).
- Rate limiting is Lua-backed counter (`internal/ratelimit/scripts/atomic_ratelimit.lua`) on `scorpion:ratelimit:<ip>`.

## Developer Workflows (project-specific)
- Start dev server: `go run main.go start --config config/config.yaml` (requires Redis reachable at configured address).
- E2E prerequisite: run Redis only stack via `docker compose -f docker-compose.test.yml up -d`, then `go test -v -count=1 ./test/e2e/... -timeout 5m`.
- Focused perf suites:
  - `go test -v -run TestPerf ./test/e2e/... -timeout 10m`
  - `go test -v -run TestPerf_HighConcurrency ./test/e2e/... -timeout 30m`
  - `go test -v -run TestPerf_SSEvsPoll ./test/e2e/... -timeout 60m` (often needs `ulimit -n 65536`).
- Local cert helper: `scripts/gen-certs.sh` writes self-signed certs expected by config paths.

## Conventions/Gotchas For AI Edits
- Handler dependencies are injected through `handlers.HTTPHandlers`; new handlers should follow this struct-based wiring pattern.
- Metrics are first-class: update counters/histograms alongside behavior changes (`internal/metrics/metrics.go`) and prefer label sets already used by nearby handlers.
- Keep auth semantics strict: token-based routes require JWT `sub` equality checks; ticket-based routes rely on IP middleware + ticket validation.
- ACK publisher is currently a log stub by default (`internal/ack/log_publisher.go`); `cfg.NATS.Enabled` currently warns but still uses stub in `cmd/scorpion/start.go`.
- Current server start path calls `ListenAndServe` (not TLS) in `internal/http/server.go`; do not assume README TLS behavior without checking runtime code.

## Integration Points
- External deps: Redis (`internal/app/redis.go`), optional NATS interface (`internal/ack/publisher.go`), Prometheus `/metrics`, optional OpenTelemetry tracer init (`internal/telemetry/tracer.go`).
- IP attribution strategy is configurable (`internal/app/ip.go`); changes around auth/rate-limit/connection uniqueness must consider proxy strategy impact.

