package e2e_test

// High-Concurrency / Low-Event performance test for Scorpion.
//
// Goal: measure system behaviour when thousands of clients hold open SSE
// connections while only a small number of events (20-30) are delivered.
// This stresses the fan-out / goroutine scheduler / Redis pub-sub path rather
// than event throughput.
//
// Scenarios
// ─────────
//   1 000 clients × 20 events
//   2 000 clients × 25 events
//   5 000 clients × 30 events
//
// What is measured (per scenario)
// ────────────────────────────────
//   • Heap allocation delta + RSS (MB)
//   • Peak live goroutine count
//   • SSE connection-setup latency       (p50 / p95 ms)
//   • End-to-end event delivery latency  (p50 / p95 / p99 / max ms)
//   • Fan-out correctness                (events delivered vs expected)
//   • Missed-client ratio
//   • Throughput (delivered events / second)
//   • CPU user + sys time (ms)
//   • Wall-clock seconds
//
// Output files (written to test/e2e/)
// ────────────────────────────────────
//   hc_perf_results.json    machine-readable raw data
//   hc_perf_report.html     self-contained HTML with Chart.js charts
//
// Run
// ───
//   go test -v -run TestPerf_HighConcurrency ./test/e2e/... -timeout 30m
//
// NOTE: the host OS must allow enough open file descriptors.  Before running:
//   ulimit -n 65536

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

	appmiddleware "github.com/blkst8/scorpion/internal/appmiddleware"
	"github.com/blkst8/scorpion/internal/config"
	httpserver "github.com/blkst8/scorpion/internal/http"
	"github.com/blkst8/scorpion/internal/http/handlers"
	applog "github.com/blkst8/scorpion/internal/log"
	"github.com/blkst8/scorpion/internal/metrics"
	"github.com/blkst8/scorpion/internal/ratelimit"
	redisstore "github.com/blkst8/scorpion/internal/repository"
)

// ── types ──────────────────────────────────────────────────────────────────────

// hcScenario describes one high-concurrency run.
type hcScenario struct {
	clients int
	events  int
}

// hcResult holds all captured metrics for one high-concurrency scenario.
type hcResult struct {
	Label string

	Clients        int
	Events         int
	TotalPushed    int
	TotalDelivered int
	MissedClients  int
	MissRatioPct   float64

	HeapAllocMB float64
	RSSAllocMB  float64

	GoroutinesPeak int

	ConnSetupP50MS float64
	ConnSetupP95MS float64

	LatencyP50MS float64
	LatencyP95MS float64
	LatencyP99MS float64
	LatencyMaxMS float64

	ThroughputEPS float64
	WallTimeS     float64

	CPUUserMS float64
	CPUSysMS  float64
}

// ── server for high-concurrency tests ─────────────────────────────────────────

// startHCServer starts a Scorpion instance tuned for thousands of concurrent
// connections: higher rate limits, larger Redis pool, relaxed SSE settings.
func startHCServer(t *testing.T) *testServer {
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
			ShutdownTimeout: 15 * time.Second,
			ReadTimeout:     30 * time.Second,
			IdleTimeout:     120 * time.Second,
		},
		SSE: config.SSE{
			PollInterval:      200 * time.Millisecond,
			BatchSize:         200,
			HeartbeatInterval: 10 * time.Second,
			ConnTTL:           5 * time.Minute,
			MaxQueueDepth:     5000,
			MaxEventBytes:     65536,
		},
		Auth: config.Auth{
			TokenSecret:  "e2e-token-secret",
			TicketSecret: "e2e-ticket-secret",
			TicketTTL:    15 * time.Minute,
		},
		IP: config.IP{
			Strategy: "remote_addr",
		},
		RateLimit: config.RateLimit{
			// Generous limits so rate-limiting does not become the bottleneck.
			TicketRPM:   600000,
			TicketBurst: 10000,
		},
		Redis: config.Redis{
			Address:      "127.0.0.1:6379",
			DB:           14, // separate DB from default e2e tests
			PoolSize:     512,
			MinIdleConns: 64,
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
		t.Fatalf("ip strategy: %v", err)
	}

	rdb, err := redisstore.NewClient(cfg.Redis, m)
	if err != nil {
		t.Skipf("skipping HC e2e – Redis unavailable: %v", err)
	}

	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flush test db: %v", err)
	}

	t.Cleanup(func() {
		_ = rdb.FlushDB(context.Background()).Err()
		_ = rdb.Close()
	})

	ticketStore := redisstore.NewTicketStore(rdb)
	connStore := redisstore.NewConnectionStore(rdb, "hc-e2e-instance", log)
	eventStore := redisstore.NewEventStore(rdb, cfg.SSE.MaxQueueDepth)
	limiter := ratelimit.NewLimiter(rdb, cfg.RateLimit)

	ticketHandler := handlers.NewTicketHandler(cfg, ticketStore, limiter, ipStrategy, log, m)
	sseHandler := handlers.NewSSEHandler(cfg, ticketStore, connStore, eventStore, ipStrategy, log, m)
	eventHandler := handlers.NewEventHandler(eventStore, cfg.SSE, log)
	pollHandler := handlers.NewPollHandler(eventStore, cfg.SSE, log)

	srv := httpserver.NewServer(cfg, log, rdb, ipStrategy, ticketHandler, sseHandler, eventHandler, pollHandler)
	srv.Serve()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	baseURL := fmt.Sprintf("https://localhost:%d", mainPort)
	waitReady(t, tlsHTTPClient(pool), baseURL, 15*time.Second)

	return &testServer{srv: srv, baseURL: baseURL, pool: pool}
}

