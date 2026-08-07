package herdr

import (
	"encoding/binary"
	"reflect"
	"testing"
)

func TestParseKERNProcargs2(t *testing.T) {
	// Synthetic KERN_PROCARGS2 buffer:
	//   argc=3 LE | exec path NUL | 2 padding NULs | argv[0..2] NULs | env bytes
	var buf []byte
	argc := make([]byte, 4)
	binary.LittleEndian.PutUint32(argc, 3)
	buf = append(buf, argc...)
	buf = append(buf, []byte("/usr/local/bin/pi\x00")...)
	buf = append(buf, 0, 0) // two padding NULs
	buf = append(buf, []byte("pi\x00")...)
	buf = append(buf, []byte("--model\x00")...)
	buf = append(buf, []byte("openai-codex/gpt-5.6-luna\x00")...)
	buf = append(buf, []byte("PATH=/usr/bin\x00HOME=/tmp\x00")...) // env bytes (ignored)

	got, err := parseKERNProcargs2(buf)
	if err != nil {
		t.Fatalf("parseKERNProcargs2: %v", err)
	}
	want := []string{"pi", "--model", "openai-codex/gpt-5.6-luna"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v, want %#v", got, want)
	}

	// Invalid buffers must fail closed.
	argc1 := make([]byte, 4)
	binary.LittleEndian.PutUint32(argc1, 1)
	argc2 := make([]byte, 4)
	binary.LittleEndian.PutUint32(argc2, 2)
	argc4097 := make([]byte, 4)
	binary.LittleEndian.PutUint32(argc4097, 4097)

	// truncated argv: argc=2, terminated exec, one terminated arg, no second arg
	var truncated []byte
	truncated = append(truncated, argc2...)
	truncated = append(truncated, []byte("/bin/echo\x00")...)
	truncated = append(truncated, 0, 0)                  // padding
	truncated = append(truncated, []byte("echo\x00")...) // only one of two args

	// unterminated exec with argc=1
	var unterminatedExec []byte
	unterminatedExec = append(unterminatedExec, argc1...)
	unterminatedExec = append(unterminatedExec, []byte("/usr/local/bin/pi")...) // no NUL

	cases := []struct {
		name string
		in   []byte
	}{
		{name: "too_short", in: []byte{1}},
		{name: "argc0", in: []byte{0, 0, 0, 0}},
		{name: "argc4097", in: argc4097},
		{name: "unterminated_exec", in: unterminatedExec},
		{name: "truncated_argv", in: truncated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseKERNProcargs2(tc.in); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}
