package tui

import (
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

// keyMsgToBytes translates a bubbletea tea.KeyMsg into the raw byte
// sequence a program running under a terminal would receive for the
// same keystroke. The result is what sm4c hands to
// tmuxctl.OneShot.SendKeys via `send-keys -H`, so the target claude
// pane sees input that is bit-identical to what it would see if the
// user were typing into a real tmux attach.
//
// The translation is intentionally explicit. Bubble Tea's
// tea.KeyMsg.String() returns human-readable names like "ctrl+c" or
// "up", which are useful for Update's own routing but are NOT the
// shape a tty actually sends. Every case below encodes the standard
// byte sequence for the corresponding key; if a future key is not
// yet mapped, keyMsgToBytes returns nil and the caller drops the
// keystroke (rather than forwarding something wrong).
//
// Alt modifier handling: in xterm-compatible terminals, Alt+X is
// encoded as ESC (0x1b) followed by the bytes for X. We preserve
// that convention so Alt+enter / Alt+b / … behave naturally inside
// claude.
//
// Notes on a few non-obvious choices:
//
//   - Enter sends 0x0d (CR), not 0x0a (LF). Real terminals emit CR
//     on the Enter key; the tty's line discipline is responsible for
//     any CR→LF translation, which happens inside the kernel for
//     the target pane.
//   - Backspace sends 0x7f (DEL). This is what every modern terminal
//     sends on the Backspace key; legacy 0x08 (^H) is reserved for
//     explicit Ctrl+H.
//   - Ctrl+A..Ctrl+Z send the corresponding control byte (0x01..0x1a).
//     Bubble Tea's KeyType for these is already the byte value, so
//     the default clause handles them via a single branch rather
//     than a giant switch.
//   - We deliberately do NOT forward bubbletea's "runes" payload
//     when a control key is pressed (msg.Type != KeyRunes); for
//     example Ctrl+C arrives as {Type: KeyCtrlC, Runes: nil}, and
//     we emit 0x03. Confusing a control type with a rune would
//     drop the modifier.
func keyMsgToBytes(msg tea.KeyMsg) []byte {
	out := encodeKeyType(msg.Type, msg.Runes)
	if out == nil {
		return nil
	}
	if msg.Alt {
		return append([]byte{0x1b}, out...)
	}
	return out
}

// encodeKeyType does the bulk of the mapping. Split out so the Alt
// prefix handling lives in exactly one place and the table here
// stays purely about the key itself.
func encodeKeyType(t tea.KeyType, runes []rune) []byte {
	switch t {
	case tea.KeyRunes:
		if len(runes) == 0 {
			return nil
		}
		buf := make([]byte, 0, len(runes)*utf8.UTFMax)
		for _, r := range runes {
			var tmp [utf8.UTFMax]byte
			n := utf8.EncodeRune(tmp[:], r)
			buf = append(buf, tmp[:n]...)
		}
		return buf
	case tea.KeySpace:
		return []byte{' '}

	// Cursor-motion keys. The xterm CSI sequences below are what
	// every terminal emulator sm4c claims to support (macOS
	// Terminal, iTerm2, Alacritty, GNOME Terminal, Kitty, WezTerm)
	// actually emits. If we ever need to support an exotic
	// terminal (e.g. urxvt in "rxvt" mode emits \e[Oa/Ob/Oc/Od
	// for ctrl-arrows), that would belong here as a separate
	// case rather than polluting the default.
	case tea.KeyUp:
		return []byte{0x1b, '[', 'A'}
	case tea.KeyDown:
		return []byte{0x1b, '[', 'B'}
	case tea.KeyRight:
		return []byte{0x1b, '[', 'C'}
	case tea.KeyLeft:
		return []byte{0x1b, '[', 'D'}
	case tea.KeyShiftTab:
		return []byte{0x1b, '[', 'Z'}

	// Navigation / editing. "CSI ~"-terminated sequences are the
	// xterm format; the numeric parameter is what tmux, vim, bash
	// readline, and claude all recognize.
	case tea.KeyHome:
		return []byte{0x1b, '[', 'H'}
	case tea.KeyEnd:
		return []byte{0x1b, '[', 'F'}
	case tea.KeyPgUp:
		return []byte{0x1b, '[', '5', '~'}
	case tea.KeyPgDown:
		return []byte{0x1b, '[', '6', '~'}
	case tea.KeyDelete:
		return []byte{0x1b, '[', '3', '~'}

	// Ctrl + arrow / ctrl + Home / End / PgUp / PgDown — the
	// ";5" parameter is xterm's "control modifier" shape.
	case tea.KeyCtrlUp:
		return []byte{0x1b, '[', '1', ';', '5', 'A'}
	case tea.KeyCtrlDown:
		return []byte{0x1b, '[', '1', ';', '5', 'B'}
	case tea.KeyCtrlRight:
		return []byte{0x1b, '[', '1', ';', '5', 'C'}
	case tea.KeyCtrlLeft:
		return []byte{0x1b, '[', '1', ';', '5', 'D'}
	case tea.KeyCtrlHome:
		return []byte{0x1b, '[', '1', ';', '5', 'H'}
	case tea.KeyCtrlEnd:
		return []byte{0x1b, '[', '1', ';', '5', 'F'}
	case tea.KeyCtrlPgUp:
		return []byte{0x1b, '[', '5', ';', '5', '~'}
	case tea.KeyCtrlPgDown:
		return []byte{0x1b, '[', '6', ';', '5', '~'}

	// Function keys F1–F12 (xterm + modern terminal conventions).
	// F1..F4 use the SS3 "ESC O" form; F5..F12 use CSI "ESC [ N ~".
	case tea.KeyF1:
		return []byte{0x1b, 'O', 'P'}
	case tea.KeyF2:
		return []byte{0x1b, 'O', 'Q'}
	case tea.KeyF3:
		return []byte{0x1b, 'O', 'R'}
	case tea.KeyF4:
		return []byte{0x1b, 'O', 'S'}
	case tea.KeyF5:
		return []byte{0x1b, '[', '1', '5', '~'}
	case tea.KeyF6:
		return []byte{0x1b, '[', '1', '7', '~'}
	case tea.KeyF7:
		return []byte{0x1b, '[', '1', '8', '~'}
	case tea.KeyF8:
		return []byte{0x1b, '[', '1', '9', '~'}
	case tea.KeyF9:
		return []byte{0x1b, '[', '2', '0', '~'}
	case tea.KeyF10:
		return []byte{0x1b, '[', '2', '1', '~'}
	case tea.KeyF11:
		return []byte{0x1b, '[', '2', '3', '~'}
	case tea.KeyF12:
		return []byte{0x1b, '[', '2', '4', '~'}
	}

	// Control-byte range: KeyType is already the byte value for
	// every Ctrl+letter and for Enter / Tab / Escape / Backspace.
	// bubbletea encodes those as `KeyType = keyCR` etc., where the
	// constant is a small positive int in 0..31 or exactly 127.
	// Anything else (negative iota values for named keys we did not
	// explicitly map) is returned as nil so the caller can drop it.
	if t >= 0 && t <= 31 {
		return []byte{byte(t)}
	}
	if t == 127 {
		return []byte{0x7f}
	}
	return nil
}
