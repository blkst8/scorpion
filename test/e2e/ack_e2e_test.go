// Package e2e – ACK end-to-end tests.
//
// Tests in this file cover the full ACK lifecycle:
//  1. Push events via POST /v1/events/:client_id
//  2. Open SSE stream → receive events
//  3. ACK each event via POST /v1/ack
//  4. Validate responses, duplicate detection, and error codes.
package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// pushEvent sends a single event via POST /v1/events/:clientID.
func pushEvent(t *testing.T, client *http.Client, base, bearer, clientID, evtType, data string) string {
	t.Helper()

	body, _ := sonic.Marshal(map[string]interface{}{
		"type": evtType,
		"data": map[string]string{"msg": data},
	})

	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/v1/events/%s", base, clientID),
		bytes.NewReader(body),
	)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("pushEvent: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("pushEvent: unexpected status %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := sonic.ConfigDefault.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("pushEvent decode: %v", err)
	}

	id, _ := result["id"].(string)
	return id
}

// sendAck sends a POST /v1/ack for the given eventID and returns the response body.
func sendAck(t *testing.T, client *http.Client, base, bearer, clientID, eventID, streamID, status string) (int, map[string]interface{}) {
	t.Helper()

	body, _ := sonic.Marshal(map[string]interface{}{
		"event_id":  eventID,
		"client_id": clientID,
		"stream_id": streamID,
		"acked_at":  time.Now().UTC().Format(time.RFC3339),
		"status":    status,
	})

	req, _ := http.NewRequest(http.MethodPost,
		base+"/v1/ack",
		bytes.NewReader(body),
	)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("sendAck: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	_ = sonic.ConfigDefault.NewDecoder(resp.Body).Decode(&result)
	return resp.StatusCode, result
}

// collectEventsUntil opens an SSE stream and collects events until it sees
// at least wantCount events or the context is cancelled. Returns the collected
// event IDs and types.
func collectEventsUntil(
	t *testing.T,
	ctx context.Context,
	client *http.Client,
	streamURL string,
	bearer string,
	wantCount int,
) []sseEvent {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		t.Fatalf("collectEvents req: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+bearer)

	resp, err := client.Do(req)
	if err != nil && ctx.Err() == nil {
		t.Fatalf("collectEvents: %v", err)
	}
	if resp == nil {
		return nil
	}
	defer resp.Body.Close()

	var (
		events []sseEvent
		cur    sseEvent
	)

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if ctx.Err() != nil {
			break
		}
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if cur.ID != "" {
				events = append(events, cur)
				if len(events) >= wantCount {
					return events
				}
			}
			cur = sseEvent{}
		}
	}
	return events
}

// ── ACK tests ─────────────────────────────────────────────────────────────────

// TestACKFullFlow pushes N events, streams them, ACKs each one, and validates
// each ACK response.
func TestACKFullFlow(t *testing.T) {
	ts := startServer(t)
	const clientID = "ack-client-1"
	const streamID = "orders"
	const numEvents = 5

	bearer := makeBearerToken(t, "e2e-token-secret", clientID)
	plain := tlsHTTPClient(ts.pool)
	streaming := streamingHTTPClient(ts.pool)

	// 1. Push events before opening the stream.
	pushedIDs := make([]string, numEvents)
	for i := 0; i < numEvents; i++ {
		pushedIDs[i] = pushEvent(t, plain, ts.baseURL, bearer, clientID,
			"order.created", fmt.Sprintf("order-%d", i))
	}
	t.Logf("pushed %d events: %v", numEvents, pushedIDs)

	// 2. Get a ticket and open the SSE stream.
	ticket := getTicket(t, plain, ts.baseURL, bearer, clientID)
	streamURL := fmt.Sprintf("%s/v1/stream/events?ticket=%s", ts.baseURL, ticket)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	received := collectEventsUntil(t, ctx, streaming, streamURL, bearer, numEvents)
	if len(received) < numEvents {
		t.Fatalf("expected %d events, got %d", numEvents, len(received))
	}

	// 3. ACK each event and validate the response.
	for _, evt := range received {
		code, body := sendAck(t, plain, ts.baseURL, bearer, clientID, evt.ID, streamID, "received")
		if code != http.StatusOK {
			t.Errorf("ACK event %s: expected 200, got %d — body: %v", evt.ID, code, body)
			continue
		}

		if ackID, ok := body["ack_id"].(string); !ok || ackID == "" {
			t.Errorf("ACK event %s: missing ack_id in response", evt.ID)
		}
		if pub, _ := body["published"].(bool); !pub {
			t.Errorf("ACK event %s: published should be true", evt.ID)
		}
		t.Logf("✓ ACKed event %s → ack_id=%s", evt.ID, body["ack_id"])
	}
}

