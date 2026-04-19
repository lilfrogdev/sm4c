package tmuxctl

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// drain reads every event from a Parser until io.EOF or the first error.
// Returns the event slice and the terminating error (io.EOF on success).
func drain(t *testing.T, p *Parser) ([]Event, error) {
	t.Helper()
	var out []Event
	for {
		ev, err := p.Next()
		if errors.Is(err, io.EOF) {
			return out, err
		}
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
}

func TestParser_EmptyStream(t *testing.T) {
	t.Parallel()
	p := NewParser(strings.NewReader(""))
	events, err := drain(t, p)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v; want EOF", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d events; want 0", len(events))
	}
}

func TestParser_SimpleCommandBlock(t *testing.T) {
	t.Parallel()
	input := "%begin 1 17 1\nhello\nworld\n%end 1 17 1\n"
	events, err := drain(t, NewParser(strings.NewReader(input)))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v; want EOF", err)
	}

	want := []Event{
		BeginEvent{Time: 1, CommandNum: 17, Flags: 1},
		DataEvent{Line: []byte("hello")},
		DataEvent{Line: []byte("world")},
		EndEvent{Time: 1, CommandNum: 17, Flags: 1},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events mismatch:\ngot  %#v\nwant %#v", events, want)
	}
}

func TestParser_CommandError(t *testing.T) {
	t.Parallel()
	input := "%begin 2 18 0\nno such window\n%error 2 18 0\n"
	events, err := drain(t, NewParser(strings.NewReader(input)))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v; want EOF", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events; want 3", len(events))
	}
	if _, ok := events[2].(ErrorEvent); !ok {
		t.Fatalf("expected ErrorEvent at index 2, got %T", events[2])
	}
}

func TestParser_OutputNotification(t *testing.T) {
	t.Parallel()
	input := "%output %42 hello\\011world\\012\n"
	events, err := drain(t, NewParser(strings.NewReader(input)))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v; want EOF", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events; want 1", len(events))
	}
	oe, ok := events[0].(OutputEvent)
	if !ok {
		t.Fatalf("event type = %T; want OutputEvent", events[0])
	}
	if oe.PaneID != "%42" {
		t.Fatalf("PaneID = %q; want %%42", oe.PaneID)
	}
	if !bytes.Equal(oe.Data, []byte("hello\tworld\n")) {
		t.Fatalf("Data = %q; want %q", oe.Data, []byte("hello\tworld\n"))
	}
}

func TestParser_ExitWithReason(t *testing.T) {
	t.Parallel()
	events, err := drain(t, NewParser(strings.NewReader("%exit server closed\n")))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v; want EOF", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events; want 1", len(events))
	}
	ex, ok := events[0].(ExitEvent)
	if !ok {
		t.Fatalf("event type = %T; want ExitEvent", events[0])
	}
	if ex.Reason != "server closed" {
		t.Fatalf("Reason = %q; want %q", ex.Reason, "server closed")
	}
}

func TestParser_ExitNoReason(t *testing.T) {
	t.Parallel()
	events, _ := drain(t, NewParser(strings.NewReader("%exit\n")))
	if len(events) != 1 {
		t.Fatalf("got %d; want 1", len(events))
	}
	if ex := events[0].(ExitEvent); ex.Reason != "" {
		t.Fatalf("Reason=%q; want empty", ex.Reason)
	}
}

func TestParser_UnknownNotificationPreserved(t *testing.T) {
	t.Parallel()
	events, _ := drain(t, NewParser(strings.NewReader("%window-add @5\n%session-changed $2 main\n")))
	if len(events) != 2 {
		t.Fatalf("got %d; want 2", len(events))
	}
	n1, ok := events[0].(NotificationEvent)
	if !ok || n1.Kind != "window-add" || !reflect.DeepEqual(n1.Args, []string{"@5"}) {
		t.Fatalf("events[0] = %#v", events[0])
	}
	n2, ok := events[1].(NotificationEvent)
	if !ok || n2.Kind != "session-changed" || !reflect.DeepEqual(n2.Args, []string{"$2", "main"}) {
		t.Fatalf("events[1] = %#v", events[1])
	}
}

func TestParser_InterleavedOutputInsideBlock(t *testing.T) {
	t.Parallel()
	// %output notifications can arrive between %begin and %end. They
	// should still be surfaced as OutputEvents, not treated as DataEvents.
	input := "%begin 1 1 0\nline1\n%output %7 hi\n%end 1 1 0\n"
	events, err := drain(t, NewParser(strings.NewReader(input)))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v", err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events; want 4", len(events))
	}
	if _, ok := events[2].(OutputEvent); !ok {
		t.Fatalf("events[2] = %T; want OutputEvent", events[2])
	}
}

func TestParser_RejectsOversizeLine(t *testing.T) {
	t.Parallel()
	p := NewParser(strings.NewReader(strings.Repeat("a", 100)))
	p.SetMaxLine(10)
	_, err := p.Next()
	if err == nil || !strings.Contains(err.Error(), "exceeds max") {
		t.Fatalf("err=%v; want oversize", err)
	}
}

func TestParser_RejectsMalformedHeader(t *testing.T) {
	t.Parallel()
	cases := []string{
		"%begin 1\n",
		"%begin foo bar baz\n",
		"%end 1 abc 0\n",
		"%output not-a-pane-id data\n",
		"%output %42 bad-escape\\9\n",
	}
	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, err := NewParser(strings.NewReader(in)).Next()
			if err == nil {
				t.Fatalf("Next(%q) unexpectedly succeeded", in)
			}
		})
	}
}

func TestParser_TrailingCRIsStripped(t *testing.T) {
	t.Parallel()
	events, _ := drain(t, NewParser(strings.NewReader("%begin 1 1 0\r\nhello\r\n%end 1 1 0\r\n")))
	if len(events) != 3 {
		t.Fatalf("got %d; want 3", len(events))
	}
	if d := events[1].(DataEvent); string(d.Line) != "hello" {
		t.Fatalf("DataEvent line = %q; want %q", d.Line, "hello")
	}
}

func TestParser_PartialFinalLineSurfacedThenEOF(t *testing.T) {
	t.Parallel()
	p := NewParser(strings.NewReader("%exit no-newline"))
	ev, err := p.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ex, ok := ev.(ExitEvent); !ok || ex.Reason != "no-newline" {
		t.Fatalf("event = %#v", ev)
	}
	if _, err := p.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("second Next err = %v; want EOF", err)
	}
}

// FuzzParser ensures the parser never panics regardless of input. No
// invariants about the output shape — just that errors are surfaced as
// errors, not panics, and that any successful parse returns a typed
// Event (not nil). Run via:
//
//	go test -run=^$ -fuzz=FuzzParser ./internal/tmuxctl/
func FuzzParser(f *testing.F) {
	seeds := []string{
		"",
		"\n",
		"%begin 1 1 0\n%end 1 1 0\n",
		"%output %1 \\033[31mhi\\033[0m\n",
		"%exit bye\n",
		"%window-add @5\n",
		"random garbage\n",
		"%output %1 bad\\9\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p := NewParser(strings.NewReader(s))
		for i := 0; i < 500; i++ {
			ev, err := p.Next()
			if err != nil {
				return
			}
			if ev == nil {
				t.Fatalf("parser returned nil event with nil error for input %q", s)
			}
		}
	})
}
