// Package tmuxctl is sm4c's client for tmux's line-oriented control mode
// protocol.
//
// tmux control mode (started with `tmux -C` or `tmux -CC`) lets a parent
// process drive a tmux server over a pair of pipes. Every command sent
// over stdin produces a response block delimited by:
//
//	%begin <unix-time> <command-num> <flags>
//	...output...
//	%end   <unix-time> <command-num> <flags>
//
// or `%error` in place of `%end` when the command fails. Alongside these
// command responses, tmux emits asynchronous notifications that describe
// server events. Notifications always start with `%`:
//
//	%output %<pane-id> <octal-escaped-bytes>
//	%window-add @<window-id>
//	%window-close @<window-id>
//	%exit [<reason>]
//
// M1 of sm4c implements:
//
//   - An octal-escape decoder (escape.go), since `%output` encodes every
//     byte that is a backslash, a C0 control, or a non-ASCII byte as a
//     three-digit octal escape `\ooo`. A naive reader that does not decode
//     these bytes will render literal "\033[31m" where tmux meant to pass
//     through an ANSI SGR sequence.
//
//   - A stream parser (parser.go) that classifies every line into a
//     structured Event, handles the nesting of begin/end blocks, and
//     preserves asynchronous notifications even when they interleave
//     with command output.
//
//   - A Client (client.go) that spawns `tmux -L sm4c -CC` on an isolated
//     socket, serializes Send calls, and fans out events to a channel.
//
// Everything in this package is subprocess- and parse-heavy — i.e. high
// attack surface. See SECURITY.md: all byte slices from tmux are treated
// as untrusted until they pass through internal/safe.
package tmuxctl
