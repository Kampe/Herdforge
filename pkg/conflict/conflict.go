package conflict

import (
	"bufio"
	"context"
	"fmt"
	"strings"
)

type ConflictChunk struct {
	FilePath string
	Ours     string
	Theirs   string
}

type Resolver struct {
	Model string
}

func NewResolver(model string) *Resolver {
	return &Resolver{Model: model}
}

func ParseConflictMarkers(content string) ([]ConflictChunk, bool) {
	var chunks []ConflictChunk
	scanner := bufio.NewScanner(strings.NewReader(content))

	inConflict := false
	inTheirs := false
	var oursBuf strings.Builder
	var theirsBuf strings.Builder
	hasConflicts := false

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "<<<<<<<") {
			inConflict = true
			inTheirs = false
			oursBuf.Reset()
			theirsBuf.Reset()
			hasConflicts = true
		} else if strings.HasPrefix(line, "=======") && inConflict {
			inTheirs = true
		} else if strings.HasPrefix(line, ">>>>>>>") && inConflict {
			inConflict = false
			chunks = append(chunks, ConflictChunk{
				Ours:   oursBuf.String(),
				Theirs: theirsBuf.String(),
			})
		} else if inConflict {
			if inTheirs {
				theirsBuf.WriteString(line + "\n")
			} else {
				oursBuf.WriteString(line + "\n")
			}
		}
	}

	return chunks, hasConflicts
}

func (r *Resolver) ResolveFile(ctx context.Context, content string) (string, error) {
	chunks, hasConflicts := ParseConflictMarkers(content)
	if !hasConflicts {
		return content, nil
	}

	// Replace conflict markers with merged union or semantic reconciliation
	result := content
	for _, chunk := range chunks {
		conflictBlock := fmt.Sprintf("<<<<<<<\n%s=======\n%s>>>>>>>\n", chunk.Ours, chunk.Theirs)
		// For deterministic fallback, prefer union of Ours + Theirs if non-duplicate
		var merged string
		if strings.TrimSpace(chunk.Ours) == strings.TrimSpace(chunk.Theirs) {
			merged = chunk.Ours
		} else {
			merged = chunk.Ours + chunk.Theirs
		}
		result = strings.Replace(result, conflictBlock, merged, 1)
	}

	return result, nil
}
