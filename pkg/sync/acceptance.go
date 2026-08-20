package sync

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const acceptanceFence = "herd-acceptance-v1"

var acceptanceFenceRE = regexp.MustCompile("(?s)```" + acceptanceFence + "\\s*(.*?)\\s*```")

// AcceptanceCommand is the exact command that must be visible in the pasted
// acceptance output. Context identifies where the command was run (for
// example, "consumer repo" or "Herdforge worktree").
type AcceptanceCommand struct {
	Command          string `json:"command"`
	Context          string `json:"context"`
	WorkingDirectory string `json:"working_directory,omitempty"`
}

// AcceptanceBlock is the machine-readable completion contract on a card.
type AcceptanceBlock struct {
	Commands []AcceptanceCommand `json:"commands"`
}

// ErrAcceptance is returned when a card's completion contract or pasted
// evidence cannot establish that every named acceptance command ran.
var ErrAcceptance = fmt.Errorf("acceptance evidence is insufficient")

// ParseAcceptanceBlock extracts and validates the one authoritative
// herd-acceptance-v1 fence from a task description. Prose such as
// "## Verification" is intentionally not consulted.
func ParseAcceptanceBlock(description string) (AcceptanceBlock, error) {
	matches := acceptanceFenceRE.FindAllStringSubmatch(description, -1)
	if len(matches) == 0 {
		return AcceptanceBlock{}, fmt.Errorf("%w: card has no %s block", ErrAcceptance, acceptanceFence)
	}
	if len(matches) != 1 {
		return AcceptanceBlock{}, fmt.Errorf("%w: card has multiple %s blocks", ErrAcceptance, acceptanceFence)
	}
	var block AcceptanceBlock
	if err := json.Unmarshal([]byte(matches[0][1]), &block); err != nil {
		return AcceptanceBlock{}, fmt.Errorf("%w: invalid %s JSON: %v", ErrAcceptance, acceptanceFence, err)
	}
	if len(block.Commands) == 0 {
		return AcceptanceBlock{}, fmt.Errorf("%w: %s must name at least one command", ErrAcceptance, acceptanceFence)
	}
	for i, command := range block.Commands {
		if strings.TrimSpace(command.Command) == "" {
			return AcceptanceBlock{}, fmt.Errorf("%w: command %d is empty", ErrAcceptance, i+1)
		}
		if strings.TrimSpace(command.Context) == "" {
			return AcceptanceBlock{}, fmt.Errorf("%w: command %q has no context", ErrAcceptance, command.Command)
		}
	}
	return block, nil
}

// ValidateAcceptanceEvidence checks pasted output against the card's exact
// command/context contract. Requiring both strings prevents a green run from
// another command or repository from being used as proxy evidence.
func ValidateAcceptanceEvidence(description, evidence string) error {
	block, err := ParseAcceptanceBlock(description)
	if err != nil {
		return err
	}
	evidence = strings.TrimSpace(evidence)
	if evidence == "" {
		return fmt.Errorf("%w: no pasted output", ErrAcceptance)
	}
	for _, command := range block.Commands {
		if !strings.Contains(evidence, command.Command) {
			return fmt.Errorf("%w: pasted output does not contain acceptance command %q", ErrAcceptance, command.Command)
		}
		if !strings.Contains(evidence, command.Context) {
			return fmt.Errorf("%w: pasted output does not identify context %q for command %q", ErrAcceptance, command.Context, command.Command)
		}
		if wd := strings.TrimSpace(command.WorkingDirectory); wd != "" && !strings.Contains(evidence, wd) {
			return fmt.Errorf("%w: pasted output does not identify working directory %q for command %q", ErrAcceptance, wd, command.Command)
		}
	}
	return nil
}
