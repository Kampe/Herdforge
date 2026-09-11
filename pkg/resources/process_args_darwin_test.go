//go:build darwin

package resources

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestCheckedDarwinPIDBounds(t *testing.T) {
	for _, tt := range []struct {
		name string
		pid  int
		want uint32
		err  bool
	}{
		{name: "negative", pid: -1, err: true}, {name: "zero", pid: 0, err: true},
		{name: "minimum", pid: 1, want: 1}, {name: "maximum", pid: darwinMaxPID, want: uint32(darwinMaxPID)},
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

func TestReadDarwinProcessArgsExitRaces(t *testing.T) {
	for _, phase := range []string{"size", "data"} {
		for _, tt := range []struct {
			name     string
			errno    syscall.Errno
			probeErr error
			want     error
			probes   int
		}{
			{"exited", syscall.EINVAL, syscall.ESRCH, os.ErrProcessDone, 1},
			{"live_or_reused", syscall.EINVAL, nil, syscall.EINVAL, 1},
			{"live_foreign", syscall.EINVAL, syscall.EPERM, syscall.EINVAL, 1},
			{"unknown", syscall.EINVAL, syscall.EIO, syscall.EINVAL, 1},
			{"sysctl_gone", syscall.ESRCH, nil, os.ErrProcessDone, 0},
			{"sysctl_permission", syscall.EPERM, syscall.ESRCH, syscall.EPERM, 0},
			{"sysctl_other", syscall.EIO, syscall.ESRCH, syscall.EIO, 0},
		} {
			t.Run(phase+"/"+tt.name, func(t *testing.T) {
				calls, probes := 0, 0
				data, err := readDarwinProcessArgsWith(42, func(pid uint32, buf []byte) (uint64, syscall.Errno) {
					calls++
					if pid != 42 {
						t.Fatalf("queried wrong pid %d", pid)
					}
					if calls == 1 {
						if buf != nil {
							t.Fatal("size query has a buffer")
						}
						if phase == "data" {
							return 16, 0
						}
					}
					if calls == 2 && len(buf) != 16 {
						t.Fatalf("data buffer length=%d", len(buf))
					}
					return 0, tt.errno
				}, func(pid int) error {
					probes++
					if pid != 42 {
						t.Fatalf("probed wrong pid %d", pid)
					}
					return tt.probeErr
				})
				if !errors.Is(err, tt.want) || data != nil {
					t.Fatalf("data=%v error=%v; want nil, %v", data, err, tt.want)
				}
				wantCalls := 1
				if phase == "data" {
					wantCalls = 2
				}
				if calls != wantCalls || probes != tt.probes {
					t.Fatalf("calls=%d probes=%d; want %d, %d", calls, probes, wantCalls, tt.probes)
				}
			})
		}
	}
}

func TestReadDarwinProcessArgsBounds(t *testing.T) {
	for _, tt := range []struct {
		name       string
		pid        int
		size, read uint64
		calls      int
		ok         bool
	}{
		{name: "invalid_pid", pid: -1},
		{name: "empty_size", pid: 42, calls: 1},
		{name: "oversized", pid: 42, size: 1<<20 + 1, calls: 1},
		{name: "grew_between_reads", pid: 42, size: 8, read: 9, calls: 2},
		{name: "empty_read", pid: 42, size: 8, calls: 2},
		{name: "shrunk", pid: 42, size: 8, read: 4, calls: 2, ok: true},
		{name: "same_size", pid: 42, size: 8, read: 8, calls: 2, ok: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			data, err := readDarwinProcessArgsWith(tt.pid, func(_ uint32, buf []byte) (uint64, syscall.Errno) {
				calls++
				if buf == nil {
					return tt.size, 0
				}
				for i := range buf {
					buf[i] = 7
				}
				return tt.read, 0
			}, func(int) error { t.Fatal("unexpected liveness probe on successful sysctl"); return nil })
			if calls != tt.calls {
				t.Fatalf("query calls=%d want %d", calls, tt.calls)
			}
			if tt.ok {
				if err != nil || len(data) != int(tt.read) {
					t.Fatalf("data=%v error=%v", data, err)
				}
				for _, v := range data {
					if v != 7 {
						t.Fatal("lost syscall output")
					}
				}
			} else if err == nil || data != nil {
				t.Fatalf("invalid read accepted: data=%v err=%v", data, err)
			}
		})
	}
}

func TestReadDarwinProcessArgsSelf(t *testing.T) {
	data, err := readDarwinProcessArgs(os.Getpid())
	if err != nil || len(data) == 0 {
		t.Fatalf("read own process arguments: length=%d error=%v", len(data), err)
	}
}
