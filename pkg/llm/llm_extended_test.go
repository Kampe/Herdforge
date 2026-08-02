package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewLocalLLMClient_DefaultBaseURL(t *testing.T) {
	c := NewLocalLLMClient("")
	if c.BaseURL != "http://127.0.0.1:11434" {
		t.Errorf("expected default URL, got %s", c.BaseURL)
	}
}

func TestGeneratePrompt_NonOKStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	client := NewLocalLLMClient(ts.URL)
	_, err := client.GeneratePrompt(context.Background(), "llama3", "hello")
	if err == nil {
		t.Fatal("expected error for non-200 status")
	}
}

func TestGeneratePrompt_BadJSONResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not json`))
	}))
	defer ts.Close()

	client := NewLocalLLMClient(ts.URL)
	_, err := client.GeneratePrompt(context.Background(), "llama3", "hello")
	if err == nil {
		t.Fatal("expected error for bad JSON response")
	}
}

func TestGeneratePrompt_ContextCanceled(t *testing.T) {
	client := NewLocalLLMClient("http://127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.GeneratePrompt(ctx, "llama3", "hello")
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}

func TestGeneratePrompt_ConnectionRefused(t *testing.T) {
	client := NewLocalLLMClient("http://127.0.0.1:1")
	_, err := client.GeneratePrompt(context.Background(), "llama3", "hello")
	if err == nil {
		t.Fatal("expected error when connection refused")
	}
}