// TestACKDuplicateRejection verifies that sending the same ACK twice returns 409.
func TestACKDuplicateRejection(t *testing.T) {
	ts := startServer(t)
	const clientID = "ack-client-dedup"
	const streamID = "payments"

	bearer := makeBearerToken(t, "e2e-token-secret", clientID)
	plain := tlsHTTPClient(ts.pool)
	streaming := streamingHTTPClient(ts.pool)

	// Push one event.
	pushEvent(t, plain, ts.baseURL, bearer, clientID, "payment.created", "pay-1")

	// Stream and collect it.
	ticket := getTicket(t, plain, ts.baseURL, bearer, clientID)
	streamURL := fmt.Sprintf("%s/v1/stream/events?ticket=%s", ts.baseURL, ticket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	events := collectEventsUntil(t, ctx, streaming, streamURL, bearer, 1)
	if len(events) == 0 {
		t.Fatal("no events received")
	}
	eventID := events[0].ID

	// First ACK — must succeed.
	code, _ := sendAck(t, plain, ts.baseURL, bearer, clientID, eventID, streamID, "received")
	if code != http.StatusOK {
		t.Fatalf("first ACK: expected 200, got %d", code)
	}

	// Second ACK — must return 409.
	code, body := sendAck(t, plain, ts.baseURL, bearer, clientID, eventID, streamID, "processed")
	if code != http.StatusConflict {
		t.Errorf("duplicate ACK: expected 409, got %d — body: %v", code, body)
	} else {
		t.Logf("✓ duplicate ACK correctly rejected with 409")
	}
}

// TestACKUnknownEvent verifies that ACKing an event that was never delivered
// returns 404.
func TestACKUnknownEvent(t *testing.T) {
	ts := startServer(t)
	const clientID = "ack-client-unknown"

	bearer := makeBearerToken(t, "e2e-token-secret", clientID)
	plain := tlsHTTPClient(ts.pool)

	code, body := sendAck(t, plain, ts.baseURL, bearer, clientID,
		"00000000-0000-0000-0000-000000000000", "unknown-stream", "received")
	if code != http.StatusNotFound {
		t.Errorf("unknown event ACK: expected 404, got %d — body: %v", code, body)
	} else {
		t.Logf("✓ unknown event ACK correctly rejected with 404")
	}
}

// TestACKClientIDMismatch verifies that a client cannot ACK events for another
// client_id.
func TestACKClientIDMismatch(t *testing.T) {
	ts := startServer(t)
	const clientID = "ack-client-owner"
	const attackerID = "ack-client-attacker"

	ownerBearer := makeBearerToken(t, "e2e-token-secret", clientID)
	attackerBearer := makeBearerToken(t, "e2e-token-secret", attackerID)
	plain := tlsHTTPClient(ts.pool)

	// Attacker tries to ACK using owner's client_id in body but attacker's token.
	body, _ := sonic.Marshal(map[string]interface{}{
		"event_id":  "00000000-0000-0000-0000-000000000001",
		"client_id": clientID, // owner's ID — mismatch with JWT sub=attacker
		"stream_id": "test",
		"acked_at":  time.Now().UTC().Format(time.RFC3339),
		"status":    "received",
	})

	req, _ := http.NewRequest(http.MethodPost, ts.baseURL+"/v1/ack", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+attackerBearer)
	req.Header.Set("Content-Type", "application/json")

	_ = ownerBearer // referenced to confirm separate tokens

	resp, err := plain.Do(req)
	if err != nil {
		t.Fatalf("mismatch request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("client_id mismatch: expected 403, got %d", resp.StatusCode)
	} else {
		t.Logf("✓ client_id mismatch correctly rejected with 403")
	}
}

// TestACKMissingFields verifies that requests with missing required fields
// return 400.
func TestACKMissingFields(t *testing.T) {
	ts := startServer(t)
	const clientID = "ack-client-fields"

	bearer := makeBearerToken(t, "e2e-token-secret", clientID)
	plain := tlsHTTPClient(ts.pool)

	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"missing event_id", map[string]interface{}{
			"client_id": clientID, "stream_id": "s", "acked_at": time.Now().Format(time.RFC3339), "status": "received",
		}},
		{"missing client_id", map[string]interface{}{
			"event_id": "some-id", "stream_id": "s", "acked_at": time.Now().Format(time.RFC3339), "status": "received",
		}},
		{"invalid status", map[string]interface{}{
			"event_id": "some-id", "client_id": clientID, "stream_id": "s",
			"acked_at": time.Now().Format(time.RFC3339), "status": "unknown_status",
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := sonic.Marshal(tc.body)
			req, _ := http.NewRequest(http.MethodPost, ts.baseURL+"/v1/ack", bytes.NewReader(b))
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set("Content-Type", "application/json")

			resp, err := plain.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s: expected 400, got %d", tc.name, resp.StatusCode)
			} else {
				t.Logf("✓ %s → 400", tc.name)
			}
		})
	}
}

