//go:build ignore

// push_events is a dev-time helper that mints a JWT for a given client and
// pushes N fake events to the Scorpion event endpoint.
//
// Usage:
//
//	go run scripts/push_events.go [flags]
//
// Flags:
//
//	-url          Base URL of the Scorpion server   (default: https://localhost:8443)
//	-client       Client ID (JWT sub)               (default: user-1)
//	-secret       Token secret                      (default: change-me-real-token-secret)
//	-count        Number of events to push          (default: 10)
//	-interval     Delay between events              (default: 200ms)
//	-type         Event type                        (default: fake.event)
//	-insecure     Skip TLS verification             (default: true)

package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// eventRequest mirrors domain.EventRequest (no ID – server generates it).
type eventRequest struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// fakePayloads is a pool of sample data bodies rotated on each push.
var fakePayloads = []map[string]any{
	{"message": "Hello, Scorpion!", "priority": "low"},
	{"action": "login", "device": "mobile", "os": "android"},
	{"order_id": "ORD-9921", "status": "shipped", "eta": "2026-04-28"},
	{"alert": "cpu_high", "value": 94.2, "host": "node-03"},
	{"score": 1500, "level": 7, "xp_earned": 250},
	{"chat": "Hey there!", "room": "general"},
	{"metric": "latency_p99", "ms": 38},
	{"feature_flag": "dark_mode", "enabled": true},
	{"promo_code": "SPRING26", "discount": 20},
	{"notification": "You have a new follower", "follower": "jane_doe"},
}

func mintToken(clientID, secret string) string {
	claims := jwt.MapClaims{
		"sub": clientID,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(24 * time.Hour).Unix(),
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := t.SignedString([]byte(secret))
	if err != nil {
		log.Fatalf("failed to sign token: %v", err)
	}
	return signed
}

func main() {
	baseURL := flag.String("url", "http://localhost:8443", "Scorpion base URL")
	clientID := flag.String("client", "user-1", "Client ID (JWT sub)")
	secret := flag.String("secret", "change-me-real-token-secret", "JWT token secret")
	count := flag.Int("count", 10, "Number of events to push")
	interval := flag.Duration("interval", 200*time.Millisecond, "Delay between events")
	eventType := flag.String("type", "fake.event", "Event type")
	insecure := flag.Bool("insecure", true, "Skip TLS certificate verification")
	flag.Parse()

	token := mintToken(*clientID, *secret)

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure}, //nolint:gosec // dev helper
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport}

	endpoint := fmt.Sprintf("%s/v1/events/%s", *baseURL, *clientID)

	log.Printf("pushing %d events → %s", *count, endpoint)
	log.Printf("client_id=%s  event_type=%s  interval=%s", *clientID, *eventType, *interval)
	fmt.Println()

	successCount := 0
	for i := 1; i <= *count; i++ {
		payload := fakePayloads[rand.Intn(len(fakePayloads))] //nolint:gosec // dev helper
		payload["seq"] = i
		payload["ts"] = time.Now().UnixMilli()

		dataBytes, err := json.Marshal(payload)
		if err != nil {
			log.Fatalf("failed to marshal data: %v", err)
		}

		body, err := json.Marshal(eventRequest{
			Type: *eventType,
			Data: json.RawMessage(dataBytes),
		})
		if err != nil {
			log.Fatalf("failed to marshal request: %v", err)
		}

		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			log.Fatalf("failed to build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[%d/%d] ✗ request error: %v", i, *count, err)
			time.Sleep(*interval)
			continue
		}
		resp.Body.Close()

		status := "✓"
		if resp.StatusCode >= 300 {
			status = "✗"
		} else {
			successCount++
		}
		log.Printf("[%d/%d] %s  HTTP %d  data=%s", i, *count, status, resp.StatusCode, dataBytes)

		if i < *count {
			time.Sleep(*interval)
		}
	}

	fmt.Println()
	log.Printf("done — %d/%d events pushed successfully", successCount, *count)
}
