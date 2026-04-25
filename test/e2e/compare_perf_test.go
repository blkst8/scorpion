package e2e_test

// compare_perf_test.go — Real-time SSE vs Poll comparison at 5 000 clients.
//
// Motivation
// ──────────
// The existing perf_test.go pushes events BEFORE the SSE connection opens,
// creating an artificial backlog that unfairly penalises SSE.  This test
// does the opposite:
//
//   1. Open all connections first (SSE streams / poll workers).
//   2. Push events AFTER all connections are established.
//   3. Measure how quickly each transport delivers those events.
//
// This represents a real-world fan-out scenario: a backend pushes a notification
// and we want to know how fast every client receives it.
//
// Intervals
// ─────────
//   SSE  : poll_interval = 500ms  (near real-time; configurable via SSEInterval)
//   Poll : client polls every 10 s (typical mobile / web background refresh)
//
// Scenarios (same load for both transports)
// ──────────────────────────────────────────
//   1 000 clients ×  5 events
//   2 000 clients × 10 events
//   5 000 clients × 20 events
//
// Metrics collected per scenario + transport
// ──────────────────────────────────────────
//   • Event delivery latency        p50 / p95 / p99 / max (ms)
//   • Throughput                    events / second
//   • Connection-setup latency      p50 / p95 (ms)    [SSE only]
//   • Missed-client ratio           %
//   • Heap allocation delta + RSS   MB
//   • Peak goroutine count
//   • CPU user + sys time           ms
//   • Wall-clock time               s
//   • Redis INFO: total_commands_processed delta
//   • Redis INFO: connected_clients at peak
//   • Redis INFO: instantaneous_ops_per_sec peak
//
// Output files (written to test/e2e/)
// ────────────────────────────────────
//   compare_results.json    machine-readable raw data
//   compare_report.html     self-contained HTML with inline SVG charts
//
// Run
// ───
//   go test -v -run TestPerf_SSEvsPoll ./test/e2e/... -timeout 60m
//   ulimit -n 65536   # required for 5 000 concurrent connections

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	appmiddleware "github.com/blkst8/scorpion/internal/app"
	"github.com/blkst8/scorpion/internal/config"
	httpserver "github.com/blkst8/scorpion/internal/http"
	"github.com/blkst8/scorpion/internal/http/handlers"
	applog "github.com/blkst8/scorpion/internal/log"
	"github.com/blkst8/scorpion/internal/metrics"
	"github.com/blkst8/scorpion/internal/ratelimit"
	"github.com/blkst8/scorpion/internal/repository"
)

// ── tunables ──────────────────────────────────────────────────────────────────

const (
	cmpSSEInterval  = 500 * time.Millisecond // SSE server poll interval (near real-time)
	cmpPollInterval = 10 * time.Second       // client-side poll interval
	cmpPushConc     = 500                    // max concurrent push goroutines
	cmpSetupConc    = 500                    // max concurrent setup goroutines
)

// ── data types ────────────────────────────────────────────────────────────────

type cmpScenario struct {
	clients int
	events  int
}

type cmpRedisMetrics struct {
	CommandsDelta        int64 // total_commands_processed delta
	PeakConnectedClients int64 // max connected_clients observed
	PeakOpsPerSec        int64 // max instantaneous_ops_per_sec observed
}

type cmpResult struct {
	Label     string
	Transport string // "SSE" | "Poll"
	Clients   int
	Events    int

	TotalPushed    int
	TotalDelivered int
	MissedClients  int
	MissRatioPct   float64

	// Latency
	LatencyP50MS float64
	LatencyP95MS float64
	LatencyP99MS float64
	LatencyMaxMS float64

	// Connection setup (SSE only; poll = 0)
	ConnSetupP50MS float64
	ConnSetupP95MS float64

	// Throughput
	ThroughputEPS float64
	WallTimeS     float64

	// Resources
	HeapDeltaMB    float64
	RSSAllocMB     float64
	GoroutinesPeak int
	CPUUserMS      float64
	CPUSysMS       float64

	// Redis
	Redis cmpRedisMetrics
}

// ── Redis INFO helpers ────────────────────────────────────────────────────────

func redisInfoInt(rdb *redis.Client, section, field string) int64 {
	info, err := rdb.Info(context.Background(), section).Result()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(info, "\r\n") {
		if strings.HasPrefix(line, field+":") {
			var v int64
			_, _ = fmt.Sscanf(strings.TrimPrefix(line, field+":"), "%d", &v)
			return v
		}
	}
	return 0
}

// ── server factories ──────────────────────────────────────────────────────────

