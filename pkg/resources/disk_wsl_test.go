package resources

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const sampleWSLProcVersion = "Linux version 6.18.33.2-microsoft-standard-WSL2 (oe-user@oe-host) (x86_64-msft-linux-gcc (GCC) 11.2.0, GNU ld (GNU Binutils) 2.37) #1 SMP PREEMPT Thu Jun 11 04:14:48 UTC 2026\n"
const sampleNativeLinuxProcVersion = "Linux version 6.8.0-40-generic (buildd@lcy02-amd64-073) (x86_64-linux-gnu-gcc-13 (Ubuntu 13.2.0-23ubuntu4) 13.2.0, GNU ld (GNU Binutils for Ubuntu) 2.42) #40-Ubuntu SMP PREEMPT_DYNAMIC Tue Jun 11 02:40:48 UTC 2026\n"

const sampleLxssRegistryOutput = `
HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Lxss
    DefaultDistribution    REG_SZ    {11111111-2222-3333-4444-555555555555}

HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Lxss\{11111111-2222-3333-4444-555555555555}
    DistributionName    REG_SZ    Ubuntu-24.04
    BasePath            REG_SZ    D:\WSL\Ubuntu-24.04
    Version             REG_DWORD    0x2

HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Lxss\{66666666-7777-8888-9999-000000000000}
    DistributionName    REG_SZ    Debian
    BasePath            REG_SZ    E:\Users\kampe\AppData\Local\Packages\TheDebianProject_76v4gfsz1904\LocalState
    Version             REG_DWORD    0x2
`

const sampleProcMounts = `
rootfs / rootfs rw 0 0
none /dev devtmpfs rw,nosuid,relatime,size=16335340k,nr_inodes=4083835,mode=755 0 0
/dev/sdb / ext4 rw,relatime,discard,errors=remount-ro,data=ordered 0 0
E:\ /mnt/e 9p rw,noatime,dirsync,aname=drvfs;path=E:\;uid=1000;gid=1000;symlinkroot=/mnt/,mmap,access=client,msize=65536,trans=fd,rfd=8,wfd=8 0 0
D:\ /mnt/d 9p rw,noatime,dirsync,aname=drvfs;path=D:\;uid=1000;gid=1000;symlinkroot=/mnt/,mmap,access=client,msize=65536,trans=fd,rfd=8,wfd=8 0 0
`

// stubWSLSignals pins every host-derived detection input so these tests are
// deterministic on macOS, native Linux, AND a real WSL box. FAC-613: the CI
// run at a8cd39e1 failed exactly these tests on real WSL because the host's
// own WSL_DISTRO_NAME/WSL_INTEROP env and /run/WSL-style marker files leaked
// into the "no signals" assertions.
func stubWSLSignals(t *testing.T, statPresent map[string]bool) {
	t.Helper()
	oldReader := wslProcVersionReader
	oldOverride := wslDetectionOverride
	oldGOOS := wslRuntimeGOOS
	oldStat := wslSignalStat
	t.Cleanup(func() {
		wslProcVersionReader = oldReader
		wslDetectionOverride = oldOverride
		wslRuntimeGOOS = oldGOOS
		wslSignalStat = oldStat
	})
	wslDetectionOverride = nil
	wslRuntimeGOOS = "linux"
	wslSignalStat = func(path string) error {
		if statPresent[path] {
			return nil
		}
		return os.ErrNotExist
	}
	t.Setenv("WSL_DISTRO_NAME", "")
	t.Setenv("WSL_INTEROP", "")
}

func TestWSLDetectionHermetic(t *testing.T) {
	stubWSLSignals(t, nil)

	// 1. WSL kernel version text contains microsoft
	wslProcVersionReader = func() ([]byte, error) {
		return []byte(sampleWSLProcVersion), nil
	}
	if !isWSLEnvironment() {
		t.Fatal("expected isWSLEnvironment to return true for sample WSL proc version")
	}

	// 2. Native Linux version text does not contain microsoft or wsl
	wslProcVersionReader = func() ([]byte, error) {
		return []byte(sampleNativeLinuxProcVersion), nil
	}
	if isWSLEnvironment() {
		t.Fatal("expected isWSLEnvironment to return false for sample native Linux proc version without signals")
	}
}

func TestWSLResolveDistroBackingDriveHermetic(t *testing.T) {
	oldExecutor := wslRegistryQueryExecutor
	defer func() { wslRegistryQueryExecutor = oldExecutor }()

	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(sampleLxssRegistryOutput), nil
	}

	// Case 1: WSL_DISTRO_NAME matches Ubuntu-24.04 on D:
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu-24.04")
	drive, err := resolveDistroBackingDrive(context.Background())
	if err != nil {
		t.Fatalf("unexpected error resolving distro drive: %v", err)
	}
	if drive != "D:" {
		t.Fatalf("expected drive D: for Ubuntu-24.04, got %q", drive)
	}

	// Case 2: WSL_DISTRO_NAME matches Debian on E:
	t.Setenv("WSL_DISTRO_NAME", "Debian")
	drive, err = resolveDistroBackingDrive(context.Background())
	if err != nil {
		t.Fatalf("unexpected error resolving distro drive: %v", err)
	}
	if drive != "E:" {
		t.Fatalf("expected drive E: for Debian, got %q", drive)
	}

	// Case 3: WSL_DISTRO_NAME unset with multiple distros -> fails closed (no arbitrary selection)
	t.Setenv("WSL_DISTRO_NAME", "")
	_, err = resolveDistroBackingDrive(context.Background())
	if err == nil {
		t.Fatal("expected error when WSL_DISTRO_NAME is unset and multiple distros are registered")
	}
}

