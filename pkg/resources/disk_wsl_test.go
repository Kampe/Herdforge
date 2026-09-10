package resources

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
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
    BasePath            REG_SZ    C:\Users\kampe\AppData\Local\Packages\TheDebianProject_76v4gfsz1904\LocalState
    Version             REG_DWORD    0x2
`

const sampleProcMounts = `
rootfs / rootfs rw 0 0
none /dev devtmpfs rw,nosuid,relatime,size=16335340k,nr_inodes=4083835,mode=755 0 0
/dev/sdb / ext4 rw,relatime,discard,errors=remount-ro,data=ordered 0 0
C:\ /mnt/c 9p rw,noatime,dirsync,aname=drvfs;path=C:\;uid=1000;gid=1000;symlinkroot=/mnt/,mmap,access=client,msize=65536,trans=fd,rfd=8,wfd=8 0 0
D:\ /mnt/d 9p rw,noatime,dirsync,aname=drvfs;path=D:\;uid=1000;gid=1000;symlinkroot=/mnt/,mmap,access=client,msize=65536,trans=fd,rfd=8,wfd=8 0 0
`

func TestWSLDetectionHermetic(t *testing.T) {
	oldReader := wslProcVersionReader
	oldOverride := wslDetectionOverride
	oldGOOS := wslRuntimeGOOS
	defer func() {
		wslProcVersionReader = oldReader
		wslDetectionOverride = oldOverride
		wslRuntimeGOOS = oldGOOS
	}()
	wslDetectionOverride = nil
	wslRuntimeGOOS = "linux"

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
	t.Setenv("WSL_DISTRO_NAME", "")
	t.Setenv("WSL_INTEROP", "")
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

	// Case 2: WSL_DISTRO_NAME matches Debian on C:
	t.Setenv("WSL_DISTRO_NAME", "Debian")
	drive, err = resolveDistroBackingDrive(context.Background())
	if err != nil {
		t.Fatalf("unexpected error resolving distro drive: %v", err)
	}
	if drive != "C:" {
		t.Fatalf("expected drive C: for Debian, got %q", drive)
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

	// Default distro in registry is Ubuntu-24.04 (D:), but running distro is Debian (C:)
	t.Setenv("WSL_DISTRO_NAME", "Debian")
	drive, err := resolveDistroBackingDrive(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drive != "C:" {
		t.Fatalf("expected running distro drive C:, got %q (must not substitute default D:)", drive)
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

	// WSL_DISTRO_NAME is empty and there are 2 distros on different drives (D: and C:).
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
    BasePath            REG_SZ    C:\WSL\Ubuntu
`
	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		return []byte(singleDistroRegistry), nil
	}

	t.Setenv("WSL_DISTRO_NAME", "")
	drive, err := resolveDistroBackingDrive(context.Background())
	if err != nil {
		t.Fatalf("unexpected error for single registered distro: %v", err)
	}
	if drive != "C:" {
		t.Fatalf("expected drive C:, got %q", drive)
	}
}

