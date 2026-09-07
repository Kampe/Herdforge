package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const kaneoCoreReadExecutable = "kaneo-core"

// Capability refusal never retries the legacy fan-out or changes transport.
func (k *KaneoProvider) requireCoreRead(ctx context.Context) error {
	if strings.TrimSpace(k.ProjectID) == "" {
		return fmt.Errorf("core task read requires project identity")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		k.coreReadMu.Lock()
		if k.coreReadReady {
			k.coreReadMu.Unlock()
			return nil
		}
		pending := k.coreReadPending
		if pending != nil {
			k.coreReadMu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-pending:
				continue
			}
		}
		pending = make(chan struct{})
		k.coreReadPending = pending
		k.coreReadMu.Unlock()
		err := probeCoreRead(ctx)
		k.coreReadMu.Lock()
		k.coreReadReady = err == nil
		k.coreReadPending = nil
		close(pending)
		k.coreReadMu.Unlock()
		return err
	}
}

func probeCoreRead(ctx context.Context) error {
	res, err := kaneoRunCLI(ctx, kaneoCoreReadExecutable, "task", "get", "--help")
	if err != nil {
		return fmt.Errorf("core task read capability: %w", err)
	}
	if res == nil {
		return fmt.Errorf("empty core task read capability response")
	}
	help := string(res.Stdout)
	hasCore := false
	for _, line := range strings.Split(help, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "--core" {
			hasCore = true
		}
	}
	if !strings.Contains(help, "Usage: "+kaneoCoreReadExecutable+" task get") || !hasCore {
		return fmt.Errorf("installed Kaneo CLI lacks core task read capability")
	}
	return ctx.Err()
}

func validateCoreTaskBody(body []byte, dto kaneoTaskDTO, project string) error {
	if dto.ProjectId != project || strings.TrimSpace(dto.Ref) == "" {
		return fmt.Errorf("core task read project/ref mismatch")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return err
	}
	labels, ok := fields["labels"]
	if !ok || string(labels) == "null" {
		return fmt.Errorf("core task read missing labels")
	}
	var names []string
	if err := json.Unmarshal(labels, &names); err != nil {
		return fmt.Errorf("core task read labels: %w", err)
	}
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("core task read empty label")
		}
	}
	return nil
}
