package tmuxctl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// NewClientForTest is a test-only accessor that wires a Client to
// arbitrary byte streams without spawning a tmux process. Defined in the
// same package so we can reach unexported fields; the `_test.go` file it
// lives in is excluded from non-test builds.
//
// By default the handshake gate is bypassed, so the synthetic stream is
// treated as if tmux's initial %begin/%end pair had already been absorbed.
func NewClientForTest(stdin io.WriteCloser, stdout io.Reader) *Client {
	return newClientForTest(stdin, stdout, true)
}

// NewClientForTestWithHandshake is like NewClientForTest but keeps the
// handshake gate enabled so a test can feed a stream that begins with a
// realistic tmux handshake block.
func NewClientForTestWithHandshake(stdin io.WriteCloser, stdout io.Reader) *Client {
	return newClientForTest(stdin, stdout, false)
}

// nopWriteCloser lets us pass a Writer as the client's stdin without
// spinning up a real pipe.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	bad := []ClientConfig{
		{},
		{TmuxBin: "tmux", SocketName: "sm4c", SessionName: "sm4c"},
		{TmuxBin: "/usr/bin/tmux", SocketName: "", SessionName: "sm4c"},
		{TmuxBin: "/usr/bin/tmux", SocketName: "a/b", SessionName: "sm4c"},
		{TmuxBin: "/usr/bin/tmux", SocketName: "sm4c", SessionName: ""},
		{TmuxBin: "/usr/bin/tmux", SocketName: "sm4c", SessionName: "sm4c", ExtraEnv: []string{"NOEQ"}},
		{TmuxBin: "/usr/bin/tmux", SocketName: "sm\x00bad", SessionName: "sm4c"},
	}
	for i, cfg := range bad {
		if err := cfg.validate(); err == nil {
			t.Errorf("case %d: validate %+v unexpectedly succeeded", i, cfg)
		}
	}

	ok := ClientConfig{
		TmuxBin:     "/usr/bin/tmux",
		SocketName:  "sm4c",
		SessionName: "sm4c",
		ExtraEnv:    []string{"TERM=xterm-256color"},
	}
	if err := ok.validate(); err != nil {
		t.Errorf("validate rejected known-good config: %v", err)
	}
}

func TestSend_RoundTripsCommandResponse(t *testing.T) {
	t.Parallel()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	c := NewClientForTest(stdinW, stdoutR)

	go func() {
		_, _ = io.Copy(io.Discard, stdinR)
	}()

	go func() {
		time.Sleep(5 * time.Millisecond)
		_, _ = stdoutW.Write([]byte("%begin 1 7 0\n@0 claude\n@1 tests\n%end 1 7 0\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := c.Send(ctx, "list-windows")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.CommandNum != 7 {
		t.Errorf("CommandNum = %d; want 7", res.CommandNum)
	}
	if res.IsError {
		t.Errorf("IsError = true; want false")
	}
	if string(res.Output) != "@0 claude\n@1 tests" {
		t.Errorf("Output = %q; want @0 claude\\n@1 tests", res.Output)
	}

	_ = stdoutW.Close()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = stdoutR.Close()
}

func TestSend_ErrorBlock(t *testing.T) {
	t.Parallel()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	c := NewClientForTest(stdinW, stdoutR)
	go func() { _, _ = io.Copy(io.Discard, stdinR) }()

	go func() {
		time.Sleep(5 * time.Millisecond)
		_, _ = stdoutW.Write([]byte("%begin 1 3 0\nno current window\n%error 1 3 0\n"))
	}()

	res, err := c.Send(context.Background(), "kill-window")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !res.IsError {
		t.Error("expected IsError = true")
	}
	if !bytes.Contains(res.Output, []byte("no current window")) {
		t.Errorf("Output missing error body: %q", res.Output)
	}

	_ = stdoutW.Close()
	_ = c.Close()
	_ = stdoutR.Close()
}

func TestSend_RejectsUnsafeCommand(t *testing.T) {
	t.Parallel()

	c := NewClientForTest(nopWriteCloser{io.Discard}, strings.NewReader(""))
	defer func() { _ = c.Close() }()

	bad := []string{
		"kill\x00window",
		"line-1\nline-2",
		"cr\rinjected",
		"esc\x1bhere",
	}
	for _, cmd := range bad {
		if _, err := c.Send(context.Background(), cmd); !errors.Is(err, ErrUnsafeCommand) {
			t.Errorf("Send(%q) err=%v; want ErrUnsafeCommand", cmd, err)
		}
	}
}

func TestAsyncEventsForwarded(t *testing.T) {
	t.Parallel()

	stdoutR, stdoutW := io.Pipe()
	c := NewClientForTest(nopWriteCloser{io.Discard}, stdoutR)
	defer func() { _ = c.Close() }()

	go func() {
		_, _ = stdoutW.Write([]byte("%output %1 hi\\012\n%window-add @5\n%exit bye\n"))
		_ = stdoutW.Close()
	}()

	var got []Event
	timeout := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				goto done
			}
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("timed out; got %d events", len(got))
		}
	}
