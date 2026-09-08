package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveShareableWorkerBroker_ValidStandaloneInjectsWorkerOnly(t *testing.T) {
	up := newAuthBoard()
	up.rejectUnfenced = false
	srv := up.serveUnfenced()
	t.Cleanup(srv.Close)

	claimDir := t.TempDir()
	t.Setenv("HERD_CLAIM_DIR", claimDir)
	t.Setenv(envFenceCoordinator, "")
	t.Setenv(envFenceBrokerMintToken, "")
	ProvisionSharedFenceForTest(t, claimDir)
	b := startTestBroker(t, srv.URL, claimDir)
	t.Setenv(envFenceBrokerURL, b.ClientBaseURL())
	t.Setenv(envFenceBrokerToken, testBrokerToken)

	cap, err := ResolveShareableWorkerBroker(context.Background(), claimDir)
	if err != nil {
		t.Fatalf("valid standalone broker must resolve: %v", err)
	}
	if cap.URL != b.ClientBaseURL() || cap.Token != testBrokerToken {
		t.Fatalf("resolved capability mismatch url=%q", cap.URL)
	}

	lane := t.TempDir()
	if err := WriteShareableWorkerBrokerEnv(lane, cap); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(ConfidentialWorkerBrokerEnvFile(lane))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, envFenceBrokerURL+"="+b.ClientBaseURL()) {
		t.Fatalf("confidential env missing worker URL: %s", body)
	}
	if !strings.Contains(body, testBrokerToken) {
		t.Fatal("confidential env missing worker token")
	}
	if strings.Contains(body, testMintToken) || strings.Contains(body, envFenceCoordinator) || strings.Contains(body, envFenceBrokerMintToken) {
		t.Fatal("confidential env leaked mint/coordinator material")
	}

	js, err := json.Marshal(cap)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(js), testBrokerToken) || strings.Contains(string(js), testMintToken) {
		t.Fatalf("serialized capability leaked a secret: %s", js)
	}
	if strings.Contains(cap.String(), testBrokerToken) {
		t.Fatal("String leaked worker token")
	}

	t.Setenv(envFenceBrokerURL, "")
	t.Setenv(envFenceBrokerToken, "")
	t.Setenv("HERD_ROOT", lane)
	child, err := NewFenceBrokerClientFromEnv()
	if err != nil {
		t.Fatalf("child must attach from confidential env: %v", err)
	}
	if err := child.Live(context.Background()); err != nil {
		t.Fatalf("child worker live: %v", err)
	}
	if _, err := NewFenceBrokerMinterFromEnv(); err == nil {
		t.Fatal("child must not mint from env")
	}
	if err := child.MutateStatus(context.Background(), "tb1", StatusInProgress, 1, "op-child", ""); err == nil {
		t.Fatal("child must not mutate without a pre-minted capability")
	}
}

