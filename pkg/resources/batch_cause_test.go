package resources

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// FAC-809: a per-pid reference-probe error must surface its cause on the
// returned usage, not vanish behind a bare MetadataUnavailable flag.
func TestInUseManyRetainsPerPIDProbeCause(t *testing.T) {
	var walked bool
	insp := LSOFProcessInspector{
		Timeout: 2 * time.Second,
		processReferencesManyFn: func(ctx context.Context, pid int, paths []string, owners map[int]int) (map[string]bool, error) {
			if walked {
				refs := make(map[string]bool, len(paths))
				return refs, nil
			}
			walked = true
			return nil, errors.New("argv snapshot unavailable")
		},
	}
	usage, err := insp.InUseMany(context.Background(), []string{t.TempDir()})
	if err != nil {
		t.Fatalf("per-pid probe errors are not batch errors: %v", err)
	}
	if !walked {
		t.Fatal("probe seam never ran")
	}
	found := false
	for _, u := range usage {
		if u.MetadataUnavailable && strings.Contains(u.MetadataCause, "reference probe: argv snapshot unavailable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("metadata cause must retain the per-pid probe error: %+v", usage)
	}
}
