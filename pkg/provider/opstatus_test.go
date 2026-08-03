package provider

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestClassifyOpError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want OpFailureClass
	}{
		{"nil", nil, OpOK},
		{"timeout", &TimeoutError{Cause: context.DeadlineExceeded}, OpTimeout},
		{"canceled", context.Canceled, OpTimeout},
		{"ambiguous pure", &AmbiguousMutationError{TaskID: "t", Want: "done", WriteErr: errors.New("other")}, OpAmbiguous},
		{"ambiguous after timeout", &AmbiguousMutationError{
			TaskID: "t", Want: "done",
			WriteErr: &TimeoutError{Cause: context.DeadlineExceeded},
		}, OpAmbiguous},
		{"provider 503", &ProviderError{StatusCode: 503, Message: "down", Retryable: true}, OpProvider},
		{"provider 404", &ProviderError{StatusCode: http.StatusNotFound, Message: "nope"}, OpProvider},
		{"plain", errors.New("boom"), OpOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyOpError(tc.err)
			if got != tc.want {
				t.Fatalf("ClassifyOpError = %q want %q", got, tc.want)
			}
		})
	}
}

func TestIsRecoverableTimeout(t *testing.T) {
	if !IsRecoverableTimeout(&TimeoutError{Cause: context.DeadlineExceeded}) {
		t.Fatal("plain timeout should be recoverable for reads")
	}
	// Ambiguous must NOT look like a safe read-retry timeout.
	if IsRecoverableTimeout(&AmbiguousMutationError{
		WriteErr: &TimeoutError{Cause: context.DeadlineExceeded},
	}) {
		t.Fatal("ambiguous mutation must not classify as recoverable timeout")
	}
	if IsRecoverableTimeout(errors.New("x")) {
		t.Fatal("plain error is not recoverable timeout")
	}
}