// startSSECompareServer starts a server optimised for near-real-time SSE fan-out.
func startSSECompareServer(t *testing.T, rdb *redis.Client) *testServer {
	t.Helper()

	tmpDir := t.TempDir()
	certFile, keyFile, pool := generateSelfSignedCert(t, tmpDir)
	mainPort := freePort(t)
	metricsPort := freePort(t)

	cfg := config.Config{
		Server: config.Server{
			Port:            mainPort,
			TLSCert:         certFile,
			TLSKey:          keyFile,
			ShutdownTimeout: 20 * time.Second,
			ReadTimeout:     60 * time.Second,
			IdleTimeout:     180 * time.Second,
		},
		SSE: config.SSE{
			PollInterval:      cmpSSEInterval,
			BatchSize:         500,
			HeartbeatInterval: 30 * time.Second,
			ConnTTL:           10 * time.Minute,
			MaxQueueDepth:     10000,
			MaxEventBytes:     65536,
		},
		Auth: config.Auth{
			TokenSecret:  "e2e-token-secret",
			TicketSecret: "e2e-ticket-secret",
			TicketTTL:    30 * time.Minute,
		},
		IP:        config.IP{Strategy: "remote_addr"},
		RateLimit: config.RateLimit{TicketRPM: 1000000, TicketBurst: 20000},
		Redis: config.Redis{
			Address:      "127.0.0.1:6379",
			DB:           12,
			PoolSize:     1024,
			MinIdleConns: 128,
			MaxRetries:   3,
		},
		Observability: config.Observability{
			MetricsPort: metricsPort,
			LogLevel:    "error",
			LogFormat:   "text",
		},
	}

	log := applog.NewLogger(cfg.Observability)
	m := metrics.NewMetrics(prometheus.NewRegistry())

	ipStrategy, err := appmiddleware.NewIPStrategy(cfg.IP)
	if err != nil {
		t.Fatalf("ip strategy (SSE): %v", err)
	}

	ticketStore := repository.NewTicketStore(rdb)
	connStore := repository.NewConnectionStore(rdb, "cmp-sse-instance", log)
	eventStore := repository.NewEventStore(rdb, cfg.SSE.MaxQueueDepth)
	limiter := ratelimit.NewLimiter(rdb, cfg.RateLimit)

	h := handlers.HTTPHandlers{
		RDB:        rdb,
		Events:     eventStore,
		Tickets:    ticketStore,
		Conns:      connStore,
		Limiter:    limiter,
		IPStrategy: ipStrategy,
		Cfg:        &cfg,
		Log:        log,
		Metrics:    m,
	}

	srv := httpserver.NewServer(cfg, log, ipStrategy, h)
	srv.Serve()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	baseURL := fmt.Sprintf("https://localhost:%d", mainPort)
	waitReady(t, tlsHTTPClient(pool), baseURL, 15*time.Second)

	return &testServer{srv: srv, baseURL: baseURL, pool: pool}
}

// startPollCompareServer starts an identical server but is only used via the
// poll endpoint — SSE is never opened by the poll clients.
func startPollCompareServer(t *testing.T, rdb *redis.Client) *testServer {
	t.Helper()

	tmpDir := t.TempDir()
	certFile, keyFile, pool := generateSelfSignedCert(t, tmpDir)
	mainPort := freePort(t)
	metricsPort := freePort(t)

	cfg := config.Config{
		Server: config.Server{
			Port:            mainPort,
			TLSCert:         certFile,
			TLSKey:          keyFile,
			ShutdownTimeout: 20 * time.Second,
			ReadTimeout:     60 * time.Second,
			IdleTimeout:     180 * time.Second,
		},
		SSE: config.SSE{
			// SSE settings are irrelevant for poll clients, but must be valid.
			PollInterval:      1 * time.Second,
			BatchSize:         500,
			HeartbeatInterval: 30 * time.Second,
			ConnTTL:           10 * time.Minute,
			MaxQueueDepth:     10000,
			MaxEventBytes:     65536,
		},
		Auth: config.Auth{
			TokenSecret:  "e2e-token-secret",
			TicketSecret: "e2e-ticket-secret",
			TicketTTL:    30 * time.Minute,
		},
		IP:        config.IP{Strategy: "remote_addr"},
		RateLimit: config.RateLimit{TicketRPM: 1000000, TicketBurst: 20000},
		Redis: config.Redis{
			Address:      "127.0.0.1:6379",
			DB:           13, // separate DB from SSE server
			PoolSize:     1024,
			MinIdleConns: 128,
			MaxRetries:   3,
		},
		Observability: config.Observability{
			MetricsPort: metricsPort,
			LogLevel:    "error",
			LogFormat:   "text",
		},
	}

	log := applog.NewLogger(cfg.Observability)
	m := metrics.NewMetrics(prometheus.NewRegistry())

	ipStrategy, err := appmiddleware.NewIPStrategy(cfg.IP)
	if err != nil {
		t.Fatalf("ip strategy (Poll): %v", err)
	}

	ticketStore := repository.NewTicketStore(rdb)
	connStore := repository.NewConnectionStore(rdb, "cmp-poll-instance", log)
	eventStore := repository.NewEventStore(rdb, cfg.SSE.MaxQueueDepth)
	limiter := ratelimit.NewLimiter(rdb, cfg.RateLimit)

	h := handlers.HTTPHandlers{
		RDB:        rdb,
		Events:     eventStore,
		Tickets:    ticketStore,
		Conns:      connStore,
		Limiter:    limiter,
		IPStrategy: ipStrategy,
		Cfg:        &cfg,
		Log:        log,
		Metrics:    m,
	}

	srv := httpserver.NewServer(cfg, log, ipStrategy, h)
	srv.Serve()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	baseURL := fmt.Sprintf("https://localhost:%d", mainPort)
	waitReady(t, tlsHTTPClient(pool), baseURL, 15*time.Second)

	return &testServer{srv: srv, baseURL: baseURL, pool: pool}
}

// ── Redis client helper ───────────────────────────────────────────────────────

func newCmpRedisClient(t *testing.T, db int) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:6379",
		DB:           db,
		PoolSize:     1024,
		MinIdleConns: 128,
	})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("skipping compare perf – Redis unavailable: %v", err)
	}
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flush compare db %d: %v", db, err)
	}
	t.Cleanup(func() {
		_ = rdb.FlushDB(context.Background()).Err()
		_ = rdb.Close()
	})
	return rdb
}

// ── SSE scenario runner ───────────────────────────────────────────────────────

