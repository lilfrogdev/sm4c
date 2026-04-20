package tui

import (
	"bytes"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// keys_test.go pins the M3c byte-level translation table. Every
// case here describes one kind of keystroke a user might press and
// the exact byte sequence a terminal would emit for it, which is
// what sm4c hands to tmux's `send-keys -H`. A regression in this
// table would silently corrupt input routing: tmux accepts the
// malformed bytes and claude sees ghost characters.

func TestKeyMsgToBytes_Runes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		runes []rune
		want  []byte
	}{
		{"ascii letter", []rune{'a'}, []byte{0x61}},
		{"ascii digit", []rune{'3'}, []byte{0x33}},
		{"ascii symbol", []rune{'/'}, []byte{0x2f}},
		{"space via rune", []rune{' '}, []byte{0x20}},
		{"multi-byte rune", []rune{'é'}, []byte{0xc3, 0xa9}},
		{"emoji", []rune{'🙂'}, []byte{0xf0, 0x9f, 0x99, 0x82}},
		{"multi-rune (paste)", []rune{'h', 'i'}, []byte{'h', 'i'}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: c.runes}
			got := keyMsgToBytes(msg)
			if !bytes.Equal(got, c.want) {
				t.Fatalf("keyMsgToBytes(%q) = % x; want % x", string(c.runes), got, c.want)
			}
		})
	}
}

func TestKeyMsgToBytes_ControlKeys(t *testing.T) {
	t.Parallel()

	// Spot-check every control key in bubbletea's KeyType: the
	// constants in key.go are already the byte value for ctrl+X,
	// so this also guards against a refactor that accidentally
	// mapped ctrl+X -> msg.Runes for some letters but not others.
	cases := []struct {
		name string
		typ  tea.KeyType
		want []byte
	}{
		{"ctrl+a", tea.KeyCtrlA, []byte{0x01}},
		{"ctrl+c", tea.KeyCtrlC, []byte{0x03}},
		{"ctrl+d", tea.KeyCtrlD, []byte{0x04}},
		{"ctrl+u", tea.KeyCtrlU, []byte{0x15}},
		{"ctrl+z", tea.KeyCtrlZ, []byte{0x1a}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := keyMsgToBytes(tea.KeyMsg{Type: c.typ})
			if !bytes.Equal(got, c.want) {
				t.Fatalf("%s = % x; want % x", c.name, got, c.want)
			}
		})
	}
}

func TestKeyMsgToBytes_NamedKeys(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		typ  tea.KeyType
		want []byte
	}{
		// Whitespace / line editing. Enter MUST be 0x0d (CR):
		// terminals emit CR for the Enter key; the tty's line
		// discipline is responsible for any CR->LF translation.
		{"enter", tea.KeyEnter, []byte{0x0d}},
		{"tab", tea.KeyTab, []byte{0x09}},
		{"backspace", tea.KeyBackspace, []byte{0x7f}},
		{"escape", tea.KeyEscape, []byte{0x1b}},
		{"space", tea.KeySpace, []byte{' '}},

		// Arrows — the xterm CSI sequences that every terminal
		// emulator on macOS and Linux emits.
		{"up", tea.KeyUp, []byte{0x1b, '[', 'A'}},
		{"down", tea.KeyDown, []byte{0x1b, '[', 'B'}},
		{"right", tea.KeyRight, []byte{0x1b, '[', 'C'}},
		{"left", tea.KeyLeft, []byte{0x1b, '[', 'D'}},

		// Navigation + editing.
		{"home", tea.KeyHome, []byte{0x1b, '[', 'H'}},
		{"end", tea.KeyEnd, []byte{0x1b, '[', 'F'}},
		{"pgup", tea.KeyPgUp, []byte{0x1b, '[', '5', '~'}},
		{"pgdown", tea.KeyPgDown, []byte{0x1b, '[', '6', '~'}},
		{"delete", tea.KeyDelete, []byte{0x1b, '[', '3', '~'}},
		{"shift+tab", tea.KeyShiftTab, []byte{0x1b, '[', 'Z'}},

		// Function keys F1–F12.
		{"f1", tea.KeyF1, []byte{0x1b, 'O', 'P'}},
		{"f4", tea.KeyF4, []byte{0x1b, 'O', 'S'}},
		{"f5", tea.KeyF5, []byte{0x1b, '[', '1', '5', '~'}},
		{"f12", tea.KeyF12, []byte{0x1b, '[', '2', '4', '~'}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := keyMsgToBytes(tea.KeyMsg{Type: c.typ})
			if !bytes.Equal(got, c.want) {
				t.Fatalf("%s = % x; want % x", c.name, got, c.want)
			}
		})
	}
}

func TestKeyMsgToBytes_AltPrefix(t *testing.T) {
	t.Parallel()

	// Alt+X in xterm-compatible terminals is encoded as ESC +
	// bytes(X). Pin this so a future refactor that tried to use
	// the "meta" modifier flag differently doesn't silently swap
	// alt+b for the wrong bytes.
	cases := []struct {
		name string
		msg  tea.KeyMsg
		want []byte
	}{
		{"alt+b as rune", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}, Alt: true}, []byte{0x1b, 'b'}},
		{"alt+enter", tea.KeyMsg{Type: tea.KeyEnter, Alt: true}, []byte{0x1b, 0x0d}},
		{"alt+left", tea.KeyMsg{Type: tea.KeyLeft, Alt: true}, []byte{0x1b, 0x1b, '[', 'D'}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := keyMsgToBytes(c.msg)
			if !bytes.Equal(got, c.want) {
				t.Fatalf("%s = % x; want % x", c.name, got, c.want)
			}
		})
	}
}

func TestKeyMsgToBytes_UnmappedKeyIsDropped(t *testing.T) {
	t.Parallel()

	// An unknown KeyType (e.g. a bubbletea extension we have not
	// mapped yet) must return nil so sendKeysToPane drops the
	// keystroke rather than forwarding garbage bytes.
	msg := tea.KeyMsg{Type: tea.KeyType(-9999)}
	if got := keyMsgToBytes(msg); got != nil {
		t.Fatalf("unmapped KeyType = % x; want nil", got)
	}

	// KeyRunes with an empty slice is also "nothing to forward".
	if got := keyMsgToBytes(tea.KeyMsg{Type: tea.KeyRunes}); got != nil {
		t.Fatalf("empty-runes KeyMsg = % x; want nil", got)
	}
}
