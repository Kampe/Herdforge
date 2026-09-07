package attention

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestAttentionPartialOutputRetainsCandidateIdentity(t *testing.T) {
	item, _ := ClassifyCandidate(readyCandidateFixture())
	r := Result{Total: 2, Candidates: []CandidateItem{item}, CandidateError: "another candidate timed out"}
	var text bytes.Buffer
	if err := WriteResult(&text, r, false, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{item.SHA, "PR=42", item.URL, "Build", "SUCCESS", "https://example.test/check/1", "UNKNOWN"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("partial result lost %q: %s", want, text.String())
		}
	}
	var quiet bytes.Buffer
	if err := WriteResult(&quiet, r, false, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(quiet.String(), "PR=42") || !strings.Contains(quiet.String(), "1 candidate(s)") {
		t.Fatalf("quiet result: %s", quiet.String())
	}
	var encoded bytes.Buffer
	if err := WriteResult(&encoded, r, true, false); err != nil {
		t.Fatal(err)
	}
	var result struct {
		State      string          `json:"state"`
		Candidates []CandidateItem `json:"candidates"`
	}
	if err := json.Unmarshal(encoded.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.State != "UNKNOWN" || len(result.Candidates) != 1 || result.Candidates[0].SHA != item.SHA {
		t.Fatalf("partial JSON: %s", encoded.String())
	}
}

type attentionFailingWriter struct{}

func (attentionFailingWriter) Write([]byte) (int, error) { return 0, errors.New("closed output") }
func TestAttentionOutputPropagatesWriteFailure(t *testing.T) {
	if err := WriteResult(attentionFailingWriter{}, Result{}, false, false); err == nil {
		t.Fatal("output failure was swallowed")
	}
}
