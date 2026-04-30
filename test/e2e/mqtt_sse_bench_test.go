package e2e_test

// mqtt_sse_bench_test.go — SSE (Scorpion) vs MQTT (EMQX) head-to-head benchmark.
//
// Motivation
// ──────────
// This test measures the overhead of each transport end-to-end under identical
// fan-out scenarios at up to 10 000 concurrent clients.  Both sides push events
// *after* all subscribers are connected so we measure real-time push latency,
// not backlog drain.
//
// Architecture
// ────────────
//   SSE  : in-process Scorpion server (same as compare_perf_test.go).
//          Each client acquires a ticket, opens a persistent SSE stream, then
//          waits for events published via POST /v1/events/:clientID.
//
//   MQTT : external EMQX broker on 127.0.0.1:1883 (no TLS, anonymous auth).
//          Each client connects with a unique client-ID, subscribes to
//          "scorpion/bench/<clientID>" at QoS-1, then waits for a publisher
//          goroutine to push events to those topics.
//
// Scenarios
// ─────────
//   1 000 clients ×  5 events
//   2 000 clients × 10 events
//   5 000 clients × 20 events
//  10 000 clients × 20 events   (needs ulimit -n 65536)
//
// Metrics collected per scenario + transport
// ──────────────────────────────────────────
//   • Event delivery latency           p50 / p95 / p99 / max (ms)
//   • Throughput                       events / second
//   • Connection-setup latency         p50 / p95 (ms)
//   • Missed-client ratio              %
//   • Heap allocation delta + RSS      MB
//   • Peak goroutine count
//   • CPU user + sys time              ms
//   • Wall-clock time                  s
//
// Output files (written to test/e2e/)
// ────────────────────────────────────
//   mqtt_sse_results.json    machine-readable raw data
//   mqtt_sse_report.html     self-contained HTML with inline SVG charts
//
// Prerequisites
// ─────────────
//   docker compose -f docker-compose.test.yml up -d   # starts Redis + EMQX
//   ulimit -n 65536
//
// Run
// ───
//   go test -v -run TestPerf_SSEvsMQTT ./test/e2e/... -timeout 90m

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net"
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
	mqtt "github.com/eclipse/paho.mqtt.golang"
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
	mqttBrokerAddr  = "tcp://127.0.0.1:1883"
	mqttSSEInterval = 200 * time.Millisecond // SSE server poll cadence
	mqttSetupConc   = 200                    // max concurrent MQTT connect goroutines
	mqttPushConc    = 500                    // max concurrent publish goroutines
	mqttConnTimeout = 30 * time.Second       // per-client MQTT connect timeout
	mqttConnRetries = 3                      // retries on connect failure before giving up
	mqttKeepAlive   = 60                     // MQTT keepalive seconds
	mqttQoS         = byte(1)                // QoS for subscribe + publish
	mqttTopicPrefix = "scorpion/bench/"
)

// ── scenario definition ───────────────────────────────────────────────────────

type mqttScenario struct {
	clients int
	events  int
}

// ── result types ──────────────────────────────────────────────────────────────

// mqttResult holds all metrics for one (transport, scenario) pair.
type mqttResult struct {
	Label     string
	Transport string // "SSE" | "MQTT"
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

	// Connection setup
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
}

// ── MQTT broker availability check ───────────────────────────────────────────

// mqttBrokerAvailable returns true if the MQTT broker TCP port is reachable.
func mqttBrokerAvailable() bool {
	const addr = "127.0.0.1:1883"
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ── SSE server for MQTT comparison ───────────────────────────────────────────

func startMQTTCmpSSEServer(t *testing.T, rdb *redis.Client) *testServer {
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
			ShutdownTimeout: 30 * time.Second,
			ReadTimeout:     120 * time.Second,
			IdleTimeout:     300 * time.Second,
		},
		SSE: config.SSE{
			PollInterval:      mqttSSEInterval,
			BatchSize:         1000,
			HeartbeatInterval: 60 * time.Second,
			ConnTTL:           20 * time.Minute,
			MaxQueueDepth:     20000,
			MaxEventBytes:     65536,
		},
		Auth: config.Auth{
			TokenSecret:  "e2e-token-secret",
			TicketSecret: "e2e-ticket-secret",
			TicketTTL:    60 * time.Minute,
		},
		IP:        config.IP{Strategy: "remote_addr"},
		RateLimit: config.RateLimit{TicketRPM: 10000000, TicketBurst: 50000},
		Redis: config.Redis{
			Address:      "127.0.0.1:6379",
			DB:           14,
			PoolSize:     2048,
			MinIdleConns: 256,
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
		t.Fatalf("ip strategy (SSE/MQTT cmp): %v", err)
	}

	ticketStore := repository.NewTicketStore(rdb)
	connStore := repository.NewConnectionStore(rdb, "mqtt-cmp-sse-instance", log)
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
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	baseURL := fmt.Sprintf("https://localhost:%d", mainPort)
	waitReady(t, tlsHTTPClient(pool), baseURL, 20*time.Second)

	return &testServer{srv: srv, baseURL: baseURL, pool: pool}
}