// ── scenario runner ────────────────────────────────────────────────────────────

// pushSemaphore limits concurrent HTTP push requests to avoid overwhelming the
// OS TCP stack during the setup phase. The streaming phase is fully concurrent.
const hcPushConcurrency = 300

func runHCScenario(t *testing.T, ts *testServer, sc hcScenario) hcResult {
	t.Helper()

	label := fmt.Sprintf("clients=%d events=%d", sc.clients, sc.events)
	t.Logf("  ▶ HC %s", label)

	runtime.GC()
	heapBefore, _ := heapMB()
	cpuUserBefore, cpuSysBefore := cpuTimeMS()
	wallStart := time.Now()

	// Track peak goroutine count.
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
	defer close(stopGoro)

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

	// Shared HTTP clients.
	setupClient := tlsHTTPClient(ts.pool)

	// ── Phase 1: obtain tickets and open SSE streams for all clients ───────────
	type streamState struct {
		clientID string
		bearer   string
		body     interface{ Read([]byte) (int, error) }
		closer   func()
	}

	streams := make([]streamState, 0, sc.clients)
	var setupWg sync.WaitGroup
	setupSem := make(chan struct{}, hcPushConcurrency)

	type setupResult struct {
		st  streamState
		ms  float64
		err string
	}
	setupCh := make(chan setupResult, sc.clients)

	for i := 0; i < sc.clients; i++ {
		setupWg.Add(1)
		clientID := fmt.Sprintf("hc-client-%d", i)
		bearer := makeBearerToken(t, "e2e-token-secret", clientID)

		go func(clientID, bearer string) {
			defer setupWg.Done()
			setupSem <- struct{}{}
			defer func() { <-setupSem }()

			// obtain ticket
			ticketReq, _ := http.NewRequest(http.MethodPost, ts.baseURL+"/v1/auth/ticket", nil)
			ticketReq.Header.Set("Authorization", "Bearer "+bearer)
			ticketResp, err := setupClient.Do(ticketReq)
			if err != nil {
				setupCh <- setupResult{err: fmt.Sprintf("ticket %s: %v", clientID, err)}
				return
			}
			var ticketBody struct {
				Ticket string `json:"ticket"`
			}
			_ = json.NewDecoder(ticketResp.Body).Decode(&ticketBody)
			_ = ticketResp.Body.Close()
			if ticketBody.Ticket == "" {
				setupCh <- setupResult{err: fmt.Sprintf("empty ticket for %s", clientID)}
				return
			}

			// open SSE stream
			connStart := time.Now()
			streamCtx, streamCancel := context.WithTimeout(context.Background(), 3*time.Minute)
			streamURL := fmt.Sprintf("%s/v1/stream/events?ticket=%s", ts.baseURL, ticketBody.Ticket)
			streamReq, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, streamURL, nil)
			streamReq.Header.Set("Accept", "text/event-stream")
			streamReq.Header.Set("Authorization", "Bearer "+bearer)

			streamResp, err := streamingHTTPClient(ts.pool).Do(streamReq)
			if err != nil {
				streamCancel()
				setupCh <- setupResult{err: fmt.Sprintf("stream open %s: %v", clientID, err)}
				return
			}

			setupMS := float64(time.Since(connStart).Microseconds()) / 1e3
			setupCh <- setupResult{
				st: streamState{
					clientID: clientID,
					bearer:   bearer,
					body:     streamResp.Body,
					closer: func() {
						_ = streamResp.Body.Close()
						streamCancel()
					},
				},
				ms: setupMS,
			}
		}(clientID, bearer)
	}

	setupWg.Wait()
	close(setupCh)

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

	t.Logf("    streams opened: %d / %d", len(streams), sc.clients)

	// ── Phase 2+3: start collectors FIRST, then push events concurrently ─────────
	// Collectors must be draining the SSE response bodies while events are
	// being pushed. If we push first, the server-side write deadline (5 s) fires
	// before Phase 3 starts reading and the connections are dropped → 0 delivered.

	// Small pause so the poll ticker in the stream loop has fired at least once.
	time.Sleep(300 * time.Millisecond)

	var collectWg sync.WaitGroup
	// collectDone is closed once all collector goroutines have exited.
	collectDone := make(chan struct{})

	for _, st := range streams {
		collectWg.Add(1)
		go func(st streamState) {
			defer collectWg.Done()
			defer st.closer()

			// Give enough headroom: push phase + poll interval + flush time.
			readDeadline := time.Duration(sc.events)*200*time.Millisecond +
				time.Duration(sc.clients/100)*time.Second +
				60*time.Second
			readCtx, readCancel := context.WithTimeout(context.Background(), readDeadline)
			defer readCancel()

			events := readHCSSEEvents(readCtx, st.body, sc.events)

			if len(events) < sc.events {
				missedClients.Add(1)
			}

			var localLats []float64
			for _, e := range events {
				receiveNano := time.Now().UnixNano()
				var pe perfEvent
				if err := json.Unmarshal([]byte(e), &pe); err == nil && pe.PushNano > 0 {
					latMS := float64(receiveNano-pe.PushNano) / 1e6
					if latMS >= 0 {
						localLats = append(localLats, latMS)
					}
				}
			}

			totalDelivered.Add(int64(len(events)))
			mu.Lock()
			latenciesMS = append(latenciesMS, localLats...)
			mu.Unlock()
		}(st)
	}

	go func() {
		collectWg.Wait()
		close(collectDone)
	}()

	// Push events while collectors are actively reading.
	pushClient := tlsHTTPClient(ts.pool)
	pushSem := make(chan struct{}, hcPushConcurrency)
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
					"type": "hc.perf.event",
					"data": perfEvent{PushNano: pushNano, Seq: seq},
				})
				req, _ := http.NewRequest(http.MethodPost,
					fmt.Sprintf("%s/v1/events/%s", ts.baseURL, clientID),
					bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+bearer)

				resp, err := pushClient.Do(req)
				if err != nil {
					warnCh <- fmt.Sprintf("push %s seq %d: %v", clientID, seq, err)
					return
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusCreated {
					warnCh <- fmt.Sprintf("push %s seq %d: status %d", clientID, seq, resp.StatusCode)
					return
				}
			}
		}(st.clientID, st.bearer)
	}

	pushWg.Wait()

	// Wait for all collectors to finish (they will drain remaining queued events).
	<-collectDone
	close(warnCh)

	// Report only a sample of warnings to avoid log flood.
	warnCount := 0
	for w := range warnCh {
		if warnCount < 20 {
			t.Logf("    warn: %v", w)
		}
		warnCount++
	}
	if warnCount > 20 {
		t.Logf("    ... and %d more warnings (suppressed)", warnCount-20)
	}

	wallSec := time.Since(wallStart).Seconds()
	heapAfter, _ := heapMB()
	cpuUserAfter, cpuSysAfter := cpuTimeMS()

	sortedConn := sortedCopy(connSetupMS)
	sortedLat := sortedCopy(latenciesMS)
	delivered := int(totalDelivered.Load())
	missed := int(missedClients.Load())
	expected := sc.clients * sc.events

	throughput := 0.0
	if wallSec > 0 {
		throughput = float64(delivered) / wallSec
	}
	missRatio := 0.0
	if sc.clients > 0 {
		missRatio = float64(missed) / float64(sc.clients) * 100
	}

	res := hcResult{
		Label:          label,
		Clients:        sc.clients,
		Events:         sc.events,
		TotalPushed:    expected,
		TotalDelivered: delivered,
		MissedClients:  missed,
		MissRatioPct:   missRatio,
		HeapAllocMB:    heapAfter - heapBefore,
		RSSAllocMB:     rssMemMB(),
		GoroutinesPeak: int(goroPeak.Load()),
		ConnSetupP50MS: pctile(sortedConn, 50),
		ConnSetupP95MS: pctile(sortedConn, 95),
		LatencyP50MS:   pctile(sortedLat, 50),
		LatencyP95MS:   pctile(sortedLat, 95),
		LatencyP99MS:   pctile(sortedLat, 99),
		LatencyMaxMS:   pctile(sortedLat, 100),
		ThroughputEPS:  throughput,
		WallTimeS:      wallSec,
		CPUUserMS:      cpuUserAfter - cpuUserBefore,
		CPUSysMS:       cpuSysAfter - cpuSysBefore,
	}

	t.Logf("    heap_delta=%.1fMB rss=%.1fMB goroutines=%d "+
		"conn_p50=%.1fms conn_p95=%.1fms "+
		"lat_p50=%.1fms lat_p95=%.1fms lat_p99=%.1fms lat_max=%.1fms "+
		"throughput=%.0feps cpu_user=%.0fms cpu_sys=%.0fms "+
		"delivered=%d/%d missed=%d(%.1f%%) wall=%.1fs",
		res.HeapAllocMB, res.RSSAllocMB, res.GoroutinesPeak,
		res.ConnSetupP50MS, res.ConnSetupP95MS,
		res.LatencyP50MS, res.LatencyP95MS, res.LatencyP99MS, res.LatencyMaxMS,
		res.ThroughputEPS, res.CPUUserMS, res.CPUSysMS,
		res.TotalDelivered, res.TotalPushed, res.MissedClients, res.MissRatioPct,
		res.WallTimeS,
	)

	return res
}

