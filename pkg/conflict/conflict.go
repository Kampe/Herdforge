package conflict

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors.
var (
	ErrUnresolved       = errors.New("conflict: unresolved conflict")
	ErrMalformedMarkers = errors.New("conflict: malformed conflict markers")
)

// UnresolvedConflictError is returned when one or more conflict chunks cannot be
// resolved deterministically or safely.
type UnresolvedConflictError struct {
	Reason string
	Chunks []ConflictChunk
	Err    error
}

func (e *UnresolvedConflictError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("conflict unresolved: %s", e.Reason)
	}
	if e.Err != nil {
		return fmt.Sprintf("conflict unresolved: %v", e.Err)
	}
	if len(e.Chunks) > 0 {
		return fmt.Sprintf("conflict unresolved: %d chunk(s) require resolution", len(e.Chunks))
	}
	return "conflict unresolved"
}

func (e *UnresolvedConflictError) Unwrap() error {
	if e.Err != nil {
		return e.Err
	}
	return ErrUnresolved
}

func (e *UnresolvedConflictError) Is(target error) bool {
	return target == ErrUnresolved
}

// MalformedMarkerError is returned when conflict markers are unmatched,
// misplaced, or structurally corrupted.
type MalformedMarkerError struct {
	Line   int
	Reason string
}

func (e *MalformedMarkerError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("malformed conflict marker at line %d: %s", e.Line, e.Reason)
	}
	return fmt.Sprintf("malformed conflict marker: %s", e.Reason)
}

func (e *MalformedMarkerError) Unwrap() error {
	return ErrMalformedMarkers
}

func (e *MalformedMarkerError) Is(target error) bool {
	return target == ErrMalformedMarkers
}

// ConflictChunk represents a single conflict region in a file.
type ConflictChunk struct {
	Ours   string
	Theirs string
	Base   string // Ancestor / common base in diff3 format
}

// Strategy represents the automatic resolution strategy to use.
type Strategy string

const (
	StrategyManual Strategy = "manual" // Fail closed unless sides are identical
	StrategyOurs   Strategy = "ours"   // Choose ours
	StrategyTheirs Strategy = "theirs" // Choose theirs
	StrategyBase   Strategy = "base"   // Choose diff3 base/ancestor
)

// ResolveChunkFunc is a caller-supplied function to resolve a single conflict chunk.
type ResolveChunkFunc func(ctx context.Context, chunk ConflictChunk) (string, error)

// Resolver resolves conflict markers in file content.
type Resolver struct {
	Model     string
	Strategy  Strategy
	ResolveFn ResolveChunkFunc
	MaxChunks int // Maximum conflict chunks permitted before failing (0 = unlimited)
}

// NewResolver returns a new Resolver for the given model with manual strategy.
func NewResolver(model string) *Resolver {
	return &Resolver{
		Model:    model,
		Strategy: StrategyManual,
	}
}

// NewResolverWithStrategy returns a new Resolver using the specified strategy.
func NewResolverWithStrategy(strategy Strategy) *Resolver {
	return &Resolver{
		Strategy: strategy,
	}
}

// NewResolverWithFunc returns a new Resolver that delegates resolution to fn.
func NewResolverWithFunc(fn ResolveChunkFunc) *Resolver {
	return &Resolver{
		ResolveFn: fn,
	}
}

type parserState int

const (
	stateOutside parserState = iota
	stateOurs
	stateBase
	stateTheirs
)

type parsedBlock struct {
	isConflict bool
	text       string
	chunk      ConflictChunk
}

