package security

import (
	"context"
	"testing"
)

func TestSecurityScanner_ScanDiff(t *testing.T) {
	scanner := NewSecurityScanner()

	cleanDiff := "diff --git a/main.go b/main.go\n+func main() { fmt.Println(\"ok\") }"
	res := scanner.ScanDiff(context.Background(), cleanDiff)
	if !res.Passed {
		t.Errorf("expected clean diff to pass scanner")
	}

	leakyDiff := "diff --git a/config.go b/config.go\n+api_key := \"sk_live_1234567890abcdef123456\""
	resLeaky := scanner.ScanDiff(context.Background(), leakyDiff)
	if resLeaky.Passed || len(resLeaky.Findings) == 0 {
		t.Errorf("expected leaky diff to fail scanner with secret finding")
	}
}
