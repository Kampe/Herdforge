package server

import (
	"context"
	"testing"
	"time"
)

func TestServer_Stop_NilHTTPSrv(t *testing.T) {
	srv := NewControlServer("127.0.0.1:18999")
	// Without Start(), httpSrv is nil — Stop should return nil
	err := srv.Stop(context.Background())
	if err != nil {
		t.Errorf("expected nil error when httpSrv is nil, got: %v", err)
	}
}

func TestServer_StartStop(t *testing.T) {
	srv := NewControlServer("127.0.0.1:0")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		if err := srv.Start(ctx); err != nil && err.Error() != "http: Server closed" {
			t.Logf("start returned: %v", err)
		}
	}()
	time.Sleep(200 * time.Millisecond)

	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("expected stop success, got err: %v", err)
	}
}
