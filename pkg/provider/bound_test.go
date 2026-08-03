package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hangHandler never writes a response. It returns when the request context
// is canceled OR after a long safety sleep (so httptest.Close cannot wedge
// forever if a client forgets to cancel).
func hangHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	})
}

// testClient returns an http.Client that dials srv without an independent
// Timeout so per-op context deadlines are the sole bound under test.
func testClient(srv *httptest.Server) *http.Client {
	return &http.Client{Transport: srv.Client().Transport}
}

func closeServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	srv.CloseClientConnections()
	srv.Close()
}

func TestKaneoHTTP_GetTask_DeadlineFires(t *testing.T) {
	server := httptest.NewServer(hangHandler())
	defer closeServer(t, server)

	kp := NewKaneoProvider(server.URL, "proj-1", false)
	kp.Client = testClient(server) // no extra client timeout; context owns the bound
	kp.Deadlines = Deadlines{Get: 80 * time.Millisecond}
	kp.Retry = RetryPolicy{MaxAttempts: 1, Clock: &fakeClock{}}

	start := time.Now()
	_, err := kp.GetTask(context.Background(), "task-1")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected deadline error on hung HTTP body")
	}
	if !IsTimeout(err) {
		t.Fatalf("want IsTimeout, got %T %v", err, err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("hung GET took %v — deadline not enforced", elapsed)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("deadline fired too early: %v", elapsed)
	}
}

func TestKaneoHTTP_GetTask_CancelPropagates(t *testing.T) {
	server := httptest.NewServer(hangHandler())
	defer closeServer(t, server)

	kp := NewKaneoProvider(server.URL, "proj-1", false)
	kp.Client = testClient(server)
	kp.Deadlines = Deadlines{Get: 30 * time.Second}
	kp.Retry = RetryPolicy{MaxAttempts: 1, Clock: &fakeClock{}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := kp.GetTask(ctx, "task-1")
	elapsed := time.Since(start)

	if !IsTimeout(err) {
		t.Fatalf("want cancel timeout, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("cancel ignored: elapsed %v", elapsed)
	}
}

func TestGitHubHTTP_GetTask_DeadlineFires(t *testing.T) {
	server := httptest.NewServer(hangHandler())
	defer server.Close()

	// Point the client at the hang server by overriding via a custom transport
	// that rewrites the host. Simpler: use Client that dials the test server
	// for any host by setting Base... GitHub hardcodes api.github.com.
	// Use a reverse approach: replace Client transport with one that always
	// hits the hang server.
	gp := NewGitHubProvider("tok", "o", "r")
	gp.Client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			// Re-issue against hang server preserving method/context.
			r2 := req.Clone(req.Context())
			r2.URL = mustURL(server.URL + req.URL.Path)
			r2.Host = ""
			r2.RequestURI = ""
			return server.Client().Transport.RoundTrip(r2)
		}),
	}
	gp.Deadlines = Deadlines{Get: 80 * time.Millisecond}
	gp.Retry = RetryPolicy{MaxAttempts: 1, Clock: &fakeClock{}}

	start := time.Now()
	_, err := gp.GetTask(context.Background(), "1")
	elapsed := time.Since(start)
	if !IsTimeout(err) {
		t.Fatalf("want IsTimeout, got %T %v", err, err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("github hung GET took %v", elapsed)
	}
}

func TestKaneoCLI_GetTask_NeverExitsKilled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX")
	}
	dir := t.TempDir()
	stub := `#!/bin/sh
sleep 300
`
	if err := os.WriteFile(filepath.Join(dir, "kaneo"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	kp := NewKaneoProvider("", "p1", true)
	kp.Deadlines = Deadlines{Get: 100 * time.Millisecond}
	kp.Retry = RetryPolicy{MaxAttempts: 1, Clock: &fakeClock{}}

	start := time.Now()
	_, err := kp.GetTask(context.Background(), "t1")
	elapsed := time.Since(start)
	if !IsTimeout(err) {
		t.Fatalf("want timeout, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("CLI hang took %v", elapsed)
	}
}

func TestKaneoHTTP_UpdateStatus_TimeoutReconcilesLanded(t *testing.T) {
	// Mock transport: PATCH waits on ctx (returns deadline error); GET shows
	// the write landed. Avoids real httptest hang connections that wedge Close.
	var patches atomic.Int32
	kp := NewKaneoProvider("http://kaneo.test", "p", false)
	kp.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPatch {
			patches.Add(1)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		body := `{"id":"task-1","ref":"FAC-1","title":"t","status":"in-progress","priority":"low","projectId":"p","labels":[]}`
		return jsonResponse(http.StatusOK, body), nil
	})}
	kp.Deadlines = Deadlines{
		Mutate:   60 * time.Millisecond,
		Readback: 2 * time.Second,
		Get:      2 * time.Second,
	}
	kp.Retry = RetryPolicy{MaxAttempts: 1, Clock: &fakeClock{}}

	err := kp.UpdateStatus(context.Background(), "task-1", StatusInProgress)
	if err != nil {
		t.Fatalf("landed write should reconcile clean: %v", err)
	}
	if patches.Load() != 1 {
		t.Fatalf("double-apply: patches=%d want 1", patches.Load())
	}
}

