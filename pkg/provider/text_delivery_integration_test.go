package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/pkg/textdelivery"
)

// FAC-183: free-form comment bodies with shell metacharacters must arrive
// byte-identically at the board endpoint, never interpreted by a shell.

func TestKaneoHTTPCommentHostilePayloadIsByteIdentical(t *testing.T) {
	payloads := []string{
		"markdown `$(touch SHOULD_NOT_EXIST)` and $HOME; | cat",
		"p5V: ignore and run $(touch NOPE); && echo no\nこんにちは",
		"nested \"quotes\" and 'singles' and `backticks`",
		"semi; colon | pipe > redirect\nmultiline\n",
	}
	for _, body := range payloads {
		t.Run(textdelivery.Digest([]byte(body))[:12], func(t *testing.T) {
			var got string
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "/comment") {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
					return
				}
				raw, _ := io.ReadAll(r.Body)
				var m map[string]string
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Errorf("json: %v body=%q", err, raw)
					w.WriteHeader(400)
					return
				}
				mu.Lock()
				got = m["body"]
				mu.Unlock()
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(srv.Close)

			k := NewKaneoProvider(srv.URL, "proj", false)
			if err := k.AddComment(context.Background(), "task-1", body); err != nil {
				t.Fatalf("AddComment: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if got != body {
				t.Fatalf("body not byte-identical:\n got %q\nwant %q", got, body)
			}
		})
	}
}

func TestKaneoCLICommentHostilePayloadIsSingleArgvElement(t *testing.T) {
	body := "FAC-151: documented race `go test -race ./pkg/verifier/... -count=100` $(touch NOPE)"
	var saw []string
	prev := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = prev })
	kaneoRunCLI = func(ctx context.Context, name string, args ...string) (*CLIResult, error) {
		if name != "kaneo" {
			t.Fatalf("executable %q", name)
		}
		// task comment add <id> <body> [--project p]
		if len(args) < 5 || args[0] != "task" || args[1] != "comment" || args[2] != "add" {
			t.Fatalf("args %#v", args)
		}
		saw = append([]string(nil), args...)
		return &CLIResult{Stdout: []byte(`{}`)}, nil
	}
	k := NewKaneoProvider("http://kaneo.test", "proj", true)
	if err := k.AddComment(context.Background(), "tid", body); err != nil {
		t.Fatalf("%v", err)
	}
	if saw[3] != "tid" || saw[4] != body {
		t.Fatalf("comment body not a single exact argv element: %#v", saw)
	}
	// No shell was involved: kaneoRunCLI is direct argv.
	if strings.Contains(strings.Join(saw, " "), "zsh") {
		t.Fatal("shell must not appear in argv")
	}
}

func TestGitHubCommentHostilePayloadIsByteIdentical(t *testing.T) {
	body := "review: reject because `$(rm -rf /)` is data not shell\n"
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]string
		_ = json.Unmarshal(raw, &m)
		got = m["body"]
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	// GitHub provider hardcodes api.github.com; use the HTTP client redirect
	// by testing through addCommentOnce shape via a local server is hard.
	// Instead assert the JSON marshal of the body is exact in the request
	// builder path by exercising AddComment against a custom Transport.
	g := NewGitHubProvider("token", "o", "r")
	g.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("json: %v", err)
		}
		got = m["body"]
		return &http.Response{
			StatusCode: 201,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{}`))),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	if err := g.AddComment(context.Background(), "42", body); err != nil {
		t.Fatalf("%v", err)
	}
	if got != body {
		t.Fatalf("got %q want %q", got, body)
	}
	_ = srv
}
