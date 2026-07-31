package termtext

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"plain Unicode", "Quartermaster ⛵", 64, "Quartermaster ⛵"},
		{"invalid UTF-8", "a" + string([]byte{0xff}) + "b", 64, "a�b"},
		{"C0 C1 and bidi controls", "a\x00\t\x7f\u0085\u202eb\u2066c\u2069", 64, "abc"},
		{"CSI OSC and DCS", "a\x1b[31mb\x1b[0mc\x1b]title\ad\x1bPsecret\x1b\\e", 64, "abcde"},
		{"eight bit controls", "a\u009b31mb\u009b0mc\u009dtitle\u009cd", 64, "abcd"},
		{"escape intermediates", "a\x1b(0b\x1bZc", 64, "abc"},
		{"unterminated string", "visible\x1b]hidden", 64, "visible"},
		{"single line", "a\r\nb\tc\u2028d\u2029e", 64, "abcde"},
		{"exact byte limit", "船", 3, "船"},
		{"rune safe truncation", "ab船cd", 6, "ab…"},
		{"small limit", "船", 2, ".."},
		{"zero limit", "text", 0, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Sanitize(test.in, test.max); got != test.want {
				t.Fatalf("Sanitize(%q, %d) = %q, want %q", test.in, test.max, got, test.want)
			}
		})
	}
}

func TestSanitizeMultiline(t *testing.T) {
	if got := SanitizeMultiline("a\r\nb\tc", 64); got != "a\nbc" {
		t.Fatalf("SanitizeMultiline() = %q, want %q", got, "a\nbc")
	}
}

func FuzzSanitize(f *testing.F) {
	f.Add([]byte("safe\x1b[31mred\x1b[0m"), uint8(64), false)
	f.Add([]byte{0xff, 0x1b, ']', 'x', 0x07}, uint8(4), true)
	f.Fuzz(func(t *testing.T, in []byte, limit uint8, multiline bool) {
		got := Sanitize(string(in), int(limit))
		if multiline {
			got = SanitizeMultiline(string(in), int(limit))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("invalid UTF-8: %x", got)
		}
		if len(got) > int(limit) {
			t.Fatalf("output length %d exceeds limit %d", len(got), limit)
		}
		for _, r := range got {
			if r == 0x1b || (unicode.IsControl(r) && (!multiline || r != '\n')) || r == '\u2028' || r == '\u2029' || unicode.Is(unicode.Bidi_Control, r) {
				t.Fatalf("unsafe rune %U survived in %q", r, got)
			}
		}
	})
}

var benchmarkSink string

func BenchmarkSanitize(b *testing.B) {
	input := strings.Repeat("Hauling \x1b[32mverified\x1b[0m pieces ⛵ ", 16)
	b.ReportAllocs()
	for b.Loop() {
		benchmarkSink = Sanitize(input, 512)
	}
}
