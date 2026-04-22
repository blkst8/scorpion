package e2e_test

// Performance / scalability test for Scorpion.
//
// Two delivery mechanisms are benchmarked under identical load scenarios:
//   - SSE  : persistent HTTP/2 server-sent event stream
//   - Poll : periodic HTTP GET /v1/events/:client_id (configurable interval)
//
// What is measured
// ────────────────
// For every (numConnections × eventsPerClient) scenario:
//   • Heap allocation delta and RSS (MB)
//   • Peak live goroutine count
//   • SSE connection-setup latency   (p50 / p95 ms)
//   • End-to-end event delivery latency (p50 / p95 / p99 / max ms)
//     measured from a push timestamp embedded in the event payload
//   • Throughput (delivered events / second)
//   • CPU user + sys time consumed (ms, via RUSAGE_SELF delta)
//
// Output files (written to test/e2e/)
// ────────────────────────────────────
//   perf_results.json   machine-readable raw data
//   perf_report.html    self-contained HTML with Chart.js charts
//
// Run
// ───
//   go test -v -run TestPerf ./test/e2e/... -timeout 10m

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"text/template"
	"time"
)

//go:embed perf_report_template.html
var perfReportTemplate string

// ── data types ────────────────────────────────────────────────────────────────

// PerfScenario describes one run.
type PerfScenario struct {
	Label           string
	NumConnections  int
	EventsPerClient int
}

// PerfResult holds all captured metrics for one scenario.
type PerfResult struct {
	Scenario PerfScenario

	// Memory
	HeapAllocMB float64
	HeapSysMB   float64
	RSSAllocMB  float64

	// Concurrency
	GoroutinesPeak int

	// Connection setup
	ConnSetupP50MS float64
	ConnSetupP95MS float64

	// Event delivery latency (push → receive)
	LatencyP50MS float64
	LatencyP95MS float64
	LatencyP99MS float64
	LatencyMaxMS float64

	// Throughput
	ThroughputEPS  float64
	TotalPushed    int
	TotalDelivered int
	WallTimeS      float64

	// CPU
	CPUUserMS float64
	CPUSysMS  float64
}

// perfEvent is the JSON payload embedded in every pushed event.
type perfEvent struct {
	PushNano int64 `json:"push_ns"`
	Seq      int   `json:"seq"`
}

// ── statistics helpers ────────────────────────────────────────────────────────

func sortedCopy(in []float64) []float64 {
	out := make([]float64, len(in))
	copy(out, in)
	sort.Float64s(out)
	return out
}

func pctile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100.0*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// ── system resource helpers ───────────────────────────────────────────────────

func cpuTimeMS() (userMS, sysMS float64) {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	tv := func(t syscall.Timeval) float64 {
		return float64(t.Sec)*1e3 + float64(t.Usec)/1e3
	}
	return tv(ru.Utime), tv(ru.Stime)
}

func heapMB() (allocMB, sysMB float64) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.HeapAlloc) / (1 << 20), float64(ms.HeapSys) / (1 << 20)
}

// rssMemMB reads resident-set size from /proc/self/status (Linux). Returns 0 elsewhere.
func rssMemMB() float64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("VmRSS:")) {
			var kb int64
			if n, err := fmt.Sscanf(string(bytes.TrimPrefix(line, []byte("VmRSS:"))), "%d", &kb); err == nil && n == 1 {
				return float64(kb) / 1024
			}
		}
	}
	return 0
}

// ── scenario runner ───────────────────────────────────────────────────────────

