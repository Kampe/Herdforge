package harvest

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/preflight"
)

// TestMain disables the disk-pressure floors so existing integration tests
// stay hermetic on a pressured host (FAC-153 incident host sat at 99%).
// Guard-assertion tests re-enable floors via t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv(preflight.EnvDiskMinFreeGB, "0")
	os.Setenv(preflight.EnvDiskMinFreePct, "0")
	os.Setenv(preflight.EnvDiskMinInodePct, "0")
	os.Exit(m.Run())
}

func TestIntegrationRunRefusesUnderDiskPressure(t *testing.T) {
	// 1 ZiB floor: any real volume reads as critically low.
	t.Setenv(preflight.EnvDiskMinFreeGB, "1099511627776")

	// nil Harvester proves ordering: the guard must refuse before phase 1
	// ever runs, otherwise this test would panic on a nil dereference.
	in := &Integration{RepoRoot: t.TempDir()}
	res, err := in.Run(context.Background())
	if err == nil {
		t.Fatal("expected fail-closed refusal under disk pressure")
	}
	if !strings.Contains(err.Error(), "disk_pressure") {
		t.Fatalf("expected structured disk_pressure evidence, got: %v", err)
	}
	if res != nil {
		t.Fatalf("expected nil result on refusal, got %+v", res)
	}
}
