package provenance

import (
	"strings"
	"testing"
)

func TestValidateBinarySource(t *testing.T) {
	cases := []struct {
		name, source, binary string
		wantErr              string
	}{
		{"current", "abcdef0123456789", "abcdef0123456789", ""},
		{"stale", "abcdef0123456789", "1234567890abcdef", "stale herd binary"},
		{"missing metadata", "abcdef0123456789", "", "no source revision metadata"},
		{"missing source", "", "abcdef0123456789", "source revision is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(Info{BinaryRevision: tc.binary}, tc.source)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestFormatReportsAllProvenanceFields(t *testing.T) {
	got := Format(Info{Path: "./bin/herd", SourceRevision: "source", BinaryRevision: "binary", BuildTime: "time"})
	for _, want := range []string{"binary path: ./bin/herd", "source revision: source", "binary build revision: binary", "binary build time: time", "STALE"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Format() = %q, missing %q", got, want)
		}
	}
}