func TestWSLResolveDistroDifferentDistroDefaultMismatch(t *testing.T) {
	oldExecutor := wslRegistryQueryExecutor
	defer func() { wslRegistryQueryExecutor = oldExecutor }()

	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(sampleLxssRegistryOutput), nil
	}

	// Default distro in registry is Ubuntu-24.04 (D:), but running distro is Debian (E:)
	t.Setenv("WSL_DISTRO_NAME", "Debian")
	drive, err := resolveDistroBackingDrive(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drive != "E:" {
		t.Fatalf("expected running distro drive E:, got %q (must not substitute default D:)", drive)
	}
}

func TestWSLResolveDistroExplicitNameMissingFailsClosed(t *testing.T) {
	oldExecutor := wslRegistryQueryExecutor
	defer func() { wslRegistryQueryExecutor = oldExecutor }()

	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(sampleLxssRegistryOutput), nil
	}

	// Explicit WSL_DISTRO_NAME is "Arch", which is missing from the registry.
	// It must fail closed and NEVER fall back to DefaultDistribution (Ubuntu-24.04 on D:).
	t.Setenv("WSL_DISTRO_NAME", "Arch")
	_, err := resolveDistroBackingDrive(context.Background())
	if err == nil {
		t.Fatal("expected explicit missing distro to fail closed, got nil error")
	}
	if !strings.Contains(err.Error(), `wsl distro "Arch" not found in Lxss registry`) {
		t.Fatalf("expected error mentioning missing distro, got %q", err.Error())
	}
}

func TestWSLResolveDistroNoNameMultiDistroNondeterminismFailsClosed(t *testing.T) {
	oldExecutor := wslRegistryQueryExecutor
	defer func() { wslRegistryQueryExecutor = oldExecutor }()

	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(sampleLxssRegistryOutput), nil
	}

	// WSL_DISTRO_NAME is empty and there are 2 distros on different drives (D: and E:).
	// Must fail closed; must never arbitrarily iterate map or guess.
	t.Setenv("WSL_DISTRO_NAME", "")
	_, err := resolveDistroBackingDrive(context.Background())
	if err == nil {
		t.Fatal("expected unauthenticated multi-distro resolution to fail closed")
	}
	if !strings.Contains(err.Error(), "cannot determine backing host drive") {
		t.Fatalf("expected error indicating inability to determine backing drive, got %q", err.Error())
	}
}

func TestWSLResolveDistroNoNameSingleDistroUnambiguous(t *testing.T) {
	oldExecutor := wslRegistryQueryExecutor
	defer func() { wslRegistryQueryExecutor = oldExecutor }()

	singleDistroRegistry := `
HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Lxss
    DefaultDistribution    REG_SZ    {11111111-2222-3333-4444-555555555555}

HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Lxss\{11111111-2222-3333-4444-555555555555}
    DistributionName    REG_SZ    Ubuntu
    BasePath            REG_SZ    E:\WSL\Ubuntu
`
	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(singleDistroRegistry), nil
	}

	t.Setenv("WSL_DISTRO_NAME", "")
	drive, err := resolveDistroBackingDrive(context.Background())
	if err != nil {
		t.Fatalf("unexpected error for single registered distro: %v", err)
	}
	if drive != "E:" {
		t.Fatalf("expected drive C:, got %q", drive)
	}
}

func TestWSLAtypicalProcVersionInspectsSecondarySignals(t *testing.T) {
	stubWSLSignals(t, nil)

	// Custom Linux kernel version text without "microsoft" or "wsl"
	wslProcVersionReader = func() ([]byte, error) {
		return []byte("Linux version 6.18.0-custom (user@build) #1 SMP PREEMPT\n"), nil
	}

	// 1. Without secondary signals -> false
	if isWSLEnvironment() {
		t.Fatal("expected false for atypical kernel without secondary signals")
	}

	// 2. With WSL_DISTRO_NAME secondary signal -> true
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	if !isWSLEnvironment() {
		t.Fatal("expected true for atypical kernel with WSL_DISTRO_NAME set")
	}
	t.Setenv("WSL_DISTRO_NAME", "")

	// 3. With an injected filesystem marker signal -> true (proves the stat
	// seam is consulted rather than the host's real filesystem)
	wslSignalStat = func(path string) error {
		if path == "/run/WSL" {
			return nil
		}
		return os.ErrNotExist
	}
	if !isWSLEnvironment() {
		t.Fatal("expected true for atypical kernel with /run/WSL marker present via seam")
	}
}

func TestWSLUnsupportedRegistryEncodingFailsClosed(t *testing.T) {
	oldExecutor := wslRegistryQueryExecutor
	defer func() { wslRegistryQueryExecutor = oldExecutor }()

	binaryRegistryOutput := `
HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Lxss\{11111111-2222-3333-4444-555555555555}
    DistributionName    REG_SZ    Ubuntu
    BasePath            REG_BINARY    0102030405
`
	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(binaryRegistryOutput), nil
	}

	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	_, err := resolveDistroBackingDrive(context.Background())
	if err == nil {
		t.Fatal("expected error on unsupported REG_BINARY BasePath")
	}
	if !strings.Contains(err.Error(), "unsupported registry value type") {
		t.Fatalf("expected error mentioning unsupported registry value type, got %q", err.Error())
	}
}