// ── SSE scenario runner ───────────────────────────────────────────────────────

func runMQTTCmpSSEScenario(t *testing.T, ts *testServer, _ *redis.Client, sc mqttScenario) mqttResult {
	t.Helper()
	label := fmt.Sprintf("clients=%d events=%d", sc.clients, sc.events)
	t.Logf("  [SSE] ▶ %s  poll_interval=%s", label, mqttSSEInterval)

	runtime.GC()
	heapBefore, _ := heapMB()
	cpuUserBefore, cpuSysBefore := cpuTimeMS()

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

	type streamState struct {
		clientID string
		bearer   string
		body     io.ReadCloser
		cancel   context.CancelFunc
	}

	var (
		mu         sync.Mutex
		connSetups []float64
		latencies  []float64
	)
	var (
		totalDelivered atomic.Int64
		missedClients  atomic.Int64
	)
	warnCh := make(chan string, sc.clients*2)

	type setupRes struct {
		st  streamState
		ms  float64
		err string
	}
	setupCh := make(chan setupRes, sc.clients)

	var setupWg sync.WaitGroup
	setupSem := make(chan struct{}, mqttSetupConc)

	wallStart := time.Now()

	// ── Phase 1: open all SSE streams ────────────────────────────────────────
	for i := 0; i < sc.clients; i++ {
		setupWg.Add(1)
		clientID := fmt.Sprintf("mqttcmp-sse-%d", i)
		bearer := makeBearerToken(t, "e2e-token-secret", clientID)

		go func(clientID, bearer string) {
			defer setupWg.Done()
			setupSem <- struct{}{}
			defer func() { <-setupSem }()

			c := tlsHTTPClient(ts.pool)

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

			connStart := time.Now()
			streamCtx, streamCancel := context.WithTimeout(context.Background(), 30*time.Minute)
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
		connSetups = append(connSetups, r.ms)
		mu.Unlock()
		streams = append(streams, r.st)
	}
	t.Logf("    [SSE] streams opened: %d/%d (setup: %.1fs)",
		len(streams), sc.clients, time.Since(wallStart).Seconds())

	// Brief warm-up pause.
	time.Sleep(mqttSSEInterval + 100*time.Millisecond)

	// ── Phase 2: start collectors ─────────────────────────────────────────────
	var collectWg sync.WaitGroup
	collectDone := make(chan struct{})

	readDeadline := time.Duration(sc.events+5)*mqttSSEInterval + 90*time.Second

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

			var collected []string
			scanner := bufio.NewScanner(st.body)
			scanner.Buffer(make([]byte, 64*1024), 64*1024)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for scanner.Scan() {
					select {
					case <-ctx.Done():
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
					if len(collected) >= sc.events {
						return
					}
				}
			}()
			select {
			case <-done:
			case <-ctx.Done():
			}

			if len(collected) < sc.events {
				missedClients.Add(1)
			}

			receiveNano := time.Now().UnixNano()
			var localLats []float64
			for _, raw := range collected {
				var pe perfEvent
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
			totalDelivered.Add(int64(len(collected)))
			mu.Lock()
			latencies = append(latencies, localLats...)
			mu.Unlock()
		}(st)
	}
	go func() { collectWg.Wait(); close(collectDone) }()

	// ── Phase 3: push events ──────────────────────────────────────────────────
	pushClient := tlsHTTPClient(ts.pool)
	pushSem := make(chan struct{}, mqttPushConc)
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
					"type": "mqtt.cmp.sse.event",
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

	sortedConn := sortedCopy(connSetups)
	sortedLat := sortedCopy(latencies)
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

	res := mqttResult{
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
	}

	t.Logf("    [SSE] delivered=%d/%d missed=%d(%.1f%%) "+
		"lat_p50=%.1fms lat_p95=%.1fms lat_p99=%.1fms lat_max=%.1fms "+
		"throughput=%.0feps rss=%.1fMB goroutines=%d wall=%.1fs",
		res.TotalDelivered, res.TotalPushed, res.MissedClients, res.MissRatioPct,
		res.LatencyP50MS, res.LatencyP95MS, res.LatencyP99MS, res.LatencyMaxMS,
		res.ThroughputEPS, res.RSSAllocMB, res.GoroutinesPeak, res.WallTimeS)

	return res
}

