package provider

import "fmt"

// escapeRawControlsInStrings rewrites raw C0 control bytes that appear INSIDE
// JSON string literals into their valid escape form, and reports whether it
// changed anything.
//
// RFC 8259 forbids unescaped bytes below 0x20 in a string, and Go's
// encoding/json rejects them outright ("invalid character '\n' in string
// literal"). A board card's description is free text written by many agents;
// one raw newline reaching that field would otherwise fail-close an entire
// page of tasks, taking the whole queue offline over one bad byte.
//
// This is deliberately narrow. Only bytes inside a string literal are touched,
// escape sequences are respected so a legitimate \" cannot desynchronise the
// scan, and structural bytes outside strings are left alone — malformed JSON
// stays malformed and is still refused by the caller's retry.
func escapeRawControlsInStrings(body []byte) ([]byte, bool) {
	out := make([]byte, 0, len(body))
	inString := false
	escaped := false
	changed := false

	for _, c := range body {
		if inString {
			if escaped {
				// This byte is the escape's argument; copy verbatim.
				out = append(out, c)
				escaped = false
				continue
			}
			switch {
			case c == '\\':
				out = append(out, c)
				escaped = true
				continue
			case c == '"':
				out = append(out, c)
				inString = false
				continue
			case c < 0x20:
				changed = true
				switch c {
				case '\n':
					out = append(out, '\\', 'n')
				case '\r':
					out = append(out, '\\', 'r')
				case '\t':
					out = append(out, '\\', 't')
				default:
					out = append(out, []byte(fmt.Sprintf("\\u%04x", c))...)
				}
				continue
			}
			out = append(out, c)
			continue
		}
		if c == '"' {
			inString = true
		}
		out = append(out, c)
	}
	// An unterminated string means the payload is truncated, not merely
	// unescaped. Repairing it would invent structure, so decline.
	if inString {
		return body, false
	}
	return out, changed
}
