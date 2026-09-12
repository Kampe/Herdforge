package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// decodeHerdrTransport draws the line between a TRANSPORT reply and raw pane
// text, and the line is POSITION, not key names.
//
// Contract verified against the installed herdr (0.9.0, `herdr api schema
// --json`, schema_version 1): success_response requires {id, result},
// error_response requires {id, error:{code,message}}. So any reply that starts
// with '{' is herdr speaking and is judged as transport; if it does not decode
// into a usable result, the read FAILED. Both historical regressions are
// pinned here:
//
//   - sniffing decoded AGENT TEXT for an "error" key mislabelled a healthy
//     pane whose own output is {"error":"example"};
//   - requiring a "result"/"ok" marker let a bare {"error":"failure"} and a
//     truncated structured reply fall through to raw text and be classified as
//     a healthy pane.
func TestDecodeHerdrTransportFailsClosedOnStructuredReplies(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		isEnvelope bool
		wantErr    error
		wantResult string
	}{
		// --- success envelopes ---
		{name: "0.9.0 success", raw: `{"id":7,"result":{"text":"done","truncated":false}}`, isEnvelope: true, wantResult: `{"text":"done","truncated":false}`},
		{name: "result without id", raw: `{"result":{"text":"done"}}`, isEnvelope: true, wantResult: `{"text":"done"}`},
		{name: "ok true with result", raw: `{"ok":true,"result":{"agents":[]}}`, isEnvelope: true, wantResult: `{"agents":[]}`},

		// --- error envelopes: every one of these must FAIL CLOSED ---
		{name: "0.9.0 error", raw: `{"id":7,"error":{"code":404,"message":"pane not found"}}`, isEnvelope: true, wantErr: ErrReadTransportEnvelope},
		{name: "bare error object", raw: `{"error":"failure"}`, isEnvelope: true, wantErr: ErrReadTransportEnvelope},
		{name: "error with null result", raw: `{"error":"failure","result":null}`, isEnvelope: true, wantErr: ErrReadTransportEnvelope},
		{name: "explicit ok false", raw: `{"ok":false}`, isEnvelope: true, wantErr: ErrReadTransportEnvelope},
		{name: "ok false with error", raw: `{"ok":false,"error":"pane not found"}`, isEnvelope: true, wantErr: ErrReadTransportEnvelope},
		{name: "success with null result", raw: `{"ok":true,"result":null}`, isEnvelope: true, wantErr: ErrReadTransportEnvelope},
		{name: "id and nothing else", raw: `{"id":7}`, isEnvelope: true, wantErr: ErrReadTransportEnvelope},
		{name: "empty object", raw: `{}`, isEnvelope: true, wantErr: ErrReadTransportEnvelope},
		{name: "top-level agent json", raw: `{"verdict":"PASS","notes":["a","b"]}`, isEnvelope: true, wantErr: ErrReadTransportEnvelope},

		// --- contradictions: an error AND a result in one reply ---
		{name: "contradiction string error", raw: `{"error":"exploded","result":{"text":"done"}}`, isEnvelope: true, wantErr: ErrReadTransportContradiction},
		{name: "contradiction with agents", raw: `{"ok":false,"error":"partial roster","result":{"agents":[{"name":"a"}]}}`, isEnvelope: true, wantErr: ErrReadTransportContradiction},
		{name: "contradiction ok true", raw: `{"id":1,"error":{"code":1},"result":{"text":"x"}}`, isEnvelope: true, wantErr: ErrReadTransportContradiction},

		// A scalar result decodes at the transport layer; the body layer is
		// where it is refused (see TestPaneReadContextRefusesUnsupportedResultBodies).
		{name: "scalar result", raw: `{"result":5}`, isEnvelope: true, wantResult: `5`},

		// --- malformed structured replies: refused, never demoted to text ---
		{name: "truncated object", raw: `{"result":{"agents":[`, isEnvelope: true, wantErr: ErrReadTransportMalformed},
		{name: "unterminated string", raw: `{"result":{"text":"half`, isEnvelope: true, wantErr: ErrReadTransportMalformed},
		{name: "not json after brace", raw: `{not json at all`, isEnvelope: true, wantErr: ErrReadTransportMalformed},

		// --- never claimed to be structured: legacy raw text ---
		{name: "plain text", raw: "build passed\nall tests green"},
		{name: "text that mentions error", raw: `error: the build failed`},
		{name: "empty", raw: ""},
		{name: "whitespace", raw: "   \n\t "},
		{name: "json array", raw: `[{"result":{"text":"x"}}]`},
		{name: "shell diagnostic", raw: "herdr: command not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, isEnvelope, err := decodeHerdrTransport(tc.raw)
			if isEnvelope != tc.isEnvelope {
				t.Fatalf("isEnvelope = %v, want %v", isEnvelope, tc.isEnvelope)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(result) != tc.wantResult {
				t.Fatalf("result = %q, want %q", result, tc.wantResult)
			}
		})
	}
}