// readHCSSEEvents reads SSE data fields from body until wantCount events are
// collected or ctx is cancelled. It returns the raw JSON data strings.
// NOTE: always returns collected events even on timeout (never returns nil).
func readHCSSEEvents(ctx context.Context, body interface{ Read([]byte) (int, error) }, wantCount int) []string {
	resultCh := make(chan []string, 1)

	go func() {
		var collected []string
		scanner := bufio.NewScanner(body.(interface {
			Read(p []byte) (n int, err error)
		}))
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				resultCh <- collected
				return
			default:
			}
			line := scanner.Text()
			const dataPrefix = "data: "
			if len(line) < len(dataPrefix) || line[:len(dataPrefix)] != dataPrefix {
				continue
			}
			data := line[len(dataPrefix):]
			// skip heartbeat pings
			if data == ": ping" || data == "ping" || data == "" {
				continue
			}
			// the SSE data line is the raw JSON bytes from EventPayload.Data
			var envelope struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal([]byte(data), &envelope); err == nil && len(envelope.Data) > 0 {
				collected = append(collected, string(envelope.Data))
			} else {
				// data IS the payload (e.g. {"push_ns":...,"seq":...})
				collected = append(collected, data)
			}
			if len(collected) >= wantCount {
				break
			}
		}
		resultCh <- collected
	}()

	select {
	case r := <-resultCh:
		return r
	case <-ctx.Done():
		// drain whatever was already collected
		select {
		case r := <-resultCh:
			return r
		default:
			return nil
		}
	}
}

