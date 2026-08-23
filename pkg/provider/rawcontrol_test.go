package provider

import (
	"net/http"
	"testing"
)

// FAC-582: Go's encoding/json refuses an unescaped control byte in a string
// ("invalid character '\n' in string literal"). Card descriptions are free text
// written by many agents, so one raw newline reaching that field would
// fail-close an entire page of tasks and take the queue offline over one byte.
func TestDecodeJSONBytesSurvivesRawNewlineInDescription(t *testing.T) {
	body := []byte("{\"ref\":\"CHA-1\",\"description\":\"## Outcome\nbounded thing\"}")
	var got struct {
		Ref         string `json:"ref"`
		Description string `json:"description"`
	}
	if err := DecodeJSONBytes(http.StatusOK, body, &got); err != nil {
		t.Fatalf("raw newline must not fail the decode: %v", err)
	}
	if got.Ref != "CHA-1" {
		t.Errorf("ref = %q want CHA-1", got.Ref)
	}
	if got.Description != "## Outcome\nbounded thing" {
		t.Errorf("description round-trip = %q", got.Description)
	}
}

// Genuinely malformed JSON must still be refused. The repair pass exists to
// survive one bad byte, not to make the decoder credulous.
func TestDecodeJSONBytesStillRefusesMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"truncated object": "{\"ref\":\"CHA-1\",",
		"unterminated str": "{\"ref\":\"CHA-1\nstill open",
		"not json at all":  "<html>nope</html>",
	} {
		var got map[string]any
		if err := DecodeJSONBytes(http.StatusOK, []byte(body), &got); err == nil {
			t.Errorf("%s: expected refusal, got nil error", name)
		}
	}
}

// An escaped quote must not desynchronise the in-string scan; if it did, the
// repair would treat structural bytes as string content and corrupt the body.
func TestEscapeRawControlsRespectsEscapedQuotes(t *testing.T) {
	body := []byte("{\"a\":\"he said \\\"hi\\\"\",\"b\":\"x\ny\"}")
	var got struct {
		A string `json:"a"`
		B string `json:"b"`
	}
	if err := DecodeJSONBytes(http.StatusOK, body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.A != `he said "hi"` {
		t.Errorf("A = %q", got.A)
	}
	if got.B != "x\ny" {
		t.Errorf("B = %q", got.B)
	}
}

// Well-formed payloads must be returned untouched — no repair, no rewrite.
func TestEscapeRawControlsLeavesCleanJSONAlone(t *testing.T) {
	clean := []byte("{\"a\":\"line1\\nline2\",\"n\":3}")
	out, changed := escapeRawControlsInStrings(clean)
	if changed {
		t.Errorf("clean JSON must not be reported as changed")
	}
	if string(out) != string(clean) {
		t.Errorf("clean JSON rewritten: %q", out)
	}
}