// The direction that must NOT regress: an agent whose own output is an error
// object, delivered inside a valid result.text, stays ordinary pane content.
func TestPaneReadContextPreservesLiteralErrorJSONInsideAResult(t *testing.T) {
	const agentOutput = `{"error":"example from the agent's own output"}`
	body, err := json.Marshal(map[string]any{"result": map[string]any{"text": agentOutput}})
	if err != nil {
		t.Fatal(err)
	}
	restore := SetRunHerdrForTest(func(...string) (string, error) { return string(body), nil })
	defer restore()
	obs, err := PaneReadContext(context.Background(), "wT:p1", 10)
	if err != nil {
		t.Fatalf("literal agent JSON raised a transport failure: %v", err)
	}
	if obs.Text != agentOutput {
		t.Fatalf("text = %q, want the agent's own output verbatim", obs.Text)
	}
	if obs.Unverified {
		t.Fatal("a valid envelope was reported unverified")
	}
}

// Whitespace around an envelope must not change its classification.
func TestDecodeHerdrTransportIgnoresSurroundingWhitespace(t *testing.T) {
	if _, isEnvelope, err := decodeHerdrTransport("  \n" + `{"ok":false,"error":"boom"}` + "\n  "); !isEnvelope || !errors.Is(err, ErrReadTransportEnvelope) {
		t.Fatalf("padded envelope = (%v, %v)", isEnvelope, err)
	}
}

