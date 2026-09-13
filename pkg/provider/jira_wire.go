package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// jiraTime decodes Jira Cloud's timestamp, which is NOT RFC3339: it uses a
// numeric UTC offset with no colon ("2026-09-01T12:00:00.000+0000"), so the
// default time.Time decoder rejects it and takes the whole issue down with it.
type jiraTime struct{ time.Time }

var jiraTimeLayouts = []string{
	"2006-01-02T15:04:05.999-0700",
	"2006-01-02T15:04:05-0700",
	time.RFC3339Nano,
	time.RFC3339,
}

func (t *jiraTime) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		// A null or absent timestamp is not a decode failure.
		if string(data) == "null" {
			return nil
		}
		return err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	for _, layout := range jiraTimeLayouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			t.Time = parsed
			return nil
		}
	}
	return fmt.Errorf("jira: unrecognized timestamp %q", raw)
}

// adfText is a Jira rich-text field. Jira Cloud REST v3 returns Atlassian
// Document Format objects where v2 returned plain strings, so a plain `string`
// field fails to decode every issue that actually has a description. It accepts
// either shape and flattens ADF to its text runs.
type adfText struct{ Text string }

func (a *adfText) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		a.Text = asString
		return nil
	}
	var node adfNode
	if err := json.Unmarshal(data, &node); err != nil {
		return fmt.Errorf("jira: rich text is neither a string nor a document: %w", err)
	}
	a.Text = strings.TrimSpace(node.flatten())
	return nil
}

// adfNode is the recursive shape of an Atlassian document. Only text runs and
// structure matter here; unknown node types contribute their children, so a
// table or list still yields its text instead of vanishing.
type adfNode struct {
	Type    string    `json:"type"`
	Text    string    `json:"text"`
	Content []adfNode `json:"content"`
}

func (n adfNode) flatten() string {
	var b strings.Builder
	n.writeTo(&b)
	return b.String()
}

func (n adfNode) writeTo(b *strings.Builder) {
	if n.Text != "" {
		b.WriteString(n.Text)
	}
	switch n.Type {
	case "hardBreak":
		b.WriteString("\n")
	}
	for i := range n.Content {
		n.Content[i].writeTo(b)
		// Block-level children are newline separated so multi-paragraph
		// descriptions do not run together into one unreadable line.
		switch n.Content[i].Type {
		case "paragraph", "heading", "listItem", "codeBlock", "blockquote", "rule":
			b.WriteString("\n")
		}
	}
}

// jqlSafeIdentifier bounds a value that is interpolated into JQL. JQL is a
// query language, so a project key or status carrying quotes or operators must
// be refused rather than escaped-and-hoped: a wrong guess here silently widens
// the query to other people's boards.
var jqlSafeIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,62}$`)

func checkJQLIdentifier(field, value string) error {
	if !jqlSafeIdentifier.MatchString(value) {
		return fmt.Errorf("jira: %s %q contains characters that are not valid in a JQL identifier", field, value)
	}
	return nil
}
