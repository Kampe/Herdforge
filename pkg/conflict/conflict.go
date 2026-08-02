package conflict

import (
	"bufio"
	"context"
	"strings"
)

type ConflictChunk struct {
	Ours   string
	Theirs string
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
	lines := strings.Split(content, "\n")
	var outLines []string

	inConflict := false
	inTheirs := false
	var oursLines []string
	var theirsLines []string

	for _, line := range lines {
		if strings.HasPrefix(line, "<<<<<<<") {
			inConflict = true
			inTheirs = false
			oursLines = nil
			theirsLines = nil
			continue
		}
		if strings.HasPrefix(line, "=======") && inConflict {
			inTheirs = true
			continue
		}
		if strings.HasPrefix(line, ">>>>>>>") && inConflict {
			inConflict = false
			// Reconcile conflict block: prefer ours + theirs
			outLines = append(outLines, oursLines...)
			outLines = append(outLines, theirsLines...)
			continue
		}
		if inConflict {
			if inTheirs {
				theirsLines = append(theirsLines, line)
			} else {
				oursLines = append(oursLines, line)
			}
		} else {
			outLines = append(outLines, line)
		}
	}

	return strings.Join(outLines, "\n"), nil
}
