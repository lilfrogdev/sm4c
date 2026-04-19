// Package safe is the single choke point for sanitizing and validating
// strings that cross a trust boundary.
//
// Every string that originates from claude output or end-user input and is
// either (a) passed as an argument to tmux/claude subprocesses or (b)
// rendered directly to the outer terminal MUST pass through a function in
// this package. This keeps the attack surface auditable: to review sm4c's
// escape-injection and command-injection posture, you review safe.go and
// its tests.
//
// Functions in this package are pure: no logging, no file I/O, no network.
package safe

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLabelRunes caps sanitized labels at a human-readable length. tmux
// itself does not impose a tight limit; we do so to keep the sidebar
// predictable and to bound worst-case render cost.
const MaxLabelRunes = 64

// MaxLineRunes caps sanitized one-line strings that are longer than a
// sidebar label but still bounded — diagnostic detail lines, resolved
// filesystem paths, version strings, error messages. 1 KiB is plenty for
// any realistic POSIX path plus a parenthetical note and keeps any
// single `sm4c doctor` row from being unboundedly long.
const MaxLineRunes = 1024

// ErrUnsafeArg is returned by Arg when the input contains a byte that
// cannot appear in a subprocess argument (NUL) or a C0 control that could
// corrupt tmux's control-mode parser.
var ErrUnsafeArg = errors.New("safe: argument contains an unsafe byte")

// Label returns s with anything dangerous or display-confusing removed and
// then truncated to MaxLabelRunes runes. The result is guaranteed to:
//
//   - contain only printable Unicode runes (unicode.IsPrint)
//   - contain no C0 controls, C1 controls, DEL, or line separators
//   - contain no BIDI-override or invisible formatting characters
//   - contain no ANSI escape sequences (CSI / OSC / DCS / SOS / PM / APC
//     and generic two-byte escapes are all dropped whole — not just the
//     ESC byte)
//   - have length-in-runes <= MaxLabelRunes
//
// Label never returns an error. Callers that want to distinguish "was
// stripped to empty" from "was empty to begin with" should compare lengths.
func Label(s string) string {
	return sanitizeLine(s, MaxLabelRunes)
}

// Line is like Label but caps output at MaxLineRunes. Use Line for
// diagnostic strings that should be longer than a session label — full
// file paths, `tmux -V` output, user-facing error detail — while still
// guaranteeing no ANSI / control bytes survive.
func Line(s string) string {
	return sanitizeLine(s, MaxLineRunes)
}

func sanitizeLine(s string, maxRunes int) string {
	if s == "" {
		return ""
	}
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	runes := 0
	for i := 0; i < len(rs) && runes < maxRunes; {
		r := rs[i]
		if r == 0x1b {
			i = skipEscape(rs, i)
			continue
		}
		if r == utf8.RuneError {
			i++
			continue
		}
		if !isSafeLabelRune(r) {
			i++
			continue
		}
		b.WriteRune(r)
		runes++
		i++
	}
	return strings.TrimSpace(b.String())
}

// skipEscape returns the index just past an ANSI escape sequence starting
// at rs[start] (which must be ESC = 0x1b). If the sequence is malformed or
// truncated, skipEscape returns len(rs) so the rest of the string is
// dropped; a truncated escape is more likely to be an injection attempt
// than a legitimate label.
func skipEscape(rs []rune, start int) int {
	n := len(rs)
	if start+1 >= n {
		return n
	}
	second := rs[start+1]
	switch second {
	case '[':
		j := start + 2
		for j < n {
			c := rs[j]
			if c >= 0x40 && c <= 0x7e {
				return j + 1
			}
			if c >= 0x20 && c <= 0x3f {
				j++
				continue
			}
			return j
		}
		return n
	case ']', 'P', 'X', '^', '_':
		j := start + 2
		for j < n {
			c := rs[j]
			if c == 0x07 {
				return j + 1
			}
			if c == 0x1b && j+1 < n && rs[j+1] == '\\' {
				return j + 2
			}
			j++
		}
		return n
	default:
		return start + 2
	}
}

// isSafeLabelRune reports whether r is allowed in a sanitized label.
func isSafeLabelRune(r rune) bool {
	if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
		return false
	}
	if !unicode.IsPrint(r) {
		return false
	}
	switch r {
	case 0x202a, 0x202b, 0x202c, 0x202d, 0x202e,
		0x2066, 0x2067, 0x2068, 0x2069:
		return false
	}
	switch r {
	case 0x200b, 0x200c, 0x200d, 0xfeff:
		return false
	}
	return true
}

// Arg validates that s is safe to pass as a literal subprocess argument via
// os/exec. Because sm4c never invokes a shell, the only strictly unsafe
// byte is NUL (which would terminate the argv C-string in the kernel); we
// additionally reject C0 control bytes other than horizontal tab because
// they would corrupt tmux's line-oriented control-mode parser if they
// leaked into an argument that tmux echoes back.
func Arg(s string) error {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x00 {
			return ErrUnsafeArg
		}
		if c < 0x20 && c != '\t' {
			return ErrUnsafeArg
		}
		if c == 0x7f {
			return ErrUnsafeArg
		}
	}
	return nil
}
