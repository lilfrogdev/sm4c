package tmuxctl

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestUnescape_Roundtrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want []byte
	}{
		{"empty", "", []byte{}},
		{"plain ascii", "hello world", []byte("hello world")},
		{"tab only", `\011`, []byte{0x09}},
		{"cr lf", `\015\012`, []byte{0x0d, 0x0a}},
		{"escape", `\033`, []byte{0x1b}},
		{"backslash itself", `\134`, []byte{'\\'}},
		{"ansi sgr", `\033[31mR\033[0m`, []byte("\x1b[31mR\x1b[0m")},
		{"docs example",
			`A\011B\015\012\033[31mR\033[0m`,
			[]byte("A\tB\r\n\x1b[31mR\x1b[0m")},
		{"nul byte", `\000`, []byte{0x00}},
		{"high byte", `\377`, []byte{0xff}},
		{"mixed printable and escape", `pre\033[1mbold\033[0mpost`,
			[]byte("pre\x1b[1mbold\x1b[0mpost")},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Unescape([]byte(tc.in))
			if err != nil {
				t.Fatalf("Unescape(%q) err=%v", tc.in, err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("Unescape(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestUnescape_RejectsTruncated(t *testing.T) {
	t.Parallel()

	cases := []string{`\`, `\0`, `\01`, `plain\01`}
	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, err := Unescape([]byte(in))
			if !errors.Is(err, ErrTruncatedEscape) {
				t.Fatalf("Unescape(%q) err=%v; want ErrTruncatedEscape", in, err)
			}
		})
	}
}

func TestUnescape_RejectsInvalidDigits(t *testing.T) {
	t.Parallel()

	// Non-octal digits (8, 9, letters) and short-form \n / \t / \\ must all
	// be rejected. A permissive decoder here is the whole point of the
	// strictness commitment in SECURITY.md.
	cases := []string{
		`\089`, `\019`, `\abc`, `\\\\`, `\n33`, `\\033`,
		`\\t`, `\\n`, `\079`, `\08 `, `\128`,
	}
	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, err := Unescape([]byte(in))
			if err == nil {
				t.Fatalf("Unescape(%q) unexpectedly succeeded", in)
			}
			if !errors.Is(err, ErrInvalidEscape) && !errors.Is(err, ErrTruncatedEscape) {
				t.Fatalf("Unescape(%q) err=%v; want invalid or truncated escape", in, err)
			}
		})
	}
}

func TestUnescape_LongStream(t *testing.T) {
	t.Parallel()

	// 100 KB of mixed printable + escapes; exercises the grow path of the
	// output slice and the hot loop under race detection.
	var b strings.Builder
	for i := 0; i < 5_000; i++ {
		b.WriteString(`hello\033[0m world\012`)
	}
	got, err := Unescape([]byte(b.String()))
	if err != nil {
		t.Fatalf("Unescape long: %v", err)
	}
	if want := 5_000 * len("hello\x1b[0m world\n"); len(got) != want {
		t.Fatalf("len(got)=%d; want %d", len(got), want)
	}
}

// FuzzUnescape asserts that Unescape never panics, never returns a byte
// slice longer than the input, and — critically — never returns a byte
// equal to `\` (0x5C) in its output unless the input contained an
// explicit `\134` escape. The "\\" shortcut is NOT supported and must
// not leak bytes past the decoder.
//
// Run via: go test -run=^$ -fuzz=FuzzUnescape ./internal/tmuxctl/
func FuzzUnescape(f *testing.F) {
	seeds := []string{
		"",
		"plain",
		`\033[31m`,
		`\\033`,
		`\134`,
		`\\`,
		`\1`,
		`\abc`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, err := Unescape([]byte(s))
		if err != nil {
			return
		}
		if len(out) > len(s) {
			t.Fatalf("Unescape grew: in=%d out=%d", len(s), len(out))
		}
	})
}
