package tmuxctl

import (
	"errors"
	"fmt"
)

// ErrTruncatedEscape is returned by Unescape when a `\` appears with
// fewer than three bytes remaining.
var ErrTruncatedEscape = errors.New("tmuxctl: truncated octal escape")

// ErrInvalidEscape is returned by Unescape when `\` is followed by
// bytes that are not three octal digits.
var ErrInvalidEscape = errors.New("tmuxctl: invalid octal escape")

// Unescape decodes a tmux control-mode `%output` data segment into the
// raw bytes tmux observed on the pane.
//
// tmux encodes every byte that is a backslash (`\`, 0x5C), a C0 control
// (0x00-0x1F), or outside printable ASCII (0x7F-0xFF) as a literal
// backslash followed by exactly three octal digits. Every other byte is
// written through as-is. The decoder must be strict about the digit count
// and the digit range (0-7): a permissive decoder here means that hostile
// `claude` output could smuggle raw escape bytes past our VT and directly
// into the host terminal. `octal digits only, three of them, no \\ shortcut`
// is the invariant enforced below, and the fuzz test in escape_test.go
// pins it down.
func Unescape(in []byte) ([]byte, error) {
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); {
		c := in[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		if i+3 >= len(in) {
			return nil, fmt.Errorf("%w at offset %d", ErrTruncatedEscape, i)
		}
		d1, d2, d3 := in[i+1], in[i+2], in[i+3]
		if !isOctalDigit(d1) || !isOctalDigit(d2) || !isOctalDigit(d3) {
			return nil, fmt.Errorf("%w at offset %d: %q", ErrInvalidEscape, i, in[i:i+4])
		}
		v := (uint16(d1-'0') << 6) | (uint16(d2-'0') << 3) | uint16(d3-'0')
		if v > 0xFF {
			return nil, fmt.Errorf("%w at offset %d: value %d > 255", ErrInvalidEscape, i, v)
		}
		out = append(out, byte(v))
		i += 4
	}
	return out, nil
}

func isOctalDigit(b byte) bool { return b >= '0' && b <= '7' }