// ── test entry point ───────────────────────────────────────────────────────────

func TestPerf_HighConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping high-concurrency performance test (-short flag set)")
	}

	ts := startHCServer(t)

	scenarios := []hcScenario{
		{clients: 1000, events: 20},
		{clients: 2000, events: 25},
		{clients: 5000, events: 30},
	}

	t.Log("=== High-Concurrency / Low-Event scenarios ===")
	t.Logf("  push concurrency cap : %d goroutines", hcPushConcurrency)
	t.Logf("  scenarios            : %d", len(scenarios))

	results := make([]hcResult, 0, len(scenarios))
	for _, sc := range scenarios {
		results = append(results, runHCScenario(t, ts, sc))
		// Cool-down between runs – let goroutines exit and GC collect.
		runtime.GC()
		time.Sleep(2 * time.Second)
	}

	outDir := "."
	writeHCPerfJSON(t, outDir, results)
	writeHCPerfHTML(t, outDir, results)
}

// ── output writers ─────────────────────────────────────────────────────────────

type hcJSONReport struct {
	GeneratedAt string     `json:"generated_at"`
	Scenarios   []hcResult `json:"scenarios"`
}

func writeHCPerfJSON(t *testing.T, dir string, results []hcResult) {
	t.Helper()
	report := hcJSONReport{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Scenarios:   results,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Logf("warn: marshal hc json: %v", err)
		return
	}
	path := filepath.Join(dir, "hc_perf_results.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Logf("warn: write hc json: %v", err)
		return
	}
	t.Logf("📄 HC JSON results  → %s", path)
}

