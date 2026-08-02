package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Platform string

const (
	PlatformSlack   Platform = "slack"
	PlatformDiscord Platform = "discord"
	PlatformTeams   Platform = "teams"
)

type Notifier struct {
	Platform   Platform
	WebhookURL string
	HTTPClient *http.Client
}

func NewNotifier(platform Platform, webhookURL string) *Notifier {
	return &Notifier{
		Platform:   platform,
		WebhookURL: webhookURL,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (n *Notifier) BuildPayload(title, body, status string) interface{} {
	message := fmt.Sprintf("[%s] %s: %s", status, title, body)
	switch n.Platform {
	case PlatformDiscord:
		return map[string]interface{}{
			"content": message,
		}
	case PlatformTeams:
		return map[string]interface{}{
			"title": title,
			"text":  fmt.Sprintf("Status: %s\n%s", status, body),
		}
	default: // Slack fallback
		return map[string]interface{}{
			"text": message,
		}
	}
}

func (n *Notifier) Notify(ctx context.Context, title, body, status string) error {
	if n.WebhookURL == "" {
		return nil // Noop if webhook URL is unconfigured
	}

	payload := n.BuildPayload(title, body, status)
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("notifier marshal failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", n.WebhookURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("notifier request creation failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("notifier dispatch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("notifier HTTP error %d: %s", resp.StatusCode, string(respBytes))
	}

	return nil
}