func runCmpSSEScenario(t *testing.T, ts *testServer, rdb *redis.Client, sc cmpScenario) cmpResult {
	t.Helper()
	label := fmt.Sprintf("clients=%d events=%d", sc.clients, sc.events)
	t.Logf("  [SSE] ▶ %s  poll_interval=%s", label, cmpSSEInterval)

	runtime.GC()
	heapBefore, _ := heapMB()
	cpuUserBefore, cpuSysBefore := cpuTimeMS()
	redisOpsBefore := redisInfoInt(rdb, "stats", "total_commands_processed")

	// goroutine peak tracker
	var goroPeak atomic.Int64
	stopGoro := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopGoro:
				return
			default:
				n := int64(runtime.NumGoroutine())
				for {
					cur := goroPeak.Load()
					if n <= cur || goroPeak.CompareAndSwap(cur, n) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()

	// redis ops/sec peak tracker
	stopRedis := make(chan struct{})
	var peakOps atomic.Int64
	go func() {
		for {
			select {
			case <-stopRedis:
				return
			default:
			}
			v := redisInfoInt(rdb, "stats", "instantaneous_ops_per_sec")
			for {
				cur := peakOps.Load()
				if v <= cur || peakOps.CompareAndSwap(cur, v) {
					break
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()

	var (
		mu          sync.Mutex
		connSetupMS []float64
		latenciesMS []float64
	)
	var (
		totalDelivered atomic.Int64
		missedClients  atomic.Int64
	)
	warnCh := make(chan string, sc.clients*2)

	// ── Phase 1: open all SSE streams FIRST ─────────────────────────────────
	type streamState struct {
		clientID string
		bearer   string
		body     io.ReadCloser
		cancel   context.CancelFunc
	}

	type setupRes struct {
		st  streamState
		ms  float64
		err string
	}
	setupCh := make(chan setupRes, sc.clients)

	var setupWg sync.WaitGroup
	setupSem := make(chan struct{}, cmpSetupConc)

	wallStart := time.Now()

	for i := 0; i < sc.clients; i++ {
		setupWg.Add(1)
		clientID := fmt.Sprintf("cmp-sse-%d", i)
		bearer := makeBearerToken(t, "e2e-token-secret", clientID)

		go func(clientID, bearer string) {
			defer setupWg.Done()
			setupSem <- struct{}{}
			defer func() { <-setupSem }()

			c := tlsHTTPClient(ts.pool)

			// ticket
			tickReq, _ := http.NewRequest(http.MethodPost, ts.baseURL+"/v1/auth/ticket", nil)
			tickReq.Header.Set("Authorization", "Bearer "+bearer)
			tickResp, err := c.Do(tickReq)
			if err != nil {
				setupCh <- setupRes{err: fmt.Sprintf("ticket %s: %v", clientID, err)}
				return
			}
			var tb struct {
				Ticket string `json:"ticket"`
			}
			_ = json.NewDecoder(tickResp.Body).Decode(&tb)
			_ = tickResp.Body.Close()
			if tb.Ticket == "" {
				setupCh <- setupRes{err: "empty ticket for " + clientID}
				return
			}

			// open SSE stream
			connStart := time.Now()
			streamCtx, streamCancel := context.WithTimeout(context.Background(), 10*time.Minute)
			url := fmt.Sprintf("%s/v1/stream/events?ticket=%s", ts.baseURL, tb.Ticket)
			req, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, url, nil)
			req.Header.Set("Accept", "text/event-stream")
			req.Header.Set("Authorization", "Bearer "+bearer)
			resp, err := streamingHTTPClient(ts.pool).Do(req)
			if err != nil {
				streamCancel()
				setupCh <- setupRes{err: fmt.Sprintf("stream %s: %v", clientID, err)}
				return
			}
			setupCh <- setupRes{
				st: streamState{
					clientID: clientID,
					bearer:   bearer,
					body:     resp.Body,
					cancel:   streamCancel,
				},
				ms: float64(time.Since(connStart).Microseconds()) / 1e3,
			}
		}(clientID, bearer)
	}
	setupWg.Wait()
	close(setupCh)

	streams := make([]streamState, 0, sc.clients)
	for r := range setupCh {
		if r.err != "" {
			warnCh <- r.err
			missedClients.Add(1)
			continue
		}
		mu.Lock()
		connSetupMS = append(connSetupMS, r.ms)
		mu.Unlock()
		streams = append(streams, r.st)
	}
	t.Logf("    [SSE] streams opened: %d / %d  (setup wall: %.1fs)",
		len(streams), sc.clients, time.Since(wallStart).Seconds())

	// Brief pause so every stream's first poll-tick has fired.
	time.Sleep(cmpSSEInterval + 100*time.Millisecond)

	// ── Phase 2: start collectors ────────────────────────────────────────────
	var collectWg sync.WaitGroup
	collectDone := make(chan struct{})

	// readDeadline: give enough time for all events to arrive via SSE.
	// Worst case: one event per poll tick × number of events.
	readDeadline := time.Duration(sc.events+5)*cmpSSEInterval + 60*time.Second

	for _, st := range streams {
		collectWg.Add(1)
		go func(st streamState) {
			defer collectWg.Done()
			defer func() {
				_ = st.body.Close()
				st.cancel()
			}()

			ctx, cancel := context.WithTimeout(context.Background(), readDeadline)
			defer cancel()

			events := readCmpSSEEvents(ctx, st.body, sc.events)
			if len(events) < sc.events {
				missedClients.Add(1)
			}

			receiveNano := time.Now().UnixNano()
			var localLats []float64
			for _, raw := range events {
				var pe perfEvent
				// Try direct unmarshal first, then envelope.
				if err := json.Unmarshal([]byte(raw), &pe); err != nil || pe.PushNano == 0 {
					var env struct {
						Data json.RawMessage `json:"data"`
					}
					if err2 := json.Unmarshal([]byte(raw), &env); err2 == nil {
						_ = json.Unmarshal(env.Data, &pe)
					}
				}
				if pe.PushNano > 0 {
					lat := float64(receiveNano-pe.PushNano) / 1e6
					if lat >= 0 {
						localLats = append(localLats, lat)
					}
				}
			}
			totalDelivered.Add(int64(len(events)))
			mu.Lock()
			latenciesMS = append(latenciesMS, localLats...)
			mu.Unlock()
		}(st)
	}

	go func() { collectWg.Wait(); close(collectDone) }()

	// ── Phase 3: push events NOW (real-time fan-out) ──────────────────────────
	pushClient := tlsHTTPClient(ts.pool)
	pushSem := make(chan struct{}, cmpPushConc)
	var pushWg sync.WaitGroup

	for _, st := range streams {
		pushWg.Add(1)
		go func(clientID, bearer string) {
			defer pushWg.Done()
			pushSem <- struct{}{}
			defer func() { <-pushSem }()

			for seq := 0; seq < sc.events; seq++ {
				pushNano := time.Now().UnixNano()
				body, _ := sonic.Marshal(map[string]any{
					"type": "cmp.sse.event",
					"data": perfEvent{PushNano: pushNano, Seq: seq},
				})
				req, _ := http.NewRequest(http.MethodPost,
					fmt.Sprintf("%s/v1/events/%s", ts.baseURL, clientID),
					bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+bearer)
				resp, err := pushClient.Do(req)
				if err != nil {
					warnCh <- fmt.Sprintf("push %s seq%d: %v", clientID, seq, err)
					return
				}
				_ = resp.Body.Close()
			}
		}(st.clientID, st.bearer)
	}
	pushWg.Wait()

	<-collectDone
	close(stopGoro)
	close(stopRedis)
	close(warnCh)

	// sample warnings
	wc := 0
	for w := range warnCh {
		if wc < 10 {
			t.Logf("    warn: %s", w)
		}
		wc++
	}

	wallSec := time.Since(wallStart).Seconds()
	heapAfter, _ := heapMB()
	cpuUserAfter, cpuSysAfter := cpuTimeMS()
	redisOpsAfter := redisInfoInt(rdb, "stats", "total_commands_processed")
	peakConns := redisInfoInt(rdb, "clients", "connected_clients")

	sortedConn := sortedCopy(connSetupMS)
	sortedLat := sortedCopy(latenciesMS)
	delivered := int(totalDelivered.Load())
	missed := int(missedClients.Load())
	expected := sc.clients * sc.events

	missRatio := 0.0
	if sc.clients > 0 {
		missRatio = float64(missed) / float64(sc.clients) * 100
	}
	throughput := 0.0
	if wallSec > 0 {
		throughput = float64(delivered) / wallSec
	}

	res := cmpResult{
		Label:          label,
		Transport:      "SSE",
		Clients:        sc.clients,
		Events:         sc.events,
		TotalPushed:    expected,
		TotalDelivered: delivered,
		MissedClients:  missed,
		MissRatioPct:   missRatio,
		LatencyP50MS:   pctile(sortedLat, 50),
		LatencyP95MS:   pctile(sortedLat, 95),
		LatencyP99MS:   pctile(sortedLat, 99),
		LatencyMaxMS:   pctile(sortedLat, 100),
		ConnSetupP50MS: pctile(sortedConn, 50),
		ConnSetupP95MS: pctile(sortedConn, 95),
		ThroughputEPS:  throughput,
		WallTimeS:      wallSec,
		HeapDeltaMB:    heapAfter - heapBefore,
		RSSAllocMB:     rssMemMB(),
		GoroutinesPeak: int(goroPeak.Load()),
		CPUUserMS:      cpuUserAfter - cpuUserBefore,
		CPUSysMS:       cpuSysAfter - cpuSysBefore,
		Redis: cmpRedisMetrics{
			CommandsDelta:        redisOpsAfter - redisOpsBefore,
			PeakConnectedClients: peakConns,
			PeakOpsPerSec:        peakOps.Load(),
		},
	}

	t.Logf("    [SSE] delivered=%d/%d missed=%d(%.1f%%) "+
		"lat_p50=%.0fms lat_p95=%.0fms lat_p99=%.0fms lat_max=%.0fms "+
		"throughput=%.0feps rss=%.1fMB goroutines=%d wall=%.1fs",
		res.TotalDelivered, res.TotalPushed, res.MissedClients, res.MissRatioPct,
		res.LatencyP50MS, res.LatencyP95MS, res.LatencyP99MS, res.LatencyMaxMS,
		res.ThroughputEPS, res.RSSAllocMB, res.GoroutinesPeak, res.WallTimeS)

	return res
}

// readCmpSSEEvents reads exactly wantCount data events from an SSE stream,
// skipping heartbeat lines. It returns early if ctx is cancelled.
func readCmpSSEEvents(ctx context.Context, body io.Reader, wantCount int) []string {
	ch := make(chan []string, 1)
	go func() {
		var collected []string
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				ch <- collected
				return
			default:
			}
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := line[len("data: "):]
			if data == "" || data == ": ping" || data == "ping" {
				continue
			}
			collected = append(collected, data)
			if len(collected) >= wantCount {
				break
			}
		}
		ch <- collected
	}()
	select {
	case r := <-ch:
		return r
	case <-ctx.Done():
		select {
		case r := <-ch:
			return r
		default:
			return nil
		}
	}
}

// ── Poll scenario runner ──────────────────────────────────────────────────────

func runCmpPollScenario(t *testing.T, ts *testServer, rdb *redis.Client, sc cmpScenario) cmpResult {
	t.Helper()
	label := fmt.Sprintf("clients=%d events=%d", sc.clients, sc.events)
	t.Logf("  [Poll] ▶ %s  poll_interval=%s", label, cmpPollInterval)

	runtime.GC()
	heapBefore, _ := heapMB()
	cpuUserBefore, cpuSysBefore := cpuTimeMS()
	redisOpsBefore := redisInfoInt(rdb, "stats", "total_commands_processed")

	var goroPeak atomic.Int64
	stopGoro := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopGoro:
				return
			default:
				n := int64(runtime.NumGoroutine())
				for {
					cur := goroPeak.Load()
					if n <= cur || goroPeak.CompareAndSwap(cur, n) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()

	stopRedis := make(chan struct{})
	var peakOps atomic.Int64
	go func() {
		for {
			select {
			case <-stopRedis:
				return
			default:
			}
			v := redisInfoInt(rdb, "stats", "instantaneous_ops_per_sec")
			for {
				cur := peakOps.Load()
				if v <= cur || peakOps.CompareAndSwap(cur, v) {
					break
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()

	var (
		mu          sync.Mutex
		latenciesMS []float64
	)
	var (
		totalDelivered atomic.Int64
		missedClients  atomic.Int64
	)
	warnCh := make(chan string, sc.clients*2)

	wallStart := time.Now()

	// Push events first (before poll clients start), then poll clients detect them.
	// NOTE: this simulates the poll use-case accurately: a backend fires an event
	// and a mobile client picks it up on the next background refresh (10 s).
	pushClient := tlsHTTPClient(ts.pool)
	pushSem := make(chan struct{}, cmpPushConc)
	var pushWg sync.WaitGroup

	type clientInfo struct {
		clientID  string
		bearer    string
		pushNanos []int64
	}
	clients := make([]clientInfo, sc.clients)
	for i := range clients {
		clients[i].clientID = fmt.Sprintf("cmp-poll-%d", i)
		clients[i].bearer = makeBearerToken(t, "e2e-token-secret", clients[i].clientID)
		clients[i].pushNanos = make([]int64, sc.events)
	}

	for i := range clients {
		pushWg.Add(1)
		go func(ci clientInfo) {
			defer pushWg.Done()
			pushSem <- struct{}{}
			defer func() { <-pushSem }()

			for seq := 0; seq < sc.events; seq++ {
				pushNano := time.Now().UnixNano()
				ci.pushNanos[seq] = pushNano
				body, _ := sonic.Marshal(map[string]any{
					"type": "cmp.poll.event",
					"data": perfEvent{PushNano: pushNano, Seq: seq},
				})
				req, _ := http.NewRequest(http.MethodPost,
					fmt.Sprintf("%s/v1/events/%s", ts.baseURL, ci.clientID),
					bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+ci.bearer)
				resp, err := pushClient.Do(req)
				if err != nil {
					warnCh <- fmt.Sprintf("push %s seq%d: %v", ci.clientID, seq, err)
					return
				}
				_ = resp.Body.Close()
			}
		}(clients[i])
	}
	pushWg.Wait()
	t.Logf("    [Poll] all events pushed, starting poll clients…")

	// Now launch poll workers.
	var pollWg sync.WaitGroup
	pollSem := make(chan struct{}, cmpSetupConc)
	httpClient := tlsHTTPClient(ts.pool)

	for _, ci := range clients {
		pollWg.Add(1)
		go func(ci clientInfo) {
			defer pollWg.Done()
			pollSem <- struct{}{}
			defer func() { <-pollSem }()

			collected := 0
			var localLats []float64
			// Poll until all events collected or deadline exceeded.
			// Deadline = 3 × poll interval + buffer (events may need 2 polls to arrive).
			deadline := time.Now().Add(3*cmpPollInterval + 30*time.Second)

			for collected < sc.events && time.Now().Before(deadline) {
				time.Sleep(cmpPollInterval)
				req, _ := http.NewRequest(http.MethodGet,
					fmt.Sprintf("%s/v1/events/%s", ts.baseURL, ci.clientID), nil)
				req.Header.Set("Authorization", "Bearer "+ci.bearer)
				resp, err := httpClient.Do(req)
				if err != nil {
					warnCh <- fmt.Sprintf("poll %s: %v", ci.clientID, err)
					continue
				}
				var pr pollResponse
				_ = json.NewDecoder(resp.Body).Decode(&pr)
				_ = resp.Body.Close()

				receiveNano := time.Now().UnixNano()
				for _, e := range pr.Events {
					var pe perfEvent
					if err := json.Unmarshal(e.Data, &pe); err == nil && pe.PushNano > 0 {
						lat := float64(receiveNano-pe.PushNano) / 1e6
						if lat >= 0 {
							localLats = append(localLats, lat)
						}
					}
				}
				collected += pr.Count
			}

			if collected < sc.events {
				missedClients.Add(1)
			}
			totalDelivered.Add(int64(collected))
			mu.Lock()
			latenciesMS = append(latenciesMS, localLats...)
			mu.Unlock()
		}(ci)
	}
	pollWg.Wait()
	close(stopGoro)
	close(stopRedis)
	close(warnCh)

	wc := 0
	for w := range warnCh {
		if wc < 10 {
			t.Logf("    warn: %s", w)
		}
		wc++
	}

	wallSec := time.Since(wallStart).Seconds()
	heapAfter, _ := heapMB()
	cpuUserAfter, cpuSysAfter := cpuTimeMS()
	redisOpsAfter := redisInfoInt(rdb, "stats", "total_commands_processed")
	peakConns := redisInfoInt(rdb, "clients", "connected_clients")

	sortedLat := sortedCopy(latenciesMS)
	delivered := int(totalDelivered.Load())
	missed := int(missedClients.Load())
	expected := sc.clients * sc.events

	missRatio := 0.0
	if sc.clients > 0 {
		missRatio = float64(missed) / float64(sc.clients) * 100
	}
	throughput := 0.0
	if wallSec > 0 {
		throughput = float64(delivered) / wallSec
	}

	res := cmpResult{
		Label:          label,
		Transport:      "Poll",
		Clients:        sc.clients,
		Events:         sc.events,
		TotalPushed:    expected,
		TotalDelivered: delivered,
		MissedClients:  missed,
		MissRatioPct:   missRatio,
		LatencyP50MS:   pctile(sortedLat, 50),
		LatencyP95MS:   pctile(sortedLat, 95),
		LatencyP99MS:   pctile(sortedLat, 99),
		LatencyMaxMS:   pctile(sortedLat, 100),
		ThroughputEPS:  throughput,
		WallTimeS:      wallSec,
		HeapDeltaMB:    heapAfter - heapBefore,
		RSSAllocMB:     rssMemMB(),
		GoroutinesPeak: int(goroPeak.Load()),
		CPUUserMS:      cpuUserAfter - cpuUserBefore,
		CPUSysMS:       cpuSysAfter - cpuSysBefore,
		Redis: cmpRedisMetrics{
			CommandsDelta:        redisOpsAfter - redisOpsBefore,
			PeakConnectedClients: peakConns,
			PeakOpsPerSec:        peakOps.Load(),
		},
	}

	t.Logf("    [Poll] delivered=%d/%d missed=%d(%.1f%%) "+
		"lat_p50=%.0fms lat_p95=%.0fms lat_p99=%.0fms lat_max=%.0fms "+
		"throughput=%.0feps rss=%.1fMB goroutines=%d wall=%.1fs",
		res.TotalDelivered, res.TotalPushed, res.MissedClients, res.MissRatioPct,
		res.LatencyP50MS, res.LatencyP95MS, res.LatencyP99MS, res.LatencyMaxMS,
		res.ThroughputEPS, res.RSSAllocMB, res.GoroutinesPeak, res.WallTimeS)

	return res
}

// ── test entry point ───────────────────────────────────────────────────────────

// TestPerf_SSEvsPoll runs the real-time SSE vs Poll comparison at scale.
//
// Environment requirements:
//   - Redis on 127.0.0.1:6379
//   - ulimit -n 65536
func TestPerf_SSEvsPoll(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SSE-vs-Poll comparison test (-short)")
	}

	scenarios := []cmpScenario{
		{clients: 1000, events: 5},
		{clients: 2000, events: 10},
		{clients: 5000, events: 20},
	}

	// Two separate Redis DBs → two isolated servers.
	sseRDB := newCmpRedisClient(t, 12)
	pollRDB := newCmpRedisClient(t, 13)

	sseServer := startSSECompareServer(t, sseRDB)
	pollServer := startPollCompareServer(t, pollRDB)

	t.Logf("=== SSE vs Poll Real-Time Comparison ===")
	t.Logf("  SSE  poll_interval : %s", cmpSSEInterval)
	t.Logf("  Poll poll_interval : %s", cmpPollInterval)
	t.Logf("  scenarios          : %d", len(scenarios))

	var sseResults, pollResults []cmpResult

	for _, sc := range scenarios {
		t.Logf("── Scenario: %d clients × %d events ──", sc.clients, sc.events)

		sseRes := runCmpSSEScenario(t, sseServer, sseRDB, sc)
		sseResults = append(sseResults, sseRes)

		// Let SSE goroutines drain before starting poll.
		runtime.GC()
		time.Sleep(3 * time.Second)
		// Flush the SSE Redis DB so state doesn't carry over.
		_ = sseRDB.FlushDB(context.Background()).Err()

		pollRes := runCmpPollScenario(t, pollServer, pollRDB, sc)
		pollResults = append(pollResults, pollRes)

		runtime.GC()
		time.Sleep(3 * time.Second)
		_ = pollRDB.FlushDB(context.Background()).Err()
	}

	outDir := "."
	writeCmpJSON(t, outDir, sseResults, pollResults)
	writeCmpHTML(t, outDir, sseResults, pollResults)
}

// ── output writers ─────────────────────────────────────────────────────────────

type cmpJSONReport struct {
	GeneratedAt  string      `json:"generated_at"`
	SSEInterval  string      `json:"sse_interval"`
	PollInterval string      `json:"poll_interval"`
	SSE          []cmpResult `json:"sse"`
	Poll         []cmpResult `json:"poll"`
}

func writeCmpJSON(t *testing.T, dir string, sse, poll []cmpResult) {
	t.Helper()
	report := cmpJSONReport{
		GeneratedAt:  time.Now().Format(time.RFC3339),
		SSEInterval:  cmpSSEInterval.String(),
		PollInterval: cmpPollInterval.String(),
		SSE:          sse,
		Poll:         poll,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Logf("warn: marshal compare json: %v", err)
		return
	}
	path := filepath.Join(dir, "compare_results.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Logf("warn: write compare json: %v", err)
		return
	}
	t.Logf("📄 Compare JSON → %s", path)
}

func writeCmpHTML(t *testing.T, dir string, sse, poll []cmpResult) {
	t.Helper()

	if len(sse) == 0 && len(poll) == 0 {
		return
	}

	labels := make([]string, len(sse))
	for i, r := range sse {
		labels[i] = r.Label
	}

	sseCol := func(fn func(cmpResult) float64) []float64 {
		out := make([]float64, len(sse))
		for i, r := range sse {
			out[i] = fn(r)
		}
		return out
	}
	pollCol := func(fn func(cmpResult) float64) []float64 {
		out := make([]float64, len(poll))
		for i, r := range poll {
			out[i] = fn(r)
		}
		return out
	}

	bar := func(title string, ss []SVGSeries, unit string) template.HTML {
		return template.HTML(SVGBarChart(title, labels, ss, unit))
	}

	latChart := bar("Event Delivery Latency p50 (ms) — SSE vs Poll", []SVGSeries{
		{Name: "SSE p50", Data: sseCol(func(r cmpResult) float64 { return r.LatencyP50MS })},
		{Name: "Poll p50", Data: pollCol(func(r cmpResult) float64 { return r.LatencyP50MS })},
	}, "ms")

	lat95Chart := bar("Event Delivery Latency p95 (ms) — SSE vs Poll", []SVGSeries{
		{Name: "SSE p95", Data: sseCol(func(r cmpResult) float64 { return r.LatencyP95MS })},
		{Name: "Poll p95", Data: pollCol(func(r cmpResult) float64 { return r.LatencyP95MS })},
	}, "ms")

	lat99Chart := bar("Event Delivery Latency p99 (ms) — SSE vs Poll", []SVGSeries{
		{Name: "SSE p99", Data: sseCol(func(r cmpResult) float64 { return r.LatencyP99MS })},
		{Name: "Poll p99", Data: pollCol(func(r cmpResult) float64 { return r.LatencyP99MS })},
	}, "ms")

	tputChart := bar("Throughput (events/sec) — SSE vs Poll", []SVGSeries{
		{Name: "SSE EPS", Data: sseCol(func(r cmpResult) float64 { return r.ThroughputEPS })},
		{Name: "Poll EPS", Data: pollCol(func(r cmpResult) float64 { return r.ThroughputEPS })},
	}, "eps")

	rssChart := bar("RSS Memory (MB) — SSE vs Poll", []SVGSeries{
		{Name: "SSE RSS", Data: sseCol(func(r cmpResult) float64 { return r.RSSAllocMB })},
		{Name: "Poll RSS", Data: pollCol(func(r cmpResult) float64 { return r.RSSAllocMB })},
	}, "MB")

	heapChart := bar("Heap Delta (MB) — SSE vs Poll", []SVGSeries{
		{Name: "SSE Heap Δ", Data: sseCol(func(r cmpResult) float64 { return r.HeapDeltaMB })},
		{Name: "Poll Heap Δ", Data: pollCol(func(r cmpResult) float64 { return r.HeapDeltaMB })},
	}, "MB")

	goroChart := bar("Peak Goroutines — SSE vs Poll", []SVGSeries{
		{Name: "SSE", Data: sseCol(func(r cmpResult) float64 { return float64(r.GoroutinesPeak) })},
		{Name: "Poll", Data: pollCol(func(r cmpResult) float64 { return float64(r.GoroutinesPeak) })},
	}, "")

	cpuChart := bar("CPU User Time (ms) — SSE vs Poll", []SVGSeries{
		{Name: "SSE user", Data: sseCol(func(r cmpResult) float64 { return r.CPUUserMS })},
		{Name: "Poll user", Data: pollCol(func(r cmpResult) float64 { return r.CPUUserMS })},
	}, "ms")

	redisOpsChart := bar("Redis Commands Delta — SSE vs Poll", []SVGSeries{
		{Name: "SSE cmds", Data: sseCol(func(r cmpResult) float64 { return float64(r.Redis.CommandsDelta) })},
		{Name: "Poll cmds", Data: pollCol(func(r cmpResult) float64 { return float64(r.Redis.CommandsDelta) })},
	}, "")

	missChart := bar("Missed Client Ratio (%) — SSE vs Poll", []SVGSeries{
		{Name: "SSE miss%", Data: sseCol(func(r cmpResult) float64 { return r.MissRatioPct })},
		{Name: "Poll miss%", Data: pollCol(func(r cmpResult) float64 { return r.MissRatioPct })},
	}, "%")

	// ── summary table ──────────────────────────────────────────────────────────
	var rows strings.Builder
	for i := range sse {
		s := sse[i]
		p := cmpResult{}
		if i < len(poll) {
			p = poll[i]
		}
		rowColor := func(sseVal, pollVal float64, lowerIsBetter bool) (string, string) {
			if lowerIsBetter {
				if sseVal < pollVal {
					return "win", "loss"
				}
				return "loss", "win"
			}
			if sseVal > pollVal {
				return "win", "loss"
			}
			return "loss", "win"
		}
		latClass, pollLatClass := rowColor(s.LatencyP50MS, p.LatencyP50MS, true)
		tputClass, pollTputClass := rowColor(s.ThroughputEPS, p.ThroughputEPS, false)
		rssClass, pollRSSClass := rowColor(s.RSSAllocMB, p.RSSAllocMB, true)
		cmdClass, pollCmdClass := rowColor(float64(s.Redis.CommandsDelta), float64(p.Redis.CommandsDelta), true)

		_, _ = fmt.Fprintf(&rows, `<tr>
<td>%s</td>
<td class="%s">%.0f</td><td class="%s">%.0f</td>
<td>%.0f / %.0f / %.0f</td><td>%.0f / %.0f / %.0f</td>
<td class="%s">%.0f</td><td class="%s">%.0f</td>
<td class="%s">%.1f</td><td class="%s">%.1f</td>
<td class="%s">%d</td><td class="%s">%d</td>
<td>%.1f%%</td><td>%.1f%%</td>
</tr>`,
			s.Label,
			latClass, s.LatencyP50MS, pollLatClass, p.LatencyP50MS,
			s.LatencyP50MS, s.LatencyP95MS, s.LatencyP99MS,
			p.LatencyP50MS, p.LatencyP95MS, p.LatencyP99MS,
			tputClass, s.ThroughputEPS, pollTputClass, p.ThroughputEPS,
			rssClass, s.RSSAllocMB, pollRSSClass, p.RSSAllocMB,
			cmdClass, s.Redis.CommandsDelta, pollCmdClass, p.Redis.CommandsDelta,
			s.MissRatioPct, p.MissRatioPct,
		)
	}

	html := `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Scorpion SSE vs Poll — Real-Time Comparison</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,sans-serif;max-width:1400px;margin:0 auto;padding:2rem;
     background:#0f172a;color:#e2e8f0}
h1{color:#38bdf8;font-size:1.6rem;margin-bottom:.25rem}
.meta{color:#94a3b8;font-size:.82rem;margin-bottom:2rem;line-height:1.8}
h2{color:#7dd3fc;font-size:1.1rem;margin:2.5rem 0 1rem;
   border-bottom:1px solid #334155;padding-bottom:.4rem}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(560px,1fr));gap:1.5rem;margin:1rem 0}
table{width:100%;border-collapse:collapse;font-size:.78rem;margin:1rem 0}
th{background:#1e3a5f;padding:.5rem .6rem;text-align:right;color:#bfdbfe;
   font-weight:600;text-transform:uppercase;letter-spacing:.04em;white-space:nowrap}
th:first-child{text-align:left}
td{padding:.4rem .5rem;border-bottom:1px solid #1e293b;text-align:right;white-space:nowrap}
td:first-child{text-align:left;font-weight:500}
tr:hover td{background:#1e293b}
.win{color:#4ade80;font-weight:700}
.loss{color:#f87171}
.badge{display:inline-block;padding:.1rem .4rem;border-radius:4px;font-size:.72rem;font-weight:600}
.ok{background:#14532d;color:#86efac}.warn{background:#78350f;color:#fcd34d}
.info-box{background:#1e293b;border:1px solid #334155;border-radius:8px;
          padding:1rem 1.5rem;margin:.5rem 0 1.5rem;font-size:.85rem;line-height:2}
.tag{display:inline-block;padding:.15rem .6rem;border-radius:4px;
     font-size:.75rem;font-weight:600;margin:0 .2rem}
.tag-sse{background:#1e40af;color:#93c5fd}
.tag-poll{background:#7c3aed;color:#ddd6fe}
svg{display:block;width:100%;height:auto}
</style>
</head>
<body>
<h1>&#9889; Scorpion &#8212; SSE vs Poll Real-Time Comparison</h1>
<div class="meta">
  Generated: ` + time.Now().Format("2006-01-02 15:04:05 MST") + `<br>
  <span class="tag tag-sse">SSE</span> server poll_interval = ` + cmpSSEInterval.String() + ` &nbsp;|&nbsp;
  <span class="tag tag-poll">Poll</span> client interval = ` + cmpPollInterval.String() + `<br>
  Events are pushed <strong>after</strong> connections are established — true real-time fan-out scenario.
</div>

<div class="info-box">
  <strong>Methodology:</strong>
  SSE clients open their streams first. Poll clients wait for events to be pushed.
  Both transports receive the same events for the same clients.
  <span class="tag tag-sse">SSE</span> latency = time from push → first poll tick that picks up the event ≈ SSE interval.
  <span class="tag tag-poll">Poll</span> latency = time from push → next client HTTP GET at ` + cmpPollInterval.String() + ` interval.
  <strong>Green</strong> = winner per metric. Redis commands delta measures total Redis load generated.
</div>

<h2>Summary Comparison Table</h2>
<table>
<thead><tr>
  <th>Scenario</th>
  <th>SSE lat p50</th><th>Poll lat p50</th>
  <th>SSE p50/p95/p99 (ms)</th><th>Poll p50/p95/p99 (ms)</th>
  <th>SSE EPS</th><th>Poll EPS</th>
  <th>SSE RSS</th><th>Poll RSS</th>
  <th>SSE Redis cmds</th><th>Poll Redis cmds</th>
  <th>SSE miss%</th><th>Poll miss%</th>
</tr></thead>
<tbody>` + rows.String() + `</tbody>
</table>

<h2>Latency Charts</h2>
<div class="grid">` + string(latChart) + string(lat95Chart) + string(lat99Chart) + `</div>

<h2>Throughput &amp; Correctness</h2>
<div class="grid">` + string(tputChart) + string(missChart) + `</div>

<h2>Resource Usage</h2>
<div class="grid">` + string(rssChart) + string(heapChart) + string(goroChart) + string(cpuChart) + `</div>

<h2>Redis Load</h2>
<div class="grid">` + string(redisOpsChart) + `</div>
</body>
</html>`

	path := filepath.Join(dir, "compare_report.html")
	if err := os.WriteFile(path, []byte(html), 0o644); err != nil {
		t.Logf("warn: write compare html: %v", err)
		return
	}
	t.Logf("📊 Compare HTML → %s", path)
}
