package herdr

import (
	"reflect"
	"testing"
)

func TestParseProcCmdlinePreservesExactArguments(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want []string
	}{
		{name: "normal", data: []byte("pi\x00--model\x00m\x00"), want: []string{"pi", "--model", "m"}},
		{name: "interior empty", data: []byte("pi\x00\x00--thinking\x00medium\x00"), want: []string{"pi", "", "--thinking", "medium"}},
		{name: "real trailing empty", data: []byte("pi\x00--flag\x00\x00"), want: []string{"pi", "--flag", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProcCmdline(tc.data)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("argv=%#v want=%#v", got, tc.want)
			}
		})
	}
}

func TestParseProcCmdlineRejectsMalformedFrames(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":        nil,
		"unterminated": []byte("pi\x00--model"),
		"empty argv0":  []byte("\x00--model\x00"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseProcCmdline(data); err == nil {
				t.Fatal("malformed cmdline accepted")
			}
		})
	}
}
