package tmuxctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/lilfrogdev/sm4c/internal/safe"
)

// Errors surfaced by Client. Callers should use errors.Is.
var (
	// ErrClosed is returned by operations on a Client that has already
	// terminated (tmux exited, Close was called, or stdin broke).
	ErrClosed = errors.New("tmuxctl: client closed")

	// ErrUnsafeCommand is returned by Send when the command string
	// contains control bytes that could corrupt the tmux line protocol.
	// Callers that hit this should not retry; fix the caller.
	ErrUnsafeCommand = errors.New("tmuxctl: command contains unsafe bytes")
)

// ClientConfig is the minimal set of knobs needed to spawn a tmux server
// in control mode.
type ClientConfig struct {
	// TmuxBin is the absolute path to the tmux binary. Must be an absolute
	// path resolved via exec.LookPath by the caller — the Client refuses
	// relative paths to close off PATH-hijack attacks.
	TmuxBin string

	// SocketName is passed to `tmux -L`. Must be non-empty and must not
	// contain path separators (tmux treats it as a basename under the
	// per-user socket directory).
	SocketName string

	// SessionName is the tmux session sm4c attaches to or creates.
	SessionName string

	// EventBuffer is the size of the async events channel. Zero means
	// default (256). If the buffer fills, the dispatcher drops the newest
	// events and sets a loss flag the caller can query.
	EventBuffer int

	// ExtraEnv is extra environment variables passed to tmux on top of
	// the parent's env. Empty slice means "inherit only". Entries must be
	// of the form KEY=VALUE.
	ExtraEnv []string
}

func (c ClientConfig) validate() error {
	if c.TmuxBin == "" {
		return errors.New("tmuxctl: TmuxBin is required")
	}
	if !filepath.IsAbs(c.TmuxBin) {
		return fmt.Errorf("tmuxctl: TmuxBin must be absolute, got %q", c.TmuxBin)
	}
	if c.SocketName == "" {
		return errors.New("tmuxctl: SocketName is required")
	}
	if strings.ContainsAny(c.SocketName, "/\\") {
		return fmt.Errorf("tmuxctl: SocketName must be a basename, got %q", c.SocketName)
	}
	if err := safe.Arg(c.SocketName); err != nil {
		return fmt.Errorf("tmuxctl: SocketName: %w", err)
	}
	if c.SessionName == "" {
		return errors.New("tmuxctl: SessionName is required")
	}
	if err := safe.Arg(c.SessionName); err != nil {
		return fmt.Errorf("tmuxctl: SessionName: %w", err)
	}
	for _, e := range c.ExtraEnv {
		if !strings.Contains(e, "=") {
			return fmt.Errorf("tmuxctl: ExtraEnv entry %q missing '='", e)
		}
	}
	return nil
}

// Result is the response to a successful Send call.
type Result struct {
	// CommandNum is the tmux-assigned command number from the %begin/%end
	// pair. Useful for correlating logs with tmux's own view of the
	// world.
	CommandNum uint64

	// Output is the command output: the joined DataEvents between
	// %begin and %end. It is the raw bytes tmux sent, with line
	// separators preserved.
	Output []byte

	// IsError is true when tmux terminated the block with %error rather
	// than %end. Output then contains the error message tmux produced.
	IsError bool
}

// Client drives a tmux server over its line-oriented control protocol.
//
// Concurrency: Send is safe to call from any goroutine but is serialized
// internally — only one in-flight command exists at a time. Events()
// returns a channel that is written from the reader goroutine; callers
// must drain it or the dispatcher will drop events.
type Client struct {
	cfg ClientConfig

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *ringBuffer

	events        chan Event
	done          chan struct{}
	ready         chan struct{}
	skipHandshake bool

	readerDone chan struct{}
	readerErr  atomic.Pointer[error]

	// sendMu serializes external Send calls.
	sendMu sync.Mutex

	// pendingMu guards pending (reader writes to it mid-block, Send
	// swaps it under Lock).
	pendingMu sync.Mutex
	pending   *pending

	// eventsLost counts events dropped because the buffer was full.
	eventsLost atomic.Uint64

	closeOnce sync.Once
}

// pending is an in-flight command. Constructed by Send, handed off to the
// reader via Client.pending, and retired either by EndEvent/ErrorEvent or
// by ctx cancellation.
type pending struct {
	cmdNum     uint64
	cmdNumSeen bool
	lines      [][]byte
	result     chan Result
	abandoned  atomic.Bool
}