// parseBlocks scans content and decomposes it into sequential text and conflict blocks.
func parseBlocks(content string) ([]parsedBlock, string, error) {
	if content == "" {
		return nil, "\n", nil
	}

	// Detect line ending convention
	lineEnding := "\n"
	if strings.Contains(content, "\r\n") {
		lineEnding = "\r\n"
	}

	lines := strings.Split(content, lineEnding)
	// If the file ended with a newline, strings.Split produces a trailing empty element
	hasTrailingNewline := strings.HasSuffix(content, lineEnding)
	if hasTrailingNewline && len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	var blocks []parsedBlock
	var currentText []string

	state := stateOutside
	var oursLines []string
	var baseLines []string
	var theirsLines []string
	conflictStartLine := 0

	flushText := func() {
		if len(currentText) > 0 {
			blocks = append(blocks, parsedBlock{
				isConflict: false,
				text:       strings.Join(currentText, lineEnding) + lineEnding,
			})
			currentText = nil
		}
	}

	for idx, line := range lines {
		lineNum := idx + 1
		cleanLine := strings.TrimRight(line, "\r")

		if strings.HasPrefix(cleanLine, "<<<<<<<") {
			if state != stateOutside {
				return nil, lineEnding, &MalformedMarkerError{
					Line:   lineNum,
					Reason: "nested conflict marker '<<<<<<<' within active conflict block",
				}
			}
			flushText()
			state = stateOurs
			conflictStartLine = lineNum
			oursLines = nil
			baseLines = nil
			theirsLines = nil
			continue
		}

		if strings.HasPrefix(cleanLine, "|||||||") {
			if state == stateOutside {
				return nil, lineEnding, &MalformedMarkerError{
					Line:   lineNum,
					Reason: "unexpected diff3 base marker '|||||||' outside conflict block",
				}
			}
			if state != stateOurs {
				return nil, lineEnding, &MalformedMarkerError{
					Line:   lineNum,
					Reason: "unexpected diff3 base marker '|||||||' after base or separator",
				}
			}
			state = stateBase
			continue
		}

		if strings.HasPrefix(cleanLine, "=======") {
			if state == stateOutside {
				return nil, lineEnding, &MalformedMarkerError{
					Line:   lineNum,
					Reason: "unexpected separator marker '=======' outside conflict block",
				}
			}
			if state == stateTheirs {
				return nil, lineEnding, &MalformedMarkerError{
					Line:   lineNum,
					Reason: "duplicate separator marker '=======' within conflict block",
				}
			}
			state = stateTheirs
			continue
		}

		if strings.HasPrefix(cleanLine, ">>>>>>>") {
			if state == stateOutside {
				return nil, lineEnding, &MalformedMarkerError{
					Line:   lineNum,
					Reason: "unexpected closing marker '>>>>>>>' outside conflict block",
				}
			}
			if state == stateOurs || state == stateBase {
				return nil, lineEnding, &MalformedMarkerError{
					Line:   lineNum,
					Reason: "closing marker '>>>>>>>' before '=======' separator",
				}
			}

			oursStr := ""
			if len(oursLines) > 0 {
				oursStr = strings.Join(oursLines, lineEnding) + lineEnding
			}
			baseStr := ""
			if len(baseLines) > 0 {
				baseStr = strings.Join(baseLines, lineEnding) + lineEnding
			}
			theirsStr := ""
			if len(theirsLines) > 0 {
				theirsStr = strings.Join(theirsLines, lineEnding) + lineEnding
			}

			blocks = append(blocks, parsedBlock{
				isConflict: true,
				chunk: ConflictChunk{
					Ours:   oursStr,
					Theirs: theirsStr,
					Base:   baseStr,
				},
			})

			state = stateOutside
			oursLines = nil
			baseLines = nil
			theirsLines = nil
			continue
		}

		switch state {
		case stateOutside:
			currentText = append(currentText, cleanLine)
		case stateOurs:
			oursLines = append(oursLines, cleanLine)
		case stateBase:
			baseLines = append(baseLines, cleanLine)
		case stateTheirs:
			theirsLines = append(theirsLines, cleanLine)
		}
	}

	if state != stateOutside {
		return nil, lineEnding, &MalformedMarkerError{
			Line:   conflictStartLine,
			Reason: "unterminated conflict block at end of content",
		}
	}

	if len(currentText) > 0 {
		trailing := ""
		if hasTrailingNewline {
			trailing = lineEnding
		}
		blocks = append(blocks, parsedBlock{
			isConflict: false,
			text:       strings.Join(currentText, lineEnding) + trailing,
		})
	}

	return blocks, lineEnding, nil
}

// Parse extracts all conflict chunks from the given content, returning an error
// if malformed markers are encountered.
func Parse(content string) ([]ConflictChunk, error) {
	blocks, _, err := parseBlocks(content)
	if err != nil {
		return nil, err
	}

	var chunks []ConflictChunk
	for _, block := range blocks {
		if block.isConflict {
			chunks = append(chunks, block.chunk)
		}
	}
	return chunks, nil
}