// Result bodies are accepted in the two shapes this repository knows, and
// herdr's own truncation flag is carried rather than dropped.
func TestPaneReadContextAcceptsKnownResultBodies(t *testing.T) {
	for _, tc := range []struct {
		name          string
		out           string
		wantText      string
		wantTruncated bool
	}{
		{"0.9.0 pane read", `{"id":1,"result":{"pane_id":"wT:p1","text":"FAIL: 2 tests\n","truncated":false}}`, "FAIL: 2 tests\n", false},
		{"herdr truncated the tail", `{"result":{"text":"...tail","truncated":true}}`, "...tail", true},
		{"explicitly empty pane", `{"result":{"text":""}}`, "", false},
		{"legacy lines array", `{"result":{"lines":["a","b"]}}`, "a\nb", false},
		{"legacy empty lines", `{"result":{"lines":[]}}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := SetRunHerdrForTest(func(...string) (string, error) { return tc.out, nil })
			defer restore()
			obs, err := PaneReadContext(context.Background(), "wT:p1", 10)
			if err != nil {
				t.Fatalf("PaneReadContext: %v", err)
			}
			if obs.Text != tc.wantText {
				t.Fatalf("text = %q, want %q", obs.Text, tc.wantText)
			}
			if obs.Truncated != tc.wantTruncated {
				t.Fatalf("truncated = %v, want %v", obs.Truncated, tc.wantTruncated)
			}
			if obs.Unverified {
				t.Fatal("a valid envelope was reported unverified")
			}
		})
	}
}

// An unrecognised result body is an UNREAD pane, not an empty one.
func TestPaneReadContextRefusesUnsupportedResultBodies(t *testing.T) {
	for _, out := range []string{
		`{"result":{"payload":1}}`,
		`{"result":{}}`,
		`{"result":[1,2,3]}`,
		`{"result":"a bare string"}`,
		`{"result":42}`,
	} {
		t.Run(out, func(t *testing.T) {
			restore := SetRunHerdrForTest(func(...string) (string, error) { return out, nil })
			defer restore()
			obs, err := PaneReadContext(context.Background(), "wT:p1", 10)
			if !errors.Is(err, ErrReadTransportUnsupportedResult) {
				t.Fatalf("err = %v, want ErrReadTransportUnsupportedResult (obs=%+v)", err, obs)
			}
			if obs.Text != "" {
				t.Fatalf("an unsupported body was returned as pane text: %q", obs.Text)
			}
		})
	}
}

// Legacy raw text is still accepted, but it is EXPLICITLY unverified: nothing
// in it proved the read succeeded, and the caller is told so.
func TestPaneReadContextMarksLegacyRawTextUnverified(t *testing.T) {
	restore := SetRunHerdrForTest(func(...string) (string, error) { return "PASS: 12 tests\n", nil })
	defer restore()
	obs, err := PaneReadContext(context.Background(), "wT:p1", 10)
	if err != nil {
		t.Fatalf("raw text: %v", err)
	}
	if obs.Text != "PASS: 12 tests\n" {
		t.Fatalf("raw text was not preserved: %q", obs.Text)
	}
	if !obs.Unverified {
		t.Fatal("raw text was accepted as a verified observation; a healthy classification would be silent")
	}
}

// Every structured failure shape surfaces as a typed error, so a caller can
// tell "the tool broke" from "the agent said this".
func TestPaneReadContextRaisesTransportFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want error
	}{
		{"error envelope", `{"id":1,"error":{"code":404,"message":"no such pane"}}`, ErrReadTransportEnvelope},
		{"bare error object", `{"error":"failure"}`, ErrReadTransportEnvelope},
		{"error plus null result", `{"error":"failure","result":null}`, ErrReadTransportEnvelope},
		{"ok false", `{"ok":false}`, ErrReadTransportEnvelope},
		{"contradiction", `{"error":"partial","result":{"text":"half"}}`, ErrReadTransportContradiction},
		{"malformed", `{"result":{"text":"half`, ErrReadTransportMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := SetRunHerdrForTest(func(...string) (string, error) { return tc.out, nil })
			defer restore()
			obs, err := PaneReadContext(context.Background(), "wT:p1", 10)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if obs.Text != "" {
				t.Fatalf("a failed read returned text: %q", obs.Text)
			}
		})
	}
}

// The roster read fails closed on the same shapes. A broken roster presenting
// as an empty fleet is the worst of these failures: "nothing needs attention"
// and "I could not look" are opposite operational facts.
func TestAgentListContextFailsClosedOnBrokenRosters(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
	}{
		{"bare error object", `{"error":"herdr is not running"}`},
		{"0.9.0 error", `{"id":1,"error":{"code":500,"message":"daemon down"}}`},
		{"error plus result", `{"error":"partial","result":{"agents":[{"name":"a"}]}}`},
		{"truncated", `{"result":{"agents":[`},
		{"raw text", "herdr: command not found"},
		{"null agents", `{"result":{"agents":null}}`},
		{"id only", `{"id":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := SetRunHerdrForTest(func(...string) (string, error) { return tc.out, nil })
			defer restore()
			agents, err := AgentListContext(context.Background())
			if err == nil {
				t.Fatalf("a broken roster succeeded with %d agents", len(agents))
			}
			if agents != nil {
				t.Fatalf("a failed roster read returned %d agents", len(agents))
			}
		})
	}
}

// cappedWriter accepts at most limit bytes and fires its stop hook once.
func TestCappedWriterBoundsAndStopsOnce(t *testing.T) {
	var stops int
	w := &cappedWriter{limit: 8, onOver: func() { stops++ }}
	for i := 0; i < 5; i++ {
		if n, err := w.Write([]byte("abcdef")); n != 6 || err != nil {
			t.Fatalf("Write = (%d, %v); a capped writer must never error the copy", n, err)
		}
	}
	if got := len(w.String()); got != 8 {
		t.Fatalf("buffered %d bytes past a limit of 8", got)
	}
	if !w.overflowed() {
		t.Fatal("overflow not recorded")
	}
	if stops != 1 {
		t.Fatalf("stop hook fired %d times, want exactly 1", stops)
	}
}

// installReadFake writes a fake herdr and points the package at it. The
// hermeticity guard is set too, so a resolution bug cannot fall through to the
// operator's live fleet: with NoLiveEnv set and no override, binaryPath fails.
func installReadFake(t *testing.T, script string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "herdr")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(NoLiveEnv, "1")
	t.Setenv(BinaryEnv, bin)
	if !IsAvailable() {
		t.Fatalf("fake herdr at %s is not resolvable", bin)
	}
	resolved, err := binaryPath()
	if err != nil || resolved != bin {
		t.Fatalf("binaryPath = (%q, %v), want the fake", resolved, err)
	}
	return bin
}

// The live-fleet guard itself: without an override, a read refuses rather than
// reaching the operator's real herdr.
func TestRunHerdrReadContextRefusesTheLiveFleet(t *testing.T) {
	t.Setenv(NoLiveEnv, "1")
	t.Setenv(BinaryEnv, "")
	if _, err := runHerdrReadContextReal(context.Background(), DefaultReadTransportLimit, "agent", "list"); err == nil {
		t.Fatal("a read without an override did not refuse the live fleet")
	}
	if IsAvailable() {
		t.Fatal("IsAvailable reported a reachable fleet under the hermeticity guard")
	}
}

// A child that never stops producing output is stopped at the byte bound and
// refused. Silently truncating it would hand the caller a fragment that looks
// like a complete answer.
func TestRunHerdrReadContextStopsInfiniteOutputAtTheBound(t *testing.T) {
	installReadFake(t, "#!/bin/sh\nwhile :; do printf 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'; done\n")
	start := time.Now()
	out, err := runHerdrReadContextReal(context.Background(), 64<<10, "pane", "read", "wT:p1")
	if !errors.Is(err, ErrReadTransportOversized) {
		t.Fatalf("err = %v, want ErrReadTransportOversized", err)
	}
	if out != "" {
		t.Fatalf("oversized output was returned as data (%d bytes)", len(out))
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("bounded read took %s; the child was not stopped promptly", elapsed)
	}
}

// A child that hangs is stopped by the caller's deadline, and the read returns
// promptly rather than leaving a detached goroutine behind.
func TestRunHerdrReadContextCancelsAHungChild(t *testing.T) {
	installReadFake(t, "#!/bin/sh\nsleep 600\n")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := runHerdrReadContextReal(ctx, DefaultReadTransportLimit, "pane", "read", "wT:p1"); err == nil {
		t.Fatal("a hung read returned success")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("cancelled read returned after %s", elapsed)
	}
	if ctx.Err() == nil {
		t.Fatal("the deadline did not fire")
	}
}

// A nonzero exit carries the child's stderr, which is the operator's only clue
// about why the read failed.
func TestRunHerdrReadContextKeepsStderrAndExitStatus(t *testing.T) {
	installReadFake(t, "#!/bin/sh\nprintf 'pane wT:p1 is gone\\n' >&2\nexit 3\n")
	_, err := runHerdrReadContextReal(context.Background(), DefaultReadTransportLimit, "pane", "read", "wT:p1")
	if err == nil {
		t.Fatal("a nonzero exit was reported as success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pane wT:p1 is gone") {
		t.Fatalf("stderr was dropped: %v", err)
	}
	if !strings.Contains(msg, "exit status 3") {
		t.Fatalf("exit status was dropped: %v", err)
	}
}

// A healthy fake round-trips through the real subprocess path.
func TestRunHerdrReadContextReadsAHealthyChild(t *testing.T) {
	installReadFake(t, "#!/bin/sh\nprintf '{\"result\":{\"text\":\"green\"}}\\n'\n")
	out, err := runHerdrReadContextReal(context.Background(), DefaultReadTransportLimit, "pane", "read", "wT:p1")
	if err != nil {
		t.Fatalf("healthy read: %v", err)
	}
	if !strings.Contains(out, `"green"`) {
		t.Fatalf("stdout = %q", out)
	}
}