func TestKaneoHTTP_UpdateStatus_TimeoutNotLandedNoDoubleApply(t *testing.T) {
	var patches atomic.Int32
	kp := NewKaneoProvider("http://kaneo.test", "p", false)
	kp.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPatch {
			patches.Add(1)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		body := `{"id":"task-1","ref":"FAC-1","title":"t","status":"to-do","priority":"low","projectId":"p","labels":[]}`
		return jsonResponse(http.StatusOK, body), nil
	})}
	kp.Deadlines = Deadlines{
		Mutate:   60 * time.Millisecond,
		Readback: 2 * time.Second,
		Get:      2 * time.Second,
	}
	kp.Retry = RetryPolicy{MaxAttempts: 1, Clock: &fakeClock{}}

	err := kp.UpdateStatus(context.Background(), "task-1", StatusInProgress)
	if !IsAmbiguous(err) {
		t.Fatalf("want AmbiguousMutationError (write lost), got %T %v", err, err)
	}
	if patches.Load() != 1 {
		t.Fatalf("must not re-PATCH on lost write: patches=%d", patches.Load())
	}
}

func TestKaneoHTTP_UpdateStatus_CleanPathStillReadback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "task-1", "ref": "FAC-1", "title": "t",
			"status": "done", "priority": "low", "projectId": "p",
			"labels": []string{},
		})
	}))
	defer closeServer(t, server)

	kp := NewKaneoProvider(server.URL, "p", false)
	kp.Client = testClient(server)
	if err := kp.UpdateStatus(context.Background(), "task-1", StatusDone); err != nil {
		t.Fatal(err)
	}
}

// Mutation non-vacuity: if UpdateStatus mapped timeout → nil success, the
// not-landed test would pass incorrectly. Force the class.
func TestKaneoHTTP_UpdateStatus_TimeoutIsNotEmptySuccess(t *testing.T) {
	kp := NewKaneoProvider("http://kaneo.test", "p", false)
	kp.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	kp.Deadlines = Deadlines{Mutate: 40 * time.Millisecond, Get: 40 * time.Millisecond, Readback: 40 * time.Millisecond}
	kp.Retry = RetryPolicy{MaxAttempts: 1, Clock: &fakeClock{}}
	err := kp.UpdateStatus(context.Background(), "task-1", StatusDone)
	if err == nil {
		t.Fatal("timeout must not become empty success")
	}
	if !IsTimeout(err) && !IsAmbiguous(err) {
		t.Fatalf("want timeout/ambiguous, got %v", err)
	}
}

func TestGitHub_UpdateStatus_TimeoutReconcile(t *testing.T) {
	var patches atomic.Int32
	gp := NewGitHubProvider("tok", "o", "r")
	gp.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPatch {
			patches.Add(1)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		// GET issue — closed state means done landed.
		body := `{"number":42,"title":"t","body":"","state":"closed","created_at":"2024-01-01T00:00:00Z","labels":[]}`
		return jsonResponse(http.StatusOK, body), nil
	})}
	gp.Deadlines = Deadlines{Mutate: 60 * time.Millisecond, Get: 2 * time.Second, Readback: 2 * time.Second}
	gp.Retry = RetryPolicy{MaxAttempts: 1, Clock: &fakeClock{}}

	err := gp.UpdateStatus(context.Background(), "42", "closed")
	if err != nil {
		t.Fatalf("reconcile landed: %v", err)
	}
	if patches.Load() != 1 {
		t.Fatalf("patches=%d", patches.Load())
	}
}

// Non-vacuity: replacing WithOpDeadline with context.Background in GetTask
// would make the hang test wait on Client.Timeout (35s+) or forever with
// server.Client(). The short-deadline tests above fail if the bound is removed.
func TestBound_MutationNote(t *testing.T) {
	if errors.Is(nil, context.DeadlineExceeded) {
		t.Fatal("sanity")
	}
	t.Log("guarded by TestKaneoHTTP_GetTask_DeadlineFires and CLI never-exits")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

func jsonResponse(code int, body string) *http.Response {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: code,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