// ── MQTT scenario runner ──────────────────────────────────────────────────────

// mqttPayload is embedded in every MQTT message.
type mqttPayload struct {
	PushNano int64 `json:"push_ns"`
	Seq      int   `json:"seq"`
}

// newMQTTClient creates and connects a Paho MQTT client with retry.
// It returns (client, connectLatencyMS, error).
func newMQTTClient(brokerURL, clientID string) (mqtt.Client, float64, error) {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(brokerURL)
	opts.SetClientID(clientID)
	opts.SetKeepAlive(mqttKeepAlive * time.Second)
	opts.SetConnectTimeout(mqttConnTimeout)
	opts.SetAutoReconnect(false)
	opts.SetCleanSession(true)
	// Suppress paho's default logger noise.
	opts.SetConnectionLostHandler(func(_ mqtt.Client, _ error) {})

	start := time.Now()
	var lastErr error
	for attempt := 0; attempt <= mqttConnRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*500) * time.Millisecond)
		}
		c := mqtt.NewClient(opts)
		tok := c.Connect()
		if !tok.WaitTimeout(mqttConnTimeout) {
			lastErr = fmt.Errorf("connect timeout for %s (attempt %d)", clientID, attempt+1)
			c.Disconnect(100)
			continue
		}
		if err := tok.Error(); err != nil {
			lastErr = fmt.Errorf("connect %s (attempt %d): %w", clientID, attempt+1, err)
			c.Disconnect(100)
			continue
		}
		return c, float64(time.Since(start).Microseconds()) / 1e3, nil
	}
	return nil, 0, lastErr
}

