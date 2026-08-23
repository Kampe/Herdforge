package reviewingest

import "testing"

// FAC-578: review-ledger rows recorded only sha+verdict, so no verdict could be
// tied back to a board card. When a bulk replay corrupted the Chainseer board,
// 558 of 593 to-do cards had no recoverable review state for exactly this
// reason. The card ref must survive into the artifact.
func TestParseTaskRefFromHeader(t *testing.T) {
	for _, key := range []string{"task", "task-id", "card", "ticket"} {
		a := Parse(key + ": cha-2345\nsha: " + dummySHA + "\n---\nbody\n")
		if a.TaskRef != "CHA-2345" {
			t.Errorf("header %q: got TaskRef %q want CHA-2345", key, a.TaskRef)
		}
		if len(a.UnknownHeaders) != 0 {
			t.Errorf("header %q must be recognised, got unknown %v", key, a.UnknownHeaders)
		}
	}
}

func TestParseTaskRefFromBodyDeclaration(t *testing.T) {
	a := Parse("sha: " + dummySHA + "\n---\nVerdict: PASS\nTask ID: CHA-2156, bound something\n")
	if a.TaskRef != "CHA-2156" {
		t.Fatalf("got %q want CHA-2156", a.TaskRef)
	}
}

func TestParseTaskRefFromReviewerSlug(t *testing.T) {
	a := Parse("sha: " + dummySHA + "\nreviewer: review-cha-2421-claude\n---\nbody\n")
	if a.TaskRef != "CHA-2421" {
		t.Fatalf("got %q want CHA-2421", a.TaskRef)
	}
}

// The misattribution that actually bit us: a review of one card routinely
// discusses sibling cards in prose. A bare ref in the body must NOT be
// harvested as this verdict's card, or the verdict credits the wrong work.
func TestParseTaskRefIgnoresBareBodyMentions(t *testing.T) {
	a := Parse("sha: " + dummySHA + "\nreviewer: nobody\n---\n" +
		"This overlaps CHA-2156 and supersedes CHA-2349, see also CHA-1801.\n")
	if a.TaskRef != "" {
		t.Fatalf("bare prose mention must not attribute a card, got %q", a.TaskRef)
	}
}

// An explicit header always wins over anything inferable from body or reviewer.
func TestParseTaskRefHeaderOutranksInference(t *testing.T) {
	a := Parse("sha: " + dummySHA + "\ntask: CHA-100\nreviewer: review-cha-999-claude\n---\nTask ID: CHA-777\n")
	if a.TaskRef != "CHA-100" {
		t.Fatalf("declared header must win, got %q", a.TaskRef)
	}
}

func TestNormalizeTaskRef(t *testing.T) {
	cases := map[string]string{
		"cha-2345":        "CHA-2345",
		"  FAC-12  ":      "FAC-12",
		"CHA-2345":        "CHA-2345",
		"nonsense":        "",
		"":                "",
		"12345":           "",
		"toolongprefix-1": "",
	}
	for in, want := range cases {
		if got := NormalizeTaskRef(in); got != want {
			t.Errorf("NormalizeTaskRef(%q) = %q want %q", in, got, want)
		}
	}
}

const dummySHA = "0123456789abcdef0123456789abcdef01234567"
