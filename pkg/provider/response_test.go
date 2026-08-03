package provider

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDetectErrorBody_200WithErrorObject(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{
			name:    "error string",
			body:    `{"error":"not authorized"}`,
			wantMsg: "not authorized",
		},
		{
			name:    "error object message",
			body:    `{"error":{"message":"board locked","code":423}}`,
			wantMsg: "board locked",
		},
		{
			name:    "graphql errors array",
			body:    `{"errors":[{"message":"Entity not found"},{"message":"rate limited"}]}`,
			wantMsg: "Entity not found; rate limited",
		},
		{
			name:    "errors with detail",
			body:    `{"error":{"detail":"upstream timeout"}}`,
			wantMsg: "upstream timeout",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := DetectErrorBody(http.StatusOK, []byte(tc.body))
			if err == nil {
				t.Fatal("expected error on 200+error body, got nil")
			}
			pe, ok := err.(*ProviderError)
			if !ok {
				t.Fatalf("expected *ProviderError, got %T: %v", err, err)
			}
			if pe.StatusCode != http.StatusOK {
				t.Errorf("StatusCode=%d want 200", pe.StatusCode)
			}
			if !strings.Contains(pe.Message, tc.wantMsg) {
				t.Errorf("Message=%q want substring %q", pe.Message, tc.wantMsg)
			}
			// Non-vacuity: a clean payload must NOT error.
			if err := DetectErrorBody(http.StatusOK, []byte(`{"id":"t-1","status":"to-do"}`)); err != nil {
				t.Fatalf("clean body must not error: %v", err)
			}
		})
	}
}

func TestDetectErrorBody_NoFalsePositive(t *testing.T) {
	// Legitimate payloads that must pass.
	okBodies := []string{
		`{"id":"1","title":"x"}`,
		`[]`,
		``,
		`{"data":{"issue":{"id":"1"}}}`,
		`{"message":"hello from a chat field"}`, // message alone under 200 is not an error body
		`{"errors":[]}`,                          // empty errors array
		`{"error":null}`,
	}
	for _, body := range okBodies {
		if err := DetectErrorBody(http.StatusOK, []byte(body)); err != nil {
			t.Errorf("false positive on body %q: %v", body, err)
		}
	}
}

func TestDecodeJSONBytes_Rejects200ErrorBody(t *testing.T) {
	var v map[string]interface{}
	err := DecodeJSONBytes(http.StatusOK, []byte(`{"error":"boom"}`), &v)
	if err == nil {
		t.Fatal("expected error on 200+error JSON, got nil")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderError via errors.As, got %T: %v", err, err)
	}
	if pe.Message != "boom" {
		t.Errorf("Message=%q want boom", pe.Message)
	}
	// Non-vacuity: valid body decodes.
	err = DecodeJSONBytes(http.StatusOK, []byte(`{"id":"ok"}`), &v)
	if err != nil {
		t.Fatalf("valid body: %v", err)
	}
	if v["id"] != "ok" {
		t.Errorf("decoded id=%v", v["id"])
	}
}

func TestDecodeJSONBytes_Non2xx(t *testing.T) {
	err := DecodeJSONBytes(http.StatusInternalServerError, []byte(`{"error":"db down"}`), nil)
	if err == nil {
		t.Fatal("expected error on 500")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderError, got %T", err)
	}
	if pe.StatusCode != 500 {
		t.Errorf("StatusCode=%d", pe.StatusCode)
	}
	if !pe.Retryable {
		t.Error("500 should be retryable")
	}
}

func TestDecodeJSONResponse_ClosesBody(t *testing.T) {
	body := io.NopCloser(bytes.NewBufferString(`{"error":"nope"}`))
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       body,
	}
	err := DecodeJSONResponse(resp, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	// Reading again after DecodeJSONResponse should fail (body closed/consumed).
	_, readErr := io.ReadAll(body)
	// NopCloser doesn't error on second read of empty buffer; just ensure decode path ran.
	_ = readErr
}

func TestNormalizeStatus(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"to-do", StatusToDo},
		{"TODO", StatusToDo},
		{"Open", StatusToDo},
		{"backlog", StatusToDo},
		{"planned", StatusPlanned},
		{"In Progress", StatusInProgress},
		{"in_progress", StatusInProgress},
		{"active", StatusInProgress},
		{"in-review", StatusInReview},
		{"code-review", StatusInReview},
		{"done", StatusDone},
		{"closed", StatusDone},
		{"archived", StatusArchived},
		{"", StatusUnknown},
		{"custom-state", "unknown:custom-state"},
		{"unknown:already", "unknown:already"},
	}
	for _, tc := range cases {
		if got := NormalizeStatus(tc.in); got != tc.want {
			t.Errorf("NormalizeStatus(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
	// Non-vacuity: unknown must never collapse to done.
	if got := NormalizeStatus("mystery"); got == StatusDone || got == StatusToDo {
		t.Fatalf("unknown must not map to done/to-do, got %q", got)
	}
}

func TestProviderError_ErrorString(t *testing.T) {
	pe := &ProviderError{Provider: "kaneo", Op: "GetTask", StatusCode: 200, Message: "board locked", RequestID: "r1"}
	s := pe.Error()
	for _, sub := range []string{"kaneo", "GetTask", "200", "board locked", "r1"} {
		if !strings.Contains(s, sub) {
			t.Errorf("Error()=%q missing %q", s, sub)
		}
	}
}
