package verifier

import (
	"context"
	"testing"
)

func TestVerifier_Execute(t *testing.T) {
	v := NewVerifier("echo hello")
	res, err := v.Execute(context.Background(), ".")
	if err != nil || !res.Passed {
		t.Fatalf("expected pass, got err=%v, res=%v", err, res)
	}

	vFail := NewVerifier("false")
	resFail, _ := vFail.Execute(context.Background(), ".")
	if resFail.Passed {
		t.Fatalf("expected fail for 'false' command, got pass")
	}
}