func TestWSLFindDriveMountPathRejectsMountSpoof(t *testing.T) {
	// Mounts table contains:
	// 1. spoofed ext4 entry at /mnt/e
	// 2. ambiguous 9p DrvFS mount at /mnt/e backed by D: without path option
	// 3. tmpfs mount at /var/log/c
	// 4. authentic 9p DrvFS mount at /media/c with path=E:\
	spoofedMounts := `
rootfs / rootfs rw 0 0
/dev/sdb /mnt/e ext4 rw,relatime 0 0
D:\134 /mnt/e 9p rw,noatime,aname=drvfs;uid=1000;gid=1000 0 0
tmpfs /var/log/c tmpfs rw,relatime 0 0
E:\134 /media/c 9p rw,noatime,aname=drvfs;path=E:\;uid=1000;gid=1000,access=client 0 0
`
	mountC, err := findDriveMountPath("E:", []byte(spoofedMounts))
	if err != nil {
		t.Fatalf("unexpected error finding mount for C:: %v", err)
	}
	// Must reject ext4 /mnt/e AND ambiguous 9p D: at /mnt/e, choosing genuine 9p /media/c
	if mountC != "/media/c" {
		t.Fatalf("expected authentic 9p mount /media/c, got spoofed %q", mountC)
	}
}

func TestWSLFindDriveMountPathRejectsAmbiguous9pDeviceDMountedAtMountCWithoutPathOption(t *testing.T) {
	// Ambiguous 9p mount at /mnt/e where device is D: and no path= option is present.
	ambiguousMounts := `
D:\134 /mnt/e 9p rw,noatime,aname=drvfs;uid=1000;gid=1000 0 0
`
	// Searching for C: must fail closed because /mnt/e is backed by D:, not C:.
	_, err := findDriveMountPath("E:", []byte(ambiguousMounts))
	if err == nil {
		t.Fatal("expected 9p mount at /mnt/e backed by device D: (without path option) to be rejected for drive C:")
	}

	// Searching for D: must resolve to /mnt/e.
	mountD, err := findDriveMountPath("D:", []byte(ambiguousMounts))
	if err != nil {
		t.Fatalf("unexpected error resolving drive D:: %v", err)
	}
	if mountD != "/mnt/e" {
		t.Fatalf("expected /mnt/e for drive D:, got %q", mountD)
	}
}

func TestWSLFindDriveMountPathRejectsContradictoryDeviceAndOptions(t *testing.T) {
	// Device says D: but options say path=E:\ (contradictory)
	contradictoryMounts := `
D:\134 /mnt/d 9p rw,noatime,aname=drvfs;path=E:\;uid=1000;gid=1000 0 0
`
	// Searching for C: must reject this mount because device contradicts options
	_, err := findDriveMountPath("E:", []byte(contradictoryMounts))
	if err == nil {
		t.Fatal("expected contradictory device vs options mount to be rejected for C:")
	}

	// Searching for D: must also reject this mount because options contradict device
	_, err = findDriveMountPath("D:", []byte(contradictoryMounts))
	if err == nil {
		t.Fatal("expected contradictory device vs options mount to be rejected for D:")
	}
}

func TestWSLFindDriveMountPathRejectsDeviceDMountedAtMountC(t *testing.T) {
	// Device D: mounted at /mnt/e (e.g. mountpoint remapped or spoofed)
	remappedMounts := `
D:\134 /mnt/e 9p rw,noatime,aname=drvfs;path=D:\;uid=1000;gid=1000 0 0
`
	// Searching for C: must reject because device and options are D:
	_, err := findDriveMountPath("E:", []byte(remappedMounts))
	if err == nil {
		t.Fatal("expected /mnt/e backed by D: to be rejected when resolving drive C:")
	}

	// Searching for D: must resolve to /mnt/e
	mountD, err := findDriveMountPath("D:", []byte(remappedMounts))
	if err != nil {
		t.Fatalf("unexpected error resolving drive D:: %v", err)
	}
	if mountD != "/mnt/e" {
		t.Fatalf("expected /mnt/e for drive D:, got %q", mountD)
	}
}

func TestWSLFindDriveMountPathDecodesProcfsOctalEscapesWithSpaces(t *testing.T) {
	// Mount point contains escaped space (\040)
	spaceMounts := `
E:\134 /mnt/my\040drive\040c 9p rw,noatime,aname=drvfs;path=E:\;uid=1000;gid=1000 0 0
`
	mountC, err := findDriveMountPath("E:", []byte(spaceMounts))
	if err != nil {
		t.Fatalf("unexpected error finding mount for C:: %v", err)
	}
	if mountC != "/mnt/my drive c" {
		t.Fatalf("expected decoded mount path %q, got %q", "/mnt/my drive c", mountC)
	}
}

