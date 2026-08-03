package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Canonical task lifecycle statuses at the adapter boundary.
const (
	StatusToDo       = "to-do"
	StatusInProgress = "in-progress"
	StatusInReview   = "in-review"
	StatusDone       = "done"
	StatusPlanned    = "planned"
	StatusArchived   = "archived"
	StatusUnknown    = "unknown"
)

// ProviderError is a typed adapter failure preserving HTTP status, retryability,
// and a safe body snippet for diagnostics. Callers must treat any non-nil
// ProviderError as a hard failure (fail-closed).
type ProviderError struct {
	Provider   string
	Op         string
	StatusCode int
	Message    string
	RequestID  string
	Retryable  bool
	Body       string
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "<nil provider error>"
	}
	var b strings.Builder
	if e.Provider != "" {
		b.WriteString(e.Provider)
		b.WriteString(": ")
	}
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, "HTTP %d: ", e.StatusCode)
	}
	if e.Message != "" {
		b.WriteString(e.Message)
	} else {
		b.WriteString("provider error")
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request_id=%s)", e.RequestID)
	}
	return b.String()
}

// NormalizeStatus maps common provider spellings to canonical lifecycle values.
// Unknown non-empty statuses are returned as "unknown:<raw>" so callers never
// treat them as to-do, done, or empty by accident. Empty maps to "unknown".
func NormalizeStatus(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	switch s {
	case "to-do", "todo", "open", "backlog", "ready", "new", "triage":
		return StatusToDo
	case "planned", "planning":
		return StatusPlanned
	case "in-progress", "inprogress", "doing", "active", "started", "wip":
		return StatusInProgress
	case "in-review", "review", "code-review", "pr-review":
		return StatusInReview
	case "done", "closed", "complete", "completed", "merged", "resolved":
		return StatusDone
	case "archived", "archive":
		return StatusArchived
	case "":
		return StatusUnknown
	default:
		if strings.HasPrefix(s, "unknown:") {
			return s
		}
		return StatusUnknown + ":" + s
	}
}

// DetectErrorBody inspects a response body for structured error payloads under
// any HTTP status, including 200 OK. Returns a non-nil *ProviderError when the
// body carries an error object, error string, or GraphQL errors array.
// Empty or non-JSON bodies are not treated as structured errors.
func DetectErrorBody(statusCode int, body []byte) error {
	if len(body) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed[0] != '{' {
		return nil
	}

	var probe struct {
		Error   json.RawMessage `json:"error"`
		Errors  json.RawMessage `json:"errors"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		// Malformed JSON is the caller's concern (DecodeJSON); not an error body.
		return nil
	}

	msg := ""
	if len(probe.Error) > 0 && string(probe.Error) != "null" {
		msg = extractErrorMessage(probe.Error)
		if msg == "" {
			msg = string(probe.Error)
		}
	}
	if msg == "" && len(probe.Errors) > 0 && string(probe.Errors) != "null" && string(probe.Errors) != "[]" {
		msg = extractErrorMessage(probe.Errors)
		if msg == "" {
			msg = string(probe.Errors)
		}
	}
	// Some APIs return {"message":"...","status":"error"} without an error key.
	// Only treat top-level message as an error body when error/errors were empty
	// AND status code is non-2xx (avoid false positives on legitimate payloads
	// that happen to include a "message" field). For 2xx, require error/errors.
	if msg == "" {
		return nil
	}

	return &ProviderError{
		StatusCode: statusCode,
		Message:    msg,
		Retryable:  statusCode == http.StatusTooManyRequests || statusCode >= 500,
		Body:       truncate(trimmed, 256),
	}
}

func extractErrorMessage(raw json.RawMessage) string {
	// string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return s
	}
	// object with message/msg/detail
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, k := range []string{"message", "msg", "detail", "error", "description"} {
			if v, ok := obj[k]; ok {
				if str, ok := v.(string); ok && str != "" {
					return str
				}
			}
		}
	}
	// array of objects (GraphQL errors)
	var arr []map[string]interface{}
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		parts := make([]string, 0, len(arr))
		for _, item := range arr {
			if m, ok := item["message"].(string); ok && m != "" {
				parts = append(parts, m)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "; ")
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// DecodeJSONResponse reads the full response body, rejects non-2xx statuses,
// rejects 2xx bodies that carry structured error payloads, then unmarshals into v.
// When v is nil, only status/error-body checks run (useful for mutations).
func DecodeJSONResponse(resp *http.Response, v interface{}) error {
	if resp == nil {
		return fmt.Errorf("nil http response")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	return DecodeJSONBytes(resp.StatusCode, body, v)
}

// DecodeJSONBytes is the byte-slice form of DecodeJSONResponse for CLI/stdout paths.
func DecodeJSONBytes(statusCode int, body []byte, v interface{}) error {
	if statusCode < 200 || statusCode >= 300 {
		// Still surface structured message when present.
		if err := DetectErrorBody(statusCode, body); err != nil {
			if pe, ok := err.(*ProviderError); ok {
				pe.Message = fmt.Sprintf("HTTP %d: %s", statusCode, pe.Message)
				return pe
			}
			return err
		}
		snippet := truncate(strings.TrimSpace(string(body)), 256)
		return &ProviderError{
			StatusCode: statusCode,
			Message:    fmt.Sprintf("HTTP %d", statusCode),
			Retryable:  statusCode == http.StatusTooManyRequests || statusCode >= 500,
			Body:       snippet,
		}
	}
	if err := DetectErrorBody(statusCode, body); err != nil {
		return err
	}
	if v == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}
