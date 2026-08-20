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

func TestObserveHandoffsRejectsRepeatedContentAsProgress(t *testing.T) {
	mailFile := writeInbox(t,
		`{"from":"lane-a","summary":"FLEET_FEEDBACK E1 lane-a","message":"poll=1 blocker: none"}`,
		`{"from":"lane-a","summary":"FLEET_FEEDBACK E2 lane-a","message":"poll=2 blocker: none"}`,
	)
	tracker := NewHandoffTracker()
	first, err := ObserveHandoffs(mailFile, "E1", []string{"lane-a"}, tracker)
	if err != nil || len(first) != 1 || !first[0].Observation.Progress {
		t.Fatalf("first handoff = %+v, err=%v", first, err)
	}
	second, err := ObserveHandoffs(mailFile, "E2", []string{"lane-a"}, tracker)
	if err != nil || len(second) != 1 || second[0].Observation.Progress || !second[0].Observation.Refocus {
		t.Fatalf("repeated handoff = %+v, err=%v", second, err)
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

func TestReplyFromLanesIgnoresRetiredLaneReplies(t *testing.T) {
	inbox := writeInbox(t,
		`{"from":"live","summary":"FLEET_FEEDBACK E1 live"}`,
		`{"from":"retired-worker","summary":"FLEET_FEEDBACK E1 retired-worker"}`,
	)
	got, missing, err := ReplyFromLanes(inbox, "E1", []string{"live"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"live"}) {
		t.Fatalf("got = %v, want only current roster lane", got)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want none", missing)
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
