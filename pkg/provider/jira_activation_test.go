package provider

import (
	"strings"
	"testing"
)

func jiraTaskConfig() TaskConfig {
	return TaskConfig{
		Type:      "jira",
		APIURL:    "https://example.atlassian.net",
		ProjectID: "HUB",
		UserEmail: "operator@example.com",
		APIKey:    "token",
	}
}

// Jira must be selectable exactly like linear/kaneo: declared type plus the
// repository's activation policy, nothing discovered or probed.
func TestJiraActivatesThroughTheProductionFactory(t *testing.T) {
	tp, err := NewProductionProvider(jiraTaskConfig())
	if err != nil {
		t.Fatalf("jira did not activate: %v", err)
	}
	bound, ok := tp.(*BoundClient)
	if !ok {
		t.Fatalf("jira provider is not deadline-bound: %T", tp)
	}
	jp, ok := bound.Inner.(*JiraProvider)
	if !ok {
		t.Fatalf("bound inner is not a JiraProvider: %T", bound.Inner)
	}
	if jp.BaseURL != "https://example.atlassian.net" || jp.UserEmail != "operator@example.com" || jp.ProjectKey != "HUB" {
		t.Fatalf("jira provider was not bound to its configured board: %+v", jp)
	}
}

// FAC-155: a type outside a non-empty enabled list must fail closed, for jira
// exactly as for every other adapter.
func TestJiraRefusedWhenNotInActivationPolicy(t *testing.T) {
	tc := jiraTaskConfig()
	tc.Enabled = []string{"kaneo"}
	if _, err := NewProductionProvider(tc); err == nil {
		t.Fatal("jira activated despite not being in task_provider.enabled")
	}
	tc.Enabled = []string{"kaneo", "jira"}
	if _, err := NewProductionProvider(tc); err != nil {
		t.Fatalf("jira refused despite being enabled: %v", err)
	}
}

func TestJiraActivationRequiresEveryCredentialPart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*TaskConfig)
		want   string
	}{
		{"no api_url", func(c *TaskConfig) { c.APIURL = "" }, "api_url"},
		{"no user_email", func(c *TaskConfig) { c.UserEmail = "" }, "user_email"},
		{"no api key", func(c *TaskConfig) { c.APIKey = "" }, "api_key_env"},
		{"no project", func(c *TaskConfig) { c.ProjectID = "" }, "project_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := jiraTaskConfig()
			tc.mutate(&cfg)
			_, err := NewProductionProvider(cfg)
			if err == nil {
				t.Fatal("activation succeeded with an incomplete credential")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error does not name the missing field %q: %v", tc.want, err)
			}
		})
	}
}

// The Jira site origin is repository-controlled, which makes it the URL the
// operator's credential is sent to. It is therefore constrained, not trusted.
func TestJiraRefusesUntrustworthyBaseURL(t *testing.T) {
	for _, raw := range []string{
		"http://example.atlassian.net",          // plaintext
		"https://user:pw@example.atlassian.net", // retargets the auth header
		"https://",                              // no host
		"https://example.atlassian.net?x=1",     // not an origin
		"::not a url",
	} {
		cfg := jiraTaskConfig()
		cfg.APIURL = raw
		if _, err := NewProductionProvider(cfg); err == nil {
			t.Fatalf("accepted an untrustworthy jira api_url: %q", raw)
		}
	}
}

func TestJiraBaseURLTrailingSlashIsNormalized(t *testing.T) {
	cfg := jiraTaskConfig()
	cfg.APIURL = "https://example.atlassian.net/"
	tp, err := NewProductionProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	jp := tp.(*BoundClient).Inner.(*JiraProvider)
	if jp.BaseURL != "https://example.atlassian.net" {
		t.Fatalf("trailing slash survived normalization: %q", jp.BaseURL)
	}
}

// A bound provider must never fan out across every project on the site just
// because a caller passed no project id.
func TestJiraListTasksFallsBackToTheBoundProjectKey(t *testing.T) {
	jp := NewJiraProvider("https://example.atlassian.net", "op@example.com", "token")
	if _, err := jp.listTasksOnce(t.Context(), "", ""); err == nil {
		t.Fatal("unbound jira ListTasks with no project must refuse, not query every project")
	}
}
