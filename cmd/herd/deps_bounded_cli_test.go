package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-607: this is the named real-CLI regression for the incident. The Kaneo
// fixture accepts the request and then stops answering, so the command must
// beat an external rc=124 kill with its own UNKNOWN diagnostic.
func TestFAC607DepsCheckTimeoutEmitsStructuredDiagnostics(t *testing.T) {
	binary := buildHerd(t)
	server := newDepsCheckKaneoServer(t, false, true)
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "base"}} {
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	herdDir := filepath.Join(repo, ".herd")
	if err := os.MkdirAll(herdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "" +
		"version: 1\n" +
		"project:\n  name: bounded-read-probe\n" +
		"task_provider:\n" +
		"  type: kaneo\n" +
		"  project_id: proj-deps\n" +
		"  api_url: " + server.URL + "\n"
	if err := os.WriteFile(filepath.Join(herdDir, "herd.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	keyDir := t.TempDir()
	provisionFence(t, binary, repo, keyDir)
	cmd := herdCmd(binary, repo, keyDir, "deps", "check", "FAC-607")
	cmd.Env = append(cmd.Env,
		"HERD_ROOT="+repo,
		"HERD_REPO_ROOT="+repo,
		"HERD_PROVIDER_READ_BUDGET=50ms",
		"KANEO_API_KEY=deps-check-test-key",
		"KANEO_API_URL="+server.URL,
	)
	out, err := cmd.CombinedOutput()
	text := string(out)
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 3 {
		t.Fatalf("exit = %v, want UNKNOWN exit 3\n%s", err, text)
	}
	for _, want := range []string{
		"UNKNOWN FAC-607",
		`"provider":"kaneo"`,
		`"phase":"GetTask FAC-607"`,
		`"applied_deadline":"50ms"`,
		`"last_successful_cache_revision":"none"`,
		`"outcome":"timed-out"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("structured timeout diagnostic omits %q:\n%s", want, text)
		}
	}
}

func TestDepsCheckClassifiesSemanticBlockerSeparatelyFromProviderFailure(t *testing.T) {
	binary := buildHerd(t)
	tests := []struct {
		name            string
		ref             string
		failProviderGet bool
		wantExit        int
		want            []string
		dontWant        []string
	}{
		{
			name:     "open blocker is BLOCKED with its gate result",
			ref:      "FAC-639",
			wantExit: 1,
			want:     []string{"BLOCKED FAC-639", "open_blocker", `"blocked_by"`, "FAC-643"},
			dontWant: []string{"UNKNOWN"},
		},
		{
			name:            "provider read failure stays UNKNOWN",
			ref:             "FAC-639",
			failProviderGet: true,
			wantExit:        3,
			want:            []string{"UNKNOWN FAC-639", "provider read failed"},
			dontWant:        []string{"BLOCKED FAC-639"},
		},
		{
			name:     "prerequisite with no open blocker stays eligible",
			ref:      "FAC-643",
			wantExit: 0,
			want:     []string{`"ref": "FAC-643"`, `"ok": true`},
			dontWant: []string{"BLOCKED", "UNKNOWN"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newDepsCheckKaneoServer(t, tt.failProviderGet, false)
			repo := t.TempDir()
			for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "base"}} {
				c := exec.Command("git", append([]string{"-C", repo}, args...)...)
				c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
					"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
				if out, err := c.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}

			herdDir := filepath.Join(repo, ".herd")
			if err := os.MkdirAll(herdDir, 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := "" +
				"version: 1\n" +
				"project:\n  name: deps-check-classification\n" +
				"task_provider:\n" +
				"  type: kaneo\n" +
				"  project_id: proj-deps\n" +
				"  api_url: " + server.URL + "\n"
			if err := os.WriteFile(filepath.Join(herdDir, "herd.yaml"), []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}

			keyDir := t.TempDir()
			provisionFence(t, binary, repo, keyDir)
			cmd := herdCmd(binary, repo, keyDir, "deps", "check", tt.ref)
			cmd.Env = append(cmd.Env,
				"HERD_ROOT="+repo,
				"HERD_REPO_ROOT="+repo,
				"HERD_PROVIDER_READ_BUDGET=2s",
				"KANEO_API_KEY=deps-check-test-key",
				"KANEO_API_URL="+server.URL,
			)
			out, err := cmd.CombinedOutput()
			text := string(out)
			gotExit := 0
			if err != nil {
				exitErr, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("run error = %T %v\n%s", err, err, text)
				}
				gotExit = exitErr.ExitCode()
			}
			if gotExit != tt.wantExit {
				t.Fatalf("exit = %d, want %d\n%s", gotExit, tt.wantExit, text)
			}
			for _, want := range tt.want {
				if !strings.Contains(text, want) {
					t.Fatalf("output omits %q:\n%s", want, text)
				}
			}
			for _, unwanted := range tt.dontWant {
				if strings.Contains(text, unwanted) {
					t.Fatalf("output contains %q:\n%s", unwanted, text)
				}
			}
		})
	}
}

func newDepsCheckKaneoServer(t *testing.T, failProviderGet, hangProviderGet bool) *httptest.Server {
	t.Helper()
	edge := `{"source_ref":"FAC-643","target_ref":"FAC-639","type":"blocks"}`
	targetDescription := "```herd-deps-v1\n" +
		`{"version":1,"task_ref":"FAC-639","task_id":"fac-639-id","edges":[` + edge + `]}` + "\n```\n"
	blockerDescription := "```herd-deps-v1\n" +
		`{"version":1,"task_ref":"FAC-643","task_id":"fac-643-id","edges":[` + edge + `]}` + "\n```\n"
	tasks := []map[string]any{
		{
			"id": "fac-639-id", "ref": "FAC-639", "title": "Dependent", "status": "to-do",
			"priority": "urgent", "projectId": "proj-deps", "description": targetDescription,
		},
		{
			"id": "fac-643-id", "ref": "FAC-643", "title": "Prerequisite", "status": "to-do",
			"priority": "urgent", "projectId": "proj-deps", "description": blockerDescription,
		},
	}
	relation := []map[string]any{{
		"id": "rel-643-639", "sourceTaskId": "fac-643-id", "targetTaskId": "fac-639-id", "relationType": "blocks",
	}}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/task", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tasks)
	})
	mux.HandleFunc("/api/task/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if hangProviderGet {
			<-r.Context().Done()
			return
		}
		if failProviderGet {
			http.Error(w, `{"error":"provider unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		want := strings.TrimPrefix(r.URL.Path, "/api/task/")
		for _, task := range tasks {
			if task["id"] == want || task["ref"] == want {
				_ = json.NewEncoder(w).Encode(task)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/api/task-relation/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(relation)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// The budget must be configurable, because it only helps if it fires BEFORE the
// operator's own `timeout` wrapper. A hardcoded budget longer than theirs
// reproduces the silence on a slow host.
func TestProviderReadBudgetIsOverridable(t *testing.T) {
	t.Setenv("HERD_PROVIDER_READ_BUDGET", "3s")
	if got := providerReadBudget(); got.String() != "3s" {
		t.Fatalf("budget = %s, want 3s from the environment override", got)
	}

	t.Setenv("HERD_PROVIDER_READ_BUDGET", "not-a-duration")
	if got := providerReadBudget(); got != 20*1000*1000*1000 {
		t.Fatalf("an unparseable override yielded %s; it must fall back to the default, not to zero "+
			"(a zero budget would make every read time out instantly)", got)
	}
}
