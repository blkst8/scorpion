// Package e2e contains end-to-end tests for Scorpion.
//
// Prerequisites (must be running before executing the suite):
//   - Redis on localhost:6379
//
// Run:
//
//	go test -v ./test/e2e/... -timeout 60s
package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"

	appmiddleware "github.com/blkst8/scorpion/internal/app"
	"github.com/blkst8/scorpion/internal/config"
	httpserver "github.com/blkst8/scorpion/internal/http"
	"github.com/blkst8/scorpion/internal/http/handlers"
	applog "github.com/blkst8/scorpion/internal/log"
	"github.com/blkst8/scorpion/internal/metrics"
	"github.com/blkst8/scorpion/internal/ratelimit"
	redisstore "github.com/blkst8/scorpion/internal/repository"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// freePort asks the kernel for a free TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("freePort close: %v", err)
	}
	return port
}

// generateSelfSignedCert writes a self-signed TLS cert + key to dir and
// returns their paths plus the DER-encoded cert pool (for use by clients).
func generateSelfSignedCert(t *testing.T, dir string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generateSelfSignedCert key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "scorpion-e2e"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("generateSelfSignedCert cert: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("generateSelfSignedCert marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")

	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	pool = x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("generateSelfSignedCert: failed to append cert to pool")
	}

	return certFile, keyFile, pool
}

// tlsHTTPClient returns an *http.Client that trusts the given cert pool.
func tlsHTTPClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}
}

// streamingHTTPClient is like tlsHTTPClient but with no global timeout so SSE
// connections are kept open until the caller's context is cancelled.
func streamingHTTPClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			TLSClientConfig:       &tls.Config{RootCAs: pool},
			ResponseHeaderTimeout: 15 * time.Second,
		},
	}
}

// makeBearerToken mints a signed HS256 JWT using token_secret that the
// TokenMiddleware will accept.  sub becomes clientID.
func makeBearerToken(t *testing.T, secret, clientID string) string {
	t.Helper()

	claims := jwt.MapClaims{
		"sub": clientID,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(10 * time.Minute).Unix(),
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("makeBearerToken: %v", err)
	}

	return signed
}

// waitReady polls GET /healthz until it returns 200 or the deadline is hit.
func waitReady(t *testing.T, client *http.Client, base string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/healthz")
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("server did not become ready within %s", timeout)
}

// ── server bootstrap ──────────────────────────────────────────────────────────

type testServer struct {
	srv     *httpserver.Server
	baseURL string
	pool    *x509.CertPool // trusted cert pool shared by all clients
}

func startServer(t *testing.T) *testServer {
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
			ShutdownTimeout: 5 * time.Second,
		},
		SSE: config.SSE{
			PollInterval:      200 * time.Millisecond,
			BatchSize:         100,
			HeartbeatInterval: 2 * time.Second,
			ConnTTL:           60 * time.Second,
			MaxQueueDepth:     1000,
			MaxEventBytes:     65536,
		},
		Auth: config.Auth{
			TokenSecret:  "e2e-token-secret",
			TicketSecret: "e2e-ticket-secret",
			TicketTTL:    5 * time.Minute,
		},
		IP: config.IP{
			Strategy: "remote_addr",
		},
		RateLimit: config.RateLimit{
			TicketRPM:   6000,
			TicketBurst: 500,
		},
		Redis: config.Redis{
			Address: "127.0.0.1:6379",
			DB:      15, // isolated DB – won't pollute other data
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

	rdb, err := appmiddleware.NewClient(cfg.Redis, m)
	if err != nil {
		t.Skipf("skipping e2e – Redis unavailable: %v", err)
	}

	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flush test db: %v", err)
	}

	t.Cleanup(func() {
		_ = rdb.FlushDB(context.Background()).Err()
		_ = rdb.Close()
	})

	ticketStore := redisstore.NewTicketStore(rdb)
	connStore := redisstore.NewConnectionStore(rdb, "e2e-instance", log)
	eventStore := redisstore.NewEventStore(rdb, cfg.SSE.MaxQueueDepth)
	limiter := ratelimit.NewLimiter(rdb, cfg.RateLimit)

	ticketHandler := handlers.NewTicketHandler(cfg, ticketStore, limiter, ipStrategy, log, m)
	sseHandler := handlers.NewSSEHandler(cfg, ticketStore, connStore, eventStore, ipStrategy, log, m)
	eventHandler := handlers.NewEventHandler(eventStore, cfg.SSE, log)
	pollHandler := handlers.NewPollHandler(eventStore, cfg.SSE, log)

	srv := httpserver.NewServer(cfg, log, rdb, ipStrategy, ticketHandler, sseHandler, eventHandler, pollHandler)
	srv.Serve()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	baseURL := fmt.Sprintf("https://localhost:%d", mainPort)
	waitReady(t, tlsHTTPClient(pool), baseURL, 10*time.Second)

	return &testServer{srv: srv, baseURL: baseURL, pool: pool}
}

// ── SSE reader ────────────────────────────────────────────────────────────────

type sseEvent struct {
	ID    string
	Event string
	Data  string
}

