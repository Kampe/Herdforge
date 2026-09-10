package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFenceBrokerClientLookupOpFailClosed pins the FAC-785 readback helper's
// contract: GET-only, 404=unknown (nil,nil), 2xx error bodies and malformed
// bodies are hard errors, ambiguous receipts are preserved as ambiguous
// (never read as applied), and path-altering op ids are refused.
func TestFenceBrokerClientLookupOpFailClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("applied receipt", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("non-GET request: %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"applied":true,"ambiguous":false,"op_id":"aa","task_id":"t1","fence_token":3,"expected_status":"done","revision":"7"}`))
		}))
		defer srv.Close()
		c := &FenceBrokerClient{BaseURL: srv.URL, Token: "token-0123456789abcdef"}
		rc, err := c.LookupOp(ctx, "aa")
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if rc == nil || !rc.Applied || rc.Ambiguous || rc.TaskID != "t1" || rc.ExpectedStatus != "done" {
			t.Fatalf("unexpected receipt: %+v", rc)
		}
	})

	t.Run("ambiguous receipt stays ambiguous", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"applied":false,"ambiguous":true,"op_id":"aa","task_id":"t1","expected_status":"done"}`))
		}))
		defer srv.Close()
		c := &FenceBrokerClient{BaseURL: srv.URL, Token: "token-0123456789abcdef"}
		rc, err := c.LookupOp(ctx, "aa")
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if rc == nil || rc.Applied || !rc.Ambiguous {
			t.Fatalf("ambiguous receipt must never read as applied: %+v", rc)
		}
	})

	t.Run("404 is unknown not error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"applied": false}`))
		}))
		defer srv.Close()
		c := &FenceBrokerClient{BaseURL: srv.URL, Token: "token-0123456789abcdef"}
		rc, err := c.LookupOp(ctx, "aa")
		if err != nil || rc != nil {
			t.Fatalf("404 must be (nil,nil), got (%+v, %v)", rc, err)
		}
	})

	t.Run("200 error body is hard error", func(t *testing.T) {
		for _, errBody := range []string{
			`{"error":"ledger mismatch"}`,
			`{"error":500}`,
			`{"error":true}`,
			`{"error":{"code":500,"message":"ledger mismatch"}}`,
			`{"error":["ledger mismatch"]}`,
		} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(errBody))
			}))
			c := &FenceBrokerClient{BaseURL: srv.URL, Token: "token-0123456789abcdef"}
			if _, err := c.LookupOp(ctx, "aa"); err == nil {
				srv.Close()
				t.Fatalf("HTTP 200 error body %s must be a hard error", errBody)
			}
			srv.Close()
		}
	})

	t.Run("receipt with foreign or empty op_id is rejected", func(t *testing.T) {
		for _, body := range []string{
			`{"applied":true,"ambiguous":false,"op_id":"bb","task_id":"t1","fence_token":3}`,
			`{"applied":true,"ambiguous":false,"op_id":"","task_id":"t1","fence_token":3}`,
			`{"applied":true,"ambiguous":false,"task_id":"t1","fence_token":3}`,
		} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			c := &FenceBrokerClient{BaseURL: srv.URL, Token: "token-0123456789abcdef"}
			if _, err := c.LookupOp(ctx, "aa"); err == nil {
				srv.Close()
				t.Fatalf("receipt %s must be rejected for op aa", body)
			}
			srv.Close()
		}
	})

	t.Run("applied receipt with missing task_id is rejected", func(t *testing.T) {
		for _, body := range []string{
			`{"applied":true,"ambiguous":false,"op_id":"aa","task_id":"","fence_token":3}`,
			`{"applied":true,"ambiguous":false,"op_id":"aa","fence_token":3}`,
		} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			c := &FenceBrokerClient{BaseURL: srv.URL, Token: "token-0123456789abcdef"}
			if _, err := c.LookupOp(ctx, "aa"); err == nil {
				srv.Close()
				t.Fatalf("applied receipt %s must be rejected for missing task_id", body)
			}
			srv.Close()
		}
	})

	t.Run("malformed body is hard error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`not-json`))
		}))
		defer srv.Close()
		c := &FenceBrokerClient{BaseURL: srv.URL, Token: "token-0123456789abcdef"}
		if _, err := c.LookupOp(ctx, "aa"); err == nil {
			t.Fatal("malformed body must be a hard error")
		}
	})

	t.Run("path-altering op id refused", func(t *testing.T) {
		c := &FenceBrokerClient{BaseURL: "http://127.0.0.1:1", Token: "token-0123456789abcdef"}
		for _, bad := range []string{"../../admin", "aa/bb", "aa bb", "aa?x=1", "aa#f", ""} {
			if _, err := c.LookupOp(ctx, bad); err == nil {
				t.Fatalf("op id %q must be refused", bad)
			}
		}
	})

	t.Run("op id never alters the request path", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.WriteHeader(404)
		}))
		defer srv.Close()
		c := &FenceBrokerClient{BaseURL: srv.URL, Token: "token-0123456789abcdef"}
		if _, err := c.LookupOp(ctx, "abcdef12"); err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if !strings.HasSuffix(gotPath, "/v1/ops/abcdef12") {
			t.Fatalf("unexpected path: %s", gotPath)
		}
	})
}

// TestValidateOpID pins the input validation boundary.
func TestValidateOpID(t *testing.T) {
	for _, ok := range []string{"a", "ABCDEF0123456789", "00000000-0000-0000-0000-000000000000"} {
		if err := ValidateOpID(ok); err != nil {
			t.Fatalf("ValidateOpID(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", " ", " x", "x ", "../../etc", "a/b", "a?b", "a#b", "a b", "a\tb", string(make([]byte, 200))} {
		if err := ValidateOpID(bad); err == nil {
			t.Fatalf("ValidateOpID(%q) = nil, want error", bad)
		}
	}
}
