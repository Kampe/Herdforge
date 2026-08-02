package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestControlServer_StartAndEndpoints(t *testing.T) {
	srv := NewControlServer("127.0.0.1:18899")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("expected server start success, got err: %v", err)
	}
	defer srv.Stop(ctx)

	time.Sleep(100 * time.Millisecond)

	// Query /v1/status
	resp, err := http.Get("http://127.0.0.1:18899/v1/status")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /v1/status, got code %d (err: %v)", resp.StatusCode, err)
	}

	var statusResp ServerStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		t.Fatalf("failed to decode status response: %v", err)
	}
	if statusResp.Status != "healthy" {
		t.Errorf("expected status 'healthy', got %s", statusResp.Status)
	}

	// Query /openapi.json
	openAPIResp, err := http.Get("http://127.0.0.1:18899/openapi.json")
	if err != nil || openAPIResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /openapi.json, got code %d", openAPIResp.StatusCode)
	}
}