func TestWSLDecodeProcfsEscapeUnit(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`E:\134`, `E:\`},
		{`/mnt/drive\040c`, `/mnt/drive c`},
		{`\011\012\040\134`, "\t\n \\"},
		{`normal/path`, `normal/path`},
		{`\999`, `\999`}, // invalid octal preserved
		{`\`, `\`},
		{`\04`, `\04`},
	}
	for _, tc := range tests {
		got := decodeProcfsEscape(tc.input)
		if got != tc.want {
			t.Errorf("decodeProcfsEscape(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestWSLCheckedMulUnit(t *testing.T) {
	// Safe product
	res, overflow := checkedMul(4096, 1000)
	if overflow || res != 4096000 {
		t.Fatalf("expected 4096000 (no overflow), got %d (overflow=%v)", res, overflow)
	}

	// Zero products
	res, overflow = checkedMul(0, 500)
	if overflow || res != 0 {
		t.Fatalf("expected 0, got %d", res)
	}
	res, overflow = checkedMul(500, 0)
	if overflow || res != 0 {
		t.Fatalf("expected 0, got %d", res)
	}

	// Overflow product
	_, overflow = checkedMul(^uint64(0), 2)
	if !overflow {
		t.Fatal("expected overflow for max uint64 * 2")
	}
}

func TestWSLRegistryInvalidByteEncodingFailsClosed(t *testing.T) {
	oldExecutor := wslRegistryQueryExecutor
	defer func() { wslRegistryQueryExecutor = oldExecutor }()

	// Registry contains null byte in string
	nullByteRegistry := "HKEY_CURRENT_USER\\Software\\Microsoft\\Windows\\CurrentVersion\\Lxss\\{11111111-2222-3333-4444-555555555555}\n" +
		"    DistributionName    REG_SZ    Ubuntu\x00corrupt\n" +
		"    BasePath            REG_SZ    E:\\WSL\\Ubuntu\n"

	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(nullByteRegistry), nil
	}

	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	_, err := resolveDistroBackingDrive(context.Background())
	if err == nil {
		t.Fatal("expected error on registry entry with embedded null byte")
	}
}

func createFakeStatBinary(t *testing.T, output string) {
	t.Helper()
	dir := t.TempDir()
	statPath := filepath.Join(dir, "stat")
	script := fmt.Sprintf("#!/bin/sh\necho %q\n", output)
	if err := os.WriteFile(statPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write fake stat binary: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestWSLProbeHostVolumeMultiplicationOverflowFailsClosed(t *testing.T) {
	// stat binary produces enormous block size causing checked multiplication overflow in actual probe
	createFakeStatBinary(t, "18446744073709551615 2 1 100 100")

	oldOverride := wslDetectionOverride
	oldMounts := wslProcMountsReader
	oldReg := wslRegistryQueryExecutor
	defer func() {
		wslDetectionOverride = oldOverride
		wslProcMountsReader = oldMounts
		wslRegistryQueryExecutor = oldReg
	}()

	isWSL := true
	wslDetectionOverride = &isWSL

	wslProcMountsReader = func() ([]byte, error) {
		return []byte(sampleProcMounts), nil
	}
	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(sampleLxssRegistryOutput), nil
	}
	t.Setenv("WSL_DISTRO_NAME", "Debian")

	guestCap := Capacity{
		FilesystemID: "guest:ext4",
		TotalBytes:   1073741824000,
		FreeBytes:    776875823104,
	}

	// Uses real default wslDriveStatFS calling probeHostVolumeCapacity
	_, err := boundWSLCapacity(guestCap, "/srv/kampe/Herdforge")
	if err == nil {
		t.Fatal("expected overflow probe error to fail closed")
	}
	if !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("expected overflow in error, got %q", err.Error())
	}
}

func TestWSLDirectProbeHostVolumeMultiplicationOverflow(t *testing.T) {
	ctx := context.Background()

	// Case 1: Total bytes multiplication overflow
	createFakeStatBinary(t, "18446744073709551615 2 1 100 100")
	_, err := probeHostVolumeCapacity(ctx, "/mnt/e")
	if err == nil {
		t.Fatal("expected probeHostVolumeCapacity to fail on total bytes multiplication overflow")
	}
	if !strings.Contains(err.Error(), "total bytes overflow") {
		t.Fatalf("expected 'total bytes overflow' in error, got %q", err.Error())
	}

	// There is deliberately NO "free bytes overflow" case. That branch is
	// defensively unreachable through this parser: freeBlocks > totalBlocks is
	// rejected first, so whenever blockSize*totalBlocks fits in uint64,
	// blockSize*freeBlocks fits too. Any input that would overflow the
	// free-bytes product trips the total-bytes guard above first. The former
	// Case 2 here ("10000000000000000000 3 2 ...") overflowed BOTH products,
	// only ever exercised the total-bytes guard, and claimed branch coverage
	// it could not have (review finding F1).

	// Case 2: Free blocks > total blocks
	createFakeStatBinary(t, "4096 100 200 100 100")
	_, err = probeHostVolumeCapacity(ctx, "/mnt/e")
	if err == nil {
		t.Fatal("expected probeHostVolumeCapacity to fail when freeBlocks > totalBlocks")
	}
	if !strings.Contains(err.Error(), "free blocks") {
		t.Fatalf("expected 'free blocks' in error, got %q", err.Error())
	}

	// Case 3: Zero block size
	createFakeStatBinary(t, "0 100 50 100 100")
	_, err = probeHostVolumeCapacity(ctx, "/mnt/e")
	if err == nil {
		t.Fatal("expected probeHostVolumeCapacity to fail when blockSize == 0")
	}
	if !strings.Contains(err.Error(), "zero block size") {
		t.Fatalf("expected 'zero block size' in error, got %q", err.Error())
	}
}

// createStallingFakeStatBinary installs a fake `stat` on PATH whose foreground
// sleep child inherits the probe's stdout/stderr pipes — the hermetic stand-in
// for a stat wedged in an uninterruptible 9p wait, where SIGKILL on the direct
// child is not enough to unblock the caller's pipe reads. The stall is
// bounded: the fixture always exits on its own after stallSeconds even if
// nothing kills it, so no mutant can deadlock the suite. The script records
// the shell's PID so cleanup can kill only this test's own child.
func createStallingFakeStatBinary(t *testing.T, stallSeconds int) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "stat.pid")
	statPath := filepath.Join(dir, "stat")
	script := fmt.Sprintf("#!/bin/sh\necho $$ > %q\nsleep %d\necho '4096 100 50 100 100'\n", pidFile, stallSeconds)
	if err := os.WriteFile(statPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write stalling fake stat binary: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return // fixture never ran
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 1 {
			return
		}
		// Own-child cleanup only: this pidfile is written solely by the script
		// this test installed, and cleanup runs moments after the spawn, so
		// PID reuse is not a realistic hazard. Best-effort — the stall is
		// bounded regardless.
		if proc, findErr := os.FindProcess(pid); findErr == nil {
			_ = proc.Kill()
		}
	})
}

func TestWSLProbeHostVolumeSubprocessTimeoutBounded(t *testing.T) {
	// F2 repair: proves acceptance criterion 2 (bounded subprocess probe) at
	// the production seam. The fake stat stalls far past the context deadline;
	// the real probeHostVolumeCapacity must return a classified timeout error
	// and a zero Capacity near the deadline, not after the stall.
	//
	// Verified-RED mutants (each watched failing before this landed):
	//  - exec.CommandContext -> exec.Command: the probe blocks until the child
	//    exits on its own; the OUTER watchdog below fails the test explicitly.
	//  - cmd.WaitDelay removed (the pre-repair production shape): the killed
	//    shell's descendant keeps the stdout pipe open, Run blocks the caller
	//    for the full stall despite the deadline; the watchdog fails the test.
	//  - ctx.Err() classification branch removed: the error loses "timed out"
	//    and the assertion below fails.
	const (
		probeDeadline = 300 * time.Millisecond
		stallSeconds  = 10 // bounded child fixture: always exits on its own
		watchdog      = 5 * time.Second
	)
	createStallingFakeStatBinary(t, stallSeconds)

	ctx, cancel := context.WithTimeout(context.Background(), probeDeadline)
	defer cancel()

	type probeResult struct {
		cap Capacity
		err error
	}
	// Buffered so the probe goroutine can never block after a watchdog failure.
	done := make(chan probeResult, 1)
	go func() {
		c, err := probeHostVolumeCapacity(ctx, ".")
		done <- probeResult{cap: c, err: err}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("expected timeout error from stalled probe, got capacity %+v", r.cap)
		}
		if !strings.Contains(r.err.Error(), "timed out") {
			t.Fatalf("expected classified 'timed out' probe error, got %q", r.err.Error())
		}
		if r.cap != (Capacity{}) {
			t.Fatalf("expected zero Capacity on timeout, got %+v", r.cap)
		}
	case <-time.After(watchdog):
		t.Fatalf("WATCHDOG: probeHostVolumeCapacity did not return within %v (deadline %v, child stall bounded at %ds) — the subprocess bound does not bound the caller", watchdog, probeDeadline, stallSeconds)
	}
}

func TestOSBackendWSLZeroHostCapacityFailsClosed(t *testing.T) {
	// F3: hostCap.TotalBytes == 0 is unreachable through the real probe — it
	// rejects zero block size and zero total blocks before multiplying — so
	// the guard in boundWSLCapacity is seam-level defensive depth. Exercise it
	// at the production OSBackend entry point via an injected host statfs, so
	// removing the guard (which would silently cap capacity to 0/0 with a nil
	// error) fails this test instead of surviving.
	oldOverride := wslDetectionOverride
	oldMounts := wslProcMountsReader
	oldReg := wslRegistryQueryExecutor
	oldStatFS := wslDriveStatFS
	defer func() {
		wslDetectionOverride = oldOverride
		wslProcMountsReader = oldMounts
		wslRegistryQueryExecutor = oldReg
		wslDriveStatFS = oldStatFS
	}()

	isWSL := true
	wslDetectionOverride = &isWSL

	wslProcMountsReader = func() ([]byte, error) {
		return []byte(sampleProcMounts), nil
	}
	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(sampleLxssRegistryOutput), nil
	}
	t.Setenv("WSL_DISTRO_NAME", "Debian")

	wslDriveStatFS = func(ctx context.Context, mountPath string) (Capacity, error) {
		return Capacity{FilesystemID: "host:c"}, nil // zero TotalBytes, nil error
	}

	_, err := (OSBackend{}).StatFS(".")
	if err == nil {
		t.Fatal("expected zero host capacity to fail closed")
	}
	if !strings.Contains(err.Error(), "invalid zero capacity") {
		t.Fatalf("expected 'invalid zero capacity' error, got %q", err.Error())
	}
}

func TestWSLHostStatFSCancelledProbeFailsClosed(t *testing.T) {
	oldOverride := wslDetectionOverride
	oldMounts := wslProcMountsReader
	oldReg := wslRegistryQueryExecutor
	oldStatFS := wslDriveStatFS
	defer func() {
		wslDetectionOverride = oldOverride
		wslProcMountsReader = oldMounts
		wslRegistryQueryExecutor = oldReg
		wslDriveStatFS = oldStatFS
	}()

	isWSL := true
	wslDetectionOverride = &isWSL

	wslProcMountsReader = func() ([]byte, error) {
		return []byte(sampleProcMounts), nil
	}
	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(sampleLxssRegistryOutput), nil
	}
	t.Setenv("WSL_DISTRO_NAME", "Debian")

	// Host volume probe fails with context cancellation
	wslDriveStatFS = func(ctx context.Context, mountPath string) (Capacity, error) {
		return Capacity{}, fmt.Errorf("host volume probe timed out: %w", context.Canceled)
	}

	guestCap := Capacity{
		FilesystemID: "guest:ext4",
		TotalBytes:   1073741824000,
		FreeBytes:    776875823104,
	}

	_, err := boundWSLCapacity(guestCap, "/srv/kampe/Herdforge")
	if err == nil {
		t.Fatal("expected cancelled probe to fail closed")
	}
	if !strings.Contains(err.Error(), "host volume probe timed out") {
		t.Fatalf("expected timeout/cancellation error, got %q", err.Error())
	}
}

func TestWSLFindDriveMountPathHermetic(t *testing.T) {
	mounts := []byte(sampleProcMounts)

	// Check D: -> /mnt/d
	mountD, err := findDriveMountPath("D:", mounts)
	if err != nil {
		t.Fatalf("unexpected error finding mount for D:: %v", err)
	}
	if mountD != "/mnt/d" {
		t.Fatalf("expected /mnt/d, got %q", mountD)
	}

	// Check E: -> /mnt/e
	mountE, err := findDriveMountPath("E:", mounts)
	if err != nil {
		t.Fatalf("unexpected error finding mount for E:: %v", err)
	}
	if mountE != "/mnt/e" {
		t.Fatalf("expected /mnt/e, got %q", mountE)
	}

	// Check unmounted G:
	_, err = findDriveMountPath("G:", mounts)
	if err == nil {
		t.Fatal("expected error for unmounted drive G:")
	}
}

func TestWSLHostVolumeCappingGuestFreeGreaterThanHostFree(t *testing.T) {
	oldOverride := wslDetectionOverride
	oldMounts := wslProcMountsReader
	oldReg := wslRegistryQueryExecutor
	oldStatFS := wslDriveStatFS
	defer func() {
		wslDetectionOverride = oldOverride
		wslProcMountsReader = oldMounts
		wslRegistryQueryExecutor = oldReg
		wslDriveStatFS = oldStatFS
	}()

	isWSL := true
	wslDetectionOverride = &isWSL

	wslProcMountsReader = func() ([]byte, error) {
		return []byte(sampleProcMounts), nil
	}
	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(sampleLxssRegistryOutput), nil
	}

	t.Setenv("WSL_DISTRO_NAME", "Debian") // Backed by C:

	// Guest ext4 reports 776 GB free out of 1 TB
	guestCap := Capacity{
		FilesystemID: "guest:ext4",
		TotalBytes:   1073741824000,
		FreeBytes:    776875823104,
		TotalInodes:  1000000,
		FreeInodes:   900000,
	}

	// Physical Windows C: drive has only 14,308,425,728 bytes free (~13.3 GB) out of 1 TB
	wslDriveStatFS = func(ctx context.Context, mountPath string) (Capacity, error) {
		if mountPath != "/mnt/e" {
			t.Fatalf("expected statfs on /mnt/e, got %q", mountPath)
		}
		return Capacity{
			FilesystemID: "host:c",
			TotalBytes:   1000000000000,
			FreeBytes:    14308425728,
			TotalInodes:  5000000,
			FreeInodes:   4000000,
		}, nil
	}

	// Test bounding logic on virtual VHD path (/srv/kampe/Herdforge)
	bounded, err := boundWSLCapacity(guestCap, "/srv/kampe/Herdforge")
	if err != nil {
		t.Fatalf("unexpected error bounding WSL capacity: %v", err)
	}

	if bounded.FreeBytes != 14308425728 {
		t.Fatalf("expected FreeBytes capped to host free bytes 14308425728, got %d", bounded.FreeBytes)
	}
	if bounded.TotalBytes != 1000000000000 {
		t.Fatalf("expected TotalBytes capped to host total bytes 1000000000000, got %d", bounded.TotalBytes)
	}
}

func TestWSLHostVolumeCappingHostFreeGreaterThanGuestFree(t *testing.T) {
	oldOverride := wslDetectionOverride
	oldMounts := wslProcMountsReader
	oldReg := wslRegistryQueryExecutor
	oldStatFS := wslDriveStatFS
	defer func() {
		wslDetectionOverride = oldOverride
		wslProcMountsReader = oldMounts
		wslRegistryQueryExecutor = oldReg
		wslDriveStatFS = oldStatFS
	}()

	isWSL := true
	wslDetectionOverride = &isWSL

	wslProcMountsReader = func() ([]byte, error) {
		return []byte(sampleProcMounts), nil
	}
	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(sampleLxssRegistryOutput), nil
	}

	t.Setenv("WSL_DISTRO_NAME", "Ubuntu-24.04") // Backed by D:

	// Guest ext4 reports 50 GB free out of 100 GB
	guestCap := Capacity{
		FilesystemID: "guest:ext4",
		TotalBytes:   100000000000,
		FreeBytes:    50000000000,
		TotalInodes:  1000000,
		FreeInodes:   900000,
	}

	// Physical Windows D: drive has 500 GB free out of 1 TB
	wslDriveStatFS = func(ctx context.Context, mountPath string) (Capacity, error) {
		if mountPath != "/mnt/d" {
			t.Fatalf("expected statfs on /mnt/d, got %q", mountPath)
		}
		return Capacity{
			FilesystemID: "host:d",
			TotalBytes:   1000000000000,
			FreeBytes:    500000000000,
			TotalInodes:  5000000,
			FreeInodes:   4000000,
		}, nil
	}

	bounded, err := boundWSLCapacity(guestCap, "/srv/kampe/Herdforge")
	if err != nil {
		t.Fatalf("unexpected error bounding WSL capacity: %v", err)
	}

	// Guest free bytes (50 GB) is smaller than host free (500 GB), so guest free is preserved
	if bounded.FreeBytes != 50000000000 {
		t.Fatalf("expected FreeBytes preserved at guest free 50000000000, got %d", bounded.FreeBytes)
	}
}

func TestWSLDrvFSMountPathBypassesVHDHostCapping(t *testing.T) {
	oldOverride := wslDetectionOverride
	oldMounts := wslProcMountsReader
	defer func() {
		wslDetectionOverride = oldOverride
		wslProcMountsReader = oldMounts
	}()

	isWSL := true
	wslDetectionOverride = &isWSL

	wslProcMountsReader = func() ([]byte, error) {
		return []byte(sampleProcMounts), nil
	}

	guestCap := Capacity{
		FilesystemID: "drvfs:c",
		TotalBytes:   1000000000000,
		FreeBytes:    14308425728,
		TotalInodes:  5000000,
		FreeInodes:   4000000,
	}

	// Path directly on /mnt/e/Users/... already queries Windows volume
	bounded, err := boundWSLCapacity(guestCap, "/mnt/e/Users/kampe/Herdforge")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bounded.FreeBytes != 14308425728 {
		t.Fatalf("expected FreeBytes unchanged at 14308425728, got %d", bounded.FreeBytes)
	}
}

func TestWSLProbeErrorFailsClosedHermetic(t *testing.T) {
	oldOverride := wslDetectionOverride
	oldReg := wslRegistryQueryExecutor
	defer func() {
		wslDetectionOverride = oldOverride
		wslRegistryQueryExecutor = oldReg
	}()

	isWSL := true
	wslDetectionOverride = &isWSL

	// Registry query fails with timeout / process error
	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return nil, errors.New("timeout executing reg.exe")
	}

	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	t.Setenv("SYSTEMDRIVE", "")

	guestCap := Capacity{
		FilesystemID: "guest:ext4",
		TotalBytes:   1073741824000,
		FreeBytes:    776875823104,
		TotalInodes:  1000000,
		FreeInodes:   900000,
	}

	_, err := boundWSLCapacity(guestCap, "/srv/kampe/Herdforge")
	if err == nil {
		t.Fatal("expected boundWSLCapacity to fail closed when registry query fails")
	}
	if !strings.Contains(err.Error(), "wsl host backing volume detection") {
		t.Fatalf("expected error mentioning wsl host backing volume detection, got %q", err.Error())
	}
}

func TestWSLMalformedRegistryOutputFailsClosedHermetic(t *testing.T) {
	oldOverride := wslDetectionOverride
	oldReg := wslRegistryQueryExecutor
	defer func() {
		wslDetectionOverride = oldOverride
		wslRegistryQueryExecutor = oldReg
	}()

	isWSL := true
	wslDetectionOverride = &isWSL

	// Registry returns empty / garbage
	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte("GARBAGE OUTPUT WITHOUT LXSS KEYS"), nil
	}

	t.Setenv("WSL_DISTRO_NAME", "Ubuntu-24.04")
	t.Setenv("SYSTEMDRIVE", "")

	guestCap := Capacity{
		FilesystemID: "guest:ext4",
		TotalBytes:   1073741824000,
		FreeBytes:    776875823104,
		TotalInodes:  1000000,
		FreeInodes:   900000,
	}

	_, err := boundWSLCapacity(guestCap, "/srv/kampe/Herdforge")
	if err == nil {
		t.Fatal("expected error on malformed registry output")
	}
}

func TestNativeDarwinLinuxNoInteropHermetic(t *testing.T) {
	oldOverride := wslDetectionOverride
	oldReg := wslRegistryQueryExecutor
	defer func() {
		wslDetectionOverride = oldOverride
		wslRegistryQueryExecutor = oldReg
	}()

	isWSL := false
	wslDetectionOverride = &isWSL

	var registryCalled bool
	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		registryCalled = true
		return nil, errors.New("forbidden call on native platform")
	}

	guestCap := Capacity{
		FilesystemID: "native:fs",
		TotalBytes:   500000000000,
		FreeBytes:    200000000000,
		TotalInodes:  1000000,
		FreeInodes:   900000,
	}

	bounded, err := boundWSLCapacity(guestCap, "/srv/kampe/Herdforge")
	if err != nil {
		t.Fatalf("unexpected error on native platform: %v", err)
	}
	if registryCalled {
		t.Fatal("registry query executor should never be called on native non-WSL platform")
	}
	if bounded.FreeBytes != 200000000000 {
		t.Fatalf("expected FreeBytes unchanged, got %d", bounded.FreeBytes)
	}
}

func TestWSLProductionRegressionPhysicalBoundIgnoredMustBeRED(t *testing.T) {
	// Demonstrates that evaluating disk admission using guest-only ext4 capacity (776 GB)
	// falsely ADMITS disk-heavy operations (e.g. 20 GB + 15 GB reserve = 35 GB required)
	// when the physical Windows host volume only has 14.3 GB free.
	//
	// Under the WSL physical bound fix, FreeBytes is capped to 14.3 GB, which causes
	// EvaluateDiskCapacity to correctly BLOCK the operation with reason "below_threshold".

	oldOverride := wslDetectionOverride
	oldMounts := wslProcMountsReader
	oldReg := wslRegistryQueryExecutor
	oldStatFS := wslDriveStatFS
	defer func() {
		wslDetectionOverride = oldOverride
		wslProcMountsReader = oldMounts
		wslRegistryQueryExecutor = oldReg
		wslDriveStatFS = oldStatFS
	}()

	isWSL := true
	wslDetectionOverride = &isWSL

	wslProcMountsReader = func() ([]byte, error) {
		return []byte(sampleProcMounts), nil
	}
	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(sampleLxssRegistryOutput), nil
	}
	t.Setenv("WSL_DISTRO_NAME", "Debian") // Backed by C:

	// Physical Windows C: drive has only 14,308,425,728 bytes free (~13.3 GB)
	wslDriveStatFS = func(ctx context.Context, mountPath string) (Capacity, error) {
		return Capacity{
			FilesystemID: "host:c",
			TotalBytes:   1000000000000,
			FreeBytes:    14308425728,
			TotalInodes:  5000000,
			FreeInodes:   4000000,
		}, nil
	}

	// Guest ext4 reports 776 GB free
	guestCap := Capacity{
		FilesystemID: "guest:ext4",
		TotalBytes:   1073741824000,
		FreeBytes:    776875823104,
		TotalInodes:  1000000,
		FreeInodes:   900000,
	}

	policy := DefaultDiskPolicy() // 15 GB reserve
	request := DiskRequest{
		Operation:      "harvest_worktree",
		Path:           "/srv/kampe/Herdforge",
		RequiredBytes:  20 * (1 << 30), // 20 GB
		RequiredInodes: 100,
	}

	// 1. Without physical host volume bound (simulating regression where boundWSLCapacity is bypassed):
	uncappedBackend := StatFSFunc(func(p string) (Capacity, error) {
		return guestCap, nil // ignores physical host volume
	})
	uncappedDecision := EvaluateDiskCapacity(uncappedBackend, request, policy)
	if !uncappedDecision.Allowed {
		t.Fatal("expected uncapped guest capacity to falsely ALLOW operation despite physical disk exhaustion")
	}

	// 2. With physical host volume bound:
	cappedBackend := StatFSFunc(func(p string) (Capacity, error) {
		return boundWSLCapacity(guestCap, p)
	})
	cappedDecision := EvaluateDiskCapacity(cappedBackend, request, policy)
	if cappedDecision.Allowed {
		t.Fatalf("expected physical host bound to BLOCK operation; got allowed with free bytes %d", cappedDecision.Evidence.FreeBytes)
	}
	if cappedDecision.Evidence.Reason != DiskReasonBelowThreshold {
		t.Fatalf("expected reason %q, got %q", DiskReasonBelowThreshold, cappedDecision.Evidence.Reason)
	}
	if cappedDecision.Evidence.FreeBytes != 14308425728 {
		t.Fatalf("expected capped free bytes 14308425728, got %d", cappedDecision.Evidence.FreeBytes)
	}
}

func TestOSBackendWSLPhysicalBoundProductionRegressionRED(t *testing.T) {
	// Exercises the real OSBackend.StatFS -> boundWSLCapacity wiring in a WSL
	// environment with SYNTHETIC guest and host capacities (FAC-810), so the
	// assertions hold on every host regardless of live disk space. When the
	// production code includes boundWSLCapacity, StatFS caps FreeBytes to
	// min(guest, host). Under the regression (bypassing boundWSLCapacity in
	// OSBackend.StatFS), StatFS returns the raw guest FreeBytes.

	oldOverride := wslDetectionOverride
	oldMounts := wslProcMountsReader
	oldReg := wslRegistryQueryExecutor
	oldHost := wslDriveStatFS
	defer func() {
		wslDetectionOverride = oldOverride
		wslProcMountsReader = oldMounts
		wslRegistryQueryExecutor = oldReg
		wslDriveStatFS = oldHost
	}()

	isWSL := true
	wslDetectionOverride = &isWSL

	wslProcMountsReader = func() ([]byte, error) {
		return []byte(sampleProcMounts), nil
	}
	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(sampleLxssRegistryOutput), nil
	}
	t.Setenv("WSL_DISTRO_NAME", "Debian") // Backed by C:

	const hostFree = uint64(14308425728)
	const hostTotal = uint64(1000000000000)
	wslDriveStatFS = func(ctx context.Context, mountPath string) (Capacity, error) {
		return Capacity{
			FilesystemID: "host:c",
			TotalBytes:   hostTotal,
			FreeBytes:    hostFree,
			TotalInodes:  5000000,
			FreeInodes:   4000000,
		}, nil
	}

	cases := []struct {
		name       string
		guest      Capacity
		probeErr   error
		wantFree   uint64
		wantTotal  uint64
		wantErr    bool
		regression bool
	}{
		{
			name:       "guest above host is capped to host free bytes",
			guest:      Capacity{FilesystemID: "guest", TotalBytes: 776000000000, FreeBytes: 776000000000, TotalInodes: 5000000, FreeInodes: 4000000},
			wantFree:   hostFree,
			wantTotal:  776000000000,
			regression: true,
		},
		{
			name:      "guest below host keeps guest free bytes",
			guest:     Capacity{FilesystemID: "guest", TotalBytes: 20000000000, FreeBytes: 5000000000, TotalInodes: 5000000, FreeInodes: 4000000},
			wantFree:  5000000000,
			wantTotal: 20000000000,
		},
		{
			name:      "zero guest free stays zero",
			guest:     Capacity{FilesystemID: "guest", TotalBytes: 20000000000, FreeBytes: 0, TotalInodes: 5000000, FreeInodes: 0},
			wantFree:  0,
			wantTotal: 20000000000,
		},
		{
			name:     "guest probe error fails closed",
			probeErr: errors.New("guest probe unavailable"),
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := withGuestStatFSProbe(func(path string) (Capacity, error) {
				if tc.probeErr != nil {
					return Capacity{}, tc.probeErr
				}
				return tc.guest, nil
			})
			t.Cleanup(restore)

			cap, err := (OSBackend{}).StatFS(".")
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected guest probe error to fail closed")
				}
				return
			}
			if err != nil {
				t.Fatalf("StatFS failed: %v", err)
			}
			if tc.regression && cap.FreeBytes > hostFree {
				t.Fatalf("REGRESSION DETECTED: OSBackend.StatFS returned uncapped free bytes %d > host free bytes %d", cap.FreeBytes, hostFree)
			}
			if cap.FreeBytes != tc.wantFree {
				t.Fatalf("expected FreeBytes = %d, got %d", tc.wantFree, cap.FreeBytes)
			}
			if cap.TotalBytes != tc.wantTotal {
				t.Fatalf("expected TotalBytes = %d, got %d", tc.wantTotal, cap.TotalBytes)
			}
		})
	}
}

func TestExtractDriveLetter(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"E:\\Users\\test", "E:"},
		{"d:\\wsl\\ubuntu", "D:"},
		{"E:\\", "E:"},
		{"\\\\?\\F:\\WSL", "F:"},
		{"/mnt/e", ""},
		{"", ""},
		{"relative\\path", ""},
	}
	for _, tc := range tests {
		got := extractDriveLetter(tc.input)
		if got != tc.want {
			t.Errorf("extractDriveLetter(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
