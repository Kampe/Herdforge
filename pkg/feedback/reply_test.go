package feedback

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeInbox(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "inbox.jsonl")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReplyFromLanesAllReplied(t *testing.T) {
	inbox := writeInbox(t,
		`{"from":"a","summary":"FLEET_FEEDBACK E1 a"}`,
		`{"from":"b","summary":"FLEET_FEEDBACK E1 b"}`,
	)
	got, missing, err := ReplyFromLanes(inbox, "E1", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("got = %v, want [a b]", got)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want none", missing)
	}
}

func TestReplyFromLanesStaleEpochDoesNotCount(t *testing.T) {
	inbox := writeInbox(t, `{"from":"a","summary":"FLEET_FEEDBACK E0 a"}`)
	got, missing, err := ReplyFromLanes(inbox, "E1", []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %v, want none (reply is for a previous epoch)", got)
	}
	if !reflect.DeepEqual(missing, []string{"a"}) {
		t.Fatalf("missing = %v, want [a]", missing)
	}
}

func TestReplyFromLanesSelfExcludesTheRequestEnvelope(t *testing.T) {
	// The census's own outbound envelope carries the bare subject with no
	// trailing lane and must never be counted as a reply.
	inbox := writeInbox(t, `{"from":"coordinator-1","summary":"FLEET_FEEDBACK E1"}`)
	got, missing, err := ReplyFromLanes(inbox, "E1", []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %v, want none", got)
	}
	if !reflect.DeepEqual(missing, []string{"a"}) {
		t.Fatalf("missing = %v, want [a]", missing)
	}
}

func TestReplyFromLanesNoEpochYet(t *testing.T) {
	got, missing, err := ReplyFromLanes(writeInbox(t), "", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %v, want none", got)
	}
	if !reflect.DeepEqual(missing, []string{"a", "b"}) {
		t.Fatalf("missing = %v, want [a b]", missing)
	}
}

func TestReplyFromLanesMissingFileIsEmptyInbox(t *testing.T) {
	got, missing, err := ReplyFromLanes(filepath.Join(t.TempDir(), "absent.jsonl"), "E1", []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %v, want none", got)
	}
	if !reflect.DeepEqual(missing, []string{"a"}) {
		t.Fatalf("missing = %v, want [a]", missing)
	}
}

func TestReplyFromLanesDedupesRepeatedReplies(t *testing.T) {
	inbox := writeInbox(t,
		`{"from":"a","summary":"FLEET_FEEDBACK E1 a"}`,
		`{"from":"a","summary":"FLEET_FEEDBACK E1 a"}`,
	)
	got, _, err := ReplyFromLanes(inbox, "E1", []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("got = %v, want [a] (deduped)", got)
	}
}

func TestReplyFromLanesMalformedLineFailsClosed(t *testing.T) {
	inbox := writeInbox(t, `{not json`)
	got, missing, err := ReplyFromLanes(inbox, "E1", []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a malformed inbox must report zero replies, got %v", got)
	}
	if !reflect.DeepEqual(missing, []string{"a"}) {
		t.Fatalf("missing = %v, want [a]", missing)
	}
}