done:
	if len(got) != 3 {
		t.Fatalf("got %d events; want 3: %#v", len(got), got)
	}
	if o, ok := got[0].(OutputEvent); !ok || o.PaneID != "%1" || string(o.Data) != "hi\n" {
		t.Errorf("events[0] = %#v", got[0])
	}
	if n, ok := got[1].(NotificationEvent); !ok || n.Kind != "window-add" {
		t.Errorf("events[1] = %#v", got[1])
	}
	if ex, ok := got[2].(ExitEvent); !ok || ex.Reason != "bye" {
		t.Errorf("events[2] = %#v", got[2])
	}
}

func TestClose_IsIdempotent(t *testing.T) {
	t.Parallel()

	c := NewClientForTest(nopWriteCloser{io.Discard}, strings.NewReader(""))
	if err := c.Close(); err != nil {
		t.Fatalf("Close #1: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close #2: %v", err)
	}
}

func TestHandshakeAbsorbed(t *testing.T) {
	t.Parallel()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	c := NewClientForTestWithHandshake(stdinW, stdoutR)
	go func() { _, _ = io.Copy(io.Discard, stdinR) }()

	go func() {
		// Emit a realistic handshake followed by real command response.
		_, _ = stdoutW.Write([]byte("%begin 1 709 0\n%end 1 709 0\n%window-add @0\n"))
		time.Sleep(5 * time.Millisecond)
		_, _ = stdoutW.Write([]byte("%begin 1 715 1\nok\n%end 1 715 1\n"))
	}()

	res, err := c.Send(context.Background(), "display-message -p ok")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.CommandNum != 715 {
		t.Fatalf("CommandNum = %d; want 715 (handshake was not absorbed)", res.CommandNum)
	}
	if string(res.Output) != "ok" {
		t.Fatalf("Output = %q; want %q", res.Output, "ok")
	}

	// window-add from before handshake end should have been forwarded.
	select {
	case ev := <-c.Events():
		if n, ok := ev.(NotificationEvent); !ok || n.Kind != "window-add" {
			t.Fatalf("events[0] = %#v", ev)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected window-add event")
	}

	_ = stdoutW.Close()
	_ = c.Close()
	_ = stdoutR.Close()
}

func TestRingBuffer_Cap(t *testing.T) {
	t.Parallel()

	r := newRingBuffer(8)
	_, _ = r.Write([]byte("abcd"))
	_, _ = r.Write([]byte("efgh"))
	_, _ = r.Write([]byte("IJKL"))
	got := r.Snapshot()
	if len(got) > 8 {
		t.Fatalf("len=%d; want <=8", len(got))
	}
	if !bytes.HasSuffix(got, []byte("IJKL")) {
		t.Fatalf("snapshot did not preserve most-recent bytes: %q", got)
	}
}
