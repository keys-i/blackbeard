// Package termtext makes untrusted text safe to print to a terminal.
package termtext

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const ellipsis = "…"

const (
	textState = iota
	escapeState
	escapeIntermediateState
	csiState
	stringState
	stringEscapeState
)

// Sanitize removes terminal controls and line breaks, returning at most
// maxBytes of valid UTF-8. A non-positive limit returns an empty string.
func Sanitize(s string, maxBytes int) string {
	return sanitize(s, maxBytes, false)
}

// SanitizeMultiline is Sanitize with line-feed preservation enabled. Carriage
// returns and every other terminal control are still removed.
func SanitizeMultiline(s string, maxBytes int) string {
	return sanitize(s, maxBytes, true)
}

func sanitize(s string, maxBytes int, multiline bool) string {
	if maxBytes <= 0 || s == "" {
		return ""
	}

	capacity := min(len(s), maxBytes)
	var out strings.Builder
	out.Grow(capacity)
	state := textState
	osc := false

	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]

		switch state {
		case escapeState:
			switch r {
			case '[':
				state = csiState
			case ']':
				state, osc = stringState, true
			case 'P', 'X', '^', '_':
				state, osc = stringState, false
			case 0x1b:
				// A new escape restarts the sequence.
			case 0x0090, 0x0098, 0x009e, 0x009f:
				state, osc = stringState, false
			case 0x009b:
				state = csiState
			case 0x009d:
				state, osc = stringState, true
			default:
				if r >= 0x20 && r <= 0x2f {
					state = escapeIntermediateState
				} else {
					state = textState
				}
			}
			continue

		case escapeIntermediateState:
			switch {
			case r == 0x1b:
				state = escapeState
			case r >= 0x30 && r <= 0x7e:
				state = textState
			case r < 0x20 || r > 0x2f:
				state = textState
			}
			continue

		case csiState:
			if r == 0x1b {
				state = escapeState
			} else if r >= 0x40 && r <= 0x7e {
				state = textState
			}
			continue

		case stringState:
			switch {
			case r == 0x1b:
				state = stringEscapeState
			case r == 0x009c || osc && r == '\a':
				state = textState
			}
			continue

		case stringEscapeState:
			switch r {
			case '\\', 0x009c:
				state = textState
			case 0x1b:
				// Stay here so the next backslash can terminate the string.
			default:
				state = stringState
			}
			continue
		}

		switch r {
		case 0x1b:
			state = escapeState
			continue
		case 0x0090, 0x0098, 0x009e, 0x009f:
			state, osc = stringState, false
			continue
		case 0x009b:
			state = csiState
			continue
		case 0x009d:
			state, osc = stringState, true
			continue
		}

		if (unicode.IsControl(r) && (!multiline || r != '\n')) || r == '\u2028' || r == '\u2029' || unicode.Is(unicode.Bidi_Control, r) {
			continue
		}
		if out.Len()+utf8.RuneLen(r) > maxBytes {
			return truncate(out.String(), maxBytes)
		}
		out.WriteRune(r)
	}

	return out.String()
}

func truncate(s string, maxBytes int) string {
	if maxBytes < len(ellipsis) {
		return strings.Repeat(".", maxBytes)
	}

	prefixBytes := min(len(s), maxBytes-len(ellipsis))
	for prefixBytes > 0 && prefixBytes < len(s) && !utf8.RuneStart(s[prefixBytes]) {
		prefixBytes--
	}
	return s[:prefixBytes] + ellipsis
}
