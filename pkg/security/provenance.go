package security

import (
	"fmt"
	"strconv"
	"strings"
)

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ValidateTaskRef rejects empty or fabricated placeholder task provenance.
func ValidateTaskRef(taskRef string) error {
	taskRef = strings.TrimSpace(taskRef)
	if taskRef == "" {
		return fmt.Errorf("%w: TaskRef required (empty provenance fail-closed)", ErrUnknownPolicy)
	}
	// Fabricated placeholders are forbidden.
	switch strings.ToLower(taskRef) {
	case "cli-launch", "unknown", "none", "null", "todo":
		return fmt.Errorf("%w: TaskRef %q is a fabricated placeholder", ErrUnknownPolicy, taskRef)
	}
	return nil
}

// ValidateLeaseGeneration performs syntactic checks only.
// Task launches MUST also call ValidateLiveTaskLease against FAC-147 records.
func ValidateLeaseGeneration(lease string, standingOK bool) error {
	lease = strings.TrimSpace(lease)
	if lease == "" || lease == "0" {
		return fmt.Errorf("%w: LeaseGeneration required (live claim/control lease)", ErrUnknownPolicy)
	}
	if strings.HasPrefix(lease, "standing:") {
		if !standingOK {
			return fmt.Errorf("%w: standing lease not allowed for task launch", ErrUnknownPolicy)
		}
		name := strings.TrimSpace(strings.TrimPrefix(lease, "standing:"))
		if name == "" {
			return fmt.Errorf("%w: standing lease missing name", ErrUnknownPolicy)
		}
		return nil
	}
	n, err := strconv.ParseInt(lease, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: LeaseGeneration %q must be positive integer claim generation or standing:<name>", ErrUnknownPolicy, lease)
	}
	if n <= 0 {
		return fmt.Errorf("%w: LeaseGeneration must be >0", ErrUnknownPolicy)
	}
	return nil
}