func TestWSLAtypicalProcVersionInspectsSecondarySignals(t *testing.T) {
	oldReader := wslProcVersionReader
	oldOverride := wslDetectionOverride
	oldGOOS := wslRuntimeGOOS
	defer func() {
		wslProcVersionReader = oldReader
		wslDetectionOverride = oldOverride
		wslRuntimeGOOS = oldGOOS
	}()
	wslDetectionOverride = nil
	wslRuntimeGOOS = "linux"

	// Custom Linux kernel version text without "microsoft" or "wsl"
	wslProcVersionReader = func() ([]byte, error) {
		return []byte("Linux version 6.18.0-custom (user@build) #1 SMP PREEMPT\n"), nil
	}

	// 1. Without secondary signals -> false
	t.Setenv("WSL_DISTRO_NAME", "")
	t.Setenv("WSL_INTEROP", "")
	if isWSLEnvironment() {
		t.Fatal("expected false for atypical kernel without secondary signals")
	}

	// 2. With WSL_DISTRO_NAME secondary signal -> true
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	if !isWSLEnvironment() {
		t.Fatal("expected true for atypical kernel with WSL_DISTRO_NAME set")
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
	if !strings.Contains(err.Error(), "unsupported registry type") {
		t.Fatalf("expected error mentioning unsupported registry type, got %q", err.Error())
	}
}

func TestWSLFindDriveMountPathRejectsMountSpoof(t *testing.T) {
	// Mounts table contains spoofed ext4/tmpfs entries ending in /c,
	// and one authentic 9p DrvFS mount at /media/c.
	spoofedMounts := `
rootfs / rootfs rw 0 0
/dev/sdb /mnt/c ext4 rw,relatime 0 0
tmpfs /var/log/c tmpfs rw,relatime 0 0
C:\134 /media/c 9p rw,noatime,aname=drvfs;path=C:\;uid=1000;gid=1000,access=client 0 0
`
	mountC, err := findDriveMountPath("C:", []byte(spoofedMounts))
	if err != nil {
		t.Fatalf("unexpected error finding mount for C:: %v", err)
	}
	// Must reject ext4 /mnt/c and choose genuine 9p /media/c
	if mountC != "/media/c" {
		t.Fatalf("expected authentic 9p mount /media/c, got spoofed %q", mountC)
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

	_, err := boundWSLCapacity(guestCap, "/home/kampe/Herdforge")
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

	// Check C: -> /mnt/c
	mountC, err := findDriveMountPath("C:", mounts)
	if err != nil {
		t.Fatalf("unexpected error finding mount for C:: %v", err)
	}
	if mountC != "/mnt/c" {
		t.Fatalf("expected /mnt/c, got %q", mountC)
	}

	// Check unmounted E:
	_, err = findDriveMountPath("E:", mounts)
	if err == nil {
		t.Fatal("expected error for unmounted drive E:")
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
		if mountPath != "/mnt/c" {
			t.Fatalf("expected statfs on /mnt/c, got %q", mountPath)
		}
		return Capacity{
			FilesystemID: "host:c",
			TotalBytes:   1000000000000,
			FreeBytes:    14308425728,
			TotalInodes:  5000000,
			FreeInodes:   4000000,
		}, nil
	}

	// Test bounding logic on virtual VHD path (/home/kampe/Herdforge)
	bounded, err := boundWSLCapacity(guestCap, "/home/kampe/Herdforge")
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

	bounded, err := boundWSLCapacity(guestCap, "/home/kampe/Herdforge")
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

	// Path directly on /mnt/c/Users/... already queries Windows volume
	bounded, err := boundWSLCapacity(guestCap, "/mnt/c/Users/kampe/Herdforge")
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

	_, err := boundWSLCapacity(guestCap, "/home/kampe/Herdforge")
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

	_, err := boundWSLCapacity(guestCap, "/home/kampe/Herdforge")
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

	bounded, err := boundWSLCapacity(guestCap, "/Users/kampe/Herdforge")
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
		Path:           "/home/kampe/Herdforge",
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
	// Directly tests production OSBackend.StatFS in a WSL environment.
	// When the production code includes boundWSLCapacity, StatFS caps FreeBytes to the host volume (14.3 GB).
	// Under the regression (bypassing boundWSLCapacity in OSBackend.StatFS), StatFS returns uncapped guest FreeBytes (776 GB).

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

	// Physical Windows C: drive has 14,308,425,728 bytes free
	wslDriveStatFS = func(ctx context.Context, mountPath string) (Capacity, error) {
		return Capacity{
			FilesystemID: "host:c",
			TotalBytes:   1000000000000,
			FreeBytes:    14308425728,
			TotalInodes:  5000000,
			FreeInodes:   4000000,
		}, nil
	}

	cap, err := (OSBackend{}).StatFS(".")
	if err != nil {
		t.Fatalf("StatFS failed: %v", err)
	}

	// In WSL, guest FreeBytes must be capped to host FreeBytes (14.3 GB), never uncapped guest free
	if cap.FreeBytes > 14308425728 {
		t.Fatalf("REGRESSION DETECTED: OSBackend.StatFS returned uncapped free bytes %d > host free bytes 14308425728", cap.FreeBytes)
	}
	if cap.FreeBytes != 14308425728 {
		t.Fatalf("expected FreeBytes = 14308425728, got %d", cap.FreeBytes)
	}
}

func TestExtractDriveLetter(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"C:\\Users\\test", "C:"},
		{"d:\\wsl\\ubuntu", "D:"},
		{"E:\\", "E:"},
		{"\\\\?\\F:\\WSL", "F:"},
		{"/mnt/c", ""},
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
