package notifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNotifier_SlackDiscordTeams(t *testing.T) {
	var receivedBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer ts.Close()

	_ = receivedBody

	slackNotif := NewNotifier(PlatformSlack, ts.URL)
	if err := slackNotif.Notify(context.Background(), "Task Completed", "FAC-47 is done", "SUCCESS"); err != nil {
		t.Errorf("expected clean Slack notify, got err: %v", err)
	}

	discordNotif := NewNotifier(PlatformDiscord, ts.URL)
	if err := discordNotif.Notify(context.Background(), "Budget Exceeded", "$10 threshold hit", "WARNING"); err != nil {
		t.Errorf("expected clean Discord notify, got err: %v", err)
	}

	teamsNotif := NewNotifier(PlatformTeams, ts.URL)
	if err := teamsNotif.Notify(context.Background(), "Review Blocked", "R3 risk review needed", "BLOCKED"); err != nil {
		t.Errorf("expected clean Teams notify, got err: %v", err)
	}
}

func TestNotifier_EmptyURL(t *testing.T) {
	n := NewNotifier(PlatformSlack, "")
	if err := n.Notify(context.Background(), "Title", "Body", "OK"); err != nil {
		t.Errorf("expected noop on empty webhook URL, got err: %v", err)
	}
}
