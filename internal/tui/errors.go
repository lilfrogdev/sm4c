package tui

import "errors"

// errUnexpectedModelType is returned by Run when tea.Program.Run
// returns a tea.Model that is not the concrete Model we passed in.
// This should be unreachable in practice — Bubble Tea guarantees it
// gives back the type it was handed — but we surface it as an error
// rather than panicking so sm4c never crashes on a future Bubble Tea
// refactor.
var errUnexpectedModelType = errors.New("tui: unexpected final model type from bubbletea runtime")
