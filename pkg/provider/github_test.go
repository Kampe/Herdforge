package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

type customTripper struct {
	targetURL *url.URL
}

func (c *customTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = c.targetURL.Scheme
	req.URL.Host = c.targetURL.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestGitHubProvider_GetTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/testowner/testrepo/issues/42" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"number": 42,
			"title": "GitHub Integration Issue",
			"body": "Issue details",
			"state": "open",
			"created_at": "2026-08-01T20:00:00Z",
			"labels": [{"name": "priority:urgent"}, {"name": "herd-smith"}]
		}`))
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("mock-token", "testowner", "testrepo")
	gp.Client = &http.Client{
		Transport: &customTripper{targetURL: u},
	}

	task, err := gp.GetTask(context.Background(), "42")
	if err != nil {
		t.Fatalf("expected task, got err: %v", err)
	}

	if task.Ref != "#42" || task.Priority != PriorityUrgent {
		t.Errorf("unexpected task output: %+v", task)
	}
}