func runMQTTScenario(t *testing.T, sc mqttScenario) mqttResult {
	t.Helper()
	label := fmt.Sprintf("clients=%d events=%d", sc.clients, sc.events)
	t.Logf("  [MQTT] ▶ %s  broker=%s qos=%d", label, mqttBrokerAddr, mqttQoS)

	runtime.GC()
	heapBefore, _ := heapMB()
	cpuUserBefore, cpuSysBefore := cpuTimeMS()

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

	type subState struct {
		clientID string
		client   mqtt.Client
		topic    string
		msgCh    chan mqtt.Message
	}

	var (
		mu         sync.Mutex
		connSetups []float64
		latencies  []float64
	)
	var (
		totalDelivered atomic.Int64
		missedClients  atomic.Int64
	)
	warnCh := make(chan string, sc.clients*2)

	wallStart := time.Now()

	// ── Phase 0: connect the publisher FIRST so it is not crowded out ─────────
	pubClient, _, pubErr := newMQTTClient(mqttBrokerAddr, fmt.Sprintf("mqttbench-pub-%d", sc.clients))
	pubConnected := pubErr == nil
	if pubErr != nil {
		t.Logf("    [MQTT] publisher connect failed: %v", pubErr)
	}

	// ── Phase 1: connect + subscribe all MQTT subscriber clients ──────────────
	type setupRes struct {
		st  subState
		ms  float64
		err string
	}
	setupCh := make(chan setupRes, sc.clients)

	var setupWg sync.WaitGroup
	setupSem := make(chan struct{}, mqttSetupConc)

	for i := 0; i < sc.clients; i++ {
		setupWg.Add(1)
		clientID := fmt.Sprintf("mqttbench-%d-%d", sc.clients, i)
		topic := mqttTopicPrefix + clientID

		go func(clientID, topic string) {
			defer setupWg.Done()
			setupSem <- struct{}{}
			defer func() { <-setupSem }()

			msgCh := make(chan mqtt.Message, sc.events*2)

			c, connMS, err := newMQTTClient(mqttBrokerAddr, clientID)
			if err != nil {
				setupCh <- setupRes{err: err.Error()}
				return
			}

			tok := c.Subscribe(topic, mqttQoS, func(_ mqtt.Client, msg mqtt.Message) {
				msgCh <- msg
			})
			if !tok.WaitTimeout(15 * time.Second) {
				c.Disconnect(250)
				setupCh <- setupRes{err: fmt.Sprintf("subscribe timeout for %s", clientID)}
				return
			}
			if err := tok.Error(); err != nil {
				c.Disconnect(250)
				setupCh <- setupRes{err: fmt.Sprintf("subscribe %s: %v", clientID, err)}
				return
			}

			setupCh <- setupRes{
				st: subState{
					clientID: clientID,
					client:   c,
					topic:    topic,
					msgCh:    msgCh,
				},
				ms: connMS,
			}
		}(clientID, topic)
	}
	setupWg.Wait()
	close(setupCh)

	subs := make([]subState, 0, sc.clients)
	for r := range setupCh {
		if r.err != "" {
			warnCh <- r.err
			missedClients.Add(1)
			continue
		}
		mu.Lock()
		connSetups = append(connSetups, r.ms)
		mu.Unlock()
		subs = append(subs, r.st)
	}
	t.Logf("    [MQTT] clients connected: %d/%d (setup: %.1fs)",
		len(subs), sc.clients, time.Since(wallStart).Seconds())

	// Brief warm-up so all subscriptions are confirmed on the broker.
	time.Sleep(500 * time.Millisecond)

	// ── Phase 2: publish events ───────────────────────────────────────────────
	type pubJob struct {
		topic    string
		seq      int
		pushNano int64
	}

	pubJobCh := make(chan pubJob, mqttPushConc*2)

	var pubWorkerWg sync.WaitGroup
	for w := 0; w < mqttPushConc; w++ {
		pubWorkerWg.Add(1)
		go func() {
			defer pubWorkerWg.Done()
			for job := range pubJobCh {
				if !pubConnected {
					continue
				}
				payload, _ := json.Marshal(mqttPayload{PushNano: job.pushNano, Seq: job.seq})
				tok := pubClient.Publish(job.topic, mqttQoS, false, payload)
				// fire-and-forget for throughput; we don't wait for ack here
				_ = tok
			}
		}()
	}

	for _, sub := range subs {
		for seq := 0; seq < sc.events; seq++ {
			pubJobCh <- pubJob{
				topic:    sub.topic,
				seq:      seq,
				pushNano: time.Now().UnixNano(),
			}
		}
	}
	close(pubJobCh)
	pubWorkerWg.Wait()

	// ── Phase 3: collect received messages ────────────────────────────────────
	// Allow enough time for QoS-1 delivery + retransmits.
	receiveDeadline := 45 * time.Second
	if sc.clients >= 5000 {
		receiveDeadline = 90 * time.Second
	}
	if sc.clients >= 10000 {
		receiveDeadline = 180 * time.Second
	}

	var collectWg sync.WaitGroup
	for _, sub := range subs {
		collectWg.Add(1)
		go func(sub subState) {
			defer collectWg.Done()
			defer sub.client.Disconnect(250)

			deadline := time.After(receiveDeadline)
			received := 0
			receiveNano := time.Now().UnixNano()
			var localLats []float64

			for received < sc.events {
				select {
				case msg := <-sub.msgCh:
					receiveNano = time.Now().UnixNano()
					var p mqttPayload
					if err := json.Unmarshal(msg.Payload(), &p); err == nil && p.PushNano > 0 {
						lat := float64(receiveNano-p.PushNano) / 1e6
						if lat >= 0 {
							localLats = append(localLats, lat)
						}
					}
					received++
				case <-deadline:
					goto done
				}
			}
		done:
			if received < sc.events {
				missedClients.Add(1)
			}
			totalDelivered.Add(int64(received))
			mu.Lock()
			latencies = append(latencies, localLats...)
			mu.Unlock()
		}(sub)
	}
	collectWg.Wait()

	if pubConnected {
		pubClient.Disconnect(500)
	}
	close(stopGoro)
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

	sortedConn := sortedCopy(connSetups)
	sortedLat := sortedCopy(latencies)
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

	res := mqttResult{
		Label:          label,
		Transport:      "MQTT",
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
	}

	t.Logf("    [MQTT] delivered=%d/%d missed=%d(%.1f%%) "+
		"lat_p50=%.1fms lat_p95=%.1fms lat_p99=%.1fms lat_max=%.1fms "+
		"throughput=%.0feps rss=%.1fMB goroutines=%d wall=%.1fs",
		res.TotalDelivered, res.TotalPushed, res.MissedClients, res.MissRatioPct,
		res.LatencyP50MS, res.LatencyP95MS, res.LatencyP99MS, res.LatencyMaxMS,
		res.ThroughputEPS, res.RSSAllocMB, res.GoroutinesPeak, res.WallTimeS)

	return res
}