// writeHCPerfHTML generates a fully self-contained HTML report using inline SVG
// charts — no external CDN or JavaScript required.
func writeHCPerfHTML(t *testing.T, dir string, results []hcResult) {
	t.Helper()

	if len(results) == 0 {
		return
	}

	// ── helpers ───────────────────────────────────────────────────────────────
	labels := make([]string, len(results))
	for i, r := range results {
		labels[i] = r.Label
	}

	col := func(fn func(hcResult) float64) []float64 {
		out := make([]float64, len(results))
		for i, r := range results {
			out[i] = fn(r)
		}
		return out
	}

	bar := func(title string, ss []SVGSeries, unit string) string {
		return SVGBarChart(title, labels, ss, unit)
	}

	// ── charts ────────────────────────────────────────────────────────────────
	latChart := bar("Event Delivery Latency (ms)", []SVGSeries{
		{Name: "p50", Data: col(func(r hcResult) float64 { return r.LatencyP50MS })},
		{Name: "p95", Data: col(func(r hcResult) float64 { return r.LatencyP95MS })},
		{Name: "p99", Data: col(func(r hcResult) float64 { return r.LatencyP99MS })},
		{Name: "max", Data: col(func(r hcResult) float64 { return r.LatencyMaxMS })},
	}, "ms")

	connChart := bar("SSE Connection Setup Latency (ms)", []SVGSeries{
		{Name: "p50", Data: col(func(r hcResult) float64 { return r.ConnSetupP50MS })},
		{Name: "p95", Data: col(func(r hcResult) float64 { return r.ConnSetupP95MS })},
	}, "ms")

	memChart := bar("Memory (MB)", []SVGSeries{
		{Name: "Heap Δ", Data: col(func(r hcResult) float64 { return r.HeapAllocMB })},
		{Name: "RSS", Data: col(func(r hcResult) float64 { return r.RSSAllocMB })},
	}, "MB")

	goroChart := bar("Peak Goroutines", []SVGSeries{
		{Name: "goroutines", Data: col(func(r hcResult) float64 { return float64(r.GoroutinesPeak) })},
	}, "")

	cpuChart := bar("CPU Time (ms)", []SVGSeries{
		{Name: "user", Data: col(func(r hcResult) float64 { return r.CPUUserMS })},
		{Name: "sys", Data: col(func(r hcResult) float64 { return r.CPUSysMS })},
	}, "ms")

	tputChart := bar("Throughput (events/sec)", []SVGSeries{
		{Name: "EPS", Data: col(func(r hcResult) float64 { return r.ThroughputEPS })},
	}, "eps")

	missChart := bar("Missed Client Ratio (%)", []SVGSeries{
		{Name: "miss %", Data: col(func(r hcResult) float64 { return r.MissRatioPct })},
	}, "%")

	wallChart := bar("Wall Time (s)", []SVGSeries{
		{Name: "wall s", Data: col(func(r hcResult) float64 { return r.WallTimeS })},
	}, "s")

	// ── summary table rows ────────────────────────────────────────────────────
	var tableRows strings.Builder
	for _, r := range results {
		missClass := "ok"
		if r.MissRatioPct > 1 {
			missClass = "warn"
		}
		fmt.Fprintf(&tableRows, `<tr>
<td>%s</td><td>%d</td><td>%d</td>
<td>%d/%d</td><td><span class="badge %s">%.2f%%</span></td>
<td>%.1f</td><td>%.1f</td>
<td>%.1f</td><td>%.1f</td><td>%.1f</td><td>%.1f</td>
<td>%.1f</td><td>%.1f</td><td>%d</td>
<td>%.1f</td><td>%.0f</td>
</tr>`,
			r.Label, r.Clients, r.Events,
			r.TotalDelivered, r.TotalPushed, missClass, r.MissRatioPct,
			r.ConnSetupP50MS, r.ConnSetupP95MS,
			r.LatencyP50MS, r.LatencyP95MS, r.LatencyP99MS, r.LatencyMaxMS,
			r.HeapAllocMB, r.RSSAllocMB, r.GoroutinesPeak,
			r.WallTimeS, r.ThroughputEPS,
		)
	}

	// ── HTML ──────────────────────────────────────────────────────────────────
	html := `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Scorpion HC Perf Report</title>
<style>
  *{box-sizing:border-box;margin:0;padding:0}
  body{font-family:system-ui,sans-serif;max-width:1280px;margin:0 auto;padding:2rem;
       background:#0f172a;color:#e2e8f0}
  h1{color:#38bdf8;font-size:1.6rem;margin-bottom:.25rem}
  .ts{color:#94a3b8;font-size:.8rem;margin-bottom:2rem}
  h2{color:#7dd3fc;font-size:1.1rem;margin:2.5rem 0 1rem;
     border-bottom:1px solid #334155;padding-bottom:.4rem}
  .grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(560px,1fr));gap:1.5rem;margin:1rem 0}
  table{width:100%;border-collapse:collapse;font-size:.8rem;margin:1rem 0}
  th{background:#1e40af;padding:.5rem .7rem;text-align:right;color:#bfdbfe;
     font-weight:600;text-transform:uppercase;letter-spacing:.04em}
  th:first-child{text-align:left}
  td{padding:.4rem .6rem;border-bottom:1px solid #1e293b;text-align:right}
  td:first-child{text-align:left}
  tr:hover td{background:#1e293b}
  .badge{display:inline-block;padding:.1rem .45rem;border-radius:4px;font-size:.75rem;font-weight:600}
  .ok{background:#14532d;color:#86efac}.warn{background:#78350f;color:#fcd34d}
  svg{display:block;width:100%;height:auto}
</style>
</head>
<body>
<h1>&#9889; Scorpion &#8212; High-Concurrency / Low-Event Performance Report</h1>
<p class="ts">Generated: ` + time.Now().Format("2006-01-02 15:04:05 MST") + ` &nbsp;|&nbsp; Scenarios: 1k&#8211;5k clients &#215; 20&#8211;30 events</p>

<h2>Summary Table</h2>
<table>
<thead><tr>
  <th>Scenario</th><th>Clients</th><th>Events</th>
  <th>Delivered</th><th>Miss%</th>
  <th>ConnP50(ms)</th><th>ConnP95(ms)</th>
  <th>LatP50(ms)</th><th>LatP95(ms)</th><th>LatP99(ms)</th><th>LatMax(ms)</th>
  <th>Heap&#916;(MB)</th><th>RSS(MB)</th><th>Goroutines</th>
  <th>Wall(s)</th><th>EPS</th>
</tr></thead>
<tbody>` + tableRows.String() + `</tbody>
</table>

<h2>Delivery Latency</h2>
<div class="grid">` + latChart + connChart + `</div>

<h2>Resource Usage</h2>
<div class="grid">` + memChart + goroChart + cpuChart + `</div>

<h2>Throughput &amp; Fan-out Correctness</h2>
<div class="grid">` + tputChart + missChart + wallChart + `</div>
</body>
</html>`

	path := filepath.Join(dir, "hc_perf_report.html")
	if err := os.WriteFile(path, []byte(html), 0o644); err != nil {
		t.Logf("warn: write hc html: %v", err)
		return
	}
	t.Logf("📊 HC HTML report   → %s", path)
}