// readSSEEvents reads lines from body until ctx is done, collecting parsed SSE
// events and counting heartbeat comment lines.
// When ctx is cancelled the body is closed so scanner.Scan() unblocks
// immediately instead of blocking forever on the next Read call.
func readSSEEvents(ctx context.Context, body io.ReadCloser) (events []sseEvent, heartbeats int) {
	// Unblock the scanner when the deadline fires.
	go func() {
		<-ctx.Done()
		_ = body.Close()
	}()

	scanner := bufio.NewScanner(body)

	var cur sseEvent
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, ": heartbeat"):
			heartbeats++
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if cur.ID != "" || cur.Event != "" || cur.Data != "" {
				events = append(events, cur)
				cur = sseEvent{}
			}
		}
	}
	return
}

// ── test ──────────────────────────────────────────────────────────────────────

func TestE2E_PushAndStream(t *testing.T) {
	ts := startServer(t)

	const (
		clientID  = "user-abc-123"
		numEvents = 5
	)

	// ── 1. push multiple events ───────────────────────────────────────────────
	type pushRequest struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}

	type pushResponse struct {
		ID   string          `json:"id"`
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}

	httpClient := tlsHTTPClient(ts.pool)
	pushedIDs := make([]string, 0, numEvents)

	// Bearer token is needed both for pushing events and for the ticket endpoint.
	bearerToken := makeBearerToken(t, "e2e-token-secret", clientID)

	for i := 1; i <= numEvents; i++ {
		payload := pushRequest{
			Type: "order.updated",
			Data: json.RawMessage(fmt.Sprintf(`{"seq":%d,"status":"processing"}`, i)),
		}

		body, _ := sonic.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/v1/events/%s", ts.baseURL, clientID),
			bytes.NewReader(body),
		)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearerToken)
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("push event %d: %v", i, err)
		}

		if resp.StatusCode != http.StatusCreated {
			bodyBytes, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Fatalf("push event %d: expected 201, got %d, body: %s", i, resp.StatusCode, string(bodyBytes))
		}

		var pr pushResponse
		if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
			_ = resp.Body.Close()
			t.Fatalf("push event %d decode: %v", i, err)
		}
		_ = resp.Body.Close()

		if pr.ID == "" {
			t.Fatalf("push event %d: missing auto-generated id", i)
		}
		if pr.Type != payload.Type {
			t.Fatalf("push event %d: type mismatch: got %q, want %q", i, pr.Type, payload.Type)
		}

		pushedIDs = append(pushedIDs, pr.ID)
	}

	t.Logf("pushed %d events for client %q: %v", numEvents, clientID, pushedIDs)

	// ── 2. obtain a ticket ────────────────────────────────────────────────────

	ticketReq, _ := http.NewRequest(http.MethodPost, ts.baseURL+"/v1/auth/ticket", nil)
	ticketReq.Header.Set("Authorization", "Bearer "+bearerToken)

	ticketResp, err := httpClient.Do(ticketReq)
	if err != nil {
		t.Fatalf("ticket request: %v", err)
	}

	if ticketResp.StatusCode != http.StatusOK {
		_ = ticketResp.Body.Close()
		t.Fatalf("ticket: expected 200, got %d", ticketResp.StatusCode)
	}

	var ticketBody struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(ticketResp.Body).Decode(&ticketBody); err != nil {
		_ = ticketResp.Body.Close()
		t.Fatalf("ticket decode: %v", err)
	}
	_ = ticketResp.Body.Close()

	if ticketBody.Ticket == "" {
		t.Fatal("ticket: empty ticket string")
	}

	t.Logf("ticket obtained (expires_in=%ds)", ticketBody.ExpiresIn)

	// ── 3. open SSE stream ────────────────────────────────────────────────────
	streamCtx, streamCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer streamCancel()

	streamURL := fmt.Sprintf("%s/v1/stream/events?ticket=%s", ts.baseURL, ticketBody.Ticket)
	streamReq, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, streamURL, nil)
	streamReq.Header.Set("Accept", "text/event-stream")
	streamReq.Header.Set("Authorization", "Bearer "+bearerToken)

	streamResp, err := streamingHTTPClient(ts.pool).Do(streamReq)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	if streamResp.StatusCode != http.StatusOK {
		_ = streamResp.Body.Close()
		t.Fatalf("stream: expected 200, got %d", streamResp.StatusCode)
	}

	// ── 4. read events & heartbeats ───────────────────────────────────────────
	readCtx, readCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer readCancel()

	// readSSEEvents closes body when readCtx expires, unblocking the scanner.
	events, heartbeats := readSSEEvents(readCtx, streamResp.Body)

	t.Logf("received %d SSE events, %d heartbeats", len(events), heartbeats)

	// ── 5. assertions ─────────────────────────────────────────────────────────

	if len(events) != numEvents {
		t.Fatalf("expected %d events, got %d", numEvents, len(events))
	}

	receivedIDs := make(map[string]bool, len(events))
	for i, e := range events {
		if e.ID == "" {
			t.Errorf("event[%d]: missing id", i)
		}
		if e.Event != "order.updated" {
			t.Errorf("event[%d]: expected type %q, got %q", i, "order.updated", e.Event)
		}
		if !strings.Contains(e.Data, "processing") {
			t.Errorf("event[%d]: data missing expected payload: %q", i, e.Data)
		}
		receivedIDs[e.ID] = true
	}

	for _, id := range pushedIDs {
		if !receivedIDs[id] {
			t.Errorf("event id %q was pushed but never received in stream", id)
		}
	}

	// HeartbeatInterval=2s, read window=8s → at least 1 heartbeat expected.
	if heartbeats == 0 {
		t.Error("expected at least one SSE heartbeat, got none")
	}
}