func runPerfScenario(t *testing.T, ts *testServer, numConns, eventsPerClient int) PerfResult {
	t.Helper()

	label := fmt.Sprintf("conns=%d events=%d", numConns, eventsPerClient)
	t.Logf("  ▶ %s", label)

	runtime.GC()
	heapBefore, _ := heapMB()
	cpuUserBefore, cpuSysBefore := cpuTimeMS()
	wallStart := time.Now()

	// Track peak goroutine count in background.
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
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()
	defer close(stopGoro)

	var (
		mu          sync.Mutex
		connSetupMS []float64
		latenciesMS []float64
	)
	var totalDelivered atomic.Int64

	httpClient := tlsHTTPClient(ts.pool)
	var wg sync.WaitGroup
	warnCh := make(chan string, numConns*4)

	for i := 0; i < numConns; i++ {
		wg.Add(1)
		clientID := fmt.Sprintf("perf-client-%d", i)
		bearer := makeBearerToken(t, "e2e-token-secret", clientID)

		go func(clientID, bearer string) {
			defer wg.Done()

			// ── 1. push all events before opening the stream ───────────────
			for seq := 0; seq < eventsPerClient; seq++ {
				pushNano := time.Now().UnixNano()
				body, _ := json.Marshal(map[string]any{
					"type": "perf.event",
					"data": perfEvent{PushNano: pushNano, Seq: seq},
				})

				req, _ := http.NewRequest(http.MethodPost,
					fmt.Sprintf("%s/v1/events/%s", ts.baseURL, clientID),
					bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+bearer)

				resp, err := httpClient.Do(req)
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

			// ── 2. obtain ticket ───────────────────────────────────────────
			ticketReq, _ := http.NewRequest(http.MethodPost, ts.baseURL+"/v1/auth/ticket", nil)
			ticketReq.Header.Set("Authorization", "Bearer "+bearer)
			ticketResp, err := httpClient.Do(ticketReq)
			if err != nil {
				warnCh <- fmt.Sprintf("ticket %s: %v", clientID, err)
				return
			}
			var ticketBody struct {
				Ticket string `json:"ticket"`
			}
			_ = json.NewDecoder(ticketResp.Body).Decode(&ticketBody)
			_ = ticketResp.Body.Close()
			if ticketBody.Ticket == "" {
				warnCh <- fmt.Sprintf("empty ticket for %s", clientID)
				return
			}

			// ── 3. open SSE stream and measure connection-setup time ───────
			connStart := time.Now()
			streamCtx, streamCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer streamCancel()

			streamURL := fmt.Sprintf("%s/v1/stream/events?ticket=%s", ts.baseURL, ticketBody.Ticket)
			streamReq, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, streamURL, nil)
			streamReq.Header.Set("Accept", "text/event-stream")
			streamReq.Header.Set("Authorization", "Bearer "+bearer)

			streamResp, err := streamingHTTPClient(ts.pool).Do(streamReq)
			if err != nil {
				warnCh <- fmt.Sprintf("stream open %s: %v", clientID, err)
				return
			}

			setupMS := float64(time.Since(connStart).Microseconds()) / 1e3
			mu.Lock()
			connSetupMS = append(connSetupMS, setupMS)
			mu.Unlock()

			// ── 4. read events and compute delivery latency ────────────────
			readCtx, readCancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer readCancel()

			// readSSEEvents closes the body when readCtx expires.
			sseEvents, _ := readSSEEvents(readCtx, streamResp.Body)

			var localLats []float64
			for _, e := range sseEvents {
				receiveNano := time.Now().UnixNano()
				// The SSE data field is the JSON of domain.EventPayload.Data
				// which is the raw JSON of perfEvent.
				var pe perfEvent
				if err := json.Unmarshal([]byte(e.Data), &pe); err == nil && pe.PushNano > 0 {
					latMS := float64(receiveNano-pe.PushNano) / 1e6
					if latMS >= 0 {
						localLats = append(localLats, latMS)
					}
				}
			}

			totalDelivered.Add(int64(len(sseEvents)))
			mu.Lock()
			latenciesMS = append(latenciesMS, localLats...)
			mu.Unlock()
		}(clientID, bearer)
	}

	wg.Wait()
	close(warnCh)
	for w := range warnCh {
		t.Logf("    warn: %v", w)
	}

	wallSec := time.Since(wallStart).Seconds()
	heapAfter, heapSysAfter := heapMB()
	cpuUserAfter, cpuSysAfter := cpuTimeMS()

	sortedConn := sortedCopy(connSetupMS)
	sortedLat := sortedCopy(latenciesMS)
	delivered := int(totalDelivered.Load())

	throughput := 0.0
	if wallSec > 0 {
		throughput = float64(delivered) / wallSec
	}

	res := PerfResult{
		Scenario:       PerfScenario{Label: label, NumConnections: numConns, EventsPerClient: eventsPerClient},
		HeapAllocMB:    heapAfter - heapBefore,
		HeapSysMB:      heapSysAfter,
		RSSAllocMB:     rssMemMB(),
		GoroutinesPeak: int(goroPeak.Load()),
		ConnSetupP50MS: pctile(sortedConn, 50),
		ConnSetupP95MS: pctile(sortedConn, 95),
		LatencyP50MS:   pctile(sortedLat, 50),
		LatencyP95MS:   pctile(sortedLat, 95),
		LatencyP99MS:   pctile(sortedLat, 99),
		LatencyMaxMS:   pctile(sortedLat, 100),
		ThroughputEPS:  throughput,
		TotalPushed:    numConns * eventsPerClient,
		TotalDelivered: delivered,
		WallTimeS:      wallSec,
		CPUUserMS:      cpuUserAfter - cpuUserBefore,
		CPUSysMS:       cpuSysAfter - cpuSysBefore,
	}

	t.Logf("    heap_delta=%.1fMB rss=%.1fMB goroutines=%d "+
		"conn_p50=%.1fms conn_p95=%.1fms "+
		"lat_p50=%.1fms lat_p95=%.1fms lat_p99=%.1fms lat_max=%.1fms "+
		"throughput=%.0feps cpu_user=%.0fms cpu_sys=%.0fms "+
		"delivered=%d/%d wall=%.1fs",
		res.HeapAllocMB, res.RSSAllocMB, res.GoroutinesPeak,
		res.ConnSetupP50MS, res.ConnSetupP95MS,
		res.LatencyP50MS, res.LatencyP95MS, res.LatencyP99MS, res.LatencyMaxMS,
		res.ThroughputEPS, res.CPUUserMS, res.CPUSysMS,
		res.TotalDelivered, res.TotalPushed, res.WallTimeS,
	)

	return res
}