// ── test entry point ──────────────────────────────────────────────────────────

// TestPerf_SSEvsMQTT runs a head-to-head SSE (Scorpion) vs MQTT (EMQX) benchmark
// at up to 10 000 concurrent clients.
//
// Prerequisites:
//
//	docker compose -f docker-compose.test.yml up -d   (starts Redis + EMQX)
//	ulimit -n 65536
//
// Run:
//
//	go test -v -run TestPerf_SSEvsMQTT ./test/e2e/... -timeout 90m
func TestPerf_SSEvsMQTT(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SSE-vs-MQTT benchmark (-short)")
	}
	if !mqttBrokerAvailable() {
		t.Skip("skipping SSE-vs-MQTT benchmark – EMQX not reachable on 127.0.0.1:1883; " +
			"run: docker compose -f docker-compose.test.yml up -d")
	}

	scenarios := []mqttScenario{
		{clients: 1000, events: 5},
		{clients: 2000, events: 10},
		{clients: 5000, events: 20},
		{clients: 10000, events: 20},
	}

	// Redis for SSE server (DB 14, isolated from compare_perf_test).
	sseRDB := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:6379",
		DB:           14,
		PoolSize:     2048,
		MinIdleConns: 256,
	})
	if err := sseRDB.Ping(context.Background()).Err(); err != nil {
		t.Skipf("skipping SSE-vs-MQTT – Redis unavailable: %v", err)
	}
	if err := sseRDB.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flush redis db 14: %v", err)
	}
	t.Cleanup(func() {
		_ = sseRDB.FlushDB(context.Background()).Err()
		_ = sseRDB.Close()
	})

	sseServer := startMQTTCmpSSEServer(t, sseRDB)

	t.Logf("=== SSE (Scorpion) vs MQTT (EMQX) Benchmark ===")
	t.Logf("  SSE  poll_interval : %s", mqttSSEInterval)
	t.Logf("  MQTT broker        : %s  qos=%d", mqttBrokerAddr, mqttQoS)
	t.Logf("  scenarios          : %d", len(scenarios))

	var sseResults, mqttResults []mqttResult

	for _, sc := range scenarios {
		t.Logf("── Scenario: %d clients × %d events ──", sc.clients, sc.events)

		// Run SSE first.
		sseRes := runMQTTCmpSSEScenario(t, sseServer, sseRDB, sc)
		sseResults = append(sseResults, sseRes)

		runtime.GC()
		time.Sleep(3 * time.Second)
		_ = sseRDB.FlushDB(context.Background()).Err()

		// Run MQTT.
		mqttRes := runMQTTScenario(t, sc)
		mqttResults = append(mqttResults, mqttRes)

		runtime.GC()
		time.Sleep(3 * time.Second)
	}

	outDir := "."
	writeMQTTJSON(t, outDir, sseResults, mqttResults)
	writeMQTTHTML(t, outDir, sseResults, mqttResults)
}

// ── output writers ─────────────────────────────────────────────────────────────

type mqttJSONReport struct {
	GeneratedAt string       `json:"generated_at"`
	SSEInterval string       `json:"sse_interval"`
	MQTTBroker  string       `json:"mqtt_broker"`
	MQTTQoS     int          `json:"mqtt_qos"`
	SSE         []mqttResult `json:"sse"`
	MQTT        []mqttResult `json:"mqtt"`
}