// Start spawns a tmux process in control mode and returns a ready
// Client. The caller owns the returned Client and must call Close.
//
// The tmux invocation is:
//
//	tmux -L <socket> -C new-session -A -s <session>
//
// -L isolates us on our own socket. -C runs in control mode. -A on
// new-session makes the call idempotent: reuse an existing session or
// create a fresh one.
//
// We deliberately do NOT pass -D (which would detach every other
// client on that session). Multiple sm4c processes can legitimately
// share the socket at the same time — e.g. a long-lived TUI in one
// terminal plus a `sm4c ls` in another — and a stray -D here would
// tear down an active attach mid-typing. Tmux handles multiple
// attached clients natively; control-mode clients coexist with
// interactive tmux attach-session clients without issue.
func Start(ctx context.Context, cfg ClientConfig) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.EventBuffer == 0 {
		cfg.EventBuffer = 256
	}

	args := []string{
		"-L", cfg.SocketName,
		"-C",
		"new-session", "-A", "-s", cfg.SessionName,
	}
	// #nosec G204 -- cfg.TmuxBin is validated as an absolute path by
	// cfg.validate() above; every other arg is either a compile-time
	// literal or safe.Arg-checked in validate.
	cmd := exec.CommandContext(ctx, cfg.TmuxBin, args...)
	cmd.Env = append(cmd.Environ(), cfg.ExtraEnv...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("tmuxctl: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("tmuxctl: stdout pipe: %w", err)
	}
	stderrBuf := newRingBuffer(64 * 1024)
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("tmuxctl: start tmux: %w", err)
	}

	c := &Client{
		cfg:        cfg,
		cmd:        cmd,
		stdin:      stdin,
		stderr:     stderrBuf,
		events:     make(chan Event, cfg.EventBuffer),
		done:       make(chan struct{}),
		ready:      make(chan struct{}),
		readerDone: make(chan struct{}),
	}
	go c.reader(stdout)

	select {
	case <-c.ready:
		return c, nil
	case <-c.readerDone:
		_ = cmd.Wait()
		err := c.ReaderErr()
		if err == nil {
			err = fmt.Errorf("tmuxctl: tmux exited before handshake; stderr=%q", c.Stderr())
		}
		return nil, err
	case <-ctx.Done():
		_ = c.Close()
		return nil, ctx.Err()
	}
}

// newClientForTest wires a Client to arbitrary byte streams so the
// dispatcher can be exercised without spawning tmux. Exported to _test
// via test-only accessor. When skipHandshake is true the reader bypasses
// the tmux handshake gate, which is the common case when a test supplies
// a synthetic stream that never includes one.
func newClientForTest(stdin io.WriteCloser, stdout io.Reader, skipHandshake bool) *Client {
	c := &Client{
		stdin:         stdin,
		stderr:        newRingBuffer(4096),
		events:        make(chan Event, 16),
		done:          make(chan struct{}),
		ready:         make(chan struct{}),
		readerDone:    make(chan struct{}),
		skipHandshake: skipHandshake,
	}
	if skipHandshake {
		close(c.ready)
	}
	go c.reader(stdout)
	return c
}

// Events returns the channel of asynchronous events (OutputEvent,
// NotificationEvent, ExitEvent, RawEvent). The channel is closed when
// the Client terminates; a closed channel is the canonical signal that
// no more events will ever arrive.
func (c *Client) Events() <-chan Event { return c.events }

// EventsLost returns the number of async events the dispatcher dropped
// because Events() was not being drained fast enough.
func (c *Client) EventsLost() uint64 { return c.eventsLost.Load() }

// Done returns a channel closed when the Client has terminated. The
// paired ReaderErr() call then reports what caused it.
func (c *Client) Done() <-chan struct{} { return c.done }

// ReaderErr returns the error that ended the reader goroutine, or nil on
// clean EOF. Safe to call after Done is closed.
func (c *Client) ReaderErr() error {
	if p := c.readerErr.Load(); p != nil {
		return *p
	}
	return nil
}

// Stderr returns a snapshot of the last ~64 KB written by tmux to
// stderr. Useful for error messages and for sm4c doctor.
func (c *Client) Stderr() []byte {
	if c.stderr == nil {
		return nil
	}
	return c.stderr.Snapshot()
}

// Send writes cmd (a tmux command line) to tmux's stdin and waits for
// the matching %end / %error response. ctx controls only the wait — once
// tmux has accepted the write, cancellation causes Send to return
// ctx.Err but does not stop tmux from processing the command; stale
// responses from canceled commands are safely discarded.
func (c *Client) Send(ctx context.Context, cmd string) (Result, error) {
	if strings.ContainsAny(cmd, "\r\n\x00") {
		return Result{}, ErrUnsafeCommand
	}
	if err := safe.Arg(cmd); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrUnsafeCommand, err)
	}

	select {
	case <-c.done:
		return Result{}, ErrClosed
	default:
	}

	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	p := &pending{result: make(chan Result, 1)}
	c.setPending(p)

	if _, err := fmt.Fprintln(c.stdin, cmd); err != nil {
		c.setPending(nil)
		return Result{}, fmt.Errorf("tmuxctl: write command: %w", err)
	}

	select {
	case r := <-p.result:
		return r, nil
	case <-ctx.Done():
		p.abandoned.Store(true)
		c.setPending(nil)
		return Result{}, ctx.Err()
	case <-c.done:
		return Result{}, ErrClosed
	}
}