// ParseConflictMarkers extracts conflict chunks from content. It returns the chunks
// and a boolean indicating whether any conflict markers were present.
func ParseConflictMarkers(content string) ([]ConflictChunk, bool) {
	chunks, err := Parse(content)
	if err != nil {
		// Fallback scanner for backward-compatibility if malformed markers exist
		fallbackChunks, hasConf := fallbackParse(content)
		return fallbackChunks, hasConf
	}
	return chunks, len(chunks) > 0
}

func fallbackParse(content string) ([]ConflictChunk, bool) {
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

func hasConflictMarker(s string) bool {
	lines := strings.Split(s, "\n")
	for _, l := range lines {
		trimmed := strings.TrimRight(l, "\r")
		if strings.HasPrefix(trimmed, "<<<<<<<") ||
			strings.HasPrefix(trimmed, "|||||||") ||
			strings.HasPrefix(trimmed, "=======") ||
			strings.HasPrefix(trimmed, ">>>>>>>") {
			return true
		}
	}
	return false
}

// ResolveFile parses conflict markers and resolves them safely.
// It fails closed by default (returns UnresolvedConflictError) if chunks cannot be
// deterministically or safely resolved. It NEVER concatenates ours and theirs.
func (r *Resolver) ResolveFile(ctx context.Context, content string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	blocks, lineEnding, err := parseBlocks(content)
	if err != nil {
		return "", err
	}

	var conflictCount int
	for _, b := range blocks {
		if b.isConflict {
			conflictCount++
		}
	}

	if conflictCount == 0 {
		return content, nil
	}

	if r.MaxChunks > 0 && conflictCount > r.MaxChunks {
		return "", &UnresolvedConflictError{
			Reason: fmt.Sprintf("conflict chunk count %d exceeds maximum permitted (%d)", conflictCount, r.MaxChunks),
			Err:    ErrUnresolved,
		}
	}

	var out strings.Builder
	var unresolvedChunks []ConflictChunk

	for _, block := range blocks {
		if !block.isConflict {
			out.WriteString(block.text)
			continue
		}

		chunk := block.chunk

		// Bounded resolution path 1: Caller-supplied resolver function
		if r.ResolveFn != nil {
			resolved, err := r.ResolveFn(ctx, chunk)
			if err != nil {
				return "", &UnresolvedConflictError{
					Reason: fmt.Sprintf("resolver function error: %v", err),
					Chunks: []ConflictChunk{chunk},
					Err:    err,
				}
			}
			if hasConflictMarker(resolved) {
				return "", &UnresolvedConflictError{
					Reason: "resolver function returned unresolved conflict markers",
					Chunks: []ConflictChunk{chunk},
					Err:    ErrUnresolved,
				}
			}
			out.WriteString(resolved)
			continue
		}

		// Bounded resolution path 2: Explicit Strategy
		switch r.Strategy {
		case StrategyOurs:
			out.WriteString(chunk.Ours)
		case StrategyTheirs:
			out.WriteString(chunk.Theirs)
		case StrategyBase:
			if chunk.Base == "" {
				return "", &UnresolvedConflictError{
					Reason: "strategy 'base' requested but chunk contains no diff3 ancestor base content",
					Chunks: []ConflictChunk{chunk},
					Err:    ErrUnresolved,
				}
			}
			out.WriteString(chunk.Base)
		case StrategyManual, "":
			// Deterministic resolution: if both branches made the exact same edit
			if chunk.Ours == chunk.Theirs {
				out.WriteString(chunk.Ours)
			} else {
				unresolvedChunks = append(unresolvedChunks, chunk)
			}
		default:
			return "", &UnresolvedConflictError{
				Reason: fmt.Sprintf("unsupported resolution strategy: %s", r.Strategy),
				Chunks: []ConflictChunk{chunk},
				Err:    ErrUnresolved,
			}
		}
	}

	if len(unresolvedChunks) > 0 {
		return "", &UnresolvedConflictError{
			Reason: fmt.Sprintf("%d conflict chunk(s) cannot be resolved automatically (ours and theirs differ)", len(unresolvedChunks)),
			Chunks: unresolvedChunks,
			Err:    ErrUnresolved,
		}
	}

	result := out.String()
	// Ensure preservation of trailing newline property if output is non-empty
	if strings.HasSuffix(content, lineEnding) && !strings.HasSuffix(result, lineEnding) {
		result += lineEnding
	} else if !strings.HasSuffix(content, lineEnding) && strings.HasSuffix(result, lineEnding) && len(result) > 0 {
		result = strings.TrimSuffix(result, lineEnding)
	}

	return result, nil
}