func writeMQTTJSON(t *testing.T, dir string, sse, mq []mqttResult) {
	t.Helper()
	report := mqttJSONReport{
		GeneratedAt: time.Now().Format(time.RFC3339),
		SSEInterval: mqttSSEInterval.String(),
		MQTTBroker:  mqttBrokerAddr,
		MQTTQoS:     int(mqttQoS),
		SSE:         sse,
		MQTT:        mq,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Logf("warn: marshal mqtt json: %v", err)
		return
	}
	path := filepath.Join(dir, "mqtt_sse_results.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Logf("warn: write mqtt json: %v", err)
		return
	}
	t.Logf("📄 MQTT vs SSE JSON → %s", path)
}

func writeMQTTHTML(t *testing.T, dir string, sse, mq []mqttResult) {
	t.Helper()
	if len(sse) == 0 && len(mq) == 0 {
		return
	}

	labels := make([]string, len(sse))
	for i, r := range sse {
		labels[i] = r.Label
	}

	sseCol := func(fn func(mqttResult) float64) []float64 {
		out := make([]float64, len(sse))
		for i, r := range sse {
			out[i] = fn(r)
		}
		return out
	}
	mqCol := func(fn func(mqttResult) float64) []float64 {
		out := make([]float64, len(mq))
		for i, r := range mq {
			out[i] = fn(r)
		}
		return out
	}

	bar := func(title string, ss []SVGSeries, unit string) template.HTML {
		return template.HTML(SVGBarChart(title, labels, ss, unit))
	}

	latP50 := bar("Event Delivery Latency p50 (ms)", []SVGSeries{
		{Name: "SSE p50", Data: sseCol(func(r mqttResult) float64 { return r.LatencyP50MS })},
		{Name: "MQTT p50", Data: mqCol(func(r mqttResult) float64 { return r.LatencyP50MS })},
	}, "ms")
	latP95 := bar("Event Delivery Latency p95 (ms)", []SVGSeries{
		{Name: "SSE p95", Data: sseCol(func(r mqttResult) float64 { return r.LatencyP95MS })},
		{Name: "MQTT p95", Data: mqCol(func(r mqttResult) float64 { return r.LatencyP95MS })},
	}, "ms")
	latP99 := bar("Event Delivery Latency p99 (ms)", []SVGSeries{
		{Name: "SSE p99", Data: sseCol(func(r mqttResult) float64 { return r.LatencyP99MS })},
		{Name: "MQTT p99", Data: mqCol(func(r mqttResult) float64 { return r.LatencyP99MS })},
	}, "ms")
	latMax := bar("Event Delivery Latency max (ms)", []SVGSeries{
		{Name: "SSE max", Data: sseCol(func(r mqttResult) float64 { return r.LatencyMaxMS })},
		{Name: "MQTT max", Data: mqCol(func(r mqttResult) float64 { return r.LatencyMaxMS })},
	}, "ms")
	tput := bar("Throughput (events/sec)", []SVGSeries{
		{Name: "SSE EPS", Data: sseCol(func(r mqttResult) float64 { return r.ThroughputEPS })},
		{Name: "MQTT EPS", Data: mqCol(func(r mqttResult) float64 { return r.ThroughputEPS })},
	}, "eps")
	rss := bar("RSS Memory (MB)", []SVGSeries{
		{Name: "SSE RSS", Data: sseCol(func(r mqttResult) float64 { return r.RSSAllocMB })},
		{Name: "MQTT RSS", Data: mqCol(func(r mqttResult) float64 { return r.RSSAllocMB })},
	}, "MB")
	heap := bar("Heap Delta (MB)", []SVGSeries{
		{Name: "SSE Heap Δ", Data: sseCol(func(r mqttResult) float64 { return r.HeapDeltaMB })},
		{Name: "MQTT Heap Δ", Data: mqCol(func(r mqttResult) float64 { return r.HeapDeltaMB })},
	}, "MB")
	goros := bar("Peak Goroutines", []SVGSeries{
		{Name: "SSE", Data: sseCol(func(r mqttResult) float64 { return float64(r.GoroutinesPeak) })},
		{Name: "MQTT", Data: mqCol(func(r mqttResult) float64 { return float64(r.GoroutinesPeak) })},
	}, "")
	cpuUser := bar("CPU User Time (ms)", []SVGSeries{
		{Name: "SSE user", Data: sseCol(func(r mqttResult) float64 { return r.CPUUserMS })},
		{Name: "MQTT user", Data: mqCol(func(r mqttResult) float64 { return r.CPUUserMS })},
	}, "ms")
	cpuSys := bar("CPU Sys Time (ms)", []SVGSeries{
		{Name: "SSE sys", Data: sseCol(func(r mqttResult) float64 { return r.CPUSysMS })},
		{Name: "MQTT sys", Data: mqCol(func(r mqttResult) float64 { return r.CPUSysMS })},
	}, "ms")
	connSetup := bar("Connection Setup Latency p50 (ms)", []SVGSeries{
		{Name: "SSE conn p50", Data: sseCol(func(r mqttResult) float64 { return r.ConnSetupP50MS })},
		{Name: "MQTT conn p50", Data: mqCol(func(r mqttResult) float64 { return r.ConnSetupP50MS })},
	}, "ms")
	miss := bar("Missed Client Ratio (%)", []SVGSeries{
		{Name: "SSE miss%", Data: sseCol(func(r mqttResult) float64 { return r.MissRatioPct })},
		{Name: "MQTT miss%", Data: mqCol(func(r mqttResult) float64 { return r.MissRatioPct })},
	}, "%")

	// ── summary table ──────────────────────────────────────────────────────────
	winClass := func(sseVal, mqVal float64, lowerIsBetter bool) (string, string) {
		if lowerIsBetter {
			if sseVal < mqVal {
				return "win", "loss"
			} else if mqVal < sseVal {
				return "loss", "win"
			}
			return "", ""
		}
		if sseVal > mqVal {
			return "win", "loss"
		} else if mqVal > sseVal {
			return "loss", "win"
		}
		return "", ""
	}

	var rows strings.Builder
	for i := range sse {
		s := sse[i]
		m := mqttResult{}
		if i < len(mq) {
			m = mq[i]
		}
		row := func(field string, sv, mv float64, lowerIsBetter bool, fmt_ string) string {
			sc, mc := winClass(sv, mv, lowerIsBetter)
			return fmt.Sprintf("<tr><td>%s</td><td class='%s'>%s</td><td class='%s'>%s</td></tr>",
				field,
				sc, fmt.Sprintf(fmt_, sv),
				mc, fmt.Sprintf(fmt_, mv))
		}
		rows.WriteString(fmt.Sprintf("<tr class='scenario-header'><td colspan='3'>%s</td></tr>", s.Label))
		rows.WriteString(row("Delivered / Pushed", float64(s.TotalDelivered), float64(m.TotalDelivered), false, "%.0f"))
		rows.WriteString(row("Miss Ratio (%)", s.MissRatioPct, m.MissRatioPct, true, "%.2f"))
		rows.WriteString(row("Latency p50 (ms)", s.LatencyP50MS, m.LatencyP50MS, true, "%.1f"))
		rows.WriteString(row("Latency p95 (ms)", s.LatencyP95MS, m.LatencyP95MS, true, "%.1f"))
		rows.WriteString(row("Latency p99 (ms)", s.LatencyP99MS, m.LatencyP99MS, true, "%.1f"))
		rows.WriteString(row("Latency max (ms)", s.LatencyMaxMS, m.LatencyMaxMS, true, "%.1f"))
		rows.WriteString(row("Throughput (eps)", s.ThroughputEPS, m.ThroughputEPS, false, "%.0f"))
		rows.WriteString(row("Conn Setup p50 (ms)", s.ConnSetupP50MS, m.ConnSetupP50MS, true, "%.1f"))
		rows.WriteString(row("RSS (MB)", s.RSSAllocMB, m.RSSAllocMB, true, "%.1f"))
		rows.WriteString(row("Heap Δ (MB)", s.HeapDeltaMB, m.HeapDeltaMB, true, "%.1f"))
		rows.WriteString(row("Goroutines peak", float64(s.GoroutinesPeak), float64(m.GoroutinesPeak), true, "%.0f"))
		rows.WriteString(row("CPU user (ms)", s.CPUUserMS, m.CPUUserMS, true, "%.0f"))
		rows.WriteString(row("CPU sys (ms)", s.CPUSysMS, m.CPUSysMS, true, "%.0f"))
		rows.WriteString(row("Wall time (s)", s.WallTimeS, m.WallTimeS, true, "%.1f"))
	}

	type tmplData struct {
		GeneratedAt  string
		SSEInterval  string
		MQTTBroker   string
		MQTTQoS      int
		TableRows    template.HTML
		LatP50Chart  template.HTML
		LatP95Chart  template.HTML
		LatP99Chart  template.HTML
		LatMaxChart  template.HTML
		TputChart    template.HTML
		RSSChart     template.HTML
		HeapChart    template.HTML
		GoroChart    template.HTML
		CPUUserChart template.HTML
		CPUSysChart  template.HTML
		ConnChart    template.HTML
		MissChart    template.HTML
	}

	data := tmplData{
		GeneratedAt:  time.Now().Format(time.RFC3339),
		SSEInterval:  mqttSSEInterval.String(),
		MQTTBroker:   mqttBrokerAddr,
		MQTTQoS:      int(mqttQoS),
		TableRows:    template.HTML(rows.String()),
		LatP50Chart:  latP50,
		LatP95Chart:  latP95,
		LatP99Chart:  latP99,
		LatMaxChart:  latMax,
		TputChart:    tput,
		RSSChart:     rss,
		HeapChart:    heap,
		GoroChart:    goros,
		CPUUserChart: cpuUser,
		CPUSysChart:  cpuSys,
		ConnChart:    connSetup,
		MissChart:    miss,
	}

	const htmlTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>SSE vs MQTT Benchmark — Scorpion</title>
<style>
  body{font-family:system-ui,sans-serif;background:#0f172a;color:#e2e8f0;margin:0;padding:2rem}
  h1{color:#38bdf8;margin-bottom:0.25rem}
  .meta{color:#94a3b8;font-size:0.85rem;margin-bottom:2rem}
  h2{color:#7dd3fc;border-bottom:1px solid #1e293b;padding-bottom:0.25rem}
  .charts{display:flex;flex-wrap:wrap;gap:1rem}
  .charts > *{flex:1 1 520px}
  table{border-collapse:collapse;width:100%;margin-bottom:2rem}
  th{background:#1e293b;color:#94a3b8;text-align:left;padding:0.5rem 0.75rem;font-size:0.8rem}
  td{padding:0.4rem 0.75rem;border-bottom:1px solid #1e293b;font-size:0.85rem}
  tr.scenario-header td{background:#1e3a5f;color:#93c5fd;font-weight:700;padding:0.6rem 0.75rem}
  td.win{color:#4ade80;font-weight:600}
  td.loss{color:#f87171}
  svg text{fill:#94a3b8}
</style>
</head>
<body>
<h1>📡 SSE (Scorpion) vs MQTT (EMQX) Benchmark</h1>
<div class="meta">
  Generated: {{.GeneratedAt}} &nbsp;|&nbsp;
  SSE poll interval: {{.SSEInterval}} &nbsp;|&nbsp;
  MQTT broker: {{.MQTTBroker}} &nbsp;|&nbsp;
  QoS: {{.MQTTQoS}}
</div>

<h2>Summary</h2>
<table>
  <thead><tr><th>Metric</th><th>SSE</th><th>MQTT</th></tr></thead>
  <tbody>{{.TableRows}}</tbody>
</table>

<h2>Charts</h2>
<div class="charts">
  {{.LatP50Chart}}
  {{.LatP95Chart}}
  {{.LatP99Chart}}
  {{.LatMaxChart}}
  {{.TputChart}}
  {{.RSSChart}}
  {{.HeapChart}}
  {{.GoroChart}}
  {{.CPUUserChart}}
  {{.CPUSysChart}}
  {{.ConnChart}}
  {{.MissChart}}
</div>
</body>
</html>`

	tmpl, err := template.New("mqtt_sse").Parse(htmlTmpl)
	if err != nil {
		t.Logf("warn: parse html template: %v", err)
		return
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Logf("warn: execute html template: %v", err)
		return
	}

	path := filepath.Join(dir, "mqtt_sse_report.html")
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		t.Logf("warn: write html report: %v", err)
		return
	}
	t.Logf("📊 MQTT vs SSE HTML report → %s", path)
}