// Close terminates the client: closes stdin (so tmux detaches cleanly),
// waits for the reader goroutine, and reaps the tmux process. Close is
// idempotent.
func (c *Client) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		<-c.readerDone
		if c.cmd != nil {
			_ = c.cmd.Wait()
		}
		close(c.done)
		closeErr = c.ReaderErr()
	})
	return closeErr
}

// setPending swaps the current in-flight request under the pending lock.
func (c *Client) setPending(p *pending) {
	c.pendingMu.Lock()
	old := c.pending
	c.pending = p
	c.pendingMu.Unlock()
	if old != nil {
		old.abandoned.Store(true)
	}
}

func (c *Client) currentPending() *pending {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return c.pending
}

func (c *Client) reader(r io.Reader) {
	defer close(c.events)
	defer close(c.readerDone)

	p := NewParser(r)

	if !c.skipHandshake {
		if err := c.consumeHandshake(p); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				e := fmt.Errorf("tmuxctl: handshake: %w", err)
				c.readerErr.Store(&e)
			}
			return
		}
		close(c.ready)
	}

	var active *pending

	for {
		ev, err := p.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				e := fmt.Errorf("tmuxctl: parse: %w", err)
				c.readerErr.Store(&e)
			}
			if active != nil && !active.abandoned.Load() {
				active.result <- Result{
					CommandNum: active.cmdNum,
					IsError:    true,
					Output:     joinLines(active.lines),
				}
			}
			return
		}
		switch e := ev.(type) {
		case BeginEvent:
			active = c.currentPending()
			if active != nil {
				active.cmdNum = e.CommandNum
				active.cmdNumSeen = true
				active.lines = active.lines[:0]
			}
		case DataEvent:
			if active != nil && !active.abandoned.Load() {
				active.lines = append(active.lines, e.Line)
			}
		case EndEvent:
			if active != nil {
				if !active.abandoned.Load() {
					active.result <- Result{
						CommandNum: e.CommandNum,
						Output:     joinLines(active.lines),
					}
				}
				active = nil
			}
		case ErrorEvent:
			if active != nil {
				if !active.abandoned.Load() {
					active.result <- Result{
						CommandNum: e.CommandNum,
						Output:     joinLines(active.lines),
						IsError:    true,
					}
				}
				active = nil
			}
		default:
			c.forward(e)
		}
	}
}

// consumeHandshake absorbs the empty `%begin`/`%end` (or `%begin`/`%error`)
// block that tmux emits as soon as it enters control mode. Any async
// notifications (%output, %window-add, %sessions-changed, ...) that
// arrive before the handshake pair completes are forwarded as normal.
//
// The handshake gate is important: without it, a Send() that races with
// the server's initial response can "steal" the empty handshake block
// and return an empty Result to the caller, while the caller's actual
// command response is discarded because no pending request is registered
// when it arrives.
func (c *Client) consumeHandshake(p *Parser) error {
	for {
		ev, err := p.Next()
		if err != nil {
			return err
		}
		if _, ok := ev.(BeginEvent); !ok {
			c.forward(ev)
			continue
		}
		for {
			inner, err := p.Next()
			if err != nil {
				return err
			}
			switch inner.(type) {
			case EndEvent, ErrorEvent:
				return nil
			case DataEvent:
				// swallow handshake body lines (usually empty).
			default:
				c.forward(inner)
			}
		}
	}
}

// forward sends e to the events channel with a non-blocking drop policy.
// If the buffer is full we increment eventsLost and move on; blocking
// here would let a slow consumer stall the tmux protocol.
func (c *Client) forward(e Event) {
	select {
	case c.events <- e:
	default:
		c.eventsLost.Add(1)
	}
}

func joinLines(lines [][]byte) []byte {
	if len(lines) == 0 {
		return nil
	}
	total := 0
	for _, l := range lines {
		total += len(l) + 1
	}
	out := make([]byte, 0, total)
	for i, l := range lines {
		if i > 0 {
			out = append(out, '\n')
		}
		out = append(out, l...)
	}
	return out
}

// ringBuffer is a size-capped writer used for tmux's stderr. It keeps
// the most recent bytes up to a hard cap and drops older bytes silently.
// Concurrency: a single writer (os/exec) + any number of Snapshot callers.
type ringBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	cap int
}

func newRingBuffer(cap int) *ringBuffer {
	return &ringBuffer{cap: cap}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if n >= r.cap {
		r.buf.Reset()
		r.buf.Write(p[n-r.cap:])
		return n, nil
	}
	if r.buf.Len()+n > r.cap {
		overflow := r.buf.Len() + n - r.cap
		tail := r.buf.Bytes()[overflow:]
		fresh := bytes.Buffer{}
		fresh.Write(tail)
		r.buf = fresh
	}
	r.buf.Write(p)
	return n, nil
}

func (r *ringBuffer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, r.buf.Len())
	copy(out, r.buf.Bytes())
	return out
}
