package freshbuild

import (
	"bufio"
	"bytes"
	"regexp"
	"strings"
)

// errorLine re matches the zsh filter: error|TS[0-9]{3,}|failed (case-insensitive).
var errorLine = regexp.MustCompile(`(?i)error|TS[0-9]{3,}|failed`)

// errorTail returns up to n diagnostic lines from log, preferring matches of
// the error filter and falling back to the last n lines (zsh || tail).
func errorTail(log []byte, n int) []string {
	if n <= 0 {
		return nil
	}
	var matched []string
	sc := bufio.NewScanner(bytes.NewReader(log))
	// Allow long lines from tsc dumps.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var all []string
	for sc.Scan() {
		line := sc.Text()
		all = append(all, line)
		if errorLine.MatchString(line) {
			matched = append(matched, line)
		}
	}
	src := matched
	if len(src) == 0 {
		src = all
	}
	if len(src) > n {
		src = src[len(src)-n:]
	}
	// Trim trailing empty noise.
	for len(src) > 0 && strings.TrimSpace(src[len(src)-1]) == "" {
		src = src[:len(src)-1]
	}
	return src
}
