package e2e_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"testing"
	"time"
)

func TestDebugServer(t *testing.T) {
	ts := startServer(t)
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: ts.pool},
		},
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(ts.baseURL + "/healthz")
		if err != nil {
			t.Logf("error: %v", err)
		} else {
			t.Logf("status: %d", resp.StatusCode)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ts.srv.Shutdown(ctx)
}
