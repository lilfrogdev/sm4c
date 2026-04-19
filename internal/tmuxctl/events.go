package tmuxctl

// Event is the sealed marker interface for every parsed tmux control-mode
// notification. Consumers should type-switch on the concrete type.
type Event interface{ isEvent() }

// BeginEvent is a `%begin <time> <command-num> <flags>` line. Subsequent
// DataEvents until the matching EndEvent/ErrorEvent belong to that
// command.
type BeginEvent struct {
	Time       int64
	CommandNum uint64
	Flags      uint64
}

// EndEvent is a `%end <time> <command-num> <flags>` line and terminates
// the output block opened by the BeginEvent with the same CommandNum.
type EndEvent struct {
	Time       int64
	CommandNum uint64
	Flags      uint64
}

// ErrorEvent is a `%error <time> <command-num> <flags>` line. Its
// CommandNum matches a preceding BeginEvent. The body (emitted as
// DataEvents between begin and error) contains the error message.
type ErrorEvent struct {
	Time       int64
	CommandNum uint64
	Flags      uint64
}

// DataEvent is one line of command output captured inside a begin/end
// block. The slice is a fresh copy — callers may retain it.
type DataEvent struct {
	Line []byte
}

// OutputEvent is an asynchronous `%output %<pane-id> <data>` notification.
// Data has already been passed through Unescape, so it contains raw bytes
// as they appeared on the pane.
type OutputEvent struct {
	PaneID string
	Data   []byte
}

// ExitEvent is the `%exit [<reason>]` notification that tmux emits just
// before disconnecting. Reason is empty if tmux did not supply one.
type ExitEvent struct {
	Reason string
}

// NotificationEvent captures every `%xxx` notification we do not yet
// decode into a structured event (e.g. `%window-add`, `%session-changed`,
// `%layout-change`). Kind is the notification name without the leading
// `%`; Args is the remainder split on ASCII whitespace.
//
// Keeping unknowns as a typed event (rather than dropping them) preserves
// forward compatibility: when tmux adds a new notification, sm4c stays
// readable instead of blowing up.
type NotificationEvent struct {
	Kind string
	Args []string
}

// RawEvent is a line that did not fit any other category. This should not
// appear in a well-formed stream; it exists so the parser can report
// surprises without panicking.
type RawEvent struct {
	Line string
}

func (BeginEvent) isEvent()        {}
func (EndEvent) isEvent()          {}
func (ErrorEvent) isEvent()        {}
func (DataEvent) isEvent()         {}
func (OutputEvent) isEvent()       {}
func (ExitEvent) isEvent()         {}
func (NotificationEvent) isEvent() {}
func (RawEvent) isEvent()          {}