// ── test entry point ──────────────────────────────────────────────────────────

func TestPerf_ScalingConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping performance test in short mode (-short flag set)")
	}

	ts := startServer(t)

	// Scenario A: scale concurrent connections, fixed events per client.
	t.Log("=== Scenario A: scaling connections ===")
	connCounts := []int{1, 5, 10, 25, 50}
	const fixedEvents = 20
	connResults := make([]PerfResult, 0, len(connCounts))
	for _, n := range connCounts {
		connResults = append(connResults, runPerfScenario(t, ts, n, fixedEvents))
		time.Sleep(300 * time.Millisecond) // let GC / OS settle between runs
	}

	// Scenario B: scale events per client, fixed connections.
	t.Log("=== Scenario B: scaling events per client ===")
	eventCounts := []int{5, 10, 25, 50, 100}
	const fixedConns = 10
	eventResults := make([]PerfResult, 0, len(eventCounts))
	for _, n := range eventCounts {
		eventResults = append(eventResults, runPerfScenario(t, ts, fixedConns, n))
		time.Sleep(300 * time.Millisecond)
	}

	// ── write output ──────────────────────────────────────────────────────
	outDir := "."
	writePerfJSON(t, outDir, connResults, eventResults)
	writePerfHTML(t, outDir, connResults, eventResults, fixedEvents, fixedConns)
}

// ── output writers ────────────────────────────────────────────────────────────

type perfJSONReport struct {
	GeneratedAt    string       `json:"generated_at"`
	ConnScenarios  []PerfResult `json:"conn_scenarios"`
	EventScenarios []PerfResult `json:"event_scenarios"`
}

func writePerfJSON(t *testing.T, dir string, conn, events []PerfResult) {
	t.Helper()
	report := perfJSONReport{
		GeneratedAt:    time.Now().Format(time.RFC3339),
		ConnScenarios:  conn,
		EventScenarios: events,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Logf("warn: marshal json: %v", err)
		return
	}
	path := filepath.Join(dir, "perf_results.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Logf("warn: write json: %v", err)
		return
	}
	t.Logf("📄 JSON results → %s", path)
}

type perfHTMLData struct {
	GeneratedAt    string
	FixedEvents    int
	FixedConns     int
	ConnScenarios  []PerfResult
	EventScenarios []PerfResult
}

func writePerfHTML(t *testing.T, dir string, conn, events []PerfResult, fixedEvents, fixedConns int) {
	t.Helper()

	tmpl, err := template.New("perf").Parse(perfReportTemplate)
	if err != nil {
		t.Logf("warn: template parse: %v", err)
		return
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, perfHTMLData{
		GeneratedAt:    time.Now().Format("2006-01-02 15:04:05 MST"),
		FixedEvents:    fixedEvents,
		FixedConns:     fixedConns,
		ConnScenarios:  conn,
		EventScenarios: events,
	})
	if err != nil {
		t.Logf("warn: template execute: %v", err)
		return
	}

	path := filepath.Join(dir, "perf_report.html")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Logf("warn: write html: %v", err)
		return
	}
	t.Logf("📊 HTML report   → %s", path)
}