// TestACKConcurrentClients verifies that multiple clients can independently
// stream and ACK their own events without cross-contamination.
func TestACKConcurrentClients(t *testing.T) {
	ts := startServer(t)
	const numClients = 5
	const eventsPerClient = 3

	var wg sync.WaitGroup
	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			clientID := fmt.Sprintf("ack-concurrent-%d", idx)
			bearer := makeBearerToken(t, "e2e-token-secret", clientID)
			plain := tlsHTTPClient(ts.pool)
			streaming := streamingHTTPClient(ts.pool)

			// Push events.
			for j := 0; j < eventsPerClient; j++ {
				pushEvent(t, plain, ts.baseURL, bearer, clientID, "msg", fmt.Sprintf("m-%d-%d", idx, j))
			}

			// Stream.
			ticket := getTicket(t, plain, ts.baseURL, bearer, clientID)
			streamURL := fmt.Sprintf("%s/v1/stream/events?ticket=%s", ts.baseURL, ticket)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			events := collectEventsUntil(t, ctx, streaming, streamURL, bearer, eventsPerClient)
			if len(events) < eventsPerClient {
				t.Errorf("client %d: expected %d events, got %d", idx, eventsPerClient, len(events))
				return
			}

			// ACK all.
			for _, evt := range events {
				code, body := sendAck(t, plain, ts.baseURL, bearer, clientID, evt.ID, "concurrent-test", "received")
				if code != http.StatusOK {
					t.Errorf("client %d ACK %s: expected 200, got %d — %v", idx, evt.ID, code, body)
				}
			}
			t.Logf("✓ client %d: %d events streamed and ACKed", idx, len(events))
		}(i)
	}
	wg.Wait()
}

// TestReadyzEndpoint checks the /readyz probe returns the expected structure.
func TestReadyzEndpoint(t *testing.T) {
	ts := startServer(t)
	client := tlsHTTPClient(ts.pool)

	resp, err := client.Get(ts.baseURL + "/readyz")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz: expected 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := sonic.ConfigDefault.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("readyz decode: %v", err)
	}

	if status, _ := body["status"].(string); status != "ready" {
		t.Errorf("readyz status: expected 'ready', got %q", status)
	}
	t.Logf("✓ /readyz: %v", body)
}

// ── helper: getTicket ─────────────────────────────────────────────────────────

// getTicket exchanges a Bearer token for a single-use SSE ticket.
func getTicket(t *testing.T, client *http.Client, base, bearer, clientID string) string {
	t.Helper()

	body, _ := sonic.Marshal(map[string]string{"client_id": clientID})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/auth/ticket", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("getTicket: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getTicket: status %d", resp.StatusCode)
	}

	// Use standard json decoder to avoid sonic strict-type issues with mixed
	// ticket response (ticket:string, expires_in:int).
	var result struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("getTicket decode: %v", err)
	}
	if result.Ticket == "" {
		t.Fatalf("getTicket: missing ticket in response")
	}
	return result.Ticket
}
