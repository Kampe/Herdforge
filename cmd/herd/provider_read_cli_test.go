package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

type contextIgnoringBoardProvider struct{}

func (contextIgnoringBoardProvider) ListTasks(context.Context, string, string) ([]*provider.Task, error) {
	select {}
}

func (contextIgnoringBoardProvider) GetTask(context.Context, string) (*provider.Task, error) {
	return nil, errors.New("not implemented")
}

func (contextIgnoringBoardProvider) ClaimTask(context.Context, string, string) error {
	return errors.New("not implemented")
}

func (contextIgnoringBoardProvider) UpdateStatus(context.Context, string, string) error {
	return errors.New("not implemented")
}

func (contextIgnoringBoardProvider) AddComment(context.Context, string, string) error {
	return errors.New("not implemented")
}

func TestFAC607BoardSyncContextIgnoringProviderIsUnknown(t *testing.T) {
	t.Setenv("HERD_PROVIDER_READ_BUDGET", "40ms")
	syncer := hsync.NewBoardSyncer(contextIgnoringBoardProvider{})

	var code int
	output := captureStderr(t, func() {
		code = runBoardSyncOnce(syncer, "project", "kaneo", false)
	})
	if code != 3 {
		t.Fatalf("exit = %d, want UNKNOWN exit 3\n%s", code, output)
	}
	for _, want := range []string{
		"board-sync: UNKNOWN",
		`"provider":"kaneo"`,
		`"phase":"ReconcileBoard provider census"`,
		`"applied_deadline":"40ms"`,
		`"last_successful_cache_revision":"none"`,
		`"outcome":"timed-out"`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("structured timeout diagnostic omits %q:\n%s", want, output)
		}
	}
}