func TestResolveShareableWorkerBroker_RefusesMissingPartialUnhealthyForeignMintAndInProcess(t *testing.T) {
	up := newAuthBoard()
	up.rejectUnfenced = false
	srv := up.serveUnfenced()
	t.Cleanup(srv.Close)

	claimDir := t.TempDir()
	t.Setenv("HERD_CLAIM_DIR", claimDir)
	t.Setenv(envFenceCoordinator, "")
	ProvisionSharedFenceForTest(t, claimDir)
	b := startTestBroker(t, srv.URL, claimDir)

	t.Run("missing", func(t *testing.T) {
		t.Setenv(envFenceBrokerURL, "")
		t.Setenv(envFenceBrokerToken, "")
		_, err := ResolveShareableWorkerBroker(context.Background(), claimDir)
		if !errors.Is(err, ErrWorkerBrokerMissing) {
			t.Fatalf("missing: %v", err)
		}
	})
	t.Run("partial_url", func(t *testing.T) {
		t.Setenv(envFenceBrokerURL, b.ClientBaseURL())
		t.Setenv(envFenceBrokerToken, "")
		_, err := ResolveShareableWorkerBroker(context.Background(), claimDir)
		if !errors.Is(err, ErrWorkerBrokerPartial) {
			t.Fatalf("partial url: %v", err)
		}
	})
	t.Run("partial_token", func(t *testing.T) {
		t.Setenv(envFenceBrokerURL, "")
		t.Setenv(envFenceBrokerToken, testBrokerToken)
		_, err := ResolveShareableWorkerBroker(context.Background(), claimDir)
		if !errors.Is(err, ErrWorkerBrokerPartial) {
			t.Fatalf("partial token: %v", err)
		}
	})
	t.Run("wrong_token", func(t *testing.T) {
		t.Setenv(envFenceBrokerURL, b.ClientBaseURL())
		t.Setenv(envFenceBrokerToken, "wrong-worker-token-min16")
		_, err := ResolveShareableWorkerBroker(context.Background(), claimDir)
		if !errors.Is(err, ErrWorkerBrokerPartial) {
			t.Fatalf("wrong token: %v", err)
		}
	})
	t.Run("unhealthy", func(t *testing.T) {
		dead := t.TempDir()
		t.Setenv("HERD_CLAIM_DIR", dead)
		ProvisionSharedFenceForTest(t, dead)
		dying := startTestBroker(t, srv.URL, dead)
		url := dying.ClientBaseURL()
		_ = dying.Close()
		t.Setenv(envFenceBrokerURL, url)
		t.Setenv(envFenceBrokerToken, testBrokerToken)
		_, err := ResolveShareableWorkerBroker(context.Background(), dead)
		if !errors.Is(err, ErrWorkerBrokerUnhealthy) {
			t.Fatalf("unhealthy: %v", err)
		}
	})
	t.Run("foreign_volume", func(t *testing.T) {
		foreign := t.TempDir()
		t.Setenv("HERD_CLAIM_DIR", foreign)
		ProvisionSharedFenceForTest(t, foreign)
		t.Setenv(envFenceBrokerURL, b.ClientBaseURL())
		t.Setenv(envFenceBrokerToken, testBrokerToken)
		_, err := ResolveShareableWorkerBroker(context.Background(), foreign)
		if !errors.Is(err, ErrWorkerBrokerForeign) {
			t.Fatalf("foreign: %v", err)
		}
	})
	t.Run("mint_equals_worker", func(t *testing.T) {
		t.Setenv("HERD_CLAIM_DIR", claimDir)
		t.Setenv(envFenceBrokerURL, b.ClientBaseURL())
		t.Setenv(envFenceBrokerToken, testBrokerToken)
		t.Setenv(envFenceBrokerMintToken, testBrokerToken)
		_, err := ResolveShareableWorkerBroker(context.Background(), claimDir)
		if !errors.Is(err, ErrWorkerBrokerMint) {
			t.Fatalf("mint as worker: %v", err)
		}
	})
	t.Run("in_process_coordinator", func(t *testing.T) {
		t.Setenv(envFenceCoordinator, "1")
		t.Setenv("HERD_ROLE", "orchestrator")
		t.Setenv(envFenceBrokerURL, "")
		t.Setenv(envFenceBrokerToken, "")
		_, err := ResolveShareableWorkerBroker(context.Background(), claimDir)
		if !errors.Is(err, ErrWorkerBrokerInProcess) {
			t.Fatalf("in-process: %v", err)
		}
		if !strings.Contains(err.Error(), "herd fence-broker --claim-dir") {
			t.Fatalf("in-process must name supported standalone routing: %v", err)
		}
	})
}

func TestApplyConfidentialWorkerBrokerEnv_IgnoresMintAndCoordinator(t *testing.T) {
	root := t.TempDir()
	path := ConfidentialWorkerBrokerEnvFile(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := strings.Join([]string{
		envFenceBrokerURL + "=http://127.0.0.1:9",
		envFenceBrokerToken + "=" + testBrokerToken,
		envFenceBrokerMintToken + "=" + testMintToken,
		envFenceCoordinator + "=1",
		envVolumeID + "=seal-must-not-load-0123456789abcdef0123456789abcdef",
	}, "\n")
	if err := os.WriteFile(path, []byte(payload+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envFenceBrokerURL, "")
	t.Setenv(envFenceBrokerToken, "")
	t.Setenv(envFenceBrokerMintToken, "")
	t.Setenv(envFenceCoordinator, "")
	t.Setenv(envVolumeID, "")
	if err := ApplyConfidentialWorkerBrokerEnv(root); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(envFenceBrokerURL) != "http://127.0.0.1:9" || os.Getenv(envFenceBrokerToken) != testBrokerToken {
		t.Fatal("worker capability was not loaded")
	}
	if os.Getenv(envFenceBrokerMintToken) != "" || os.Getenv(envFenceCoordinator) != "" || os.Getenv(envVolumeID) != "" {
		t.Fatal("mint/coordinator/seal leaked into process env")
	}
}
