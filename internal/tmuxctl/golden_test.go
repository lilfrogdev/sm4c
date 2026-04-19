package tmuxctl

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
)

// TestParser_Golden drives the parser over a hand-crafted transcript that
// exercises every event type we care about in M1: notifications, command
// response blocks (both %end and %error), asynchronous %output during
// and between blocks, and a terminating %exit. Updating the fixture is a
// deliberate action — regenerate only after hand-verifying with a real
// tmux session.
func TestParser_Golden(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/session.ctrl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	p := NewParser(bytes.NewReader(data))

	var got []Event
	for {
		ev, err := p.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parser error after %d events: %v", len(got), err)
		}
		got = append(got, ev)
	}

	// Expected high-level shape, asserted by type + salient fields so that
	// incidental byte-level choices in the fixture can evolve without
	// rewriting 30 lines of expectations.
	want := []struct {
		check func(Event) bool
		name  string
	}{
		{func(e Event) bool {
			n, ok := e.(NotificationEvent)
			return ok && n.Kind == "sessions-changed"
		}, "sessions-changed"},
		{func(e Event) bool {
			n, ok := e.(NotificationEvent)
			return ok && n.Kind == "session-changed" && len(n.Args) == 2
		}, "session-changed $0 sm4c"},
		{func(e Event) bool {
			n, ok := e.(NotificationEvent)
			return ok && n.Kind == "window-add" && len(n.Args) == 1 && n.Args[0] == "@0"
		}, "window-add @0"},
		{func(e Event) bool {
			b, ok := e.(BeginEvent)
			return ok && b.CommandNum == 1
		}, "begin 1"},
		{func(e Event) bool {
			d, ok := e.(DataEvent)
			return ok && string(d.Line) == "@0 claude"
		}, "list-windows data"},
		{func(e Event) bool {
			end, ok := e.(EndEvent)
			return ok && end.CommandNum == 1
		}, "end 1"},
		{func(e Event) bool {
			o, ok := e.(OutputEvent)
			return ok && o.PaneID == "%1" && bytes.Contains(o.Data, []byte("\x1b[?25hclaude> "))
		}, "output show-cursor + prompt"},
		{func(e Event) bool {
			o, ok := e.(OutputEvent)
			return ok && o.PaneID == "%1" && bytes.HasSuffix(o.Data, []byte("\r\n"))
		}, "output greeting with CRLF"},
		{func(e Event) bool {
			n, ok := e.(NotificationEvent)
			return ok && n.Kind == "window-renamed" && len(n.Args) == 2 && n.Args[1] == "claude-refactor"
		}, "window-renamed"},
		{func(e Event) bool {
			_, ok := e.(BeginEvent)
			return ok
		}, "begin 2"},
		{func(e Event) bool {
			_, ok := e.(EndEvent)
			return ok
		}, "end 2"},
		{func(e Event) bool {
			o, ok := e.(OutputEvent)
			return ok && o.PaneID == "%1" && bytes.Contains(o.Data, []byte("\x1b[2J\x1b[H"))
		}, "output clear screen"},
		{func(e Event) bool {
			_, ok := e.(BeginEvent)
			return ok
		}, "begin 3"},
		{func(e Event) bool {
			d, ok := e.(DataEvent)
			return ok && string(d.Line) == "no current window"
		}, "error body"},
		{func(e Event) bool {
			er, ok := e.(ErrorEvent)
			return ok && er.CommandNum == 3
		}, "error 3"},
		{func(e Event) bool {
			ex, ok := e.(ExitEvent)
			return ok && ex.Reason == "server exited"
		}, "exit with reason"},
	}

	if len(got) != len(want) {
		t.Fatalf("event count mismatch: got %d, want %d\nevents: %#v", len(got), len(want), got)
	}
	for i, w := range want {
		if !w.check(got[i]) {
			t.Errorf("event %d (%s) failed shape check; got %#v", i, w.name, got[i])
		}
	}
}
