//go:build darwin

package resources

import "testing"

func TestCheckedDarwinPIDBounds(t *testing.T) {
	for _, tt := range []struct {
		name string
		pid  int
		want uint32
		err  bool
	}{
		{name: "negative", pid: -1, err: true},
		{name: "zero", pid: 0, err: true},
		{name: "minimum", pid: 1, want: 1},
		{name: "maximum", pid: darwinMaxPID, want: uint32(darwinMaxPID)},
		{name: "overflow", pid: darwinMaxPID + 1, err: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checkedDarwinPID(tt.pid)
			if tt.err {
				if err == nil {
					t.Fatalf("checkedDarwinPID(%d) accepted out-of-range PID %d", tt.pid, got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("checkedDarwinPID(%d) = %d, %v; want %d, nil", tt.pid, got, err, tt.want)
			}
		})
	}
}
