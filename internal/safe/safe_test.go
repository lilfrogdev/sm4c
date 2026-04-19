package safe

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLabel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain ascii", "api refactor", "api refactor"},
		{"trims whitespace", "   hi   ", "hi"},
		{"strips escape sequence", "foo\x1b[31mbar\x1b[0m", "foobar"},
		{"strips BEL", "foo\x07bar", "foobar"},
		{"strips CR/LF", "foo\r\nbar", "foobar"},
		{"strips NUL", "foo\x00bar", "foobar"},
		{"strips DEL", "foo\x7fbar", "foobar"},
		{"strips C1 control", "foo\u0085bar", "foobar"},
		{"strips RLO bidi", "safe\u202egnarly", "safegnarly"},
		{"strips LRO bidi", "safe\u202dgnarly", "safegnarly"},
		{"strips PDI", "safe\u2069gnarly", "safegnarly"},
		{"strips ZWSP", "safe\u200bgnarly", "safegnarly"},
		{"strips ZWJ", "safe\u200dgnarly", "safegnarly"},
		{"strips BOM", "\ufefffoo", "foo"},
		{"allows emoji", "ship-it 🚀", "ship-it 🚀"},
		{"allows nonlatin", "さくら", "さくら"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Label(tc.in)
			if got != tc.want {
				t.Fatalf("Label(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLabelTruncates(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", MaxLabelRunes+50)
	got := Label(long)
	if n := utf8.RuneCountInString(got); n != MaxLabelRunes {
		t.Fatalf("Label(long) returned %d runes; want %d", n, MaxLabelRunes)
	}
}

func TestLabelTruncatesMultibyte(t *testing.T) {
	t.Parallel()

	// 3-byte runes — ensure we count runes, not bytes.
	long := strings.Repeat("あ", MaxLabelRunes+10)
	got := Label(long)
	if n := utf8.RuneCountInString(got); n != MaxLabelRunes {
		t.Fatalf("Label(multibyte long) returned %d runes; want %d", n, MaxLabelRunes)
	}
}

func TestArg(t *testing.T) {
	t.Parallel()

	okCases := []string{
		"",
		"plain",
		"with spaces ok",
		"tabs\tare\tok",
		"unicode さくら",
		"emoji 🚀",
		"punct: -_.,/=",
	}
	for _, s := range okCases {
		s := s
		t.Run("ok/"+s, func(t *testing.T) {
			t.Parallel()
			if err := Arg(s); err != nil {
				t.Fatalf("Arg(%q) = %v; want nil", s, err)
			}
		})
	}

	badCases := map[string]string{
		"nul":     "foo\x00bar",
		"bel":     "foo\x07bar",
		"esc":     "foo\x1bbar",
		"cr":      "foo\rbar",
		"lf":      "foo\nbar",
		"del":     "foo\x7fbar",
		"c0-low":  "\x01",
		"c0-high": "\x1f",
	}
	for name, s := range badCases {
		name, s := name, s
		t.Run("bad/"+name, func(t *testing.T) {
			t.Parallel()
			err := Arg(s)
			if !errors.Is(err, ErrUnsafeArg) {
				t.Fatalf("Arg(%q) = %v; want ErrUnsafeArg", s, err)
			}
		})
	}
}

// FuzzLabel asserts that Label always returns a string composed only of
// safe runes, regardless of input. Run via: go test -run=^$ -fuzz=FuzzLabel
func FuzzLabel(f *testing.F) {
	seeds := []string{
		"", "plain", "\x1b[31m", "\u202e", "\ufeff", "\x00\x01\x02",
		strings.Repeat("a", 1000),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := Label(s)
		if utf8.RuneCountInString(got) > MaxLabelRunes {
			t.Fatalf("Label exceeded MaxLabelRunes: %d", utf8.RuneCountInString(got))
		}
		for _, r := range got {
			if !isSafeLabelRune(r) {
				t.Fatalf("Label leaked unsafe rune U+%04X from input %q", r, s)
			}
		}
	})
}
